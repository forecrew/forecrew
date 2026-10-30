//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
)

// claudeLabels and codexLabels are the labels of workers that have one
// runtime each.
var (
	claudeLabels = []string{"linux", "runtime:claude-code"}
	codexLabels  = []string{"linux", "runtime:codex"}
)

func TestOnboardingApproved(t *testing.T) {
	w := newWorld(t, false)
	w.startWorker("box", claudeLabels...)
	w.onboard()

	g := w.gate(domain.GateProfileApproval)
	if !strings.Contains(g.Title, "Approve the profile of app") {
		t.Errorf("gate title = %q", g.Title)
	}
	pr, err := w.tracker.GetPR(t.Context(), repo, g.SubjectNumber)
	must(t, err)
	if pr.Head != "agent/onboarding" || pr.State != "open" {
		t.Errorf("profile PR = %+v", pr)
	}
	w.decide(g, "approve", "")
	w.waitProject(domain.ProjectOnboarded)

	if got := w.mainFile(".forecrew/profile.yaml"); !strings.Contains(got, `"name":"app"`) {
		t.Errorf("profile on main = %q", got)
	}
	w.noSecretInLogs("onboarding-analyst")
	merged, err := w.tracker.GetPR(t.Context(), repo, pr.Number)
	must(t, err)
	if !merged.Merged || w.project().Profile == nil {
		t.Errorf("merged = %v, profile = %s", merged.Merged, w.project().Profile)
	}
}

// onboarded runs the onboarding of a new world with the workers it needs.
func onboarded(t *testing.T) (w *world, claude, codex *workerProc) {
	t.Helper()
	w = newWorld(t, false)
	claude, codex = w.startWorker("claude-box", claudeLabels...), w.startWorker("codex-box", codexLabels...)
	w.onboard()
	w.decide(w.gate(domain.GateProfileApproval), "approve", "")
	w.waitProject(domain.ProjectOnboarded)
	return w, claude, codex
}

// seeded is a world whose project is onboarded already, with both workers.
func seeded(t *testing.T) (w *world, claude, codex *workerProc) {
	t.Helper()
	w = newWorld(t, true)
	return w, w.startWorker("claude-box", claudeLabels...), w.startWorker("codex-box", codexLabels...)
}

const contributing = "Add a CONTRIBUTING.md section on running tests"

// mergeTask approves the merge gate of the task and returns its PR number.
func (w *world) mergeTask() int {
	w.t.Helper()
	g := w.gate(domain.GateMergeApproval)
	w.decide(g, "approve", "")
	return g.SubjectNumber
}

func TestFeatureHappyPath(t *testing.T) {
	w, claude, codex := onboarded(t)
	w.ag.Tasks = 2
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")

	w.decide(w.gate(domain.GateSpecApproval), "approve", "")
	prs := []int{w.mergeTask(), w.mergeTask()}
	w.waitGoal(goal.ID, domain.GoalDone)

	for i, n := range []string{"1", "2"} {
		if got := w.mainFile("work/task-" + n + ".txt"); !strings.Contains(got, "round 1") {
			t.Errorf("task %s file on main = %q", n, got)
		}
		pr, err := w.tracker.GetPR(t.Context(), repo, prs[i])
		must(t, err)
		if !pr.Merged || len(w.reviews(pr.Number)) != 1 {
			t.Errorf("PR %d merged = %v, reviews = %v", pr.Number, pr.Merged, w.reviews(pr.Number))
		}
	}
	for _, task := range w.tasks() {
		if task.Status != domain.TaskMerged {
			t.Errorf("%s is %s", task.Ref(), task.Status)
		}
	}
	// One job per step, and the reviewer, which needs the codex runtime, ran on the codex worker.
	for role, want := range map[string]int{"product-owner": 1, "architect": 1, "developer": 2, "reviewer": 2} {
		js := w.jobs(role)
		if len(js) != want {
			t.Errorf("%s jobs = %d, want %d", role, len(js), want)
		}
		on := claude.Name
		if role == "reviewer" {
			on = codex.Name
		}
		for _, j := range js {
			if ran := w.ag.workersOf(j.ID); j.Status != domain.JobSucceeded || !slices.Equal(ran, []string{on}) {
				t.Errorf("%s job %s is %s and ran on %v, want succeeded on %s", role, j.Ref(), j.Status, ran, on)
			}
		}
	}
}

func TestSpecFeedbackLoop(t *testing.T) {
	w, _, _ := seeded(t)
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")

	w.decide(w.gate(domain.GateSpecApproval), "request-changes", "Cover the failure output too.")
	w.decide(w.gate(domain.GateSpecApproval), "approve", "")
	w.mergeTask()
	w.waitGoal(goal.ID, domain.GoalDone)

	owners := w.jobs("product-owner")
	if len(owners) != 2 {
		t.Fatalf("product-owner jobs = %d, want 2", len(owners))
	}
	if strings.Contains(owners[0].Spec.Prompt, "Cover the failure output too.") || !strings.Contains(owners[1].Spec.Prompt, "Cover the failure output too.") {
		t.Errorf("the feedback is not in the second prompt only:\n%s", owners[1].Spec.Prompt)
	}
}

func TestReviewerRequestsChangesThenApproves(t *testing.T) {
	w, _, _ := seeded(t)
	w.ag.Approve = func(_, round int) bool { return round > 1 }
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")
	w.decide(w.gate(domain.GateSpecApproval), "approve", "")

	pr := w.mergeTask()
	w.waitGoal(goal.ID, domain.GoalDone)

	devs := w.jobs("developer")
	if len(devs) != 2 || !strings.Contains(devs[1].Spec.Prompt, "Say more.") || devs[0].HeadSHA == devs[1].HeadSHA {
		t.Fatalf("developer jobs = %+v", devs)
	}
	if got := len(w.jobs("reviewer")); got != 2 {
		t.Errorf("reviewer jobs = %d, want 2", got)
	}
	if got := w.reviews(pr); len(got) != 2 {
		t.Errorf("reviews of PR %d = %v, want two", pr, got)
	}
	if got := w.mainFile("work/task-1.txt"); !strings.Contains(got, "round 2") {
		t.Errorf("task file on main = %q", got)
	}
}

func TestFailingChecksThenFix(t *testing.T) {
	w, _, _ := seeded(t)
	w.ci.setPolicy(func(_ string, attempt int) github.CheckState {
		if attempt == 0 {
			return github.ChecksFailure
		}
		return github.ChecksSuccess
	})
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")
	w.decide(w.gate(domain.GateSpecApproval), "approve", "")

	pr := w.mergeTask()
	w.waitGoal(goal.ID, domain.GoalDone)

	devs := w.jobs("developer")
	if len(devs) != 2 || !strings.Contains(devs[1].Spec.Prompt, "Feedback to address") || devs[0].HeadSHA == devs[1].HeadSHA {
		t.Fatalf("developer jobs = %+v", devs)
	}
	first, err := w.tracker.CheckStatus(t.Context(), repo, devs[0].HeadSHA)
	must(t, err)
	last, err := w.tracker.CheckStatus(t.Context(), repo, devs[1].HeadSHA)
	must(t, err)
	if first.State != github.ChecksFailure || last.State != github.ChecksSuccess {
		t.Errorf("checks = %s then %s", first.State, last.State)
	}
	if merged, _ := w.tracker.GetPR(t.Context(), repo, pr); !merged.Merged {
		t.Errorf("PR %d is not merged", pr)
	}
}

func TestRepoWithoutChecks(t *testing.T) {
	w, _, _ := seeded(t)
	w.ci.setPolicy(func(string, int) github.CheckState { return github.ChecksNone })
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")
	w.decide(w.gate(domain.GateSpecApproval), "approve", "")

	// With no checks forecrewd waits before it believes there are none; the
	// push event GitHub sends then settles it.
	change := w.waitChange("agent/TASK-1")
	w.clock.Advance(11 * time.Minute)
	must(t, w.tracker.RequestChecks(repo, change.HeadSHA))
	g := w.gate(domain.GateMergeApproval)
	if !strings.Contains(g.Summary, "Checks: none, the repo has no checks.") {
		t.Errorf("gate summary = %q", g.Summary)
	}
	w.decide(g, "approve", "")
	w.waitGoal(goal.ID, domain.GoalDone)
}

func TestWorkerKilledMidJob(t *testing.T) {
	w := newWorld(t, true)
	started := make(chan struct{})
	var first atomic.Bool
	w.ag.Before = func(ctx context.Context, _ call) error {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-ctx.Done() // the worker is killed while the agent works
			return ctx.Err()
		}
		return nil
	}
	victim := w.startWorker("victim", claudeLabels...)
	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner"}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	<-started
	victim.kill()

	// The lease would run out after five minutes; make it so now. The
	// reaper requeues the job once, and a second worker completes it.
	_, err := w.pool.Exec(t.Context(), `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, job.ID)
	must(t, err)
	heir := w.startWorker("heir", claudeLabels...)
	eventually(t, "the job to succeed", func() bool {
		j, err := w.st.Jobs().Get(t.Context(), w.account.ID, job.ID)
		must(t, err)
		return j.Status == domain.JobSucceeded
	})
	got, err := w.st.Jobs().Get(t.Context(), w.account.ID, job.ID)
	must(t, err)
	if ran := w.ag.workersOf(job.ID); got.Attempts != 1 || !slices.Equal(ran, []string{victim.Name, heir.Name}) {
		t.Errorf("job ran on %v after %d lost attempts, want %s then %s after 1", ran, got.Attempts, victim.Name, heir.Name)
	}
}

func TestJobsGoToWorkersWithTheLabels(t *testing.T) {
	w := newWorld(t, true)
	plain := w.startWorker("plain", claudeLabels...)
	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner", Needs: []string{"gpu"}}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	eventually(t, "the job status to name the missing worker", func() bool {
		var info mcp.JobInfo
		w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info)
		return info.Status == domain.JobQueued && info.Reason == "no matching worker"
	})

	gpu := w.startWorker("gpu", append([]string{"gpu"}, claudeLabels...)...)
	eventually(t, "the job to succeed", func() bool {
		var info mcp.JobInfo
		w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info)
		return info.Status == domain.JobSucceeded
	})
	if ran := w.ag.workersOf(job.ID); !slices.Equal(ran, []string{gpu.Name}) {
		t.Errorf("job ran on %v, want only %s, not %s", ran, gpu.Name, plain.Name)
	}
}

// noSecretInLogs checks that the scripted agents of role printed the runtime
// credential in their events and that the logs the instance kept hold it
// redacted.
func (w *world) noSecretInLogs(role string) {
	w.t.Helper()
	redacted := 0
	for _, j := range w.jobs(role) {
		events, err := w.st.JobEvents().List(w.t.Context(), w.account.ID, j.ID, 0, 1000)
		must(w.t, err)
		for _, e := range events {
			if strings.Contains(e.Text, apiKey) || strings.Contains(string(e.Data), apiKey) {
				w.t.Errorf("event %d of %s holds the credential: %q", e.Seq, j.Ref(), e.Text)
			}
			if strings.Contains(e.Text, "key [REDACTED]") {
				redacted++
			}
		}
	}
	if redacted == 0 {
		w.t.Errorf("no %s event shows the redacted credential", role)
	}
}

package playbooks_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/workflow"
)

// startBugfix starts the bugfix playbook for a bug report, with the matrix at
// every commit the test engineer and the developer will push.
func startBugfix(t *testing.T) *v2Run {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	for _, ref := range []string{"agent/TASK-1", "tests-1", "tests-2", v2sha(1), v2sha(2)} {
		v.matrixFiles(t, ref)
	}
	g := domain.Goal{
		AccountID: v.account, ProjectID: v.project.ID, Kind: domain.GoalBug, Title: "Checkout crashes", Body: "Paying with a saved card panics.",
		Status: domain.GoalDraft, CreatedBy: "user:ana",
	}
	must(t, v.st.Goals().Create(t.Context(), &g))
	r := startRun(t, v, "bugfix", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	run := &v2Run{&goalRun{runner: r, goal: g, heads: map[string]int{}}}
	run.workerOnline(t, "linux-box", "docker", "linux")
	run.workerOnline(t, "windows-box", "windows")
	return run
}

// bugMap is the output of a test engineer for a bug: one test for AC-1.
func bugMap(escalation any) map[string]any {
	tests := []any{map[string]any{"criterion": "AC-1", "suite": "unit", "id": "pkg::TestAC1Charge", "file": "pkg/a_test.go"}}
	if escalation != nil {
		tests = []any{}
	}
	return map[string]any{
		"summary": "A test that pays with a saved card.", "tests": tests, "files": []any{"pkg/a_test.go"},
		"untestable": []any{}, "assumptions": []any{}, "escalation": escalation,
	}
}

// reproduce answers the test engineer of a round of the reproduction.
func (r *v2Run) reproduce(t *testing.T, round int, output map[string]any) workflow.JobRequest {
	t.Helper()
	return r.answer(t, "tasks/0/repro/"+string(rune('0'+round))+"/write_tests", output, 0.5, "tests-"+string(rune('0'+round)))
}

// baseJobs plays the base check of the regression test: the unit suite runs
// in both environments of the matrix.
func (r *v2Run) baseJobs(t *testing.T, how outcome) {
	t.Helper()
	for _, j := range r.commands(t, 2, how) {
		if j.Spec.BaseRef != "main" || j.Spec.Overlay == nil || !slices.Equal(j.Spec.Overlay.Paths, []string{"pkg/a_test.go"}) {
			t.Errorf("base check job = %+v overlay %+v", j.Spec, j.Spec.Overlay)
		}
	}
}

// A bug is reproduced by a test that fails on the default branch, fixed with
// the test locked, verified, reviewed and merged on approval; the goal is
// closed with a summary that names the test.
func TestBugfixDelivery(t *testing.T) {
	r := startBugfix(t)

	eng := r.reproduce(t, 1, bugMap(nil))
	tasks := r.tasks(t)
	if len(tasks) != 1 || tasks[0].Title != "Fix: Checkout crashes" || !strings.Contains(tasks[0].Body, "**AC-1**") {
		t.Fatalf("tasks = %+v", tasks)
	}
	if eng.Role != "test-engineer" || eng.Branch != "agent/TASK-1" || eng.TaskID != tasks[0].ID || eng.SpecID == uuid.Nil() || eng.Feedback != "" {
		t.Errorf("test engineer request = %+v", eng)
	}
	if g := r.goalNow(t); g.Status != domain.GoalPlanned || g.Kind != domain.GoalBug {
		t.Errorf("goal = %+v", g)
	}
	issue := r.goalIssue(t)
	if !slices.Contains(issue.Labels, "forecrew:bug") || !slices.Contains(issue.Labels, "forecrew:goal") {
		t.Errorf("goal issue labels = %v", issue.Labels)
	}
	if r.startedStep(t, "tasks/0/delivery/") {
		t.Error("the developer started before the base check")
	}

	r.baseJobs(t, failedRun)
	dev := r.push(t, 1)
	if dev.Role != "developer" || !slices.Equal(dev.LockedPaths, []string{"pkg/a_test.go"}) || dev.Feedback != "" {
		t.Errorf("developer request = %+v", dev)
	}
	pr := r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	if got := commandKeys(r.commands(t, 3, passedRun)); !slices.Equal(got, []string{"unit@linux-go", "e2e@linux-go", "unit@win"}) {
		t.Errorf("matrix jobs = %v", got)
	}
	r.baseJobs(t, failedRun)
	r.review(t, 0, 1, 1, approveReview())

	g := r.pendingGate(t, domain.GateMergeApproval)
	if !strings.Contains(g.Summary, "Verification: Passed: ") {
		t.Errorf("merge gate:\n%s", g.Summary)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("merged = %v, goal %s", r.prNow(t, pr.Number).Merged, r.goalNow(t).Status)
	}
	comments, err := r.tracker.ListComments(t.Context(), "acme/app", issue.Number, time.Time{})
	must(t, err)
	var summary string
	for _, c := range comments {
		if strings.Contains(c.Body, "## Done") {
			summary = c.Body
		}
	}
	if !strings.Contains(summary, "Regression tests") || !strings.Contains(summary, "pkg::TestAC1Charge") {
		t.Errorf("summary:\n%s", summary)
	}
}

// A regression test that passes on the default branch does not reproduce the
// bug: a needs_you gate shows it, and approving it with a note runs the test
// engineer again with the note.
func TestBugfixTestPassesOnBase(t *testing.T) {
	r := startBugfix(t)
	r.reproduce(t, 1, bugMap(nil))
	r.baseJobs(t, passedRun)

	g := r.pendingGate(t, domain.GateNeedsYou)
	for _, want := range []string{"passed on the base branch", "pkg::TestAC1Charge", "Approve to run the test engineer again"} {
		if !strings.Contains(g.Summary, want) {
			t.Errorf("gate lacks %q:\n%s", want, g.Summary)
		}
	}
	if r.startedStep(t, "tasks/0/delivery/") {
		t.Error("the developer started with a test that passes on base")
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "It needs two saved cards.")
	eng := r.reproduce(t, 2, bugMap(nil))
	for _, want := range []string{"passed on the base branch", "It needs two saved cards."} {
		if !strings.Contains(eng.Feedback, want) {
			t.Errorf("second request lacks %q:\n%s", want, eng.Feedback)
		}
	}
	r.baseJobs(t, failedRun)
	r.push(t, 1)
}

// A test engineer that cannot reproduce the bug asks a human, with what it
// tried; rejecting the gate stops the run and cancels the goal.
func TestBugfixCannotReproduce(t *testing.T) {
	r := startBugfix(t)
	r.reproduce(t, 1, bugMap("cannot_reproduce: paid with a saved card on web, no panic"))
	g := r.pendingGate(t, domain.GateNeedsYou)
	for _, want := range []string{"cannot_reproduce: paid with a saved card on web", "<untrusted-data"} {
		if !strings.Contains(g.Summary, want) {
			t.Errorf("gate lacks %q:\n%s", want, g.Summary)
		}
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	if r.goalNow(t).Status != domain.GoalCanceled || r.startedStep(t, "tasks/0/delivery/") {
		t.Errorf("goal %s", r.goalNow(t).Status)
	}
}

// The delivery of the bugfix playbook is the one of goal-v2, written out
// again because the engine cannot include steps from another file.
func TestBugfixDeliveryIsGoalV2s(t *testing.T) {
	delivery := func(file string) string {
		b, err := os.ReadFile("../../packs/playbooks/" + file)
		must(t, err)
		s := string(b)
		i, j := strings.Index(s, "      # One delivery per merge decision"), strings.Index(s, "  # The summary on the goal issue")
		if i < 0 || j < i {
			t.Fatalf("%s: no delivery loop", file)
		}
		return s[i:j]
	}
	if delivery("bugfix.yaml") != delivery("goal-v2.yaml") {
		t.Error("the delivery loop of bugfix.yaml differs from goal-v2.yaml: change both")
	}
}

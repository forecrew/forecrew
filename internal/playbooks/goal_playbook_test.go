package playbooks_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// goalRun is a run of the goal playbook. The test plays the workers (it
// answers each job), GitHub (checks, merges) and the human (gates).
type goalRun struct {
	*runner
	goal  domain.Goal
	heads map[string]int // head commits pushed per branch
}

// startGoal creates a goal for platforms and starts the goal playbook for it.
func startGoal(t *testing.T, platforms ...string) *goalRun {
	t.Helper()
	return startGoalOn(t, newEnv(t, domain.ProjectOnboarded), platforms...)
}

// startGoalOn is startGoal on a given env, whose fakes the test may have
// wrapped.
func startGoalOn(t *testing.T, v *env, platforms ...string) *goalRun {
	t.Helper()
	g := v.newGoal(t, platforms...)
	r := startRun(t, v, "goal", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	return &goalRun{runner: r, goal: g, heads: map[string]int{}}
}

// answer waits for the job step at path and completes its job with output.
// A job that pushes a commit reports its sha.
func (r *goalRun) answer(t *testing.T, path string, output any, cost float64, sha ...string) workflow.JobRequest {
	t.Helper()
	s := r.waitStep(t, path, domain.StepWaiting)
	raw, err := json.Marshal(output)
	must(t, err)
	r.finishJob(t, s.JobID, raw, strings.Join(sha, ""), cost)
	return r.request(s.JobID)
}

// spec answers the product owner of the round.
func (r *goalRun) spec(t *testing.T, round int, texts ...string) workflow.JobRequest {
	t.Helper()
	req := r.answer(t, fmt.Sprintf("spec/%d/write", round), specOutput(texts...), 0.5)
	r.waitStep(t, fmt.Sprintf("spec/%d/render", round), domain.StepSucceeded)
	return req
}

// approveSpec answers the product owner and approves the spec.
func (r *goalRun) approveSpec(t *testing.T) {
	t.Helper()
	r.spec(t, 1, "A payment succeeds.", "A declined card shows an error.")
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
}

// design answers the architect with tasks, one per criterion.
func (r *goalRun) design(t *testing.T, tasks ...map[string]any) workflow.JobRequest {
	t.Helper()
	return r.answer(t, "architecture/1/design", planOutput(tasks...), 0.75)
}

// oneTask approves the spec and the plan of a goal with a single task.
func (r *goalRun) oneTask(t *testing.T) {
	t.Helper()
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, []string{"go"}))
}

// build is the path of a step of the n-th round of the m-th delivery of the
// i-th task.
func build(i, m, n int, step string) string {
	return fmt.Sprintf("tasks/%d/delivery/%d/build/%d/%s", i, m, n, step)
}

func branch(task int) string { return fmt.Sprintf("agent/TASK-%d", task+1) }

// develop answers the developer job of a round. A rework round first moves
// the PR's head, as the job's push does.
func (r *goalRun) develop(t *testing.T, task, delivery, round int) workflow.JobRequest {
	t.Helper()
	r.waitStep(t, build(task, delivery, round, "dev/1/develop"), domain.StepWaiting)
	r.heads[branch(task)]++
	sha := fmt.Sprintf("%s-%d", branch(task), r.heads[branch(task)])
	if pr, err := r.tracker.FindPR(t.Context(), "acme/app", branch(task)); err == nil {
		must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, sha))
	}
	return r.answer(t, build(task, delivery, round, "dev/1/develop"), developerOutput(fmt.Sprintf("Round %d.", round)), 1, sha)
}

// pr waits for the PR of the task to be opened or updated in the round.
func (r *goalRun) pr(t *testing.T, task, delivery, round int) github.PullRequest {
	t.Helper()
	r.waitStep(t, build(task, delivery, round, "pr"), domain.StepSucceeded)
	pr, err := r.tracker.FindPR(t.Context(), "acme/app", branch(task))
	must(t, err)
	return pr
}

// checks reports the conclusion of the checks of the PR head, as the webhook
// does, once the playbook waits for it.
func (r *goalRun) checks(t *testing.T, task, delivery, round int, conclusion domain.CheckConclusion) {
	t.Helper()
	r.waitStep(t, build(task, delivery, round, "gated/0/checks"), domain.StepWaiting)
	pr := r.pr(t, task, delivery, round)
	states := map[domain.CheckConclusion]github.CheckState{domain.ChecksSuccess: github.ChecksSuccess, domain.ChecksFailure: github.ChecksFailure}
	if state, ok := states[conclusion]; ok {
		must(t, r.tracker.SetCheckStatus("acme/app", pr.HeadSHA, state, 1))
	}
	r.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Conclusion: conclusion})
}

// review answers the reviewer of the round.
func (r *goalRun) review(t *testing.T, task, delivery, round int, output map[string]any) workflow.JobRequest {
	t.Helper()
	return r.answer(t, build(task, delivery, round, "gated/0/review"), output, 0.25)
}

// green runs a round with passing checks and an approving review, up to the
// merge gate.
func (r *goalRun) green(t *testing.T, task, delivery, round int) domain.Gate {
	t.Helper()
	r.develop(t, task, delivery, round)
	r.checks(t, task, delivery, round, domain.ChecksSuccess)
	r.review(t, task, delivery, round, approveReview())
	return r.pendingGate(t, domain.GateMergeApproval)
}

func (r *goalRun) tasks(t *testing.T) []domain.Task {
	t.Helper()
	ts, err := r.st.Tasks().List(t.Context(), r.account, store.TaskFilter{GoalID: r.goal.ID})
	must(t, err)
	return ts
}

func (r *goalRun) goalNow(t *testing.T) domain.Goal { return r.env.goalNow(t, r.goal.ID) }

func (r *goalRun) issue(t *testing.T, n int) github.Issue {
	t.Helper()
	i, err := r.tracker.GetIssue(t.Context(), "acme/app", n)
	must(t, err)
	return i
}

// goalIssue returns the goal's GitHub issue.
func (r *goalRun) goalIssue(t *testing.T) github.Issue {
	t.Helper()
	return r.issue(t, r.goalNow(t).IssueNumber)
}

// prNow returns the PR with number n.
func (r *goalRun) prNow(t *testing.T, n int) github.PullRequest {
	t.Helper()
	pr, err := r.tracker.GetPR(t.Context(), "acme/app", n)
	must(t, err)
	return pr
}

// startedStep reports whether any step run starts with prefix.
func (r *goalRun) startedStep(t *testing.T, prefix string) bool {
	t.Helper()
	steps, err := r.st.StepRuns().ListByRun(t.Context(), r.account, r.run.ID)
	must(t, err)
	return slices.ContainsFunc(steps, func(s domain.StepRun) bool { return strings.HasPrefix(s.StepID, prefix) })
}

// reviewEvents returns the events of every review on the PRs of the repo.
func (r *goalRun) reviewEvents(t *testing.T, prs ...int) []github.ReviewEvent {
	t.Helper()
	var out []github.ReviewEvent
	for _, n := range prs {
		rs, err := r.tracker.ListReviews(t.Context(), "acme/app", n)
		must(t, err)
		for _, rv := range rs {
			out = append(out, rv.Event)
		}
	}
	return out
}

func TestGoalHappyPath(t *testing.T) {
	r := startGoal(t, "web")
	specID := r.specPhase(t)
	prs := []int{r.firstTask(t, specID)}
	prs = append(prs, r.secondTask(t, prs[0]))

	r.waitRun(t, domain.RunSucceeded)
	if r.goalNow(t).Status != domain.GoalDone || r.goalIssue(t).State != "closed" || !r.prNow(t, prs[1]).Merged {
		t.Errorf("goal %s, issue %s", r.goalNow(t).Status, r.goalIssue(t).State)
	}
	for i, task := range r.tasks(t) {
		c, err := r.st.Changes().List(t.Context(), r.account, store.ChangeFilter{TaskID: task.ID})
		must(t, err)
		if task.Status != domain.TaskMerged || len(c) != 1 || c[0].State != domain.ChangeMerged || c[0].Number != prs[i] {
			t.Errorf("task %d = %+v, changes %+v", i+1, task, c)
		}
	}
	// Cost: 0.5 + 0.75 + 2 * (1 developer + 0.25 reviewer).
	comments, _ := r.tracker.ListComments(t.Context(), "acme/app", r.goalNow(t).IssueNumber, t0.Add(-1))
	last := comments[len(comments)-1].Body
	for _, want := range []string{"GOAL-1 is delivered: 2 tasks merged.", "TASK-1 Charge cards: PR #", "1 developer run, $1.25", "Cost of the 6 jobs of the goal: $3.75."} {
		if !strings.Contains(last, want) {
			t.Errorf("summary lacks %q:\n%s", want, last)
		}
	}
	if got := r.reviewEvents(t, prs...); !slices.Equal(got, []github.ReviewEvent{github.ReviewComment, github.ReviewComment}) {
		t.Errorf("reviews = %v", got)
	}
	if r.startedStep(t, "finally/") {
		t.Error("finally steps ran in a run that succeeded")
	}
}

// specPhase runs the spec and the plan of the happy path and returns the
// approved spec's ID.
func (r *goalRun) specPhase(t *testing.T) uuid.UUID {
	t.Helper()
	po := r.spec(t, 1, "A payment succeeds.", "A declined card shows an error.")
	if po.Role != "product-owner" || po.GoalID != r.goal.ID || po.Repo != "acme/app" || po.Feedback != "" || len(po.Needs) != 0 {
		t.Errorf("product owner request = %+v", po)
	}
	issue := r.goalIssue(t)
	if issue.Title != "GOAL-1: Add checkout" || !slices.Equal(issue.Labels, []string{"forecrew:goal"}) ||
		!strings.Contains(issue.Body, "Customers can pay by card.") || !strings.Contains(issue.Body, "**AC-2** A declined card shows an error.") {
		t.Errorf("goal issue = %+v", issue)
	}
	g := r.pendingGate(t, domain.GateSpecApproval)
	if g.SubjectRepo != "acme/app" || g.SubjectNumber != issue.Number || g.GoalID != r.goal.ID || g.SpecID == uuid.Nil() ||
		!strings.Contains(g.Summary, "2 acceptance criteria") || r.goalNow(t).Status != domain.GoalSpecReview {
		t.Errorf("spec gate = %+v, goal %s", g, r.goalNow(t).Status)
	}
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
	arch := r.design(t,
		planTask("Charge cards", []string{"AC-1"}, []string{"go", "linux"}),
		planTask("Show declines", []string{"AC-2"}, []string{"node"}, 1))
	if arch.Role != "architect" || arch.SpecID != g.SpecID || arch.GoalID != r.goal.ID {
		t.Errorf("architect request = %+v", arch)
	}
	return g.SpecID
}

// firstTask runs the first task up to its approval and returns its PR's number.
func (r *goalRun) firstTask(t *testing.T, specID uuid.UUID) int {
	t.Helper()
	dev := r.develop(t, 0, 1, 1)
	tasks := r.tasks(t)
	if len(tasks) != 2 || dev.Role != "developer" || dev.Branch != "agent/TASK-1" || dev.TaskID != tasks[0].ID || dev.SpecID != specID ||
		!slices.Equal(dev.Needs, []string{"go", "linux", "web"}) || dev.Feedback != "" || dev.Repo != "acme/app" {
		t.Fatalf("developer request = %+v, tasks %+v", dev, tasks)
	}
	if got := r.goalNow(t).Status; got != domain.GoalInProgress {
		t.Errorf("goal status = %s", got)
	}
	pr := r.pr(t, 0, 1, 1)
	if pr.Base != "main" || pr.Title != "TASK-1: Charge cards" || !strings.Contains(pr.Body, "Round 1.") ||
		!strings.Contains(pr.Body, fmt.Sprintf("Closes #%d", tasks[0].IssueNumber)) || !strings.Contains(pr.Body, "**AC-1** A payment succeeds.") {
		t.Errorf("pr = %+v", pr)
	}
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	rev := r.review(t, 0, 1, 1, approveReview())
	if rev.Role != "reviewer" || rev.Branch != "agent/TASK-1" || rev.TaskID != tasks[0].ID || rev.Repo != "acme/app" {
		t.Errorf("reviewer request = %+v", rev)
	}
	mg := r.pendingGate(t, domain.GateMergeApproval)
	if mg.SubjectNumber != pr.Number || mg.TaskID != tasks[0].ID || !strings.Contains(mg.Title, "TASK-1: Charge cards") ||
		!strings.Contains(mg.Summary, "Checks: success.") || !strings.Contains(mg.Summary, "Review: approve") || !strings.Contains(mg.Summary, "Cards only") {
		t.Errorf("merge gate = %+v", mg)
	}
	// Nothing merges before the human approves: the reviewer only comments
	// and the next task waits.
	if got := r.reviewEvents(t, pr.Number); !slices.Equal(got, []github.ReviewEvent{github.ReviewComment}) {
		t.Errorf("reviews = %v", got)
	}
	if r.prNow(t, pr.Number).Merged || r.startedStep(t, "tasks/1/") || r.taskStatus(t, 0) != domain.TaskMergeReady {
		t.Errorf("merged = %v, task = %s", r.prNow(t, pr.Number).Merged, r.taskStatus(t, 0))
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	return pr.Number
}

// secondTask runs the second task, after the first one's PR merged, up to its
// approval, and returns its PR's number.
func (r *goalRun) secondTask(t *testing.T, firstPR int) int {
	t.Helper()
	dev := r.develop(t, 1, 1, 1)
	tasks := r.tasks(t)
	if dev.Branch != "agent/TASK-2" || !slices.Equal(dev.Needs, []string{"node", "web"}) || dev.TaskID != tasks[1].ID {
		t.Errorf("second developer request = %+v", dev)
	}
	if !r.prNow(t, firstPR).Merged || tasks[0].Status != domain.TaskMerged {
		t.Errorf("task 1: merged = %v, status %s", r.prNow(t, firstPR).Merged, tasks[0].Status)
	}
	r.checks(t, 1, 1, 1, domain.ChecksSuccess)
	r.review(t, 1, 1, 1, approveReview())
	pr := r.pr(t, 1, 1, 1)
	r.pendingGate(t, domain.GateMergeApproval)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	return pr.Number
}

func (r *goalRun) taskStatus(t *testing.T, i int) domain.TaskStatus {
	t.Helper()
	return r.tasks(t)[i].Status
}

func TestGoalSpecFeedback(t *testing.T) {
	r := startGoal(t)
	r.spec(t, 1, "A payment succeeds.")
	first := r.pendingGate(t, domain.GateSpecApproval)
	r.decide(t, domain.GateSpecApproval, mcp.ToolRequestChanges, "Also cover declined cards.")

	req := r.spec(t, 2, "A payment succeeds.", "A declined card shows an error.")
	if req.Role != "product-owner" || !strings.Contains(req.Feedback, "Also cover declined cards.") {
		t.Errorf("second request = %+v", req)
	}
	var second domain.Gate
	eventually(t, "the second spec gate", func() bool {
		gs, err := r.gates.ListPending(t.Context(), r.account, r.project.ID)
		must(t, err)
		for _, g := range gs {
			if g.Kind == domain.GateSpecApproval && g.ID != first.ID {
				second = g
			}
		}
		return second.ID != uuid.Nil()
	})
	if second.SpecID == first.SpecID || !strings.Contains(second.Title, "revision 2") {
		t.Errorf("gates: %+v then %+v", first, second)
	}
	if body := r.goalIssue(t).Body; !strings.Contains(body, "revision 2") || strings.Contains(body, "revision 1") || !strings.Contains(body, "AC-2") {
		t.Errorf("goal issue body:\n%s", body)
	}
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
	arch := r.design(t, planTask("Charge cards", []string{"AC-1", "AC-2"}, nil))
	if arch.SpecID != second.SpecID {
		t.Errorf("the architect got spec %s, want the approved %s", arch.SpecID, second.SpecID)
	}
}

// The fourth spec round needs a needs_you gate.
func TestGoalSpecRoundsRunOut(t *testing.T) {
	r := startGoal(t)
	for round := 1; round <= 3; round++ {
		r.spec(t, round, "A payment succeeds.")
		r.decide(t, domain.GateSpecApproval, mcp.ToolRequestChanges, fmt.Sprintf("Change %d.", round))
	}
	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "not met after 3 iterations") {
		t.Errorf("gate = %+v", g)
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.spec(t, 4, "A payment succeeds.")
	r.decide(t, domain.GateSpecApproval, mcp.ToolApprove, "")
	r.design(t, planTask("Charge cards", []string{"AC-1"}, nil))
	r.develop(t, 0, 1, 1)
}

func TestGoalSpecRejected(t *testing.T) {
	r := startGoal(t)
	r.spec(t, 1, "A payment succeeds.")
	r.decide(t, domain.GateSpecApproval, mcp.ToolReject, "No.")
	r.waitRun(t, domain.RunCanceled)
	r.waitStep(t, "finally/settle", domain.StepSucceeded)
	if r.goalNow(t).Status != domain.GoalCanceled || r.goalIssue(t).State != "closed" {
		t.Errorf("goal %s, issue %s", r.goalNow(t).Status, r.goalIssue(t).State)
	}
}

func TestGoalFailingChecks(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksFailure)
	// The reviewer reviews even when the checks failed.
	r.review(t, 0, 1, 1, approveReview())

	dev := r.develop(t, 0, 1, 2)
	if dev.Role != "developer" || dev.Branch != "agent/TASK-1" || !strings.Contains(dev.Feedback, "The checks of") || !strings.Contains(dev.Feedback, "failed") ||
		strings.Contains(dev.Feedback, "The reviewer asks") {
		t.Errorf("rework request = %+v", dev)
	}
	if r.startedStep(t, "tasks/0/delivery/1/approval/0/") {
		t.Error("a merge gate opened for failing checks")
	}
	pr := r.pr(t, 0, 1, 2)
	r.checks(t, 0, 1, 2, domain.ChecksSuccess)
	r.review(t, 0, 1, 2, approveReview())
	g := r.pendingGate(t, domain.GateMergeApproval)
	if g.SubjectNumber != pr.Number || !strings.Contains(g.Summary, "Checks: success.") {
		t.Errorf("gate = %+v", g)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged {
		t.Error("pr not merged")
	}
}

func TestGoalReviewerRequestsChanges(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	pr := r.pr(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, reviewOutput("request_changes"))

	dev := r.develop(t, 0, 1, 2)
	for _, want := range []string{"The reviewer asks for changes: The charge is not retried.", "`pay.go:12` **major**: Retry on timeout.", "AC-1 is not met: No retry test."} {
		if !strings.Contains(dev.Feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, dev.Feedback)
		}
	}
	if strings.Contains(dev.Feedback, "The checks of") {
		t.Errorf("feedback mentions the checks:\n%s", dev.Feedback)
	}
	// The same PR is updated, and the first review stays on it.
	if again := r.pr(t, 0, 1, 2); again.Number != pr.Number {
		t.Errorf("round 2 PR = #%d, round 1 #%d", again.Number, pr.Number)
	}
	if got := r.reviewEvents(t, pr.Number); !slices.Equal(got, []github.ReviewEvent{github.ReviewComment}) {
		t.Errorf("reviews = %v", got)
	}
	r.checks(t, 0, 1, 2, domain.ChecksSuccess)
	r.review(t, 0, 1, 2, approveReview())
	r.pendingGate(t, domain.GateMergeApproval)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if got := r.reviewEvents(t, pr.Number); !slices.Equal(got, []github.ReviewEvent{github.ReviewComment, github.ReviewComment}) {
		t.Errorf("reviews = %v", got)
	}
}

func TestGoalChangesRequestedAtTheMergeGate(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	first := r.green(t, 0, 1, 1)
	pr := r.pr(t, 0, 1, 1)
	r.decide(t, domain.GateMergeApproval, mcp.ToolRequestChanges, "Use cents everywhere.")

	// Back to the developer, in a new delivery, with the human's words.
	dev := r.develop(t, 0, 2, 1)
	if dev.Feedback != "Use cents everywhere." || dev.Branch != "agent/TASK-1" {
		t.Errorf("developer request = %+v", dev)
	}
	if r.prNow(t, pr.Number).Merged || r.taskStatus(t, 0) != domain.TaskInProgress {
		t.Errorf("merged = %v, task %s", r.prNow(t, pr.Number).Merged, r.taskStatus(t, 0))
	}
	if again := r.pr(t, 0, 2, 1); again.Number != pr.Number {
		t.Errorf("PR #%d, want #%d", again.Number, pr.Number)
	}
	r.checks(t, 0, 2, 1, domain.ChecksSuccess)
	r.review(t, 0, 2, 1, approveReview())
	var second domain.Gate
	eventually(t, "the second merge gate", func() bool {
		gs, err := r.gates.ListPending(t.Context(), r.account, r.project.ID)
		must(t, err)
		for _, g := range gs {
			if g.Kind == domain.GateMergeApproval && g.ID != first.ID {
				second = g
			}
		}
		return second.ID != uuid.Nil()
	})
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("merged = %v, goal %s", r.prNow(t, pr.Number).Merged, r.goalNow(t).Status)
	}
}

// Three rounds that need rework end in a needs_you gate; approving it gives
// one more round, rejecting it ends the run.
func TestGoalEscalationAtMaxIterations(t *testing.T) {
	fail := func(t *testing.T) *goalRun {
		r := startGoal(t)
		r.oneTask(t)
		for round := 1; round <= 3; round++ {
			r.develop(t, 0, 1, round)
			r.checks(t, 0, 1, round, domain.ChecksFailure)
			r.review(t, 0, 1, round, reviewOutput("request_changes"))
		}
		g := r.pendingGate(t, domain.GateNeedsYou)
		if !strings.Contains(g.Summary, "not met after 3 iterations") {
			t.Errorf("gate = %+v", g)
		}
		if r.startedStep(t, build(0, 1, 4, "")) {
			t.Error("a fourth round started without a human")
		}
		return r
	}

	t.Run("approved", func(t *testing.T) {
		r := fail(t)
		r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
		dev := r.develop(t, 0, 1, 4)
		if !strings.Contains(dev.Feedback, "The reviewer asks for changes") {
			t.Errorf("fourth round request = %+v", dev)
		}
		r.checks(t, 0, 1, 4, domain.ChecksSuccess)
		r.review(t, 0, 1, 4, approveReview())
		r.pendingGate(t, domain.GateMergeApproval)
		r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
		r.waitRun(t, domain.RunSucceeded)
	})

	t.Run("rejected", func(t *testing.T) {
		r := fail(t)
		pr := r.pr(t, 0, 1, 3)
		r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
		r.waitRun(t, domain.RunCanceled)
		r.waitStep(t, "finally/settle", domain.StepSucceeded)
		ts := r.tasks(t)
		if r.goalNow(t).Status != domain.GoalCanceled || ts[0].Status != domain.TaskClosed || r.prNow(t, pr.Number).Merged ||
			r.goalIssue(t).State != "closed" || r.issue(t, ts[0].IssueNumber).State != "closed" {
			t.Errorf("goal %s, task %s, merged %v", r.goalNow(t).Status, ts[0].Status, r.prNow(t, pr.Number).Merged)
		}
	})
}

// A repo without checks reports the conclusion none, and the playbook goes on
// to the review.
func TestGoalRepoWithoutChecks(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksNone)
	r.review(t, 0, 1, 1, approveReview())
	g := r.pendingGate(t, domain.GateMergeApproval)
	if !strings.Contains(g.Summary, "Checks: none, the repo has no checks.") {
		t.Errorf("gate summary:\n%s", g.Summary)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, g.SubjectNumber).Merged {
		t.Error("pr not merged")
	}
}

// A human merges the PR on GitHub while the gate is open: it counts as the
// approval and the playbook does not merge again.
func TestGoalMergedOnGitHubAtTheGate(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	g := r.green(t, 0, 1, 1)
	_, err := r.tracker.MergePR(t.Context(), "acme/app", g.SubjectNumber, github.MergeInput{})
	must(t, err)
	// What the webhook does.
	_, err = r.gates.Resolve(t.Context(), r.github(), g.ID, domain.GateApproved, "")
	must(t, err)

	r.waitRun(t, domain.RunSucceeded)
	sigs, err := r.bus.Lookup(t.Context(), r.account, domain.PRKey("acme/app", g.SubjectNumber))
	must(t, err)
	if len(sigs) != 0 || r.taskStatus(t, 0) != domain.TaskMerged || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("%d pr signals, task %s, goal %s", len(sigs), r.taskStatus(t, 0), r.goalNow(t).Status)
	}
}

// A human merges the PR while the reviewer is still working: no gate opens.
func TestGoalMergedOnGitHubBeforeTheGate(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	pr := r.pr(t, 0, 1, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.waitStep(t, build(0, 1, 1, "gated/0/review"), domain.StepWaiting)
	_, err := r.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	r.review(t, 0, 1, 1, approveReview())

	r.waitRun(t, domain.RunSucceeded)
	if r.startedStep(t, "tasks/0/delivery/1/approval/0/") || r.taskStatus(t, 0) != domain.TaskMerged {
		t.Errorf("task %s", r.taskStatus(t, 0))
	}
}

// A human closes the PR at the gate: the run is canceled and nothing merges.
func TestGoalClosedOnGitHubAtTheGate(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	g := r.green(t, 0, 1, 1)
	must(t, r.tracker.ClosePR("acme/app", g.SubjectNumber))
	_, err := r.gates.Resolve(t.Context(), r.github(), g.ID, domain.GateRejected, "")
	must(t, err)
	r.waitRun(t, domain.RunCanceled)
	r.waitStep(t, "finally/settle", domain.StepSucceeded)
	if r.prNow(t, g.SubjectNumber).Merged || r.goalNow(t).Status != domain.GoalCanceled || r.taskStatus(t, 0) != domain.TaskClosed {
		t.Errorf("goal %s, task %s", r.goalNow(t).Status, r.taskStatus(t, 0))
	}
}

// A merge the GitHub refuses (failing checks that appeared after the review)
// opens a needs_you gate and never merges around it.
func TestGoalMergeFailureEscalates(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	g := r.green(t, 0, 1, 1)
	pr := r.prNow(t, g.SubjectNumber)
	must(t, r.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksFailure, 1))
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	esc := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(esc.Summary, "checks failed") || r.prNow(t, pr.Number).Merged {
		t.Errorf("gate = %+v", esc)
	}
	must(t, r.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksSuccess, 1))
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged {
		t.Error("pr not merged after the retry")
	}
}

// An architect whose plan leaves a criterion uncovered gets one more try,
// with the criterion in its feedback; nothing is stored from the first plan.
func TestGoalPlanRerunsForUncoveredCriteria(t *testing.T) {
	r := startGoal(t)
	r.approveSpec(t)
	r.design(t, planTask("Charge cards", []string{"AC-1"}, nil))
	req := r.answer(t, "architecture/2/design", planOutput(planTask("Charge cards", []string{"AC-1", "AC-2"}, nil)), 0.75)
	if !strings.Contains(req.Feedback, "does not cover these acceptance criteria: AC-2") {
		t.Errorf("rerun request = %+v", req)
	}
	dev := r.develop(t, 0, 1, 1)
	if ts := r.tasks(t); len(ts) != 1 || dev.TaskID != ts[0].ID {
		t.Errorf("tasks = %+v", ts)
	}
}

// Still uncovered after the second try: a needs_you gate asks a human.
// Rejecting it ends the run; approving it reruns the architect, whose
// feedback is the last plan's problem.
func TestGoalPlanStaysUncovered(t *testing.T) {
	uncovered := func(t *testing.T) *goalRun {
		r := startGoal(t)
		r.approveSpec(t)
		r.design(t, planTask("Charge cards", []string{"AC-1"}, nil))
		r.answer(t, "architecture/2/design", planOutput(planTask("Charge cards", []string{"AC-1"}, nil)), 0.75)
		g := r.pendingGate(t, domain.GateNeedsYou)
		if !strings.Contains(g.Summary, "not met after 2 iterations") || len(r.tasks(t)) != 0 {
			t.Errorf("gate = %+v, %d tasks", g, len(r.tasks(t)))
		}
		// The approver can read why on the goal issue.
		comments, _ := r.tracker.ListComments(t.Context(), "acme/app", r.goalNow(t).IssueNumber, t0.Add(-1))
		var problems int
		for _, c := range comments {
			if strings.Contains(c.Body, "forecrew:plan-problem:") && strings.Contains(c.Body, "does not cover these acceptance criteria: AC-2") {
				problems++
			}
		}
		if problems != 2 {
			t.Errorf("%d plan problems commented, want 2", problems)
		}
		return r
	}

	t.Run("approved", func(t *testing.T) {
		r := uncovered(t)
		r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
		req := r.answer(t, "architecture/3/design", planOutput(planTask("Charge cards", []string{"AC-1", "AC-2"}, nil)), 0.75)
		if !strings.Contains(req.Feedback, "does not cover these acceptance criteria: AC-2") {
			t.Errorf("third request = %+v", req)
		}
		r.develop(t, 0, 1, 1)
		if r.goalNow(t).Status != domain.GoalInProgress || len(r.tasks(t)) != 1 {
			t.Errorf("goal %s, %d tasks", r.goalNow(t).Status, len(r.tasks(t)))
		}
	})

	t.Run("rejected", func(t *testing.T) {
		r := uncovered(t)
		r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
		r.waitRun(t, domain.RunCanceled)
		r.waitStep(t, "finally/settle", domain.StepSucceeded)
		if r.goalNow(t).Status != domain.GoalCanceled {
			t.Errorf("goal %s", r.goalNow(t).Status)
		}
	})
}

// A developer that stops with an escalation opens no PR: a needs_you gate
// asks a human, the reason is on the task's issue, and approving the gate
// reruns the developer.
func TestGoalDeveloperEscalates(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.waitStep(t, build(0, 1, 1, "dev/1/develop"), domain.StepWaiting)
	stuck := developerOutput("Stopped.")
	stuck["escalation"] = "The payment sandbox is missing."
	r.answer(t, build(0, 1, 1, "dev/1/develop"), stuck, 1)
	r.pendingGate(t, domain.GateNeedsYou)
	issue := r.issue(t, r.tasks(t)[0].IssueNumber)
	comments, _ := r.tracker.ListComments(t.Context(), "acme/app", issue.Number, t0.Add(-1))
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "The payment sandbox is missing.") {
		t.Errorf("comments = %+v", comments)
	}
	if _, err := r.tracker.FindPR(t.Context(), "acme/app", "agent/TASK-1"); err == nil || r.startedStep(t, build(0, 1, 1, "pr")) {
		t.Error("a PR was opened for an escalated job")
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.answer(t, build(0, 1, 1, "dev/2/develop"), developerOutput("Done."), 1, "agent/TASK-1-1")
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.review(t, 0, 1, 1, approveReview())
	r.pendingGate(t, domain.GateMergeApproval)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
}

// request returns the request of the job with ID.
func (r *runner) request(id uuid.UUID) workflow.JobRequest {
	for _, req := range r.jobs.Requests() {
		if req.ID == id {
			return req
		}
	}
	return workflow.JobRequest{}
}

// task_done fails after the merge and a human rejects the needs_you gate:
// the task stays merged and only the goal is canceled.
func TestGoalSettleAfterAFailureFollowingTheMerge(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarded)
	v.wrapped = failMerged{v.tracker}
	v.register()
	r := startGoalOn(t, v)
	r.oneTask(t)
	g := r.green(t, 0, 1, 1)
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	esc := r.pendingGate(t, domain.GateNeedsYou)
	if !r.prNow(t, g.SubjectNumber).Merged || !strings.Contains(esc.Summary, "boom") {
		t.Fatalf("merged = %v, gate = %+v", r.prNow(t, g.SubjectNumber).Merged, esc)
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	r.waitStep(t, "finally/settle", domain.StepSucceeded)
	if r.taskStatus(t, 0) != domain.TaskMerged || r.goalNow(t).Status != domain.GoalCanceled {
		t.Errorf("task %s, goal %s", r.taskStatus(t, 0), r.goalNow(t).Status)
	}
}

// A human merges the PR on GitHub while a needs_you gate is pending and then
// rejects the gate: the task is merged, not closed.
func TestGoalSettleAfterAHumanMerge(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	g := r.green(t, 0, 1, 1)
	pr := r.prNow(t, g.SubjectNumber)
	must(t, r.tracker.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksFailure, 1))
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.pendingGate(t, domain.GateNeedsYou)
	_, err := r.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	r.decide(t, domain.GateNeedsYou, mcp.ToolReject, "")
	r.waitRun(t, domain.RunCanceled)
	r.waitStep(t, "finally/settle", domain.StepSucceeded)
	if r.taskStatus(t, 0) != domain.TaskMerged || r.goalNow(t).Status != domain.GoalCanceled {
		t.Errorf("task %s, goal %s", r.taskStatus(t, 0), r.goalNow(t).Status)
	}
}

// The checks are those of the commit the developer pushed, even when the PR
// reports another head: a rework push the API does not show yet must not be
// judged by the checks of the commit before, which are stored already.
func TestGoalChecksAreOfTheDevelopersCommit(t *testing.T) {
	r := startGoal(t)
	r.oneTask(t)
	r.develop(t, 0, 1, 1)
	stale := r.pr(t, 0, 1, 1).HeadSHA
	r.checks(t, 0, 1, 1, domain.ChecksFailure)
	r.review(t, 0, 1, 1, approveReview())

	// Round 2: the job pushed "job-2", the PR still shows the old head.
	r.answer(t, build(0, 1, 2, "dev/1/develop"), developerOutput("Round 2."), 1, "job-2")
	s := r.waitStep(t, build(0, 1, 2, "gated/0/checks"), domain.StepWaiting)
	if want := domain.ChecksKey("acme/app", "job-2"); s.WaitKey != want {
		t.Fatalf("the checks wait is on %s, want %s", s.WaitKey, want)
	}
	pr := r.pr(t, 0, 1, 2)
	if pr.HeadSHA != stale {
		t.Fatalf("PR head = %s, want the stale %s", pr.HeadSHA, stale)
	}
	r.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: "job-2", Conclusion: domain.ChecksSuccess})
	r.review(t, 0, 1, 2, approveReview())
	g := r.pendingGate(t, domain.GateMergeApproval)
	if !strings.Contains(g.Summary, "Checks: success.") || !strings.Contains(g.Summary, " at job-2.") || strings.Contains(g.Summary, stale) {
		t.Errorf("gate summary:\n%s", g.Summary)
	}
}

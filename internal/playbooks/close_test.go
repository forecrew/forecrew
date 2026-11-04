package playbooks_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// costJob stores a job of role for the goal (and task, when it has an ID)
// that succeeded and cost cost.
func (v *env) costJob(t *testing.T, role string, goal domain.Goal, task uuid.UUID, cost float64) {
	t.Helper()
	ctx := t.Context()
	id, err := newJobCreator(v).Create(ctx, workflow.JobRequest{
		AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Role: role, GoalID: goal.ID, TaskID: task,
	})
	must(t, err)
	w := domain.Worker{AccountID: v.account, Name: "w-" + id.String(), TokenHash: []byte(id.String())}
	must(t, v.st.Workers().Create(ctx, &w))
	// The queue is first in, first out: jobs queued before finish first.
	for {
		j, err := v.st.Jobs().LeaseNext(ctx, v.account, w.ID, []string{domain.RuntimeLabel("")}, time.Hour)
		must(t, err)
		res := store.JobResult{Status: domain.JobSucceeded, Output: []byte(`{}`)}
		if j.ID == id {
			res.CostUSD = cost
		}
		_, err = v.st.Jobs().Complete(ctx, v.account, j.ID, w.ID, res)
		must(t, err)
		if j.ID == id {
			return
		}
	}
}

// delivered is a planned goal with two tasks that are merged, a PR for each
// and jobs that cost 0.50 (product owner; the one of the spec step is free), 0.75 and 0.25 (developer runs of
// the first task), 1.00 (reviewer of the first) and 2.00 (developer of the
// second).
func delivered(t *testing.T) (*planned, []domain.Task) {
	t.Helper()
	p := newPlanned(t)
	tasks := p.plan(t, "One", "Two")
	p.costJob(t, "product-owner", p.goal, uuid.Nil(), 0.5)
	p.costJob(t, "developer", p.goal, tasks[0].ID, 0.75)
	p.costJob(t, "developer", p.goal, tasks[0].ID, 0.25)
	p.costJob(t, "reviewer", p.goal, tasks[0].ID, 1)
	p.costJob(t, "developer", p.goal, tasks[1].ID, 2)
	must(t, p.st.Goals().UpdateStatus(t.Context(), p.account, p.goal.ID, domain.GoalPlanned, domain.GoalInProgress))
	for i, task := range tasks {
		for _, s := range taskPathTo(domain.TaskMerged) {
			must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, task.ID, p.taskNow(t, task.ID).Status, s))
		}
		c := domain.Change{
			AccountID: p.account, ProjectID: p.project.ID, TaskID: task.ID, Repo: "acme/app", Number: 40 + i,
			Branch: "agent/" + task.Ref(), BaseRef: "main", HeadSHA: "abc", State: domain.ChangeMerged, URL: "https://github.com/acme/app/pull/" + itoa(40+i),
		}
		must(t, p.st.Changes().Upsert(t.Context(), &c))
	}
	return p, tasks
}

func TestGoalClose(t *testing.T) {
	p, tasks := delivered(t)
	with := map[string]any{"goal_id": p.goal.ID.String()}
	out, err := p.callKey(t, "goal.close", "k", with)
	must(t, err)
	if m := toMap(t, out); m["tasks"] != 2.0 || m["cost_usd"] != 4.5 {
		t.Errorf("output = %v", m)
	}
	comments := p.goalComments(t)
	if len(comments) != 1 {
		t.Fatalf("%d comments", len(comments))
	}
	for _, want := range []string{
		"GOAL-1 is delivered: 2 tasks merged.",
		"#" + itoa(tasks[0].IssueNumber) + " TASK-1 One: PR #40 (https://github.com/acme/app/pull/40), 2 developer runs, $2.00",
		"#" + itoa(tasks[1].IssueNumber) + " TASK-2 Two: PR #41 (https://github.com/acme/app/pull/41), 1 developer run, $2.00",
		"Cost of the 6 jobs of the goal: $4.50.",
	} {
		if !strings.Contains(comments[0].Body, want) {
			t.Errorf("comment lacks %q:\n%s", want, comments[0].Body)
		}
	}
	issue, _ := p.tracker.GetIssue(t.Context(), "acme/app", p.issue)
	if issue.State != "closed" || p.goalNow(t, p.goal.ID).Status != domain.GoalDone {
		t.Errorf("issue %s, goal %s", issue.State, p.goalNow(t, p.goal.ID).Status)
	}

	// Closing again changes nothing.
	out, err = p.callKey(t, "goal.close", "k", with)
	must(t, err)
	comments = p.goalComments(t)
	if len(comments) != 1 || toMap(t, out)["cost_usd"] != 4.5 {
		t.Errorf("second call: %d comments, %v", len(comments), out)
	}
}

// A call that stopped after the comment closes the issue without a second one.
func TestGoalCloseResumes(t *testing.T) {
	p, _ := delivered(t)
	with := map[string]any{"goal_id": p.goal.ID.String()}
	p.wrapped = failing{Tracker: p.tracker, errs: map[string]error{"UpdateIssue": errors.New("boom")}}
	p.register()
	_, err := p.call(t, "goal.close", with)
	wantErr(t, err, "close issue")
	p.wrapped = nil
	p.register()
	_, err = p.call(t, "goal.close", with)
	must(t, err)
	comments := p.goalComments(t)
	if len(comments) != 1 || p.goalNow(t, p.goal.ID).Status != domain.GoalDone {
		t.Errorf("%d comments, goal %s", len(comments), p.goalNow(t, p.goal.ID).Status)
	}
}

func TestGoalCloseErrors(t *testing.T) {
	p, tasks := delivered(t)
	other := newPlanned(t)
	other.plan(t, "One")
	_, err := other.call(t, "goal.close", map[string]any{"goal_id": other.goal.ID.String()})
	wantErr(t, err, "TASK-1 is open, not merged")
	_, err = p.call(t, "goal.close", map[string]any{"goal_id": domain.NewID().String()})
	wantErr(t, err, "get goal")
	_, err = p.call(t, "goal.close", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")

	for _, method := range []string{"ListComments", "CreateComment"} {
		p.wrapped = failing{Tracker: p.tracker, errs: map[string]error{method: errors.New("boom")}}
		p.register()
		_, err = p.call(t, "goal.close", map[string]any{"goal_id": p.goal.ID.String()})
		wantErr(t, err, "boom")
	}
	if got := p.taskNow(t, tasks[0].ID).Status; got != domain.TaskMerged || p.goalNow(t, p.goal.ID).Status != domain.GoalInProgress {
		t.Errorf("task %s, goal %s", got, p.goalNow(t, p.goal.ID).Status)
	}
}

func TestGoalSettle(t *testing.T) {
	p := newPlanned(t)
	tasks := p.plan(t, "One", "Two", "Three")
	must(t, p.st.Goals().UpdateStatus(t.Context(), p.account, p.goal.ID, domain.GoalPlanned, domain.GoalInProgress))
	for _, s := range taskPathTo(domain.TaskMerged) {
		must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, tasks[0].ID, p.taskNow(t, tasks[0].ID).Status, s))
	}
	must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, tasks[1].ID, domain.TaskOpen, domain.TaskInProgress))
	with := map[string]any{"goal_id": p.goal.ID.String()}

	out, err := p.callKey(t, "goal.settle", "k", with)
	must(t, err)
	if toMap(t, out)["status"] != "canceled" {
		t.Errorf("output = %v", out)
	}
	want := []domain.TaskStatus{domain.TaskMerged, domain.TaskClosed, domain.TaskClosed}
	for i, task := range tasks {
		issue, _ := p.tracker.GetIssue(t.Context(), "acme/app", task.IssueNumber)
		wantState := "closed"
		if i == 0 {
			wantState = "open"
		}
		if got := p.taskNow(t, task.ID).Status; got != want[i] || issue.State != wantState {
			t.Errorf("task %d: %s, issue %s", i+1, got, issue.State)
		}
	}
	issue, _ := p.tracker.GetIssue(t.Context(), "acme/app", p.issue)
	comments := p.goalComments(t)
	if p.goalNow(t, p.goal.ID).Status != domain.GoalCanceled || issue.State != "closed" || len(comments) != 1 || !strings.Contains(comments[0].Body, "stay merged") {
		t.Errorf("goal %s, issue %s, comments %+v", p.goalNow(t, p.goal.ID).Status, issue.State, comments)
	}

	// Settling again changes nothing, and a crash between the comment and the
	// close would not repeat the comment either.
	_, err = p.callKey(t, "goal.settle", "k", with)
	must(t, err)
	comments = p.goalComments(t)
	if len(comments) != 1 {
		t.Errorf("%d comments after the second call", len(comments))
	}
}

func TestGoalSettleBeforeTheIssueExists(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	_, err := v.call(t, "goal.settle", map[string]any{"goal_id": g.ID.String()})
	must(t, err)
	if got := v.goalNow(t, g.ID).Status; got != domain.GoalCanceled {
		t.Errorf("goal %s", got)
	}
}

func TestGoalSettleLeavesADoneGoal(t *testing.T) {
	p, _ := delivered(t)
	_, err := p.call(t, "goal.close", map[string]any{"goal_id": p.goal.ID.String()})
	must(t, err)
	out, err := p.call(t, "goal.settle", map[string]any{"goal_id": p.goal.ID.String()})
	must(t, err)
	if toMap(t, out)["status"] != "done" || p.goalNow(t, p.goal.ID).Status != domain.GoalDone {
		t.Errorf("output = %v, goal %s", out, p.goalNow(t, p.goal.ID).Status)
	}
}

func TestGoalSettleErrors(t *testing.T) {
	p := newPlanned(t)
	p.plan(t, "One")
	with := map[string]any{"goal_id": p.goal.ID.String()}
	_, err := p.call(t, "goal.settle", map[string]any{"goal_id": domain.NewID().String()})
	wantErr(t, err, "get goal")
	_, err = p.call(t, "goal.settle", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")

	// A GitHub failure leaves the goal canceled and the next call finishes.
	p.wrapped = failing{Tracker: p.tracker, errs: map[string]error{"UpdateIssue": errors.New("boom")}}
	p.register()
	_, err = p.call(t, "goal.settle", with)
	wantErr(t, err, "close issue")
	p.wrapped = nil
	p.register()
	_, err = p.call(t, "goal.settle", with)
	must(t, err)
	issue, _ := p.tracker.GetIssue(t.Context(), "acme/app", p.issue)
	if issue.State != "closed" || p.goalNow(t, p.goal.ID).Status != domain.GoalCanceled || len(p.goalComments(t)) != 1 {
		t.Errorf("issue %s, goal %s, %d comments", issue.State, p.goalNow(t, p.goal.ID).Status, len(p.goalComments(t)))
	}

	q := newPlanned(t)
	q.wrapped = failing{Tracker: q.tracker, errs: map[string]error{"GetIssue": errors.New("boom")}}
	q.register()
	_, err = q.call(t, "goal.settle", map[string]any{"goal_id": q.goal.ID.String()})
	wantErr(t, err, "get issue")
	q.wrapped = failing{Tracker: q.tracker, errs: map[string]error{"CreateComment": errors.New("boom")}}
	q.register()
	_, err = q.call(t, "goal.settle", map[string]any{"goal_id": q.goal.ID.String()})
	wantErr(t, err, "comment on")
}

// goalComments returns the comments on the goal issue except the plan.
func (p *planned) goalComments(t *testing.T) []github.Comment {
	t.Helper()
	all, err := p.tracker.ListComments(t.Context(), "acme/app", p.issue, t0.Add(-1))
	must(t, err)
	return slices.DeleteFunc(all, func(c github.Comment) bool { return strings.Contains(c.Body, "forecrew:plan:") })
}

// A task whose PR is merged is merged after the settle, whether the change
// says so or only GitHub does; the others are closed.
func TestGoalSettleKeepsMergedTasks(t *testing.T) {
	p := newPlanned(t)
	tasks := p.plan(t, "Recorded", "Merged on GitHub", "Open PR", "No PR")
	for _, task := range tasks {
		must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, task.ID, domain.TaskOpen, domain.TaskInProgress))
	}
	change := func(task domain.Task, number int, state domain.ChangeState) {
		c := domain.Change{
			AccountID: p.account, ProjectID: p.project.ID, TaskID: task.ID, Repo: "acme/app", Number: number,
			Branch: "b", BaseRef: "main", HeadSHA: "abc", State: state, URL: "u",
		}
		must(t, p.st.Changes().Upsert(t.Context(), &c))
	}
	change(tasks[0], 90, domain.ChangeMerged)
	pr := p.openPR(t)
	_, err := p.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	change(tasks[1], pr.Number, domain.ChangeOpen)
	must(t, p.tracker.AddBranch("acme/app", "other"))
	open, err := p.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "t", Head: "other", Base: "main"})
	must(t, err)
	change(tasks[2], open.Number, domain.ChangeOpen)

	_, err = p.call(t, "goal.settle", map[string]any{"goal_id": p.goal.ID.String()})
	must(t, err)
	want := []domain.TaskStatus{domain.TaskMerged, domain.TaskMerged, domain.TaskClosed, domain.TaskClosed}
	for i, task := range tasks {
		if got := p.taskNow(t, task.ID).Status; got != want[i] {
			t.Errorf("task %d = %s, want %s", i+1, got, want[i])
		}
	}

	// GitHub failing to answer is an error, and the next call finishes.
	q := newPlanned(t)
	task := q.plan(t, "One")[0]
	pr = q.openPR(t)
	c := domain.Change{AccountID: q.account, ProjectID: q.project.ID, TaskID: task.ID, Repo: "acme/app", Number: pr.Number, Branch: "b", BaseRef: "main", HeadSHA: "abc", State: domain.ChangeOpen, URL: "u"}
	must(t, q.st.Changes().Upsert(t.Context(), &c))
	q.wrapped = failMerged{q.tracker}
	_, err = q.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	q.register()
	_, err = q.call(t, "goal.settle", map[string]any{"goal_id": q.goal.ID.String()})
	wantErr(t, err, "get pull request")
}

// failMerged is a tracker whose GetPR fails for a merged PR.
type failMerged struct{ github.Tracker }

func (f failMerged) GetPR(ctx context.Context, repo string, number int) (github.PullRequest, error) {
	pr, err := f.Tracker.GetPR(ctx, repo, number)
	if err == nil && pr.Merged {
		return github.PullRequest{}, errors.New("boom")
	}
	return pr, err
}

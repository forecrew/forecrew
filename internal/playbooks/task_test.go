package playbooks_test

import (
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// plan runs goal.plan for tasks with one criterion each and returns the
// stored tasks.
func (p *planned) plan(t *testing.T, titles ...string) []domain.Task {
	t.Helper()
	var ts []map[string]any
	for _, title := range titles {
		ts = append(ts, planTask(title, []string{"AC-1", "AC-2"}, []string{"go"}))
	}
	_, err := p.call(t, "goal.plan", p.with(planOutput(ts...)))
	must(t, err)
	tasks, err := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	must(t, err)
	return tasks
}

func (p *planned) taskNow(t *testing.T, id uuid.UUID) domain.Task {
	t.Helper()
	task, err := p.st.Tasks().Get(t.Context(), p.account, id)
	must(t, err)
	return task
}

func TestGoalTaskMove(t *testing.T) {
	tests := []struct {
		name       string
		start      domain.TaskStatus
		to         domain.TaskStatus
		want       domain.TaskStatus
		wantErrMsg string
	}{
		{"start", domain.TaskOpen, domain.TaskInProgress, domain.TaskInProgress, ""},
		{"to review", domain.TaskInProgress, domain.TaskInReview, domain.TaskInReview, ""},
		{"skips states", domain.TaskOpen, domain.TaskMergeReady, domain.TaskMergeReady, ""},
		{"merged while in progress", domain.TaskInProgress, domain.TaskMerged, domain.TaskMerged, ""},
		{"same state", domain.TaskInReview, domain.TaskInReview, domain.TaskInReview, ""},
		{"rework from review", domain.TaskInReview, domain.TaskInProgress, domain.TaskInProgress, ""},
		{"rework from merge ready", domain.TaskMergeReady, domain.TaskInProgress, domain.TaskInProgress, ""},
		{"close", domain.TaskInReview, domain.TaskClosed, domain.TaskClosed, ""},
		{"back from merged", domain.TaskMerged, domain.TaskInProgress, domain.TaskMerged, "invalid state transition"},
		{"back to open", domain.TaskInReview, domain.TaskOpen, domain.TaskInReview, "invalid state transition"},
		{"unknown state", domain.TaskOpen, "soon", domain.TaskOpen, "unknown task status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPlanned(t)
			task := p.plan(t, "One")[0]
			for _, s := range taskPathTo(tt.start) {
				must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, task.ID, p.taskNow(t, task.ID).Status, s))
			}
			out, err := p.call(t, "goal.task_move", map[string]any{"task_id": task.ID.String(), "to": string(tt.to)})
			if tt.wantErrMsg != "" {
				wantErr(t, err, tt.wantErrMsg)
			} else {
				must(t, err)
				if toMap(t, out)["status"] != string(tt.want) {
					t.Errorf("output = %v", out)
				}
			}
			if got := p.taskNow(t, task.ID).Status; got != tt.want {
				t.Errorf("status = %s, want %s", got, tt.want)
			}
		})
	}
}

// taskPathTo returns the states a task goes through from open to s.
func taskPathTo(s domain.TaskStatus) []domain.TaskStatus {
	path := []domain.TaskStatus{domain.TaskInProgress, domain.TaskInReview, domain.TaskMergeReady, domain.TaskMerged}
	for i, p := range path {
		if p == s {
			return path[:i+1]
		}
	}
	return nil
}

func TestGoalTaskMoveErrors(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	_, err := p.call(t, "goal.task_move", map[string]any{"task_id": domain.NewID().String(), "to": "in_progress"})
	wantErr(t, err, "get task")
	_, err = p.call(t, "goal.task_move", map[string]any{"task_id": task.ID.String(), "to": "in_progress", "x": 1})
	wantErr(t, err, "unknown field")

	other := domain.Project{AccountID: p.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, p.st.Projects().Create(t.Context(), &other))
	foreign := domain.Goal{AccountID: p.account, ProjectID: other.ID, Title: "x", Status: domain.GoalDraft}
	must(t, p.st.Goals().Create(t.Context(), &foreign))
	spec := domain.Spec{AccountID: p.account, ProjectID: other.ID, GoalID: foreign.ID, Revision: 1, Body: "x"}
	must(t, p.st.Specs().Create(t.Context(), &spec))
	ft := domain.Task{AccountID: p.account, ProjectID: other.ID, GoalID: foreign.ID, SpecID: spec.ID, Seq: 1, Title: "x", Status: domain.TaskOpen}
	must(t, p.st.Tasks().Create(t.Context(), &ft))
	_, err = p.call(t, "goal.task_move", map[string]any{"task_id": ft.ID.String(), "to": "in_progress"})
	wantErr(t, err, "belongs to another project")
}

func TestGoalRound(t *testing.T) {
	p := newPlanned(t)
	tasks := p.plan(t, "One", "Two")
	with := func(task domain.Task, iteration int) map[string]any {
		return map[string]any{
			"task_id": task.ID.String(), "iteration": iteration, "human": " Use cents. ", "review": "Checks failed.",
		}
	}

	out, err := p.call(t, "goal.round", with(tasks[0], 1))
	must(t, err)
	m := toMap(t, out)
	if m["branch"] != "agent/TASK-1" || m["feedback"] != "Use cents." || m["task_id"] != tasks[0].ID.String() || m["ref"] != "TASK-1" || m["title"] != "One" {
		t.Errorf("first round = %v", m)
	}
	first := p.taskNow(t, tasks[0].ID)
	if first.Status != domain.TaskInProgress || first.Branch != "agent/TASK-1" || p.goalNow(t, p.goal.ID).Status != domain.GoalInProgress {
		t.Errorf("task = %+v, goal %s", first, p.goalNow(t, p.goal.ID).Status)
	}

	// Rework after a review: back to in progress, with the review feedback.
	must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, first.ID, domain.TaskInProgress, domain.TaskInReview))
	out, err = p.call(t, "goal.round", with(tasks[0], 2))
	must(t, err)
	if toMap(t, out)["feedback"] != "Checks failed." || p.taskNow(t, first.ID).Status != domain.TaskInProgress {
		t.Errorf("rework = %v, status %s", toMap(t, out), p.taskNow(t, first.ID).Status)
	}

	// The next task leaves the goal as it is.
	out, err = p.call(t, "goal.round", with(tasks[1], 1))
	must(t, err)
	if toMap(t, out)["branch"] != "agent/TASK-2" || p.goalNow(t, p.goal.ID).Status != domain.GoalInProgress {
		t.Errorf("second task = %v", toMap(t, out))
	}
}

func TestGoalRoundErrors(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	_, err := p.call(t, "goal.round", map[string]any{"task_id": domain.NewID().String()})
	wantErr(t, err, "get task")
	must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, task.ID, domain.TaskOpen, domain.TaskClosed))
	_, err = p.call(t, "goal.round", map[string]any{"task_id": task.ID.String(), "iteration": 1})
	wantErr(t, err, "invalid state transition")
	_, err = p.call(t, "goal.round", map[string]any{"task_id": task.ID.String(), "x": 1})
	wantErr(t, err, "unknown field")
}

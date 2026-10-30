package playbooks

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// taskPath is the way a task goes forward through its states.
var taskPath = []domain.TaskStatus{domain.TaskOpen, domain.TaskInProgress, domain.TaskInReview, domain.TaskMergeReady, domain.TaskMerged}

// loadTask returns the task of the run's project with the given ID.
func (a *actions) loadTask(ctx context.Context, run domain.WorkflowRun, id uuid.UUID) (domain.Task, error) {
	t, err := a.Store.Tasks().Get(ctx, run.AccountID, id)
	if err != nil {
		return domain.Task{}, fmt.Errorf("get task %s: %w", id, err)
	}
	if t.ProjectID != run.ProjectID {
		return domain.Task{}, fmt.Errorf("task %s belongs to another project", t.Ref())
	}
	return t, nil
}

// moveTask moves t to the state to: back to in progress for rework, forward
// through the states in between (a PR merged by a human while the task was
// in progress still ends merged) or to closed. A task already in to stays.
// It returns the task as moved, or an error wrapping
// domain.ErrInvalidTransition.
func (a *actions) moveTask(ctx context.Context, t domain.Task, to domain.TaskStatus) (domain.Task, error) {
	for t.Status != to {
		next := to
		if i, j := slices.Index(taskPath, t.Status), slices.Index(taskPath, to); !t.Status.CanTransition(to) && i >= 0 && j > i {
			next = taskPath[i+1]
		}
		if _, err := t.Status.Transition(next); err != nil {
			return t, fmt.Errorf("move %s to %s: %w", t.Ref(), to, err)
		}
		if err := a.Store.Tasks().UpdateStatus(ctx, t.AccountID, t.ID, t.Status, next); err != nil {
			return t, fmt.Errorf("move %s to %s: %w", t.Ref(), next, err)
		}
		t.Status = next
	}
	return t, nil
}

// taskMove is the action goal.task_move. It moves with.task_id to the state
// with.to (see moveTask) and outputs the state.
func (a *actions) taskMove(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID uuid.UUID         `json:"task_id"`
		To     domain.TaskStatus `json:"to"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if !slices.Contains(domain.TaskStatuses(), with.To) {
		return nil, fmt.Errorf("with: unknown task status %q", with.To)
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	if t, err = a.moveTask(ctx, t, with.To); err != nil {
		return nil, err
	}
	return map[string]string{"status": string(t.Status)}, nil
}

// roundOutput is the output of goal.round.
type roundOutput struct {
	// TaskID, Ref and Title name the task for the steps of a foreach whose
	// own item hides the task's.
	TaskID   uuid.UUID `json:"task_id"`
	Ref      string    `json:"ref"`
	Title    string    `json:"title"`
	Branch   string    `json:"branch"`
	Feedback string    `json:"feedback"`
}

// taskRound is the action goal.round, run before every developer job. It
// puts with.task_id in progress (rework of a task in review or ready to
// merge included), the goal in progress with its first task, and records
// the task's branch. It outputs the task's ID, ref and title, its branch and
// the feedback for the developer job: with.human, the feedback of the merge
// gate, in the first round of a delivery, and with.review, the failures and
// review comments of the last round, in the others.
func (a *actions) taskRound(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID    uuid.UUID `json:"task_id"`
		Iteration int       `json:"iteration"`
		Human     string    `json:"human"`
		Review    string    `json:"review"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, t.GoalID)
	if err != nil {
		return nil, err
	}
	if t, err = a.moveTask(ctx, t, domain.TaskInProgress); err != nil {
		return nil, err
	}
	if g.Status == domain.GoalPlanned {
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, domain.GoalPlanned, domain.GoalInProgress); err != nil {
			return nil, fmt.Errorf("move %s to in progress: %w", g.Ref(), err)
		}
	}
	if branch := taskBranch(t); t.Branch != branch {
		t.Branch = branch
		if err := a.Store.Tasks().Update(ctx, &t); err != nil {
			return nil, fmt.Errorf("record the branch of %s: %w", t.Ref(), err)
		}
	}
	feedback := with.Review
	if with.Iteration <= 1 {
		feedback = with.Human
	}
	return roundOutput{TaskID: t.ID, Ref: t.Ref(), Title: t.Title, Branch: t.Branch, Feedback: strings.TrimSpace(feedback)}, nil
}

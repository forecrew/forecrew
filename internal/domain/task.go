package domain

import (
	"time"
	"uuid"
)

// TaskStatus is the state of a task.
type TaskStatus string

// Task states.
const (
	TaskOpen       TaskStatus = "open"
	TaskInProgress TaskStatus = "in_progress"
	TaskInReview   TaskStatus = "in_review"
	TaskMergeReady TaskStatus = "merge_ready"
	TaskMerged     TaskStatus = "merged"
	TaskClosed     TaskStatus = "closed"
)

// TaskStatuses returns every task state.
func TaskStatuses() []TaskStatus {
	return []TaskStatus{TaskOpen, TaskInProgress, TaskInReview, TaskMergeReady, TaskMerged, TaskClosed}
}

var taskTransitions = transitions[TaskStatus]{
	TaskOpen:       {TaskInProgress, TaskClosed},
	TaskInProgress: {TaskInReview, TaskClosed},
	TaskInReview:   {TaskMergeReady, TaskInProgress, TaskClosed},
	TaskMergeReady: {TaskMerged, TaskInProgress, TaskClosed},
}

// CanTransition reports whether a task may move from s to to.
func (s TaskStatus) CanTransition(to TaskStatus) bool {
	return taskTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s TaskStatus) Transition(to TaskStatus) (TaskStatus, error) {
	return taskTransitions.apply("task", s, to)
}

// Terminal reports whether no transition leaves s.
func (s TaskStatus) Terminal() bool {
	return s == TaskMerged || s == TaskClosed
}

// Task is one unit of work planned from a spec, delivered as one Change.
type Task struct {
	ID          uuid.UUID  `json:"id"`
	AccountID   uuid.UUID  `json:"account_id"`
	ProjectID   uuid.UUID  `json:"project_id"`
	GoalID      uuid.UUID  `json:"goal_id"`
	SpecID      uuid.UUID  `json:"spec_id"`
	Number      int64      `json:"number"` // TASK-<number>
	Seq         int        `json:"seq"`    // order within the spec, from 1
	Title       string     `json:"title"`
	Body        string     `json:"body"` // markdown
	Status      TaskStatus `json:"status"`
	IssueNumber int        `json:"issue_number,omitempty"` // GitHub sub-issue of the goal issue, 0 if none
	Branch      string     `json:"branch,omitempty"`       // agent/<...> branch once work started
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Ref returns the task's human ref, e.g. TASK-12.
func (t Task) Ref() string { return Ref(RefTask, t.Number) }

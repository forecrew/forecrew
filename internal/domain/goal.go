package domain

import (
	"time"
	"uuid"
)

// GoalStatus is the state of a goal.
type GoalStatus string

// Goal states.
const (
	GoalDraft      GoalStatus = "draft"
	GoalSpecReview GoalStatus = "spec_review"
	GoalPlanned    GoalStatus = "planned"
	GoalInProgress GoalStatus = "in_progress"
	GoalDone       GoalStatus = "done"
	GoalCanceled   GoalStatus = "canceled"
)

// GoalStatuses returns every goal state.
func GoalStatuses() []GoalStatus {
	return []GoalStatus{GoalDraft, GoalSpecReview, GoalPlanned, GoalInProgress, GoalDone, GoalCanceled}
}

var goalTransitions = transitions[GoalStatus]{
	GoalDraft:      {GoalSpecReview, GoalCanceled},
	GoalSpecReview: {GoalPlanned, GoalCanceled},
	GoalPlanned:    {GoalInProgress, GoalCanceled},
	GoalInProgress: {GoalDone, GoalCanceled},
}

// CanTransition reports whether a goal may move from s to to.
func (s GoalStatus) CanTransition(to GoalStatus) bool {
	return goalTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s GoalStatus) Transition(to GoalStatus) (GoalStatus, error) {
	return goalTransitions.apply("goal", s, to)
}

// Terminal reports whether no transition leaves s.
func (s GoalStatus) Terminal() bool {
	return s == GoalDone || s == GoalCanceled
}

// Goal is what a human wants; it is refined into a spec and then tasks.
type Goal struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Number    int64     `json:"number"` // GOAL-<number>
	Title     string    `json:"title"`
	Body      string    `json:"body"` // markdown
	// Platforms the goal targets (worker labels such as "web" or "ios"),
	// normalized with NormalizeLabels; empty when not restricted.
	Platforms   []string   `json:"platforms,omitempty"`
	Status      GoalStatus `json:"status"`
	IssueNumber int        `json:"issue_number,omitempty"` // GitHub issue mirroring the goal, 0 if none
	CreatedBy   string     `json:"created_by"`             // Actor.String()
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Ref returns the goal's human ref, e.g. GOAL-3.
func (g Goal) Ref() string { return Ref(RefGoal, g.Number) }

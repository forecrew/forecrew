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

// GoalKind says what a goal is for.
type GoalKind string

// Goal kinds.
const (
	GoalFeature GoalKind = "feature" // the default; every goal made before kinds existed
	GoalBug     GoalKind = "bug"     // a bug report (report_bug)
)

// GoalKinds returns every goal kind.
func GoalKinds() []GoalKind { return []GoalKind{GoalFeature, GoalBug} }

// OrDefault returns k, or GoalFeature when k is empty.
func (k GoalKind) OrDefault() GoalKind {
	if k == "" {
		return GoalFeature
	}
	return k
}

// Reversibility is how hard a change is to undo.
type Reversibility string

// Reversibility grades.
const (
	ReversibleEasy Reversibility = "easy"
	ReversibleHard Reversibility = "hard"
	Irreversible   Reversibility = "irreversible"
)

// Reversibilities returns every reversibility grade.
func Reversibilities() []Reversibility {
	return []Reversibility{ReversibleEasy, ReversibleHard, Irreversible}
}

// GoalSize is the triage of a goal: how big and how reversible it is, and
// which councils it gets (goals.size).
type GoalSize struct {
	Impact        Level         `json:"impact"`
	Reversibility Reversibility `json:"reversibility"`
	Councils      []CouncilKind `json:"councils"` // product and technical, in that order; empty for none
	Reason        string        `json:"reason"`
}

// Validate checks the fields, wrapping ErrInvalidRecord.
func (s GoalSize) Validate() error {
	var p problems
	checkEnum(&p, "impact", s.Impact, Levels())
	checkEnum(&p, "reversibility", s.Reversibility, Reversibilities())
	for _, c := range s.Councils {
		checkEnum(&p, "councils", c, []CouncilKind{CouncilProduct, CouncilTechnical})
	}
	p.checkText("reason", s.Reason)
	return p.errOf(ErrInvalidRecord, "goal size")
}

// Goal is what a human wants; it is refined into a spec and then tasks.
type Goal struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Number    int64     `json:"number"`         // GOAL-<number>
	Kind      GoalKind  `json:"kind,omitempty"` // empty is stored as feature
	Title     string    `json:"title"`
	Body      string    `json:"body"` // markdown
	// Platforms the goal targets (worker labels such as "web" or "ios"),
	// normalized with NormalizeLabels; empty when not restricted.
	Platforms []string   `json:"platforms,omitempty"`
	Status    GoalStatus `json:"status"`
	// Size is the triage result; nil until the goal is triaged.
	Size        *GoalSize `json:"size,omitempty"`
	IssueNumber int       `json:"issue_number,omitempty"` // GitHub issue mirroring the goal, 0 if none
	CreatedBy   string    `json:"created_by"`             // Actor.String()
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Ref returns the goal's human ref, e.g. GOAL-3.
func (g Goal) Ref() string { return Ref(RefGoal, g.Number) }

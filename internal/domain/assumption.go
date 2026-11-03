package domain

import (
	"strings"
	"time"
	"uuid"
)

// Level grades confidence, impact and risk.
type Level string

// Levels.
const (
	LevelLow    Level = "low"
	LevelMedium Level = "medium"
	LevelHigh   Level = "high"
)

// Levels returns every level, lowest first.
func Levels() []Level { return []Level{LevelLow, LevelMedium, LevelHigh} }

// VerifyBy is how an assumption can be checked.
type VerifyBy string

// Ways to verify an assumption.
const (
	VerifyByTest     VerifyBy = "test"
	VerifyByAskHuman VerifyBy = "ask-human"
	VerifyByResearch VerifyBy = "research"
)

// VerifyBys returns every way to verify an assumption.
func VerifyBys() []VerifyBy { return []VerifyBy{VerifyByTest, VerifyByAskHuman, VerifyByResearch} }

// StatedAssumption is an assumption as an agent states it in its output
// (the assumption item of packs/schemas/common.json) or records it with
// the job MCP tool assumption_record.
type StatedAssumption struct {
	Statement  string   `json:"statement"`
	Confidence Level    `json:"confidence"` // how sure the agent is that it holds
	Impact     Level    `json:"impact"`     // how much changes if it is wrong
	VerifyBy   VerifyBy `json:"verify_by"`
}

// Validate checks the fields, wrapping ErrInvalidRecord.
func (a StatedAssumption) Validate() error {
	var p problems
	a.check(&p)
	return p.errOf(ErrInvalidRecord, "assumption")
}

func (a StatedAssumption) check(p *problems) {
	p.checkText("statement", a.Statement)
	checkEnum(p, "confidence", a.Confidence, Levels())
	checkEnum(p, "impact", a.Impact, Levels())
	checkEnum(p, "verify_by", a.VerifyBy, VerifyBys())
}

// AssumptionStatus is the state of a registered assumption.
type AssumptionStatus string

// Assumption states.
const (
	AssumptionUnverified  AssumptionStatus = "unverified"
	AssumptionVerified    AssumptionStatus = "verified"    // checked and true, with evidence
	AssumptionInvalidated AssumptionStatus = "invalidated" // checked and false: dependents are flagged
	AssumptionAccepted    AssumptionStatus = "accepted"    // a human takes the risk without checking
)

// AssumptionStatuses returns every assumption state.
func AssumptionStatuses() []AssumptionStatus {
	return []AssumptionStatus{AssumptionUnverified, AssumptionVerified, AssumptionInvalidated, AssumptionAccepted}
}

var assumptionTransitions = transitions[AssumptionStatus]{
	AssumptionUnverified: {AssumptionVerified, AssumptionInvalidated, AssumptionAccepted},
	AssumptionAccepted:   {AssumptionVerified, AssumptionInvalidated},
	AssumptionVerified:   {AssumptionInvalidated}, // later evidence can still prove it wrong
}

// CanTransition reports whether an assumption may move from s to to.
func (s AssumptionStatus) CanTransition(to AssumptionStatus) bool {
	return assumptionTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s AssumptionStatus) Transition(to AssumptionStatus) (AssumptionStatus, error) {
	return assumptionTransitions.apply("assumption", s, to)
}

// Open reports whether the assumption still needs checking or accepting.
func (s AssumptionStatus) Open() bool { return s == AssumptionUnverified }

// DependentKind is what depends on an assumption.
type DependentKind string

// Dependent kinds. ID is a ref (GOAL-3, SPEC-4, TASK-12, DEC-2) or UUID for
// goal, spec, task and decision, owner/name#number for change, and a repo
// path for test.
const (
	DependentGoal     DependentKind = "goal"
	DependentSpec     DependentKind = "spec"
	DependentTask     DependentKind = "task"
	DependentChange   DependentKind = "change"
	DependentDecision DependentKind = "decision"
	DependentTest     DependentKind = "test"
)

// DependentKinds returns every dependent kind.
func DependentKinds() []DependentKind {
	return []DependentKind{DependentGoal, DependentSpec, DependentTask, DependentChange, DependentDecision, DependentTest}
}

// Dependent is something flagged when the assumption it rests on is
// invalidated.
type Dependent struct {
	Kind DependentKind `json:"kind"`
	ID   string        `json:"id"`
}

// Assumption is a registered assumption (docs/contracts/data-model.md,
// assumptions): something an agent took as true, tracked until it is
// verified, invalidated or accepted.
type Assumption struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Number    int64     `json:"number"`              // A-<number>
	GoalID    uuid.UUID `json:"goal_id,omitzero"`    // goal it was made in; duplicates merge per goal
	MadeBy    string    `json:"made_by"`             // role name
	JobID     uuid.UUID `json:"job_id,omitzero"`     // source: the job that stated it
	SpecID    uuid.UUID `json:"spec_id,omitzero"`    // source: the spec it is part of
	CouncilID uuid.UUID `json:"council_id,omitzero"` // source: the council brief it comes from
	StatedAssumption
	Status     AssumptionStatus `json:"status"`
	Evidence   string           `json:"evidence,omitempty"` // how it was verified or invalidated
	Dependents []Dependent      `json:"dependents"`
	ResolvedBy string           `json:"resolved_by,omitempty"` // Actor.String()
	ResolvedAt time.Time        `json:"resolved_at,omitzero"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// Ref returns the assumption's human ref, e.g. A-42.
func (a Assumption) Ref() string { return Ref(RefAssumption, a.Number) }

// Validate checks the fields, reporting every problem at once and wrapping
// ErrInvalidRecord.
func (a Assumption) Validate() error {
	var p problems
	a.check(&p)
	p.checkText("made_by", a.MadeBy)
	checkEnum(&p, "status", a.Status, AssumptionStatuses())
	for _, d := range a.Dependents {
		checkEnum(&p, "dependents.kind", d.Kind, DependentKinds())
		p.checkText("dependents.id", d.ID)
	}
	return p.errOf(ErrInvalidRecord, "assumption")
}

// Resolve moves the assumption to verified, invalidated or accepted.
// Verified and invalidated need evidence.
func (a *Assumption) Resolve(by Actor, to AssumptionStatus, evidence string, now time.Time) error {
	if to != AssumptionAccepted && strings.TrimSpace(evidence) == "" {
		var p problems
		p.addf("%s needs evidence", to)
		return p.errOf(ErrInvalidRecord, "assumption "+a.Ref())
	}
	status, err := a.Status.Transition(to)
	if err != nil {
		return err
	}
	a.Status = status
	a.Evidence = evidence
	a.ResolvedBy = by.String()
	a.ResolvedAt = now
	a.UpdatedAt = now
	return nil
}

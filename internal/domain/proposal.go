package domain

import (
	"strings"
	"time"
	"uuid"
)

// ProposalScope is what an improvement proposal changes.
type ProposalScope string

// Proposal scopes.
const (
	ScopeProject  ProposalScope = "project"  // the project or Forecrew's setup for it
	ScopePlatform ProposalScope = "platform" // Forecrew itself: a setup action with an issue to file
)

// ProposalScopes returns every proposal scope.
func ProposalScopes() []ProposalScope { return []ProposalScope{ScopeProject, ScopePlatform} }

// ProposalCategory groups improvement proposals.
type ProposalCategory string

// Proposal categories.
const (
	CategoryObservability ProposalCategory = "observability"
	CategoryTesting       ProposalCategory = "testing"
	CategoryCI            ProposalCategory = "ci"
	CategorySecurity      ProposalCategory = "security"
	CategoryDocs          ProposalCategory = "docs"
	CategoryKnowledge     ProposalCategory = "knowledge"
	CategoryTooling       ProposalCategory = "tooling"
	CategoryWorkers       ProposalCategory = "workers"
	CategoryRoles         ProposalCategory = "roles"
	CategoryCost          ProposalCategory = "cost"
)

// ProposalCategories returns every proposal category.
func ProposalCategories() []ProposalCategory {
	return []ProposalCategory{CategoryObservability, CategoryTesting, CategoryCI, CategorySecurity, CategoryDocs,
		CategoryKnowledge, CategoryTooling, CategoryWorkers, CategoryRoles, CategoryCost}
}

// Effort is a T-shirt size.
type Effort string

// Efforts.
const (
	EffortS Effort = "s"
	EffortM Effort = "m"
	EffortL Effort = "l"
)

// Efforts returns every effort, smallest first.
func Efforts() []Effort { return []Effort{EffortS, EffortM, EffortL} }

// ProposalStatus is the state of an improvement proposal.
type ProposalStatus string

// Proposal states. Rejected, done and superseded are final.
const (
	ProposalProposed   ProposalStatus = "proposed"
	ProposalAccepted   ProposalStatus = "accepted" // its action is under way
	ProposalRejected   ProposalStatus = "rejected"
	ProposalSnoozed    ProposalStatus = "snoozed" // hidden until SnoozedUntil
	ProposalDone       ProposalStatus = "done"    // the goal closed, the PR merged or the setup was done
	ProposalSuperseded ProposalStatus = "superseded"
)

// ProposalStatuses returns every proposal state.
func ProposalStatuses() []ProposalStatus {
	return []ProposalStatus{ProposalProposed, ProposalAccepted, ProposalRejected, ProposalSnoozed, ProposalDone, ProposalSuperseded}
}

var proposalTransitions = transitions[ProposalStatus]{
	ProposalProposed: {ProposalAccepted, ProposalRejected, ProposalSnoozed, ProposalSuperseded},
	ProposalSnoozed:  {ProposalProposed, ProposalAccepted, ProposalRejected, ProposalSuperseded},
	ProposalAccepted: {ProposalDone, ProposalSuperseded},
}

// CanTransition reports whether a proposal may move from s to to.
func (s ProposalStatus) CanTransition(to ProposalStatus) bool {
	return proposalTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s ProposalStatus) Transition(to ProposalStatus) (ProposalStatus, error) {
	return proposalTransitions.apply("proposal", s, to)
}

// Final reports whether no transition leaves s. A dedupe key is unique
// among the proposals of a project that are not final.
func (s ProposalStatus) Final() bool {
	return s == ProposalRejected || s == ProposalDone || s == ProposalSuperseded
}

// ProposalEvidence is one signal a proposal rests on.
type ProposalEvidence struct {
	Kind   string `json:"kind"` // detector or source, e.g. workers, ask_human, verification
	Ref    string `json:"ref"`  // what it points at: a ref (JOB-41), path or URL
	Detail string `json:"detail"`
}

// ProposalAction is what accepting a proposal does: exactly one field is set.
type ProposalAction struct {
	Goal     *GoalAction     `json:"goal,omitempty"`
	ConfigPR *ConfigPRAction `json:"config_pr,omitempty"`
	Setup    *SetupAction    `json:"setup,omitempty"`
}

// GoalAction creates and starts a goal of kind feature.
type GoalAction struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Platforms []string `json:"platforms,omitempty"`
}

// ConfigPRAction opens a PR forecrewd writes, with files under .forecrew/.
type ConfigPRAction struct {
	Files []ConfigFile `json:"files"`
}

// ConfigFile is one file of a ConfigPRAction.
type ConfigFile struct {
	Path    string `json:"path"` // relative to the repo root, under .forecrew/
	Content string `json:"content"`
}

// SetupAction is a step only a human can take, done when they mark it.
type SetupAction struct {
	Instructions string `json:"instructions"`
	// Issue is a ready-to-file issue text for the Forecrew repo, for
	// platform proposals; Forecrew never files it itself.
	Issue string `json:"issue,omitempty"`
}

// ProposalMeasure is what is measured again after a proposal is done.
type ProposalMeasure struct {
	Detector string  `json:"detector"`
	Value    float64 `json:"value"` // the detector's value when proposed
}

// ProposalOutcome is the measure before and after.
type ProposalOutcome struct {
	Before     float64   `json:"before"`
	After      float64   `json:"after"`
	MeasuredAt time.Time `json:"measured_at"`
}

// ProposalDecision is what a human decides on a proposal.
type ProposalDecision string

// Proposal decisions (decide_proposal).
const (
	ProposalAccept ProposalDecision = "accept"
	ProposalReject ProposalDecision = "reject"
	ProposalSnooze ProposalDecision = "snooze"
)

// Proposal is an improvement proposal (docs/contracts/data-model.md,
// proposals): something Forecrew suggests a project, or its setup for the
// project, should gain.
type Proposal struct {
	ID             uuid.UUID          `json:"id"`
	AccountID      uuid.UUID          `json:"account_id"`
	ProjectID      uuid.UUID          `json:"project_id"`
	Number         int64              `json:"number"` // PROP-<number>
	Scope          ProposalScope      `json:"scope"`
	Category       ProposalCategory   `json:"category"`
	Title          string             `json:"title"`
	Rationale      string             `json:"rationale"`
	Evidence       []ProposalEvidence `json:"evidence"`
	Benefit        string             `json:"benefit"`
	Effort         Effort             `json:"effort"`
	Risk           Level              `json:"risk"`
	Action         ProposalAction     `json:"action"`
	Measure        *ProposalMeasure   `json:"measure,omitempty"`
	Status         ProposalStatus     `json:"status"`
	DedupeKey      string             `json:"dedupe_key"`
	EvidenceWeight float64            `json:"evidence_weight"`
	SnoozedUntil   time.Time          `json:"snoozed_until,omitzero"`
	DecidedBy      string             `json:"decided_by,omitempty"` // Actor.String()
	Reason         string             `json:"reason,omitempty"`
	Outcome        *ProposalOutcome   `json:"outcome,omitempty"`
	GateID         uuid.UUID          `json:"gate_id,omitzero"` // the improvement_proposal gate
	GoalID         uuid.UUID          `json:"goal_id,omitzero"` // the goal a goal action created
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

// Ref returns the proposal's human ref, e.g. PROP-3.
func (p Proposal) Ref() string { return Ref(RefProposal, p.Number) }

// Validate checks the fields, wrapping ErrInvalidRecord: exactly one
// action, config PRs only touch .forecrew/, and platform proposals are
// setup actions.
func (p Proposal) Validate() error {
	var pr problems
	checkEnum(&pr, "scope", p.Scope, ProposalScopes())
	checkEnum(&pr, "category", p.Category, ProposalCategories())
	checkEnum(&pr, "effort", p.Effort, Efforts())
	checkEnum(&pr, "risk", p.Risk, Levels())
	checkEnum(&pr, "status", p.Status, ProposalStatuses())
	pr.checkText("title", p.Title)
	pr.checkText("rationale", p.Rationale)
	pr.checkText("benefit", p.Benefit)
	pr.checkText("dedupe_key", p.DedupeKey)
	if p.EvidenceWeight < 0 {
		pr.addf("evidence_weight must not be negative")
	}
	p.Action.check(&pr)
	if p.Scope == ScopePlatform && p.Action.Setup == nil {
		pr.addf("a platform proposal is a setup action")
	}
	if p.Measure != nil {
		pr.checkText("measure.detector", p.Measure.Detector)
	}
	return pr.errOf(ErrInvalidRecord, "proposal")
}

func (a ProposalAction) check(p *problems) {
	n := 0
	if g := a.Goal; g != nil {
		n++
		p.checkText("action.goal.title", g.Title)
	}
	if c := a.ConfigPR; c != nil {
		n++
		if len(c.Files) == 0 {
			p.addf("action.config_pr needs files")
		}
		for _, f := range c.Files {
			if !RelativePath(f.Path) || !strings.HasPrefix(f.Path, ".forecrew/") {
				p.addf("action.config_pr: %q must be a file under .forecrew/", f.Path)
			}
		}
	}
	if s := a.Setup; s != nil {
		n++
		p.checkText("action.setup.instructions", s.Instructions)
	}
	if n != 1 {
		p.addf("action needs exactly one of goal, config_pr and setup")
	}
}

// Decide applies a human's decision: accept, reject (with a reason) or
// snooze until a time after now. A snoozed proposal can be decided again.
func (p *Proposal) Decide(by Actor, d ProposalDecision, reason string, until, now time.Time) error {
	var pr problems
	var to ProposalStatus
	switch d {
	case ProposalAccept:
		to = ProposalAccepted
	case ProposalReject:
		to = ProposalRejected
		pr.checkText("reason", reason)
	case ProposalSnooze:
		to = ProposalSnoozed
		if !until.After(now) {
			pr.addf("snooze needs a time in the future")
		}
	default:
		pr.addf("decision must be accept, reject or snooze, got %q", d)
	}
	if err := pr.errOf(ErrInvalidRecord, "proposal "+p.Ref()); err != nil {
		return err
	}
	status, err := p.Status.Transition(to)
	if err != nil {
		return err
	}
	p.Status = status
	p.DecidedBy = by.String()
	p.Reason = reason
	p.SnoozedUntil = time.Time{}
	if d == ProposalSnooze {
		p.SnoozedUntil = until
	}
	p.UpdatedAt = now
	return nil
}

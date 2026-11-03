package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// CouncilKind is the standard council a council follows.
type CouncilKind string

// Council kinds; adhoc is a council started with a question of its own.
const (
	CouncilOnboarding CouncilKind = "onboarding"
	CouncilProduct    CouncilKind = "product"
	CouncilTechnical  CouncilKind = "technical"
	CouncilTestDesign CouncilKind = "test_design"
	CouncilIncident   CouncilKind = "incident"
	CouncilAdhoc      CouncilKind = "adhoc"
)

// CouncilKinds returns every council kind.
func CouncilKinds() []CouncilKind {
	return []CouncilKind{CouncilOnboarding, CouncilProduct, CouncilTechnical, CouncilTestDesign, CouncilIncident, CouncilAdhoc}
}

// CouncilStatus is the state of a council.
type CouncilStatus string

// Council states.
const (
	CouncilRunning  CouncilStatus = "running"
	CouncilDecided  CouncilStatus = "decided" // a human or the policy chose
	CouncilFailed   CouncilStatus = "failed"  // the council could not finish, or its brief was rejected
	CouncilCanceled CouncilStatus = "canceled"
)

// CouncilStatuses returns every council state.
func CouncilStatuses() []CouncilStatus {
	return []CouncilStatus{CouncilRunning, CouncilDecided, CouncilFailed, CouncilCanceled}
}

var councilTransitions = transitions[CouncilStatus]{
	CouncilRunning: {CouncilDecided, CouncilFailed, CouncilCanceled},
}

// CanTransition reports whether a council may move from s to to.
func (s CouncilStatus) CanTransition(to CouncilStatus) bool {
	return councilTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s CouncilStatus) Transition(to CouncilStatus) (CouncilStatus, error) {
	return councilTransitions.apply("council", s, to)
}

// Terminal reports whether no transition leaves s.
func (s CouncilStatus) Terminal() bool { return s != CouncilRunning }

// CouncilStage is a step of the council protocol.
type CouncilStage string

// Council stages, in protocol order.
const (
	StageProposal  CouncilStage = "proposal"  // members write independently
	StageCritique  CouncilStage = "critique"  // members see every proposal
	StageRevision  CouncilStage = "revision"  // at most MaxRevisionRounds rounds
	StageSynthesis CouncilStage = "synthesis" // the facilitator writes the brief
)

// CouncilStages returns every stage in protocol order.
func CouncilStages() []CouncilStage {
	return []CouncilStage{StageProposal, StageCritique, StageRevision, StageSynthesis}
}

// Revision rounds. A council runs at most MaxCouncilRevisions revision
// rounds of its own. request_changes on its council_decision gate adds one
// more, the human's round, with the feedback as a constraint, numbered
// after the last round run; it is followed by a second synthesis. A council
// gets one human round: after it, its gate can only be approved or rejected.
const (
	MaxCouncilRevisions = 2
	MaxRevisionRounds   = MaxCouncilRevisions + 1 // with the human's round
	MaxSynthesisRounds  = 2                       // the second follows the human's round
)

// MaxRound returns the highest round a contribution of stage may have.
func (s CouncilStage) MaxRound() int {
	switch s {
	case StageRevision:
		return MaxRevisionRounds
	case StageSynthesis:
		return MaxSynthesisRounds
	}
	return 1
}

// Council is a temporary group of agents answering one question with a
// decision brief (docs/contracts/data-model.md, councils).
type Council struct {
	ID          uuid.UUID     `json:"id"`
	AccountID   uuid.UUID     `json:"account_id"`
	ProjectID   uuid.UUID     `json:"project_id"`
	Number      int64         `json:"number"`           // COUNCIL-<number>
	GoalID      uuid.UUID     `json:"goal_id,omitzero"` // nil for a council of its own
	Kind        CouncilKind   `json:"kind"`
	Question    string        `json:"question"`
	Constraints []string      `json:"constraints"` // given at start or added by a human mid-debate
	Status      CouncilStatus `json:"status"`
	// Brief is the facilitator's synthesis, set when it is written.
	Brief     *CouncilBrief `json:"brief,omitempty"`
	GateID    uuid.UUID     `json:"gate_id,omitzero"` // the council_decision gate, if one was opened
	RunID     uuid.UUID     `json:"run_id,omitzero"`  // the council playbook's workflow run
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// Ref returns the council's human ref, e.g. COUNCIL-2.
func (c Council) Ref() string { return Ref(RefCouncil, c.Number) }

// Validate checks the fields and the brief, wrapping ErrInvalidRecord.
func (c Council) Validate() error {
	var p problems
	checkEnum(&p, "kind", c.Kind, CouncilKinds())
	checkEnum(&p, "status", c.Status, CouncilStatuses())
	p.checkText("question", c.Question)
	for _, s := range c.Constraints {
		p.checkText("constraints", s)
	}
	if c.Status == CouncilDecided && c.Brief == nil {
		p.addf("a decided council needs a brief")
	}
	if c.Brief != nil {
		c.Brief.check(&p)
	}
	return p.errOf(ErrInvalidRecord, "council")
}

// CouncilContribution is one member's output at one stage of a council.
// (Council, Role, Runtime, Stage, Round) is unique, so a retried job cannot
// add a second one; a council has one member per role and runtime, so the
// same role may sit on several runtimes. The facilitator's opening
// brief (framing the question and constraints, stored on the Council) and
// its checks whether positions still move are jobs of the council's run,
// not contributions; its synthesis is the contribution of stage synthesis.
type CouncilContribution struct {
	ID        uuid.UUID       `json:"id"`
	AccountID uuid.UUID       `json:"account_id"`
	CouncilID uuid.UUID       `json:"council_id"`
	Role      string          `json:"role"`    // member role, e.g. architect or devils-advocate
	Runtime   string          `json:"runtime"` // runtime adapter the member ran on
	Stage     CouncilStage    `json:"stage"`
	Round     int             `json:"round"` // from 1 to Stage.MaxRound()
	JobID     uuid.UUID       `json:"job_id,omitzero"`
	Content   json.RawMessage `json:"content"` // the job's output, valid against the stage's schema
	// EvidenceCheck is the result of checking every source the content
	// cites; nil until checked, empty when it cites none.
	EvidenceCheck []SourceCheck `json:"evidence_check"`
	CreatedAt     time.Time     `json:"created_at"`
}

// SourceKind is what a cited source is.
type SourceKind string

// Source kinds.
const (
	SourcePath SourceKind = "path" // a repo path at the council's commit
	SourceKB   SourceKind = "kb"   // a knowledge entry path
	SourceURL  SourceKind = "url"  // fetched once by forecrewd and archived
	SourceJob  SourceKind = "job"  // a job ref
)

// SourceKinds returns every source kind.
func SourceKinds() []SourceKind { return []SourceKind{SourcePath, SourceKB, SourceURL, SourceJob} }

// SourceStatus is the outcome of checking a source.
type SourceStatus string

// Source check outcomes.
const (
	SourceOK          SourceStatus = "ok"
	SourceMissing     SourceStatus = "missing"     // the path, entry or job does not exist
	SourceUnreachable SourceStatus = "unreachable" // the URL did not answer, or too large or slow
	SourceRefused     SourceStatus = "refused"     // not fetched: a private address or a redirect to one
)

// SourceStatuses returns every source check outcome.
func SourceStatuses() []SourceStatus {
	return []SourceStatus{SourceOK, SourceMissing, SourceUnreachable, SourceRefused}
}

// SourceCheck is the check of one source a contribution cites.
type SourceCheck struct {
	Source     string       `json:"source"` // as cited
	Kind       SourceKind   `json:"kind"`
	Status     SourceStatus `json:"status"`
	ArtifactID uuid.UUID    `json:"artifact_id,omitzero"` // the archived copy of a URL
	Detail     string       `json:"detail,omitempty"`
}

// Validate checks the fields, wrapping ErrInvalidRecord.
func (c CouncilContribution) Validate() error {
	var p problems
	p.checkText("role", c.Role)
	checkEnum(&p, "stage", c.Stage, CouncilStages())
	if c.Round < 1 || c.Round > c.Stage.MaxRound() {
		p.addf("round of a %s must be from 1 to %d, got %d", c.Stage, c.Stage.MaxRound(), c.Round)
	}
	if len(c.Content) == 0 || !json.Valid(c.Content) {
		p.addf("content must be JSON")
	}
	for _, s := range c.EvidenceCheck {
		p.checkText("evidence_check.source", s.Source)
		checkEnum(&p, "evidence_check.kind", s.Kind, SourceKinds())
		checkEnum(&p, "evidence_check.status", s.Status, SourceStatuses())
	}
	return p.errOf(ErrInvalidRecord, "council contribution")
}

// CouncilBrief is the decision brief a council ends with
// (docs/contracts/council-brief.schema.json).
type CouncilBrief struct {
	Question       string             `json:"question"`
	Options        []BriefOption      `json:"options"`
	Recommendation string             `json:"recommendation"` // name of an option
	Votes          []BriefVote        `json:"votes"`
	Dissent        []BriefDissent     `json:"dissent"`
	Assumptions    []StatedAssumption `json:"assumptions"`
	OpenQuestions  []string           `json:"open_questions"`
	Reversible     bool               `json:"reversible"`
	// BudgetExhausted says the synthesis was written early because the
	// council's token or time budget ran out.
	BudgetExhausted bool `json:"budget_exhausted"`
}

// BriefOption is one option a council weighed.
type BriefOption struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Pros    []string `json:"pros"`
	Cons    []string `json:"cons"`
	Cost    Level    `json:"cost"`
	Risk    Level    `json:"risk"`
}

// BriefVote is a member's vote.
type BriefVote struct {
	Member string `json:"member"` // role
	Option string `json:"option"` // name of an option
	Reason string `json:"reason"`
}

// BriefDissent is a member's disagreement with the recommendation.
type BriefDissent struct {
	Member   string `json:"member"`
	Position string `json:"position"`
}

// Validate checks the rules the brief's schema cannot express: option
// names are unique, and the recommendation and every vote name an option.
// It wraps ErrInvalidRecord.
func (b CouncilBrief) Validate() error {
	var p problems
	b.check(&p)
	return p.errOf(ErrInvalidRecord, "council brief")
}

func (b CouncilBrief) check(p *problems) {
	names := map[string]bool{}
	for _, o := range b.Options {
		if names[o.Name] {
			p.addf("option %q is listed twice", o.Name)
		}
		names[o.Name] = true
	}
	if !names[b.Recommendation] {
		p.addf("recommendation %q is not an option", b.Recommendation)
	}
	for _, v := range b.Votes {
		if !names[v.Option] {
			p.addf("%s votes for %q, which is not an option", v.Member, v.Option)
		}
	}
	for _, a := range b.Assumptions {
		a.check(p)
	}
}

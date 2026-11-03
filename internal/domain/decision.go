package domain

import (
	"time"
	"uuid"
)

// DecidedByPolicy is Decision.DecidedBy when the autonomy policy accepted a
// recommendation without a human.
const DecidedByPolicy = "policy"

// DecisionOption is one option a decision chose among.
type DecisionOption struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

// Decision is a recorded decision (docs/contracts/data-model.md,
// decisions): what was asked, the options, the choice and why, who decided
// and the assumptions it rests on.
type Decision struct {
	ID          uuid.UUID        `json:"id"`
	AccountID   uuid.UUID        `json:"account_id"`
	ProjectID   uuid.UUID        `json:"project_id"`
	Number      int64            `json:"number"` // DEC-<number>
	GoalID      uuid.UUID        `json:"goal_id,omitzero"`
	Question    string           `json:"question"`
	Options     []DecisionOption `json:"options"`
	Choice      string           `json:"choice"`
	Reasons     string           `json:"reasons"`
	Dissent     string           `json:"dissent,omitempty"`
	DecidedBy   string           `json:"decided_by"` // Actor.String() or DecidedByPolicy
	CouncilID   uuid.UUID        `json:"council_id,omitzero"`
	GateID      uuid.UUID        `json:"gate_id,omitzero"`
	Assumptions []uuid.UUID      `json:"assumptions"` // IDs of the assumptions it rests on
	CreatedAt   time.Time        `json:"created_at"`
}

// Ref returns the decision's human ref, e.g. DEC-9.
func (d Decision) Ref() string { return Ref(RefDecision, d.Number) }

// Validate checks the fields, wrapping ErrInvalidRecord. A decision by
// policy needs a council: policy only ever accepts a council's
// recommendation.
func (d Decision) Validate() error {
	var p problems
	p.checkText("question", d.Question)
	p.checkText("choice", d.Choice)
	p.checkText("decided_by", d.DecidedBy)
	if d.DecidedBy == DecidedByPolicy && d.CouncilID == uuid.Nil() {
		p.addf("a decision by policy needs a council")
	}
	for _, o := range d.Options {
		p.checkText("options.name", o.Name)
	}
	return p.errOf(ErrInvalidRecord, "decision")
}

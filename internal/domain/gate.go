package domain

import (
	"fmt"
	"time"
	"uuid"
)

// GateKind is what a gate asks a human to decide.
type GateKind string

// Gate kinds.
const (
	GateProfileApproval GateKind = "profile_approval"
	GateSpecApproval    GateKind = "spec_approval"
	GateMergeApproval   GateKind = "merge_approval"
	GateNeedsYou        GateKind = "needs_you" // escalation: approve = retry, reject = stop
)

// GateKinds returns every gate kind.
func GateKinds() []GateKind {
	return []GateKind{GateProfileApproval, GateSpecApproval, GateMergeApproval, GateNeedsYou}
}

// GateStatus is the state of a gate. The terminal states are also the
// decisions a human can make.
type GateStatus string

// Gate states.
const (
	GatePending          GateStatus = "pending"
	GateApproved         GateStatus = "approved"
	GateChangesRequested GateStatus = "changes_requested"
	GateRejected         GateStatus = "rejected"
)

// GateStatuses returns every gate state.
func GateStatuses() []GateStatus {
	return []GateStatus{GatePending, GateApproved, GateChangesRequested, GateRejected}
}

var gateTransitions = transitions[GateStatus]{
	GatePending: {GateApproved, GateChangesRequested, GateRejected},
}

// CanTransition reports whether a gate may move from s to to. Terminal
// states are final: a new review round opens a new gate.
func (s GateStatus) CanTransition(to GateStatus) bool {
	return gateTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s GateStatus) Transition(to GateStatus) (GateStatus, error) {
	return gateTransitions.apply("gate", s, to)
}

// Terminal reports whether no transition leaves s.
func (s GateStatus) Terminal() bool {
	return s != GatePending
}

// AllowsDecision reports whether decision is valid for the gate kind:
// needs_you gates accept only approved and rejected.
func (k GateKind) AllowsDecision(decision GateStatus) bool {
	switch decision {
	case GateApproved, GateRejected:
		return true
	case GateChangesRequested:
		return k != GateNeedsYou
	}
	return false
}

// Gate is a decision waiting for a human.
type Gate struct {
	ID            uuid.UUID  `json:"id"`
	AccountID     uuid.UUID  `json:"account_id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	Number        int64      `json:"number"` // GATE-<number>
	Kind          GateKind   `json:"kind"`
	Status        GateStatus `json:"status"`
	Title         string     `json:"title"`
	Summary       string     `json:"summary"` // markdown shown to the reviewer
	SubjectRepo   string     `json:"subject_repo"`
	SubjectNumber int        `json:"subject_number,omitempty"` // issue or PR number the gate is about
	GoalID        uuid.UUID  `json:"goal_id,omitzero"`
	SpecID        uuid.UUID  `json:"spec_id,omitzero"`
	TaskID        uuid.UUID  `json:"task_id,omitzero"`
	JobID         uuid.UUID  `json:"job_id,omitzero"`
	ResolvedBy    string     `json:"resolved_by,omitempty"` // Actor.String()
	Feedback      string     `json:"feedback,omitempty"`
	ResolvedAt    time.Time  `json:"resolved_at,omitzero"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Ref returns the gate's human ref, e.g. GATE-7.
func (g Gate) Ref() string { return Ref(RefGate, g.Number) }

// Resolve records a decision on a pending gate.
func (g *Gate) Resolve(by Actor, decision GateStatus, feedback string, now time.Time) error {
	if !g.Kind.AllowsDecision(decision) {
		return fmt.Errorf("%s on %s gate: %w", decision, g.Kind, ErrInvalidDecision)
	}
	status, err := g.Status.Transition(decision)
	if err != nil {
		return err
	}
	g.Status = status
	g.ResolvedBy = by.String()
	g.Feedback = feedback
	g.ResolvedAt = now
	return nil
}

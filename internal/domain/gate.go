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
	// GateTestPlanApproval approves the onboarding PR with the test
	// strategy, .forecrew/matrix.yaml and .forecrew/environments.
	GateTestPlanApproval GateKind = "test_plan_approval"
	GateNeedsYou         GateKind = "needs_you" // escalation: approve = retry, reject = stop

	// Phase 3 gates (docs/contracts/conventions.md, Gate decisions).
	GateAssumptionReview    GateKind = "assumption_review"    // a high-impact assumption: approve = accept, reject = invalidate
	GateCouncilDecision     GateKind = "council_decision"     // a council brief: approve = the recommendation
	GateKnowledgeProposal   GateKind = "knowledge_proposal"   // a change to a protected or read-only knowledge section
	GateAccessRequest       GateKind = "access_request"       // an agent asks for knowledge access; approve = a policy PR
	GateImprovementProposal GateKind = "improvement_proposal" // an improvement proposal: approve = accept
)

// GateKinds returns every gate kind.
func GateKinds() []GateKind {
	return []GateKind{GateProfileApproval, GateSpecApproval, GateMergeApproval, GateTestPlanApproval, GateNeedsYou,
		GateAssumptionReview, GateCouncilDecision, GateKnowledgeProposal, GateAccessRequest, GateImprovementProposal}
}

// GateOrigin says what opened a gate.
type GateOrigin string

// Gate origins.
const (
	// GateFromPlaybook gates are opened by a playbook step, escalations
	// included: a needs_you gate from a playbook means approve = retry,
	// reject = stop. Empty is stored as playbook.
	GateFromPlaybook GateOrigin = "playbook"
	// GateFromAskHuman gates are needs_you gates the job MCP tool ask_human
	// opens on its job (JobID set): Summary holds the question, the
	// approval's Feedback is the answer, reject declines. No playbook step
	// waits on them.
	GateFromAskHuman GateOrigin = "ask_human"
)

// OrDefault returns o, or GateFromPlaybook when o is empty.
func (o GateOrigin) OrDefault() GateOrigin {
	if o == "" {
		return GateFromPlaybook
	}
	return o
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
	Origin        GateOrigin `json:"origin,omitempty"` // empty is stored as playbook
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

// AsksHuman reports whether the gate is an ask_human question of its job
// rather than a playbook's gate.
func (g Gate) AsksHuman() bool { return g.Origin == GateFromAskHuman }

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

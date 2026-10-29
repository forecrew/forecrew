package mcp

import (
	"fmt"

	"github.com/forecrew/forecrew/internal/domain"
)

// GateDecision checks the input of a gate tool (approve, request_changes or
// reject) against the kind of the gate it targets and returns the decision
// to record. Every error wraps ErrInvalidInput:
//   - gate is required;
//   - request_changes needs feedback;
//   - request_changes is not allowed on needs_you gates, which are approved
//     (retry) or rejected (stop).
func GateDecision(tool string, kind domain.GateKind, in GateDecisionInput) (domain.GateStatus, error) {
	if in.Gate == "" {
		return "", fmt.Errorf("%s: gate is required: %w", tool, ErrInvalidInput)
	}
	var decision domain.GateStatus
	switch tool {
	case ToolApprove:
		decision = domain.GateApproved
	case ToolReject:
		decision = domain.GateRejected
	case ToolRequestChanges:
		if in.Feedback == "" {
			return "", fmt.Errorf("%s: feedback is required: %w", tool, ErrInvalidInput)
		}
		decision = domain.GateChangesRequested
	default:
		return "", fmt.Errorf("%s is not a gate tool: %w", tool, ErrInvalidInput)
	}
	if !kind.AllowsDecision(decision) {
		return "", fmt.Errorf("%s is not allowed on %s gates; use %s to retry or %s to stop: %w",
			tool, kind, ToolApprove, ToolReject, ErrInvalidInput)
	}
	return decision, nil
}

package mcp

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestGateDecision(t *testing.T) {
	in := GateDecisionInput{Gate: "GATE-7", Feedback: "add tests"}
	want := map[string]domain.GateStatus{
		ToolApprove:        domain.GateApproved,
		ToolRequestChanges: domain.GateChangesRequested,
		ToolReject:         domain.GateRejected,
	}
	for _, kind := range domain.GateKinds() {
		for tool, decision := range want {
			got, err := GateDecision(tool, kind, in)
			if kind == domain.GateNeedsYou && tool == ToolRequestChanges {
				if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "not allowed on needs_you gates") {
					t.Errorf("%s on %s: err = %v, want ErrInvalidInput", tool, kind, err)
				}
				continue
			}
			if err != nil || got != decision {
				t.Errorf("%s on %s = %s, %v; want %s", tool, kind, got, err, decision)
			}
		}
	}
}

func TestGateDecisionInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		tool string
		in   GateDecisionInput
		want string
	}{
		{name: "no gate", tool: ToolApprove, in: GateDecisionInput{}, want: "gate is required"},
		{name: "no feedback", tool: ToolRequestChanges, in: GateDecisionInput{Gate: "GATE-1"}, want: "feedback is required"},
		{name: "not a gate tool", tool: ToolListJobs, in: GateDecisionInput{Gate: "GATE-1"}, want: "not a gate tool"},
	}
	for _, tt := range tests {
		_, err := GateDecision(tt.tool, domain.GateSpecApproval, tt.in)
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want ErrInvalidInput containing %q", tt.name, err, tt.want)
		}
	}
}

func TestTools(t *testing.T) {
	tools := Tools()
	if len(tools) != 31 || !slices.Contains(tools, ToolReject) {
		t.Fatalf("Tools = %v", tools)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(tools)))) != 31 {
		t.Fatalf("duplicate tool names: %v", tools)
	}
}

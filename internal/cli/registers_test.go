package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestAssumptionsCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ListAssumptionsInput
	e.svc.ListAssumptionsFunc = func(_ context.Context, _ domain.Actor, in mcp.ListAssumptionsInput) ([]mcp.AssumptionInfo, error) {
		got = in
		return []mcp.AssumptionInfo{
			{Ref: "A-1", Source: "JOB-3", MadeBy: "architect", Status: domain.AssumptionUnverified,
				Statement: "Single instance", Confidence: domain.LevelMedium, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest},
			{Ref: "A-2", MadeBy: "developer", Status: domain.AssumptionAccepted,
				Statement: "UTF-8", Confidence: domain.LevelHigh, Impact: domain.LevelLow, VerifyBy: domain.VerifyByResearch},
		}, nil
	}
	out, errOut, code := e.run(t, "assumptions", "app", "--goal", "GOAL-1", "--task", "TASK-2", "--status", "unverified", "--impact", "high", "--verify-by", "test")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if got != (mcp.ListAssumptionsInput{Project: "app", Goal: "GOAL-1", Task: "TASK-2", Status: "unverified", Impact: "high", VerifyBy: "test"}) {
		t.Fatalf("input = %+v", got)
	}
	for _, want := range []string{"ASSUMPTION  STATUS", "A-1         unverified  high    medium      test       architect  JOB-3   Single instance",
		"A-2         accepted    low     high        research   developer  -       UTF-8"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	e.run(t, "assumptions", "app")
	if got != (mcp.ListAssumptionsInput{Project: "app"}) {
		t.Fatalf("input without flags = %+v", got)
	}
	if _, _, code := e.run(t, "assumptions"); code == 0 {
		t.Error("assumptions without a project succeeded")
	}
}

func TestAssumptionsResolveCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ResolveAssumptionInput
	e.svc.ResolveAssumptionFunc = func(_ context.Context, _ domain.Actor, in mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error) {
		got = in
		return mcp.AssumptionInfo{Ref: "A-42", Status: in.Status}, nil
	}
	out, _, code := e.run(t, "assumptions", "resolve", "A-42", "invalidated", "--evidence", "it scales out")
	if code != 0 || strings.TrimSpace(out) != "A-42 is invalidated" {
		t.Fatalf("code %d, output %q", code, out)
	}
	if got != (mcp.ResolveAssumptionInput{Assumption: "A-42", Status: "invalidated", Evidence: "it scales out"}) {
		t.Fatalf("input = %+v", got)
	}
	e.svc.ResolveAssumptionFunc = func(context.Context, domain.Actor, mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error) {
		return mcp.AssumptionInfo{}, mcp.ErrInvalidInput
	}
	_, errOut, code := e.run(t, "assumptions", "resolve", "A-42", "verified")
	if code == 0 || !strings.Contains(errOut, "invalid input") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestDecisionsCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ListDecisionsInput
	e.svc.ListDecisionsFunc = func(_ context.Context, _ domain.Actor, in mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error) {
		got = in
		return []mcp.DecisionInfo{
			{Ref: "DEC-1", DecidedBy: "user:me", Choice: "approved", Question: "Approve the spec\n(revision 2)", Assumptions: []string{"A-1", "A-2"}},
			{Ref: "DEC-2", DecidedBy: "policy", Choice: "postgres", Question: "Which queue?"},
		}, nil
	}
	out, _, code := e.run(t, "decisions", "app", "--goal", "GOAL-1", "--council", "0192f5a0-0000-7000-8000-00000000000c")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if got != (mcp.ListDecisionsInput{Project: "app", Goal: "GOAL-1", Council: "0192f5a0-0000-7000-8000-00000000000c"}) {
		t.Fatalf("input = %+v", got)
	}
	for _, want := range []string{"DEC-1     user:me  approved  Approve the spec (revision 2)  A-1,A-2", "DEC-2     policy   postgres  Which queue?                   -"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	e.run(t, "decisions", "app")
	if got != (mcp.ListDecisionsInput{Project: "app"}) {
		t.Fatalf("input without flags = %+v", got)
	}
}

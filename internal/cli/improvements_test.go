package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestProposalsCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ListProposalsInput
	e.svc.ListProposalsFunc = func(_ context.Context, _ domain.Actor, in mcp.ListProposalsInput) ([]mcp.ProposalInfo, error) {
		got = in
		return []mcp.ProposalInfo{
			{Ref: "PROP-1", Proposal: domain.Proposal{Status: domain.ProposalProposed, Category: domain.CategoryCI, Effort: domain.EffortS, Risk: domain.LevelLow, Title: "Add  CI"}},
			{Ref: "PROP-2", Proposal: domain.Proposal{Status: domain.ProposalDone, Category: domain.CategoryWorkers, Effort: domain.EffortM, Risk: domain.LevelHigh, Title: "Add a worker",
				Outcome: &domain.ProposalOutcome{Before: 4, After: 0.5, MeasuredAt: time.Unix(0, 0)}}},
		}, nil
	}
	out, errOut, code := e.run(t, "proposals", "app", "--status", "done", "--category", "ci")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if got != (mcp.ListProposalsInput{Project: "app", Status: "done", Category: "ci"}) {
		t.Fatalf("input = %+v", got)
	}
	for _, want := range []string{"PROPOSAL  STATUS", "PROP-1    proposed  ci        s       low   -         Add CI", "4 -> 0.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, _, code := e.run(t, "proposals"); code == 0 {
		t.Error("proposals without a project succeeded")
	}
}

func TestProposalsDecideCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.DecideProposalInput
	e.svc.DecideProposalFunc = func(_ context.Context, _ domain.Actor, in mcp.DecideProposalInput) (mcp.ProposalInfo, error) {
		got = in
		return mcp.ProposalInfo{Ref: "PROP-3", Proposal: domain.Proposal{Status: domain.ProposalSnoozed}}, nil
	}
	out, _, code := e.run(t, "proposals", "decide", "PROP-3", "snooze", "--reason", "later", "--until", "2030-01-02T03:04:05Z")
	if code != 0 || strings.TrimSpace(out) != "PROP-3 is snoozed" {
		t.Fatalf("code %d, output %q", code, out)
	}
	if got != (mcp.DecideProposalInput{Proposal: "PROP-3", Decision: "snooze", Reason: "later", Until: "2030-01-02T03:04:05Z"}) {
		t.Fatalf("input = %+v", got)
	}
	e.svc.DecideProposalFunc = func(context.Context, domain.Actor, mcp.DecideProposalInput) (mcp.ProposalInfo, error) {
		return mcp.ProposalInfo{}, mcp.ErrConflict
	}
	if _, _, code := e.run(t, "proposals", "decide", "PROP-3", "accept"); code == 0 {
		t.Error("a refused decision succeeded")
	}
}

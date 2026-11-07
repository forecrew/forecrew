package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/advisor"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
)

// newImprovement stores a proposal with its pending gate.
func (f *fixture) newImprovement(t *testing.T, a domain.ProposalAction) domain.Proposal {
	t.Helper()
	f.gates.WithRecorder(gates.Recorders{f.regs, advisor.New(advisor.Config{Store: f.st, Now: f.clock.Now})})
	p := domain.Proposal{AccountID: f.account.ID, ProjectID: f.project.ID, Scope: domain.ScopeProject, Category: domain.CategoryWorkers, Title: "Add a worker",
		Rationale: "Jobs wait", Benefit: "No waiting", Effort: domain.EffortS, Risk: domain.LevelLow, Status: domain.ProposalProposed, DedupeKey: "workers:" + domain.NewID().String(), Action: a}
	_, err := f.gates.Open(t.Context(), gates.OpenRequest{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.GateImprovementProposal, Title: p.Title, SubjectRepo: "acme/app",
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			p.GateID = g.ID
			return tx.Proposals().Create(ctx, &p)
		}})
	must(t, err)
	return p
}

// clockedFixture is a fixture whose service tells the time by its clock.
func clockedFixture(t *testing.T) *fixture {
	t.Helper()
	var f *fixture
	f = newFixture(t, func(c *tools.Config) { c.Now = func() time.Time { return f.clock.Now() } })
	return f
}

var setup = domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Add a worker"}}

func TestListProposals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	got, err := f.svc.ListProposals(ctx, f.by, mcp.ListProposalsInput{Project: "app"})
	must(t, err)
	if len(got) != 1 || got[0].Ref != p.Ref() || !strings.HasPrefix(got[0].GateRef, "GATE-") || !got[0].CanDecide {
		t.Fatalf("proposals %+v", got)
	}
	for _, in := range []mcp.ListProposalsInput{{Project: "app", Status: domain.ProposalDone}, {Project: "app", Category: domain.CategoryCost}} {
		if got, err = f.svc.ListProposals(ctx, f.by, in); err != nil || len(got) != 0 {
			t.Fatalf("%+v: %+v, %v", in, got, err)
		}
	}
	for _, in := range []mcp.ListProposalsInput{{Project: "app", Status: "nope"}, {Project: "app", Category: "nope"}} {
		_, err := f.svc.ListProposals(ctx, f.by, in)
		wantErr(t, err, mcp.ErrInvalidInput)
	}
}

func TestProposalOnItsGateCard(t *testing.T) {
	f := newFixture(t)
	p := f.newImprovement(t, setup)
	gs, err := f.svc.ListPendingGates(t.Context(), f.by, mcp.ProjectFilterInput{Project: "app"})
	must(t, err)
	if len(gs) != 1 || gs[0].Proposal == nil || gs[0].Proposal.Ref != p.Ref() || gs[0].Proposal.Benefit != "No waiting" {
		t.Fatalf("gates %+v", gs)
	}
}

func TestAcceptAndRejectResolveTheGate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	got, err := f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	must(t, err)
	g, err := f.st.Gates().Get(ctx, f.account.ID, p.GateID)
	must(t, err)
	if got.Status != domain.ProposalAccepted || g.Status != domain.GateApproved {
		t.Fatalf("accepted %+v, gate %+v", got, g)
	}
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	wantErr(t, err, mcp.ErrConflict)

	r := f.newImprovement(t, setup)
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: r.Ref(), Decision: domain.ProposalReject})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: r.Ref(), Decision: domain.ProposalReject, Reason: "We have enough"})
	must(t, err)
	if got.Status != domain.ProposalRejected || got.Reason != "We have enough" || got.CanDecide {
		t.Fatalf("rejected %+v", got)
	}
	decisions, err := f.st.Decisions().List(ctx, f.account.ID, store.DecisionFilter{ProjectID: f.project.ID})
	must(t, err)
	if n := len(decisions); n != 2 || decisions[1].Reasons != "We have enough" {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestSnoozeHidesUntilTheDate(t *testing.T) {
	f := clockedFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	until := f.clock.Now().Add(48 * time.Hour)
	got, err := f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalSnooze, Until: until.Format(time.RFC3339)})
	must(t, err)
	if got.Status != domain.ProposalSnoozed || !got.SnoozedUntil.Equal(until) {
		t.Fatalf("snoozed %+v", got)
	}
	count := func() (gs, ps int) {
		t.Helper()
		g, err := f.svc.ListPendingGates(ctx, f.by, mcp.ProjectFilterInput{Project: "app"})
		must(t, err)
		l, err := f.svc.ListProposals(ctx, f.by, mcp.ListProposalsInput{Project: "app"})
		must(t, err)
		return len(g), len(l)
	}
	if g, l := count(); g != 0 || l != 0 {
		t.Fatalf("while snoozed: %d gates, %d proposals", g, l)
	}
	shown, err := f.svc.ListProposals(ctx, f.by, mcp.ListProposalsInput{Project: "app", Status: domain.ProposalSnoozed})
	must(t, err)
	if len(shown) != 1 {
		t.Fatalf("by status: %+v", shown)
	}
	f.clock.Advance(49 * time.Hour)
	if g, l := count(); g != 1 || l != 1 {
		t.Fatalf("after the date: %d gates, %d proposals", g, l)
	}
	if a := f.auditActions(t); !strings.Contains(strings.Join(a, " "), "proposal.snoozed") {
		t.Fatalf("audit %v", a)
	}
}

func TestDecideRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	for name, c := range map[string]struct {
		in   mcp.DecideProposalInput
		want error
	}{
		"unknown":    {mcp.DecideProposalInput{Proposal: "PROP-99", Decision: domain.ProposalAccept}, mcp.ErrNotFound},
		"decision":   {mcp.DecideProposalInput{Proposal: p.Ref(), Decision: "maybe"}, mcp.ErrInvalidInput},
		"no date":    {mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalSnooze}, mcp.ErrInvalidInput},
		"past":       {mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalSnooze, Until: "2001-01-01T00:00:00Z"}, mcp.ErrInvalidInput},
		"early done": {mcp.DecideProposalInput{Proposal: p.Ref(), Decision: mcp.ProposalDone}, mcp.ErrConflict},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.DecideProposal(ctx, f.by, c.in)
			wantErr(t, err, c.want)
		})
	}
	// While the advisor revises, the proposal has no gate to decide on.
	_, err := f.gates.Resolve(ctx, f.by, p.GateID, domain.GateChangesRequested, "narrower")
	must(t, err)
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	wantErr(t, err, mcp.ErrConflict)
}

func TestSnoozeOfAcceptedIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	_, err := f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	must(t, err)
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalSnooze, Until: "2099-01-01T00:00:00Z"})
	if !errors.Is(err, mcp.ErrConflict) {
		t.Fatalf("got %v", err)
	}
}

// A setup action is a checklist item: done when the human marks it.
func TestSetupIsDoneWhenMarked(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	_, err := f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	must(t, err)
	got, err := f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: mcp.ProposalDone})
	must(t, err)
	if got.Status != domain.ProposalDone {
		t.Fatalf("got %+v", got)
	}
	g := f.newImprovement(t, domain.ProposalAction{Goal: &domain.GoalAction{Title: "x"}})
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: g.Ref(), Decision: domain.ProposalAccept})
	must(t, err)
	_, err = f.svc.DecideProposal(ctx, f.by, mcp.DecideProposalInput{Proposal: g.Ref(), Decision: mcp.ProposalDone})
	wantErr(t, err, mcp.ErrConflict)
}

// Accepting resolves a gate, so it needs the gate's permission and a person.
func TestDecidingNeedsTheGatePermissionToo(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	p := f.newImprovement(t, setup)
	only := f.memberWith(t, f.project.ID, domain.PermProposalDecide, domain.PermProjectRead)
	_, err := f.svc.DecideProposal(ctx, only, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalAccept})
	wantErr(t, err, authz.ErrDenied)
	got, err := f.svc.ListProposals(ctx, only, mcp.ListProposalsInput{Project: "app"})
	must(t, err)
	if len(got) != 1 || got[0].CanDecide {
		t.Fatalf("proposals %+v", got)
	}
	reader := f.memberWith(t, f.project.ID, domain.PermProjectRead)
	_, err = f.svc.DecideProposal(ctx, reader, mcp.DecideProposalInput{Proposal: p.Ref(), Decision: domain.ProposalSnooze, Until: "2099-01-01T00:00:00Z"})
	wantErr(t, err, authz.ErrDenied)
}

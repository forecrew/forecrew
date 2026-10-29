package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	st      *testkit.Store
	bus     *testkit.SignalBus
	svc     *gates.Keeper
	account uuid.UUID
	project domain.Project
	by      domain.Actor
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	bus := testkit.NewSignalBus(st)
	return fixture{
		st: st, bus: bus, svc: gates.New(st, bus, clock.Now), account: acc.ID, project: p,
		by: domain.Actor{AccountID: acc.ID, Kind: domain.ActorUser, Name: "ana"},
	}
}

func (f fixture) open(t *testing.T, kind domain.GateKind) domain.Gate {
	t.Helper()
	g, err := f.svc.Open(t.Context(), gates.OpenRequest{
		AccountID: f.account, ProjectID: f.project.ID, Kind: kind, Title: "check", SubjectRepo: "acme/app", SubjectNumber: 3,
	})
	must(t, err)
	return g
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func TestOpen(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.open(t, domain.GateSpecApproval)
	if g.Status != domain.GatePending || g.Number != 1 || g.SubjectNumber != 3 {
		t.Fatalf("Open = %+v", g)
	}
	entries, _ := f.st.Audit().List(ctx, f.account, store.AuditFilter{SubjectID: g.ID})
	if len(entries) != 1 || entries[0].Action != "gate.opened" || entries[0].Actor != "system:forecrewd" {
		t.Fatalf("audit = %+v", entries)
	}

	// A given ID makes Open idempotent.
	id := domain.NewID()
	req := gates.OpenRequest{ID: id, AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "stuck", SubjectRepo: "acme/app"}
	first, err := f.svc.Open(ctx, req)
	must(t, err)
	again, err := f.svc.Open(ctx, req)
	if err != nil || first.ID != id || again.ID != id || again.Number != first.Number {
		t.Fatalf("Open twice = %+v, %+v, %v", first, again, err)
	}
	if all, _ := f.st.Gates().List(ctx, f.account, store.GateFilter{}); len(all) != 2 {
		t.Fatalf("gates = %d, want 2", len(all))
	}

	_, err = f.svc.Open(ctx, gates.OpenRequest{AccountID: f.account, ProjectID: f.project.ID, Kind: "vibe_check"})
	wantErr(t, err, mcp.ErrInvalidInput)

	// A project of another account is rejected and nothing is audited.
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	op := domain.Project{AccountID: other.ID, Name: "x", Repo: "acme/x", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(ctx, &op))
	_, err = f.svc.Open(ctx, gates.OpenRequest{AccountID: f.account, ProjectID: op.ID, Kind: domain.GateNeedsYou})
	wantErr(t, err, store.ErrNotFound)
	if entries, _ := f.st.Audit().List(ctx, f.account, store.AuditFilter{}); len(entries) != 2 {
		t.Fatalf("audit = %d entries, want 2", len(entries))
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.open(t, domain.GateNeedsYou)
	_, err := f.svc.Resolve(ctx, f.by, g.ID, domain.GateChangesRequested, "x")
	wantErr(t, err, domain.ErrInvalidDecision)
	got, err := f.svc.Resolve(ctx, f.by, g.ID, domain.GateApproved, "retry")
	if err != nil || got.Status != domain.GateApproved || got.ResolvedBy != "user:ana" || !got.ResolvedAt.Equal(t0) {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	_, err = f.svc.Resolve(ctx, f.by, g.ID, domain.GateRejected, "")
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.Resolve(ctx, f.by, domain.NewID(), domain.GateApproved, "")
	wantErr(t, err, gates.ErrNotFound)

	sigs, _ := f.bus.Lookup(ctx, f.account, domain.GateKey(g.ID))
	if len(sigs) != 1 {
		t.Fatalf("signals = %v", sigs)
	}
	p, err := sigs[0].Decode()
	if err != nil || p.(*domain.GateResolved).Feedback != "retry" || p.(*domain.GateResolved).Decision != domain.GateApproved {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	entries, _ := f.st.Audit().List(ctx, f.account, store.AuditFilter{SubjectID: g.ID})
	if len(entries) != 2 || entries[0].Action != "gate.resolved" || entries[0].Actor != "user:ana" {
		t.Fatalf("audit = %+v", entries)
	}
	var data map[string]string
	if err := json.Unmarshal(entries[0].Data, &data); err != nil || data["decision"] != "approved" || data["feedback"] != "retry" {
		t.Fatalf("audit data = %s, %v", entries[0].Data, err)
	}
}

type failingPublisher struct{ err error }

func (p failingPublisher) Publish(context.Context, domain.Signal) (bool, error) { return false, p.err }

func TestResolvePublishError(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("boom")
	svc := gates.New(f.st, failingPublisher{boom}, nil)
	g := f.open(t, domain.GateMergeApproval)
	got, err := svc.Resolve(t.Context(), f.by, g.ID, domain.GateRejected, "")
	wantErr(t, err, boom)
	if got.Status != domain.GateRejected || got.ResolvedAt.IsZero() {
		t.Fatalf("gate = %+v; the decision is recorded even when publishing fails", got)
	}
}

func TestFind(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.open(t, domain.GateProfileApproval)
	for _, ref := range []string{"GATE-1", "gate-1", g.ID.String()} {
		if got, err := f.svc.Find(ctx, f.account, ref); err != nil || got.ID != g.ID {
			t.Errorf("Find(%q) = %+v, %v", ref, got, err)
		}
	}
	for ref, want := range map[string]error{
		"GATE-9":                   gates.ErrNotFound,
		domain.NewID().String():    gates.ErrNotFound,
		"JOB-1":                    mcp.ErrInvalidInput,
		"":                         mcp.ErrInvalidInput,
		"not-a-ref-or-uuid-at-all": mcp.ErrInvalidInput,
	} {
		if _, err := f.svc.Find(ctx, f.account, ref); !errors.Is(err, want) {
			t.Errorf("Find(%q) err = %v, want %v", ref, err, want)
		}
	}
	// Gates of another account are not found.
	if _, err := f.svc.Find(ctx, domain.NewID(), "GATE-1"); !errors.Is(err, gates.ErrNotFound) {
		t.Errorf("other account: err = %v", err)
	}
}

func TestDecide(t *testing.T) {
	tools := map[string]domain.GateStatus{
		mcp.ToolApprove:        domain.GateApproved,
		mcp.ToolRequestChanges: domain.GateChangesRequested,
		mcp.ToolReject:         domain.GateRejected,
	}
	for _, kind := range domain.GateKinds() {
		for tool, want := range tools {
			t.Run(string(kind)+"/"+tool, func(t *testing.T) {
				f := newFixture(t)
				g := f.open(t, kind)
				got, err := f.svc.Decide(t.Context(), f.by, tool, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: "why"})
				if kind == domain.GateNeedsYou && tool == mcp.ToolRequestChanges {
					wantErr(t, err, mcp.ErrInvalidInput)
					if stored, _ := f.svc.Get(t.Context(), f.account, g.ID); stored.Status != domain.GatePending {
						t.Fatalf("gate resolved anyway: %s", stored.Status)
					}
					return
				}
				if err != nil || got.Status != want || got.Feedback != "why" {
					t.Fatalf("Decide = %+v, %v; want %s", got, err, want)
				}
			})
		}
	}
}

func TestDecideErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.open(t, domain.GateSpecApproval)
	tests := []struct {
		name string
		tool string
		in   mcp.GateDecisionInput
		want error
	}{
		{"no gate", mcp.ToolApprove, mcp.GateDecisionInput{}, mcp.ErrInvalidInput},
		{"bad ref", mcp.ToolApprove, mcp.GateDecisionInput{Gate: "JOB-1"}, mcp.ErrInvalidInput},
		{"unknown gate", mcp.ToolReject, mcp.GateDecisionInput{Gate: "GATE-9"}, gates.ErrNotFound},
		{"no feedback", mcp.ToolRequestChanges, mcp.GateDecisionInput{Gate: "GATE-1"}, mcp.ErrInvalidInput},
	}
	for _, tt := range tests {
		if _, err := f.svc.Decide(ctx, f.by, tt.tool, tt.in); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestListPending(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a := f.open(t, domain.GateSpecApproval)
	f.open(t, domain.GateMergeApproval)
	_, err := f.svc.Resolve(ctx, f.by, a.ID, domain.GateApproved, "")
	must(t, err)
	for _, project := range []uuid.UUID{uuid.Nil(), f.project.ID} {
		pending, err := f.svc.ListPending(ctx, f.account, project)
		if err != nil || len(pending) != 1 || pending[0].Kind != domain.GateMergeApproval {
			t.Fatalf("ListPending(%s) = %+v, %v", project, pending, err)
		}
	}
	if pending, _ := f.svc.ListPending(ctx, f.account, domain.NewID()); len(pending) != 0 {
		t.Fatalf("other project: %+v", pending)
	}
}

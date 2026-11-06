package fleet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// agentProvider adds an agent provider that was just in contact, with a shape.
func (e *env) agentProvider(account uuid.UUID) (domain.CapacityProvider, domain.Shape) {
	e.t.Helper()
	ctx := e.t.Context()
	_, hash := fleet.NewProviderToken()
	p := domain.CapacityProvider{AccountID: account, Name: "agent-" + uuid.New().String()[:8], Kind: domain.ProviderFake, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash,
		Capabilities: domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSLinux}, Arch: []domain.MachineArch{domain.ArchAMD64}, MaxCPU: 64, MaxMemoryMB: 1 << 20, MaxDiskGB: 1000}}
	must(e.t, e.st.CapacityProviders().Create(ctx, &p))
	must(e.t, e.st.CapacityProviders().Seen(ctx, p.ID, nil))
	p, err := e.st.CapacityProviders().Get(ctx, p.ID)
	must(e.t, err)
	return p, e.shape(p, p.Name+"-small", nil)
}

// agentMachine adds a machine of p in state, through its real transitions.
func (e *env) agentMachine(p domain.CapacityProvider, s domain.Shape, state domain.MachineState) domain.Machine {
	e.t.Helper()
	ctx := e.t.Context()
	m := domain.Machine{AccountID: p.AccountID, ProviderID: p.ID, ShapeID: s.ID}
	must(e.t, e.st.Machines().Create(ctx, &m))
	if state != domain.MachineRequested {
		must(e.t, e.st.Machines().SetExternalID(ctx, m.ID, "vm-"+m.ID.String()))
		must(e.t, e.st.Machines().UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	}
	if state == domain.MachineDraining || state == domain.MachineFailed {
		must(e.t, e.st.Machines().UpdateState(ctx, m.ID, domain.MachineBooting, state, ""))
	}
	return e.machine(m.ID)
}

func TestDesiredAndDeliver(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	p, s := e.agentProvider(account)
	waiting := e.agentMachine(p, s, domain.MachineRequested)
	up := e.agentMachine(p, s, domain.MachineBooting)
	e.agentMachine(p, s, domain.MachineDraining)
	e.agentMachine(p, s, domain.MachineFailed)

	d, err := e.c.Desired(ctx, p, []uuid.UUID{up.ID})
	must(t, err)
	if len(d.Machines) != 2 || !d.Undelivered() {
		t.Fatalf("desired = %+v", d)
	}
	must(t, e.c.Deliver(ctx, p, &d))
	byID := map[uuid.UUID]fleet.CreateRequest{}
	for _, r := range d.Machines {
		byID[r.MachineID] = r
	}
	first := byID[waiting.ID]
	if first.Bootstrap == nil || !strings.HasPrefix(first.Bootstrap.Token, "fcb_") || first.Shape.ID != s.ID || first.Tags[fleet.TagOrg] != account.String() {
		t.Fatalf("delivered %+v", first)
	}
	if byID[up.ID].Bootstrap != nil || byID[up.ID].Shape.ID != s.ID {
		t.Fatalf("a machine it has got %+v", byID[up.ID])
	}
	if _, err := e.st.BootstrapTokens().GetByTokenHash(ctx, mcp.HashToken(first.Bootstrap.Token)); err != nil {
		t.Fatalf("token: %v", err)
	}

	// While the agent lists it, a machine keeps its token: nothing is minted.
	again, err := e.c.Desired(ctx, p, []uuid.UUID{up.ID, waiting.ID})
	must(t, err)
	if again.Undelivered() || again.Generation != d.Generation {
		t.Fatalf("again = %+v, was %s", again, d.Generation)
	}
	must(t, e.c.Deliver(ctx, p, &again))
	if slices.ContainsFunc(again.Machines, func(r fleet.CreateRequest) bool { return r.Bootstrap != nil }) {
		t.Fatalf("a token for a machine the agent has: %+v", again.Machines)
	}
	if _, err := e.st.BootstrapTokens().GetByTokenHash(ctx, mcp.HashToken(first.Bootstrap.Token)); err != nil {
		t.Fatalf("the token was replaced: %v", err)
	}

	// Dropped from have, it gets a new one and the old one stops working.
	third, err := e.c.Desired(ctx, p, []uuid.UUID{up.ID})
	must(t, err)
	must(t, e.c.Deliver(ctx, p, &third))
	if _, err := e.st.BootstrapTokens().GetByTokenHash(ctx, mcp.HashToken(first.Bootstrap.Token)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old token: %v", err)
	}

	// A paused provider gets no requested machines, which changes the list.
	p.Status = domain.ProviderPaused
	paused, err := e.c.Desired(ctx, p, nil)
	must(t, err)
	if len(paused.Machines) != 1 || paused.Machines[0].MachineID != up.ID || paused.Undelivered() || paused.Generation == d.Generation {
		t.Fatalf("paused = %+v", paused)
	}
}

// A machine that left requested between the look and the delivery is not
// delivered: it gets no token.
func TestDeliverSkipsAMachineThatMovedOn(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	p, s := e.agentProvider(account)
	m := e.agentMachine(p, s, domain.MachineRequested)
	d, err := e.c.Desired(ctx, p, nil)
	must(t, err)
	must(t, e.st.Machines().UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineDestroyed, ""))
	must(t, e.c.Deliver(ctx, p, &d))
	if len(d.Machines) != 0 {
		t.Fatalf("delivered %+v", d.Machines)
	}
}

func TestReportState(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	p, s := e.agentProvider(account)
	other, otherShape := e.agentProvider(account)

	m := e.agentMachine(p, s, domain.MachineRequested)
	if err := e.c.ReportState(ctx, p, m.ID, domain.MachineBooting, "", ""); !errors.Is(err, fleet.ErrBadReport) {
		t.Errorf("no external ID: %v", err)
	}
	if err := e.c.ReportState(ctx, other, m.ID, domain.MachineBooting, "vm-1", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another provider's machine: %v", err)
	}
	if err := e.c.ReportState(ctx, p, uuid.New(), domain.MachineBooting, "vm-1", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown machine: %v", err)
	}
	for _, st := range []domain.MachineState{domain.MachineReady, domain.MachineBusy, domain.MachineDraining, domain.MachineRequested, "nonsense"} {
		if err := e.c.ReportState(ctx, p, m.ID, st, "vm-1", ""); !errors.Is(err, fleet.ErrBadReport) {
			t.Errorf("%s: %v", st, err)
		}
	}
	must(t, e.c.ReportState(ctx, p, m.ID, domain.MachineBooting, "vm-1", ""))
	if got := e.machine(m.ID); got.State != domain.MachineBooting || got.ExternalID != "vm-1" {
		t.Fatalf("machine = %+v", got)
	}
	must(t, e.c.ReportState(ctx, p, m.ID, domain.MachineBooting, "vm-1", "")) // a retry

	// The worker called before the report arrived: the report is old news.
	must(t, e.st.Machines().UpdateState(ctx, m.ID, domain.MachineBooting, domain.MachineReady, ""))
	must(t, e.c.ReportState(ctx, p, m.ID, domain.MachineBooting, "vm-1", ""))

	// Found gone while in use: it fails on the way, so its job is requeued.
	must(t, e.c.ReportState(ctx, p, m.ID, domain.MachineDestroyed, "", ""))
	got := e.machine(m.ID)
	if got.State != domain.MachineDestroyed || got.Error == "" || got.DestroyedAt.IsZero() {
		t.Fatalf("machine = %+v", got)
	}
	must(t, e.c.ReportState(ctx, p, m.ID, domain.MachineDestroyed, "", "")) // a retry
	if err := e.c.ReportState(ctx, p, m.ID, domain.MachineBooting, "vm-1", ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("booting after the end: %v", err)
	}
	if err := e.c.ReportState(ctx, p, m.ID, domain.MachineFailed, "", "late"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("failed after the end: %v", err)
	}

	f := e.agentMachine(other, otherShape, domain.MachineRequested)
	why := "create: token fcp_" + strings.Repeat("a", 43) + " refused \x00" + strings.Repeat("é", 2000)
	must(t, e.c.ReportState(ctx, other, f.ID, domain.MachineFailed, "", why))
	must(t, e.c.ReportState(ctx, other, f.ID, domain.MachineFailed, "", why)) // a retry
	got = e.machine(f.ID)
	if got.State != domain.MachineFailed || strings.Contains(got.Error, "fcp_") || strings.ContainsRune(got.Error, 0) || len(got.Error) > 2000 || !strings.Contains(got.Error, "[redacted]") {
		t.Fatalf("machine = %+v", got)
	}
	must(t, e.c.ReportState(ctx, other, f.ID, domain.MachineDestroyed, "", ""))
	if got := e.machine(f.ID); got.State != domain.MachineDestroyed {
		t.Fatalf("failed then gone: %+v", got)
	}
}

func TestReconcileAgent(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	p, s := e.agentProvider(account)

	crashed := e.agentMachine(p, s, domain.MachineRequested) // started, never reported
	waiting := e.agentMachine(p, s, domain.MachineRequested) // not started: fine
	lost := e.agentMachine(p, s, domain.MachineBooting)      // gone
	kept := e.agentMachine(p, s, domain.MachineBooting)      // listed
	dead := e.agentMachine(p, s, domain.MachineFailed)       // listed: destroy it
	finished := e.agentMachine(p, s, domain.MachineFailed)   // not listed: over
	destroy, err := e.c.ReconcileAgent(ctx, p, []fleet.Instance{
		{ExternalID: "vm-crashed", MachineID: crashed.ID},
		{ExternalID: kept.ExternalID, MachineID: kept.ID},
		{ExternalID: dead.ExternalID, MachineID: dead.ID},
		{ExternalID: "vm-stray", Tags: map[string]string{}},
	})
	must(t, err)
	slices.Sort(destroy)
	want := []string{dead.ExternalID, "vm-stray"}
	slices.Sort(want)
	if !slices.Equal(destroy, want) {
		t.Fatalf("destroy = %v, want %v", destroy, want)
	}
	if got := e.machine(crashed.ID); got.State != domain.MachineBooting || got.ExternalID != "vm-crashed" {
		t.Fatalf("crashed = %+v", got)
	}
	if got := e.machine(waiting.ID); got.State != domain.MachineRequested {
		t.Fatalf("waiting = %+v", got)
	}
	// Lost failed in this pass and nothing holds it, so it is over too.
	if got := e.machine(lost.ID); got.State != domain.MachineDestroyed || got.Error == "" {
		t.Fatalf("lost = %+v", got)
	}
	if got := e.machine(dead.ID); got.State != domain.MachineFailed {
		t.Fatalf("dead = %+v", got)
	}
	if got := e.machine(finished.ID); got.State != domain.MachineDestroyed {
		t.Fatalf("finished = %+v", got)
	}
	if e.audited(account, fleet.ActionOrphan) != 1 {
		t.Error("the orphan is not audited")
	}
}

// A revoked agent cannot start or report anything, so its machines must not
// hold limits for good.
func TestRevokedAgentProvider(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, _ := e.org("a")
	p, s := e.agentProvider(account)
	waiting := e.agentMachine(p, s, domain.MachineRequested)
	up := e.agentMachine(p, s, domain.MachineBooting)
	draining := e.agentMachine(p, s, domain.MachineDraining)
	must(t, e.st.CapacityProviders().SetStatus(ctx, p.ID, domain.ProviderRevoked))
	e.reconcile()
	if got := e.machine(waiting.ID); got.State != domain.MachineDestroyed {
		t.Errorf("requested = %+v", got)
	}
	for _, id := range []uuid.UUID{up.ID, draining.ID} {
		if got := e.machine(id); got.State != domain.MachineFailed || got.Error != "its location was revoked" {
			t.Errorf("machine = %+v", got)
		}
	}
}

func TestAgentMachinesFollowDemand(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account, project := e.org("a")
	p, _ := e.agentProvider(account)
	e.job(account, project, nil)
	e.reconcile()
	d, err := e.c.Desired(ctx, p, nil)
	must(t, err)
	if len(d.Machines) != 1 || !d.Undelivered() {
		t.Fatalf("desired = %+v", d)
	}
	// Past the online window nothing new is asked of it.
	e.clock.Advance(domain.OnlineWindow + time.Second)
	e.job(account, project, nil)
	e.reconcile()
	if n := len(e.active()); n != 1 {
		t.Fatalf("%d machines on an offline provider", n)
	}
}

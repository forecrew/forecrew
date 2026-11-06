package testkit

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// FleetStoreSuite checks the contract of the fleet repositories on st, the
// fake and the Postgres store alike.
func FleetStoreSuite(t *testing.T, st store.Store, account, project uuid.UUID) {
	t.Helper()
	s := &fleetSuite{t: t, st: st, account: account, project: project}
	prov := s.providers()
	shape := s.shapes(prov)
	s.machines(prov, shape)
	s.retire(prov, shape)
	s.tokens(prov, shape)
	s.policies()
}

type fleetSuite struct {
	t                *testing.T
	st               store.Store
	account, project uuid.UUID
}

func (s *fleetSuite) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *fleetSuite) want(err, target error) {
	s.t.Helper()
	if !errors.Is(err, target) {
		s.t.Fatalf("error = %v, want %v", err, target)
	}
}

func fleetHash(b byte) []byte {
	h := make([]byte, 32)
	h[31] = b
	return h
}

func (s *fleetSuite) providers() domain.CapacityProvider {
	ctx := s.t.Context()
	r := s.st.CapacityProviders()
	budget := 12.5
	own := domain.CapacityProvider{AccountID: s.account, Name: "home", Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderActive,
		Limits: domain.ProviderLimits{MaxMachines: 3, MonthlyBudget: &budget}, NetworkAllow: []string{"10.0.0.5"}, Tags: map[string]string{"owner": "me"}}
	s.must(r.Create(ctx, &own))
	s.want(r.Create(ctx, &domain.CapacityProvider{AccountID: s.account, Name: "home", Kind: domain.ProviderFake}), store.ErrConflict)
	if err := r.Create(ctx, &domain.CapacityProvider{Name: "Bad Name", Kind: domain.ProviderFake}); !errors.Is(err, domain.ErrInvalidRecord) {
		s.t.Fatalf("invalid provider: %v", err)
	}
	agent := domain.CapacityProvider{Name: "cloud", Kind: domain.ProviderAWS} // instance-wide
	s.must(r.Create(ctx, &agent))
	if agent.Mode != domain.ProviderModeAgent || agent.Status != domain.ProviderPending {
		s.t.Fatalf("defaults: %+v", agent)
	}

	got, err := r.Get(ctx, own.ID)
	s.must(err)
	if got.Limits.MaxMachines != 3 || *got.Limits.MonthlyBudget != budget || got.Tags["owner"] != "me" || len(got.NetworkAllow) != 1 || got.AccountID != s.account {
		s.t.Fatalf("stored %+v", got)
	}
	_, err = r.Get(ctx, domain.NewID())
	s.want(err, store.ErrNotFound)
	if g, err := r.GetByName(ctx, uuid.Nil(), "cloud"); err != nil || g.ID != agent.ID {
		s.t.Fatalf("by name: %+v %v", g, err)
	}
	_, err = r.GetByName(ctx, s.account, "cloud")
	s.want(err, store.ErrNotFound)
	if l, err := r.List(ctx, s.account); err != nil || len(l) != 1 || l[0].ID != own.ID {
		s.t.Fatalf("list: %+v %v", l, err)
	}
	if l, err := r.List(ctx, uuid.Nil()); err != nil || len(l) != 1 || l[0].ID != agent.ID {
		s.t.Fatalf("list instance-wide: %+v %v", l, err)
	}
	if l, err := r.ListAll(ctx); err != nil || len(l) != 2 || l[0].Name != "cloud" {
		s.t.Fatalf("list all: %+v %v", l, err)
	}

	own.Limits.MaxMachines, own.Tags, own.NetworkAllow = 9, nil, nil
	s.must(r.Update(ctx, &own))
	got, err = r.Get(ctx, own.ID)
	s.must(err)
	if got.Limits.MaxMachines != 9 || len(got.Tags) != 0 || len(got.NetworkAllow) != 0 {
		s.t.Fatalf("updated %+v", got)
	}
	s.want(r.Update(ctx, &domain.CapacityProvider{ID: domain.NewID(), Name: "gone", Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderPaused}), store.ErrNotFound)

	caps := domain.ProviderCapabilities{OS: []domain.MachineOS{domain.OSLinux}, MaxCPU: 8}
	s.must(r.Seen(ctx, own.ID, &caps))
	s.must(r.Seen(ctx, own.ID, nil))
	got, err = r.Get(ctx, own.ID)
	s.must(err)
	if got.LastSeenAt.IsZero() || got.Capabilities.MaxCPU != 8 {
		s.t.Fatalf("seen %+v", got)
	}

	s.want(r.SetStatus(ctx, agent.ID, domain.ProviderActive), store.ErrConflict) // no token
	s.must(r.SetToken(ctx, agent.ID, fleetHash(1)))
	if g, err := r.GetByTokenHash(ctx, fleetHash(1)); err != nil || g.ID != agent.ID || g.Status != domain.ProviderActive {
		s.t.Fatalf("by token: %+v %v", g, err)
	}
	s.must(r.SetStatus(ctx, agent.ID, domain.ProviderRevoked))
	s.want(r.SetStatus(ctx, agent.ID, domain.ProviderActive), store.ErrConflict)
	s.want(r.SetStatus(ctx, domain.NewID(), domain.ProviderPaused), store.ErrNotFound)
	return own
}

func (s *fleetSuite) shapes(prov domain.CapacityProvider) domain.Shape {
	ctx := s.t.Context()
	r := s.st.Shapes()
	budget := 40.5
	shape := domain.Shape{AccountID: s.account, ProviderID: prov.ID, Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Labels: []string{"Docker"}, ProviderConfig: map[string]any{"node": "pve1"}, Image: "forecrew-linux", PricePerMinute: 0.25, MonthlyBudget: &budget, Enabled: true}
	s.must(r.Create(ctx, &shape))
	if shape.Max != 1 || shape.MaxJobs != 1 || shape.BootTimeoutSeconds != domain.DefaultBootTimeoutSeconds || shape.Pool != domain.PoolBuild {
		s.t.Fatalf("defaults: %+v", shape)
	}
	dup := shape
	dup.ID = uuid.Nil()
	s.want(r.Create(ctx, &dup), store.ErrConflict)
	foreign := shape
	foreign.ID, foreign.Name, foreign.AccountID = uuid.Nil(), "foreign", uuid.Nil()
	s.want(r.Create(ctx, &foreign), store.ErrNotFound) // another organization's provider
	bad := shape
	bad.ID, bad.Name, bad.CPU = uuid.Nil(), "bad", 0
	if err := r.Create(ctx, &bad); !errors.Is(err, domain.ErrInvalidRecord) {
		s.t.Fatalf("invalid shape: %v", err)
	}

	got, err := r.Get(ctx, shape.ID)
	s.must(err)
	if got.PricePerMinute != 0.25 || *got.MonthlyBudget != budget || got.ProviderConfig["node"] != "pve1" || got.Labels[0] != "docker" || !got.Enabled {
		s.t.Fatalf("stored %+v", got)
	}
	if g, err := r.GetByName(ctx, s.account, "small"); err != nil || g.ID != shape.ID {
		s.t.Fatalf("by name: %+v %v", g, err)
	}
	cloud, err := s.st.CapacityProviders().GetByName(ctx, uuid.Nil(), "cloud")
	s.must(err)
	hosted := domain.Shape{ProviderID: cloud.ID, Name: "cloud-small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 1, MemoryMB: 1024, DiskGB: 10, Image: "forecrew-linux", Enabled: true}
	s.must(r.Create(ctx, &hosted))
	if l, err := r.List(ctx, s.account); err != nil || len(l) != 1 {
		s.t.Fatalf("list: %+v %v", l, err)
	}
	if l, err := r.List(ctx, uuid.Nil()); err != nil || len(l) != 1 || l[0].ID != hosted.ID {
		s.t.Fatalf("list instance-wide: %+v %v", l, err)
	}
	if l, err := r.ListAvailable(ctx, s.account); err != nil || len(l) != 2 {
		s.t.Fatalf("available: %+v %v", l, err)
	}

	shape.CPU, shape.Max, shape.WarmMin, shape.MonthlyBudget, shape.ProviderConfig = 4, 3, 1, nil, nil
	s.must(r.Update(ctx, &shape))
	got, err = r.Get(ctx, shape.ID)
	s.must(err)
	if got.CPU != 4 || got.Max != 3 || got.MonthlyBudget != nil || got.AccountID != s.account {
		s.t.Fatalf("updated %+v", got)
	}
	gone := shape
	gone.ID = domain.NewID()
	s.want(r.Update(ctx, &gone), store.ErrNotFound)
	return shape
}

func (s *fleetSuite) machines(prov domain.CapacityProvider, shape domain.Shape) {
	ctx := s.t.Context()
	r := s.st.Machines()
	m := domain.Machine{AccountID: s.account, ProviderID: prov.ID, ShapeID: shape.ID}
	s.must(r.Create(ctx, &m))
	if m.State != domain.MachineRequested || m.AssignedAt.IsZero() {
		s.t.Fatalf("created %+v", m)
	}
	_, err := r.Get(ctx, domain.NewID())
	s.want(err, store.ErrNotFound)

	s.want(r.Assign(ctx, m.ID, s.account), store.ErrConflict) // it has one
	s.want(r.Assign(ctx, domain.NewID(), s.account), store.ErrNotFound)
	s.must(r.SetExternalID(ctx, m.ID, "vm-1"))
	if g, err := r.GetByExternalID(ctx, prov.ID, "vm-1"); err != nil || g.ID != m.ID {
		s.t.Fatalf("by external id: %+v %v", g, err)
	}

	// UpdateState is a compare-and-set along the table of the domain.
	if err := r.UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBusy, ""); !errors.Is(err, domain.ErrInvalidTransition) {
		s.t.Fatalf("transition: %v", err)
	}
	s.want(r.UpdateState(ctx, m.ID, domain.MachineBooting, domain.MachineReady, ""), store.ErrConflict)
	s.want(r.UpdateState(ctx, domain.NewID(), domain.MachineBooting, domain.MachineReady, ""), store.ErrNotFound)
	s.must(r.UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	s.must(r.MarkUp(ctx, m.ID))
	up, err := r.Get(ctx, m.ID)
	s.must(err)
	if up.State != domain.MachineBooting || up.BootingAt.IsZero() || up.ReadyAt.IsZero() {
		s.t.Fatalf("up: %+v", up)
	}
	s.must(r.MarkUp(ctx, m.ID)) // once
	again, err := r.Get(ctx, m.ID)
	s.must(err)
	if !again.ReadyAt.Equal(up.ReadyAt) {
		s.t.Fatalf("ready_at moved from %s to %s", up.ReadyAt, again.ReadyAt)
	}

	w := domain.Worker{AccountID: s.account, Name: "machine-suite", TokenHash: fleetHash(2), Ephemeral: true}
	s.must(s.st.Workers().Create(ctx, &w))
	s.must(r.SetWorker(ctx, m.ID, w.ID))
	if g, err := r.GetByWorker(ctx, s.account, w.ID); err != nil || g.ID != m.ID {
		s.t.Fatalf("by worker: %+v %v", g, err)
	}
	s.must(r.UpdateState(ctx, m.ID, domain.MachineBooting, domain.MachineReady, ""))

	j := domain.Job{AccountID: s.account, ProjectID: s.project, Role: "developer", Runtime: "codex", Spec: domain.JobSpec{Repo: "acme/app", Prompt: "go"}}
	s.must(s.st.Jobs().Create(ctx, &j))
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	taken, err := r.TakeJob(ctx, m.ID, j.ID, deadline)
	s.must(err)
	if taken.JobsTaken != 1 || taken.JobID != j.ID || !taken.DeadlineAt.Equal(deadline) || taken.State != domain.MachineReady {
		s.t.Fatalf("taken %+v", taken)
	}
	_, err = r.TakeJob(ctx, m.ID, j.ID, deadline) // max_jobs is 1
	s.want(err, store.ErrConflict)
	_, err = r.TakeJob(ctx, domain.NewID(), j.ID, deadline)
	s.want(err, store.ErrNotFound)

	s.must(r.UpdateState(ctx, m.ID, domain.MachineReady, domain.MachineFailed, "it broke"))
	s.must(r.UpdateState(ctx, m.ID, domain.MachineFailed, domain.MachineDestroyed, ""))
	gone, err := r.Get(ctx, m.ID)
	s.must(err)
	if gone.Error != "it broke" || gone.DestroyedAt.IsZero() {
		s.t.Fatalf("destroyed %+v", gone)
	}

	live := domain.Machine{AccountID: s.account, ProviderID: prov.ID, ShapeID: shape.ID}
	s.must(r.Create(ctx, &live))
	for _, tt := range []struct {
		f    store.MachineFilter
		want int
	}{
		{store.MachineFilter{AccountID: s.account}, 2},
		{store.MachineFilter{AccountID: domain.NewID()}, 0},
		{store.MachineFilter{ProviderID: prov.ID, Active: true}, 1},
		{store.MachineFilter{ShapeID: shape.ID, State: domain.MachineDestroyed}, 1},
		{store.MachineFilter{Since: gone.DestroyedAt.Add(time.Hour)}, 1},
		{store.MachineFilter{Since: gone.DestroyedAt.Add(-time.Hour)}, 2},
		{store.MachineFilter{Limit: 1}, 1},
	} {
		l, err := r.List(ctx, tt.f)
		s.must(err)
		if len(l) != tt.want {
			s.t.Errorf("filter %+v: %d machines, want %d", tt.f, len(l), tt.want)
		}
	}
	s.must(r.UpdateState(ctx, live.ID, domain.MachineRequested, domain.MachineDestroyed, ""))
}

// retire checks that Retire leaves a machine in use alone: in another state,
// with a job taken since the look, or with a job running.
func (s *fleetSuite) retire(prov domain.CapacityProvider, shape domain.Shape) {
	ctx := s.t.Context()
	r := s.st.Machines()
	m := domain.Machine{AccountID: s.account, ProviderID: prov.ID, ShapeID: shape.ID}
	s.must(r.Create(ctx, &m))
	s.must(r.UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	w := domain.Worker{AccountID: s.account, Name: "machine-retire", TokenHash: fleetHash(7), Ephemeral: true}
	s.must(s.st.Workers().Create(ctx, &w))
	s.must(r.SetWorker(ctx, m.ID, w.ID))
	s.must(r.UpdateState(ctx, m.ID, domain.MachineBooting, domain.MachineReady, ""))

	s.want(r.Retire(ctx, m.ID, domain.MachineBusy, 0), store.ErrConflict)
	s.want(r.Retire(ctx, m.ID, domain.MachineReady, 1), store.ErrConflict)
	s.want(r.Retire(ctx, domain.NewID(), domain.MachineReady, 0), store.ErrNotFound)
	if err := r.Retire(ctx, m.ID, domain.MachineRequested, 0); !errors.Is(err, domain.ErrInvalidTransition) {
		s.t.Fatalf("transition: %v", err)
	}
	j, err := s.st.Jobs().LeaseNext(ctx, s.account, w.ID, []string{"runtime:codex"}, domain.LeaseDuration)
	s.must(err)
	s.want(r.Retire(ctx, m.ID, domain.MachineReady, 0), store.ErrConflict) // it runs a job
	_, err = s.st.Jobs().Complete(ctx, s.account, j.ID, w.ID, store.JobResult{Status: domain.JobSucceeded})
	s.must(err)
	s.must(r.Retire(ctx, m.ID, domain.MachineReady, 0))
	got, err := r.Get(ctx, m.ID)
	s.must(err)
	if got.State != domain.MachineDraining {
		s.t.Fatalf("machine = %s", got.State)
	}
	s.must(r.UpdateState(ctx, m.ID, domain.MachineDraining, domain.MachineDestroyed, ""))
}

func (s *fleetSuite) tokens(prov domain.CapacityProvider, shape domain.Shape) {
	ctx := s.t.Context()
	r := s.st.BootstrapTokens()
	cloud, err := s.st.Shapes().GetByName(ctx, uuid.Nil(), "cloud-small")
	s.must(err)
	warm := domain.Machine{ProviderID: cloud.ProviderID, ShapeID: cloud.ID}
	s.must(s.st.Machines().Create(ctx, &warm))
	if !warm.AssignedAt.IsZero() {
		s.t.Fatalf("warm machine assigned: %+v", warm)
	}
	s.want(r.Assign(ctx, warm.ID, s.account, time.Now()), store.ErrNotFound) // no token yet

	first := domain.BootstrapToken{MachineID: warm.ID, TokenHash: fleetHash(3)}
	s.must(r.Issue(ctx, &first))
	second := domain.BootstrapToken{MachineID: warm.ID, TokenHash: fleetHash(4)}
	s.must(r.Issue(ctx, &second)) // a new delivery replaces it
	_, err = r.GetByTokenHash(ctx, fleetHash(3))
	s.want(err, store.ErrNotFound)
	_, err = r.Consume(ctx, fleetHash(3))
	s.want(err, store.ErrNotFound)
	if err := r.Issue(ctx, &domain.BootstrapToken{MachineID: warm.ID}); !errors.Is(err, domain.ErrInvalidRecord) {
		s.t.Fatalf("no hash: %v", err)
	}

	_, err = r.Consume(ctx, fleetHash(4))
	s.want(err, store.ErrConflict) // unassigned
	s.must(s.st.Machines().Assign(ctx, warm.ID, s.account))
	s.must(r.Assign(ctx, warm.ID, s.account, time.Now().Add(time.Hour)))
	_, err = r.Consume(ctx, fleetHash(4))
	s.want(err, store.ErrConflict) // not booting
	s.must(s.st.Machines().UpdateState(ctx, warm.ID, domain.MachineRequested, domain.MachineBooting, ""))
	used, err := r.Consume(ctx, fleetHash(4))
	s.must(err)
	if used.UsedAt.IsZero() || used.AccountID != s.account || used.MachineID != warm.ID {
		s.t.Fatalf("used %+v", used)
	}
	_, err = r.Consume(ctx, fleetHash(4))
	s.want(err, store.ErrConflict) // once
	third := domain.BootstrapToken{MachineID: warm.ID, AccountID: s.account, TokenHash: fleetHash(5), ExpiresAt: time.Now().Add(time.Hour)}
	s.want(r.Issue(ctx, &third), store.ErrConflict) // never after use

	// An expired token is no use either.
	m := domain.Machine{AccountID: s.account, ProviderID: prov.ID, ShapeID: shape.ID}
	s.must(s.st.Machines().Create(ctx, &m))
	s.must(s.st.Machines().UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""))
	old := domain.BootstrapToken{MachineID: m.ID, AccountID: s.account, TokenHash: fleetHash(6), ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.must(r.Issue(ctx, &old))
	_, err = r.Consume(ctx, fleetHash(6))
	s.want(err, store.ErrConflict)
}

func (s *fleetSuite) policies() {
	ctx := s.t.Context()
	r := s.st.FleetPolicies()
	p, err := r.Get(ctx, s.account)
	s.must(err)
	if p.Routing != domain.RoutingOwnOnly || p.Plan != domain.PlanFree {
		s.t.Fatalf("default %+v", p)
	}
	budget := 200.0
	p = domain.FleetPolicy{AccountID: s.account, Routing: domain.RoutingOwnFirst, Plan: domain.PlanPaid, AllowedShapes: []string{"small"},
		Quotas: domain.FleetQuotas{ConcurrentMachines: 5, MonthlyBudget: &budget}, Egress: []string{"10.0.0.0/8"}}
	s.must(r.Put(ctx, &p))
	p.Quotas.ConcurrentMachines = 6
	s.must(r.Put(ctx, &p))
	got, err := r.Get(ctx, s.account)
	s.must(err)
	if got.Routing != domain.RoutingOwnFirst || got.Quotas.ConcurrentMachines != 6 || *got.Quotas.MonthlyBudget != budget || len(got.Egress) != 1 || got.AllowedShapes[0] != "small" {
		s.t.Fatalf("stored %+v", got)
	}
	bad := domain.FleetPolicy{AccountID: s.account, Routing: "anywhere", Plan: domain.PlanFree}
	if err := r.Put(ctx, &bad); !errors.Is(err, domain.ErrInvalidRecord) {
		s.t.Fatalf("invalid policy: %v", err)
	}
}

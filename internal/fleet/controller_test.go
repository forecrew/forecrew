package fleet_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// env is a controller on the fake store with one organization and a direct
// fake provider.
type env struct {
	t       *testing.T
	ctx     context.Context
	st      *testkit.Store
	clock   *testkit.Clock
	account uuid.UUID
	project uuid.UUID
	fake    *testkit.FakeProvider
	drivers map[uuid.UUID]fleet.Provider
	prov    domain.CapacityProvider
	c       *fleet.Controller
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	e := &env{t: t, ctx: t.Context(), clock: clock, st: testkit.NewStore(clock.Now), fake: testkit.NewFakeProvider(), drivers: map[uuid.UUID]fleet.Provider{}}
	e.account, e.project = e.org("acme")
	e.prov = e.provider(e.account, "home", e.fake)
	e.restart()
	return e
}

// restart is forecrewd coming back: a new controller on the same store.
func (e *env) restart() {
	e.c = fleet.New(fleet.Config{
		Store: e.st, Now: e.clock.Now, Currency: "CHF", ServerURL: "https://forecrew.example.com",
		Direct: func(p domain.CapacityProvider) fleet.Provider { return e.drivers[p.ID] },
	})
}

func (e *env) org(name string) (account, project uuid.UUID) {
	a := domain.Account{Name: name}
	must(e.t, e.st.Accounts().Create(e.ctx, &a))
	p := domain.Project{AccountID: a.ID, Name: "app", Repo: name + "/app", Status: domain.ProjectOnboarded}
	must(e.t, e.st.Projects().Create(e.ctx, &p))
	return a.ID, p.ID
}

func (e *env) provider(account uuid.UUID, name string, d fleet.Provider) domain.CapacityProvider {
	p := domain.CapacityProvider{AccountID: account, Name: name, Kind: domain.ProviderFake, Mode: domain.ProviderModeDirect, Status: domain.ProviderActive}
	must(e.t, e.st.CapacityProviders().Create(e.ctx, &p))
	if d != nil {
		e.drivers[p.ID] = d
	}
	return p
}

// shape adds a shape; edit changes it first. The default takes one job, is
// destroyed at once when idle, and may have 5 machines.
func (e *env) shape(p domain.CapacityProvider, name string, edit func(*domain.Shape)) domain.Shape {
	s := domain.Shape{AccountID: p.AccountID, ProviderID: p.ID, Name: name, OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Labels: []string{"runtime:codex"}, Image: "forecrew-linux", Max: 5, BootTimeoutSeconds: 60, Enabled: true}
	if edit != nil {
		edit(&s)
	}
	must(e.t, e.st.Shapes().Create(e.ctx, &s))
	return s
}

func (e *env) job(account, project uuid.UUID, edit func(*domain.Job)) domain.Job {
	j := domain.Job{AccountID: account, ProjectID: project, Role: "developer", Runtime: "codex", Spec: domain.JobSpec{Repo: "acme/app", Prompt: "go", Timeout: domain.Duration(time.Hour)}}
	if edit != nil {
		edit(&j)
	}
	must(e.t, e.st.Jobs().Create(e.ctx, &j))
	return j
}

func (e *env) reconcile() {
	e.t.Helper()
	must(e.t, e.c.Reconcile(e.ctx))
}

func (e *env) machines(f store.MachineFilter) []domain.Machine {
	e.t.Helper()
	m, err := e.st.Machines().List(e.ctx, f)
	must(e.t, err)
	return m
}

func (e *env) active() []domain.Machine { return e.machines(store.MachineFilter{Active: true}) }

func (e *env) machine(id uuid.UUID) domain.Machine {
	e.t.Helper()
	m, err := e.st.Machines().Get(e.ctx, id)
	must(e.t, err)
	return m
}

// boot is the guest of req calling forecrewd: the worker gets its token.
func (e *env) boot(req fleet.CreateRequest) domain.Worker {
	e.t.Helper()
	g, err := fleet.Exchange(e.ctx, e.st, hashOf(req.Bootstrap.Token), "v1")
	must(e.t, err)
	if g.Wait {
		e.t.Fatal("told to wait")
	}
	m := e.machine(req.MachineID)
	w, err := e.st.Workers().Get(e.ctx, m.AccountID, m.WorkerID)
	must(e.t, err)
	return w
}

// lease is a poll of w, as the worker API does it.
func (e *env) lease(w domain.Worker) (domain.Job, error) {
	var j domain.Job
	err := e.st.WithTx(e.ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if j, err = tx.Jobs().LeaseNext(ctx, w.AccountID, w.ID, w.Labels, domain.LeaseDuration); err != nil {
			return err
		}
		return fleet.RecordLease(ctx, tx, w, j)
	})
	return j, err
}

func (e *env) finishJob(w domain.Worker, j domain.Job) {
	e.t.Helper()
	_, err := e.st.Jobs().Complete(e.ctx, w.AccountID, j.ID, w.ID, store.JobResult{Status: domain.JobSucceeded})
	must(e.t, err)
}

func (e *env) reason(j domain.Job) fleet.Reason {
	e.t.Helper()
	r, ok, err := e.c.Explain(e.ctx, j)
	must(e.t, err)
	if !ok {
		e.t.Fatalf("no reason for %s", j.Ref())
	}
	return r
}

func (e *env) audited(account uuid.UUID, action string) int {
	e.t.Helper()
	l, err := e.st.Audit().List(e.ctx, account, store.AuditFilter{})
	must(e.t, err)
	n := 0
	for _, a := range l {
		if a.Action == action {
			n++
		}
	}
	return n
}

func wantReason(t *testing.T, got fleet.Reason, code fleet.ReasonCode, sentence string) {
	t.Helper()
	if got.Code != code || got.Sentence != sentence {
		t.Fatalf("reason = %s %q, want %s %q", got.Code, got.Sentence, code, sentence)
	}
}

// The whole life of a machine: job queued, machine created, job run, machine
// destroyed, on the fake provider.
func TestMachineLife(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)

	e.reconcile()
	reqs := e.fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d creates", len(reqs))
	}
	req := reqs[0]
	if !strings.HasPrefix(req.Bootstrap.Token, "fcb_") || req.Bootstrap.ServerURL != "https://forecrew.example.com" {
		t.Fatalf("bootstrap = %v", req.Bootstrap)
	}
	if req.Tags[fleet.TagMachine] != req.MachineID.String() || req.Tags[fleet.TagOrg] != e.account.String() {
		t.Fatalf("tags = %v", req.Tags)
	}
	m := e.machine(req.MachineID)
	if m.State != domain.MachineBooting || m.ExternalID == "" || m.AccountID != e.account {
		t.Fatalf("machine = %+v", m)
	}
	wantReason(t, e.reason(j), fleet.ReasonBooting, "Starting a machine")

	e.reconcile() // no second machine for the same job
	if n := len(e.fake.Requests()); n != 1 {
		t.Fatalf("%d creates", n)
	}

	e.clock.Advance(20 * time.Second)
	e.runOnMachine(req, j)
	if e.audited(e.account, fleet.ActionCreated) != 1 || e.audited(e.account, fleet.ActionDestroyed) != 1 {
		t.Fatal("create and destroy not audited")
	}
}

// runOnMachine runs job j on the machine of req, from boot to its end.
func (e *env) runOnMachine(req fleet.CreateRequest, j domain.Job) {
	t := e.t
	t.Helper()
	w := e.boot(req)
	if !w.Ephemeral || w.Pool != domain.PoolBuild || w.MaxJobs != 1 || !slices.Contains(w.Labels, "linux") {
		t.Fatalf("worker = %+v", w)
	}
	if got := e.reason(j); got != (fleet.Reason{}) {
		t.Fatalf("a job with a ready machine waits: %+v", got)
	}
	e.reconcile() // a ready machine with a job on its way stays
	if m := e.machine(req.MachineID); m.State != domain.MachineReady {
		t.Fatalf("machine = %s", m.State)
	}

	leased, err := e.lease(w)
	must(t, err)
	m := e.machine(req.MachineID)
	if leased.ID != j.ID || m.State != domain.MachineBusy || m.JobID != j.ID || m.JobsTaken != 1 {
		t.Fatalf("after lease: %+v", m)
	}
	if want := leased.LeaseExpiresAt.Add(time.Hour); !m.DeadlineAt.Equal(want) {
		t.Fatalf("deadline = %s, want %s", m.DeadlineAt, want)
	}
	e.reconcile()
	upd := e.fake.Updates()[m.ExternalID]
	if !upd.Deadline.Equal(m.DeadlineAt) || upd.Tags[fleet.TagJob] != j.Ref() {
		t.Fatalf("update = %+v", upd)
	}
	if m := e.machine(req.MachineID); m.State != domain.MachineBusy {
		t.Fatalf("machine with a running job = %s", m.State)
	}

	e.finishJob(w, leased)
	e.reconcile()
	m = e.machine(req.MachineID)
	if m.State != domain.MachineDestroyed || m.DestroyedAt.IsZero() || len(e.fake.Instances()) != 0 {
		t.Fatalf("after the job: %+v, %d instances", m, len(e.fake.Instances()))
	}
	cur, err := e.st.Workers().Get(e.ctx, e.account, w.ID)
	must(t, err)
	if !cur.Revoked() {
		t.Fatal("worker not revoked")
	}
}

func TestMaxJobs(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", func(s *domain.Shape) { s.MaxJobs = 2; s.IdleSeconds = 60 })
	first := e.job(e.account, e.project, nil)
	e.reconcile()
	w := e.boot(e.fake.Requests()[0])
	id := e.fake.Requests()[0].MachineID
	j, err := e.lease(w)
	must(t, err)
	e.finishJob(w, j)

	// It still has a job in it: it stays for its idle time, then goes.
	e.reconcile()
	if m := e.machine(id); m.State != domain.MachineBusy {
		t.Fatalf("machine = %s", m.State)
	}
	second := e.job(e.account, e.project, nil)
	e.reconcile()
	if len(e.fake.Requests()) != 1 {
		t.Fatal("a second machine for a job the first can take")
	}
	j, err = e.lease(w)
	must(t, err)
	if j.ID != second.ID || first.ID == second.ID {
		t.Fatalf("leased %s", j.Ref())
	}
	third := e.job(e.account, e.project, nil)
	_, err = e.lease(w) // its last job is taken
	if !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("third lease: %v", err)
	}
	_ = third
	e.finishJob(w, j)
	e.reconcile()
	if m := e.machine(id); m.State != domain.MachineDestroyed {
		t.Fatalf("machine = %s", m.State)
	}
	if n := len(e.fake.Requests()); n != 2 {
		t.Fatalf("%d creates: the third job needs a new machine", n)
	}
}

func TestIdleMachine(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", func(s *domain.Shape) { s.MaxJobs = 2; s.IdleSeconds = 60 })
	e.job(e.account, e.project, nil)
	e.reconcile()
	w := e.boot(e.fake.Requests()[0])
	id := e.fake.Requests()[0].MachineID
	j, err := e.lease(w)
	must(t, err)
	e.clock.Advance(10 * time.Minute)
	e.finishJob(w, j)
	e.clock.Advance(59 * time.Second)
	e.reconcile()
	if m := e.machine(id); m.State != domain.MachineBusy {
		t.Fatalf("idle time counts from the end of the job: %s", m.State)
	}
	e.clock.Advance(time.Second)
	e.reconcile()
	if m := e.machine(id); m.State != domain.MachineDestroyed {
		t.Fatalf("machine = %s", m.State)
	}
}

func TestWithdrawnWhenNotNeeded(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = fleet.ErrCapacity })
	e.reconcile()
	m := e.active()
	if len(m) != 1 || m[0].State != domain.MachineRequested {
		t.Fatalf("no room must leave it requested: %+v", m)
	}
	e.reconcile()
	if len(e.active()) != 1 {
		t.Fatal("a second row while the first waits for room")
	}
	_, err := e.st.Jobs().Cancel(e.ctx, e.account, j.ID)
	must(t, err)
	e.reconcile()
	if got := e.machine(m[0].ID); got.State != domain.MachineDestroyed {
		t.Fatalf("machine = %s", got.State)
	}
}

func TestWarmMinimum(t *testing.T) {
	e := newEnv(t)
	s := e.shape(e.prov, "small", func(s *domain.Shape) { s.WarmMin = 2 })
	e.reconcile()
	reqs := e.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d machines, want the warm minimum", len(reqs))
	}
	w := e.boot(reqs[0])
	e.boot(reqs[1])
	e.reconcile()
	if n := len(e.active()); n != 2 {
		t.Fatalf("%d machines: ready machines at the minimum stay", n)
	}

	// A job takes one, and the minimum is made up at once; the machine does
	// not come back after its job.
	e.job(e.account, e.project, nil)
	e.reconcile()
	if n := len(e.fake.Requests()); n != 3 {
		t.Fatalf("%d creates", n)
	}
	j, err := e.lease(w)
	must(t, err)
	e.reconcile()
	if n := len(e.fake.Requests()); n != 3 {
		t.Fatalf("%d creates after the lease", n)
	}
	e.finishJob(w, j)
	e.reconcile()
	if m := e.machine(reqs[0].MachineID); m.State != domain.MachineDestroyed {
		t.Fatalf("machine after its job = %s", m.State)
	}
	if n := len(e.active()); n != 2 {
		t.Fatalf("%d machines", n)
	}

	// Above the minimum a machine goes.
	s.WarmMin = 1
	must(t, e.st.Shapes().Update(e.ctx, &s))
	e.boot(e.fake.Requests()[2])
	e.reconcile()
	if n := len(e.active()); n != 1 {
		t.Fatalf("%d machines above warm_min 1", n)
	}

	// A disabled shape keeps nothing warm and starts nothing new.
	s.Enabled = false
	must(t, e.st.Shapes().Update(e.ctx, &s))
	e.reconcile()
	if n := len(e.active()); n != 0 {
		t.Fatalf("%d machines of a disabled shape", n)
	}
}

// hosted returns an instance-wide provider and a shape on it.
func (e *env) hosted(edit func(*domain.Shape)) (*testkit.FakeProvider, domain.Shape) {
	f := testkit.NewFakeProvider()
	p := e.provider(uuid.Nil(), "cloud", f)
	return f, e.shape(p, "cloud-small", edit)
}

func (e *env) policy(account uuid.UUID, routing domain.FleetRouting, edit func(*domain.FleetPolicy)) {
	p := domain.DefaultFleetPolicy(account)
	p.Routing = routing
	if edit != nil {
		edit(&p)
	}
	must(e.t, e.st.FleetPolicies().Put(e.ctx, &p))
}

func TestWarmMachineAssignment(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.WarmMin = 1; s.BootTimeoutSeconds = 60 })
	e.policy(e.account, domain.RoutingHosted, func(p *domain.FleetPolicy) { p.Egress = []string{"10.1.0.0/16"} })
	e.reconcile()
	req := cloud.Requests()[0]
	m := e.machine(req.MachineID)
	if m.AccountID != uuid.Nil() || m.State != domain.MachineBooting {
		t.Fatalf("warm machine = %+v", m)
	}
	if _, ok := req.Tags[fleet.TagOrg]; ok {
		t.Fatalf("tags of an unassigned machine: %v", req.Tags)
	}

	// Its worker waits, which makes it warm and stops the boot timeout.
	hash := hashOf(req.Bootstrap.Token)
	g, err := fleet.Exchange(e.ctx, e.st, hash, "v1")
	must(t, err)
	if !g.Wait || !e.machine(m.ID).Warm() {
		t.Fatalf("grant = %+v, machine = %+v", g, e.machine(m.ID))
	}
	e.clock.Advance(time.Hour)
	e.reconcile()
	if got := e.machine(m.ID); got.State != domain.MachineBooting || len(cloud.Requests()) != 1 {
		t.Fatalf("a warm machine waits as long as it takes: %+v", got)
	}

	j := e.job(e.account, e.project, nil)
	e.reconcile()
	m = e.machine(m.ID)
	if m.AccountID != e.account || m.AssignedAt.IsZero() {
		t.Fatalf("assigned = %+v", m)
	}
	if e.audited(e.account, fleet.ActionAssigned) != 1 {
		t.Fatal("assignment not audited")
	}
	upd := cloud.Updates()[m.ExternalID]
	if upd.Tags[fleet.TagOrg] != e.account.String() || !slices.Contains(upd.NetworkAllow, "10.1.0.0/16") {
		t.Fatalf("update = %+v", upd)
	}
	if n := len(cloud.Requests()); n != 2 {
		t.Fatalf("%d creates: the warm minimum is made up", n)
	}
	_ = j
	g, err = fleet.Exchange(e.ctx, e.st, hash, "v1")
	must(t, err)
	if g.Wait || g.WorkerToken == "" {
		t.Fatalf("grant = %+v", g)
	}
}

func TestAssignedWarmMachineThatNeverCallsAgain(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.WarmMin = 1 })
	e.policy(e.account, domain.RoutingHosted, nil)
	e.reconcile()
	req := cloud.Requests()[0]
	_, err := fleet.Exchange(e.ctx, e.st, hashOf(req.Bootstrap.Token), "v1")
	must(t, err)
	e.job(e.account, e.project, nil)
	e.reconcile()
	e.clock.Advance(time.Minute)
	e.reconcile()
	if m := e.machine(req.MachineID); m.State != domain.MachineDestroyed || !strings.Contains(m.Error, "did not come up") {
		t.Fatalf("machine = %+v", m)
	}
}

func TestLimitsStopCreation(t *testing.T) {
	budget := 5.0
	tests := []struct {
		name     string
		shape    func(*domain.Shape)
		policy   func(*domain.FleetPolicy)
		provider func(*domain.CapacityProvider)
		code     fleet.ReasonCode
		sentence string
	}{
		{name: "machine type full", shape: func(s *domain.Shape) { s.Max = 1 }, code: fleet.ReasonQueue, sentence: "First in line for Small"},
		{name: "organization limit", policy: func(p *domain.FleetPolicy) { p.Quotas.ConcurrentMachines = 1 }, code: fleet.ReasonLimit, sentence: "At your limit of 1 machine at once"},
		{name: "location limit", provider: func(p *domain.CapacityProvider) { p.Limits.MaxMachines = 1 }, code: fleet.ReasonLimit, sentence: "Home is at its limit of 1 machine at once"},
		{name: "shape budget", shape: func(s *domain.Shape) { s.PricePerMinute = 1; s.MonthlyBudget = &budget }, code: fleet.ReasonBudget, sentence: "Small's limit of CHF 5 this month is used up"},
		{name: "location budget", shape: func(s *domain.Shape) { s.PricePerMinute = 1 }, provider: func(p *domain.CapacityProvider) { p.Limits.MonthlyBudget = &budget }, code: fleet.ReasonBudget, sentence: "Home's limit of CHF 5 this month is used up"},
		{name: "monthly minutes", policy: func(p *domain.FleetPolicy) { p.Quotas.MonthlyMinutes = 5 }, code: fleet.ReasonBudget, sentence: "This month's limit of 5 minutes is used up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.provider != nil {
				tt.provider(&e.prov)
				must(t, e.st.CapacityProviders().Update(e.ctx, &e.prov))
			}
			e.shape(e.prov, "small", tt.shape)
			e.policy(e.account, domain.RoutingOwnOnly, tt.policy)
			e.job(e.account, e.project, nil)
			e.reconcile()
			if n := len(e.active()); n != 1 {
				t.Fatalf("%d machines", n)
			}
			e.boot(e.fake.Requests()[0])
			e.clock.Advance(6 * time.Minute) // the budgets are used up by now
			j := e.job(e.account, e.project, nil)
			e.reconcile()
			if n := len(e.active()); n != 1 {
				t.Fatalf("%d machines past the limit", n)
			}
			// The first job holds the machine; the second waits with why.
			wantReason(t, e.reason(j), tt.code, tt.sentence)
			if j, _ := e.st.Jobs().Get(e.ctx, e.account, j.ID); j.Status != domain.JobQueued {
				t.Fatalf("job = %s: it waits rather than fails", j.Status)
			}
		})
	}
}

func TestHostedBudgetCountsHostedMachinesOnly(t *testing.T) {
	e := newEnv(t)
	budget := 5.0
	cloud, _ := e.hosted(func(s *domain.Shape) { s.PricePerMinute = 1; s.BootTimeoutSeconds = 3600 })
	e.shape(e.prov, "paid-at-home", func(s *domain.Shape) { s.PricePerMinute = 9; s.Max = 1; s.BootTimeoutSeconds = 3600 })
	e.policy(e.account, domain.RoutingOwnFirst, func(p *domain.FleetPolicy) { p.Quotas.MonthlyBudget = &budget })
	e.job(e.account, e.project, nil)
	e.reconcile()
	e.job(e.account, e.project, nil) // the own shape is full: this one goes hosted
	e.reconcile()
	if len(e.fake.Requests()) != 1 || len(cloud.Requests()) != 1 {
		t.Fatalf("own %d, hosted %d", len(e.fake.Requests()), len(cloud.Requests()))
	}
	e.clock.Advance(4 * time.Minute)
	e.job(e.account, e.project, nil)
	e.reconcile()
	if len(cloud.Requests()) != 2 {
		t.Fatalf("hosted %d: 4 of the 5 are used, whatever the own machine costs", len(cloud.Requests()))
	}
	e.clock.Advance(2 * time.Minute)
	last := e.job(e.account, e.project, nil)
	e.reconcile()
	if n := len(cloud.Requests()); n != 2 {
		t.Fatalf("hosted %d past the budget", n)
	}
	e.policy(e.account, domain.RoutingHosted, func(p *domain.FleetPolicy) { p.Quotas.MonthlyBudget = &budget })
	wantReason(t, e.reason(last), fleet.ReasonBudget, "This month's limit of CHF 5 is used up")
}

func TestBudgetsRestartEachMonth(t *testing.T) {
	e := newEnv(t)
	budget := 5.0
	e.shape(e.prov, "small", func(s *domain.Shape) { s.PricePerMinute = 1; s.MonthlyBudget = &budget })
	e.job(e.account, e.project, nil)
	e.reconcile()
	w := e.boot(e.fake.Requests()[0])
	j, err := e.lease(w)
	must(t, err)
	e.clock.Advance(6 * time.Minute)
	e.finishJob(w, j)
	e.reconcile()
	if n := len(e.active()); n != 0 {
		t.Fatalf("%d machines", n)
	}
	next := e.job(e.account, e.project, nil)
	e.reconcile()
	wantReason(t, e.reason(next), fleet.ReasonBudget, "Small's limit of CHF 5 this month is used up")

	e.clock.Advance(30 * 24 * time.Hour) // into April
	e.reconcile()
	if n := len(e.active()); n != 1 {
		t.Fatalf("%d machines in the new month", n)
	}
}

func TestBootFailureReplaced(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.reconcile()
	first := e.fake.Requests()[0]

	e.clock.Advance(59 * time.Second)
	e.reconcile()
	if m := e.machine(first.MachineID); m.State != domain.MachineBooting {
		t.Fatalf("machine = %s", m.State)
	}
	e.clock.Advance(time.Second)
	e.reconcile()
	m := e.machine(first.MachineID)
	if m.State != domain.MachineDestroyed || !strings.Contains(m.Error, "did not come up within 60 seconds") {
		t.Fatalf("machine = %+v", m)
	}
	reqs := e.fake.Requests()
	if len(reqs) != 2 || reqs[1].MachineID == first.MachineID {
		t.Fatalf("%d creates", len(reqs))
	}
	if len(e.fake.Instances()) != 1 {
		t.Fatalf("%d instances", len(e.fake.Instances()))
	}
	if got, _ := e.st.Jobs().Get(e.ctx, e.account, j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job = %s", got.Status)
	}
	if e.audited(e.account, fleet.ActionFailed) != 1 {
		t.Fatal("failure not audited")
	}
	// A copied token cannot bring it back.
	if _, err := fleet.Exchange(e.ctx, e.st, hashOf(first.Bootstrap.Token), "v1"); !errors.Is(err, fleet.ErrBootstrapRefused) {
		t.Fatalf("exchange = %v", err)
	}
}

func TestCreateFailureReplaced(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.job(e.account, e.project, nil)
	e.fake.Configure(func(p *testkit.FakeProvider) { p.FailCreates = 1 })
	e.reconcile()
	failed := e.machines(store.MachineFilter{State: domain.MachineFailed})
	if len(failed) != 1 || !strings.Contains(failed[0].Error, "failed to create") {
		t.Fatalf("failed = %+v", failed)
	}
	e.reconcile()
	if m := e.machine(failed[0].ID); m.State != domain.MachineDestroyed {
		t.Fatalf("machine = %s", m.State)
	}
	if n := len(e.fake.Requests()); n != 1 {
		t.Fatalf("%d creates", n)
	}
}

func TestMachineGoneFromProvider(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	_, err := e.lease(w)
	must(t, err)
	e.fake.Vanish(e.machine(req.MachineID).ExternalID)
	e.reconcile()
	m := e.machine(req.MachineID)
	if m.Error != "gone from the provider" || m.State == domain.MachineBusy {
		t.Fatalf("machine = %+v", m)
	}
	if cur, _ := e.st.Workers().Get(e.ctx, e.account, w.ID); !cur.Revoked() {
		t.Fatal("worker not revoked")
	}
}

// Whatever step a crash interrupts, the next controller finishes it with no
// second machine.
func TestCrashAndRestart(t *testing.T) {
	t.Run("after the row, before the provider heard", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		e.job(e.account, e.project, nil)
		e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = fleet.ErrCapacity })
		e.reconcile()
		e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = nil })
		e.restart()
		e.reconcile()
		e.reconcile()
		if n, m := len(e.fake.Instances()), len(e.active()); n != 1 || m != 1 {
			t.Fatalf("%d instances, %d machines", n, m)
		}
	})
	t.Run("after the provider started it, before the report", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		e.job(e.account, e.project, nil)
		e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = fleet.ErrCapacity })
		e.reconcile()
		id := e.active()[0].ID
		e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = nil })
		e.fake.Plant("vm-crash", id, nil)
		e.restart()
		e.reconcile()
		m := e.machine(id)
		if m.State != domain.MachineBooting || m.ExternalID != "vm-crash" {
			t.Fatalf("machine = %+v", m)
		}
		if n := len(e.fake.Instances()); n != 1 || len(e.fake.Requests()) != 0 {
			t.Fatalf("%d instances, %d creates", n, len(e.fake.Requests()))
		}
	})
	t.Run("while machines run", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", func(s *domain.Shape) { s.WarmMin = 1 })
		e.job(e.account, e.project, nil)
		e.reconcile()
		before := len(e.fake.Requests())
		e.restart()
		e.reconcile()
		e.restart()
		e.reconcile()
		if n := len(e.fake.Requests()); n != before || n != 2 {
			t.Fatalf("%d creates, was %d", n, before)
		}
	})
}

func TestOrphans(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.fake.Plant("vm-unknown", uuid.Nil(), nil)
	e.fake.Plant("vm-stranger", domain.NewID(), nil)
	e.reconcile()
	if n := len(e.fake.Instances()); n != 0 {
		t.Fatalf("%d instances left", n)
	}
	if e.audited(e.account, fleet.ActionOrphan) != 2 {
		t.Fatal("orphans not audited")
	}

	// Past its time: destroyed whatever it is doing.
	e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	_, err := e.lease(w)
	must(t, err)
	e.clock.Advance(2 * time.Hour)
	e.reconcile()
	if m := e.machine(req.MachineID); m.State != domain.MachineDestroyed {
		t.Fatalf("machine past its deadline = %s", m.State)
	}
	if n := len(e.fake.Instances()); n != 0 {
		t.Fatalf("%d instances left", n)
	}

	// A machine that failed and is still there.
	e.fake.Plant("vm-late", req.MachineID, nil)
	e.reconcile()
	if slices.Contains(instanceIDs(e.fake), "vm-late") {
		t.Fatal("instance of a destroyed machine left")
	}
}

func instanceIDs(f *testkit.FakeProvider) []string {
	var out []string
	for _, i := range f.Instances() {
		out = append(out, i.ExternalID)
	}
	return out
}

func TestOrphanOfAnotherOrganizationIsAuditedThere(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(nil)
	other, _ := e.org("other")
	cloud.Plant("vm-1", uuid.Nil(), map[string]string{fleet.TagOrg: other.String()})
	cloud.Plant("vm-2", uuid.Nil(), nil)
	e.reconcile()
	if n := len(cloud.Instances()); n != 0 {
		t.Fatalf("%d instances", n)
	}
	if e.audited(other, fleet.ActionOrphan) != 1 {
		t.Fatal("orphan not audited in its organization")
	}
}

// A machine of organization A never leases a job of organization B.
func TestOrganizationsStayApart(t *testing.T) {
	e := newEnv(t)
	cloud, _ := e.hosted(func(s *domain.Shape) { s.MaxJobs = 2 })
	orgB, projectB := e.org("other")
	e.policy(e.account, domain.RoutingHosted, nil)
	e.policy(orgB, domain.RoutingHosted, nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	a := e.boot(cloud.Requests()[0])

	jobB := e.job(orgB, projectB, nil)
	e.reconcile()
	reqs := cloud.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d creates: B must get its own machine", len(reqs))
	}
	if reqs[1].Tags[fleet.TagOrg] != orgB.String() {
		t.Fatalf("tags = %v", reqs[1].Tags)
	}

	j, err := e.lease(a)
	must(t, err)
	if j.AccountID != e.account {
		t.Fatalf("leased %+v", j)
	}
	if _, err := e.lease(a); !errors.Is(err, store.ErrNoJob) {
		t.Fatalf("with a place left and B's job queued: %v", err)
	}
	if cur, _ := e.st.Jobs().Get(e.ctx, orgB, jobB.ID); cur.Status != domain.JobQueued {
		t.Fatalf("job of B = %s", cur.Status)
	}

	// Nor does a shape of one organization serve another.
	e.shape(e.prov, "only-a", func(s *domain.Shape) { s.Labels = append(s.Labels, "only-a") })
	j = e.job(orgB, projectB, func(j *domain.Job) { j.Needs = []string{"only-a"} })
	if got := e.reason(j); got.Code != fleet.ReasonNoShape {
		t.Fatalf("reason = %+v", got)
	}
}

func TestWaitReasons(t *testing.T) {
	windows := func(j *domain.Job) {
		j.Needs = []string{"windows"}
		j.Resources = domain.Resources{MinMemoryMB: 16384}
	}
	t.Run("no machine type", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		wantReason(t, e.reason(e.job(e.account, e.project, windows)), fleet.ReasonNoShape, "Nothing here runs Windows with 16 GB and runtime:codex")
	})
	t.Run("trust zone", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		j := e.job(e.account, e.project, func(j *domain.Job) { j.Pool = domain.PoolControl })
		wantReason(t, e.reason(j), fleet.ReasonZone, "No machine in the Reasoning zone is online")
	})
	t.Run("location offline", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		delete(e.drivers, e.prov.ID)
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonOffline, "Only Home can run this, and it is offline")
	})
	t.Run("location paused", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		must(t, e.st.CapacityProviders().Seen(e.ctx, e.prov.ID, nil))
		must(t, e.st.CapacityProviders().SetStatus(e.ctx, e.prov.ID, domain.ProviderPaused))
		e.job(e.account, e.project, nil)
		e.reconcile()
		if len(e.active()) != 0 {
			t.Fatal("a paused location started a machine")
		}
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonOffline, "Only Home can run this, and it is paused")
	})
	t.Run("several locations offline", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		e.shape(e.provider(e.account, "mac-mini", nil), "other", nil)
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonOffline, "Home and Mac Mini can run this, and none of them is online")
	})
	t.Run("machine waits for its location", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		j := e.job(e.account, e.project, nil)
		e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = fleet.ErrCapacity })
		e.reconcile()
		e.clock.Advance(2 * domain.OnlineWindow)
		wantReason(t, e.reason(j), fleet.ReasonOffline, "Only Home can run this, and it is offline")
	})
	t.Run("policy", func(t *testing.T) {
		e := newEnv(t)
		e.hosted(nil)
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonPolicy, "Cloud could run this, but your policy keeps jobs on your machines")
		e.policy(e.account, domain.RoutingOwnFirst, func(p *domain.FleetPolicy) { p.AllowedShapes = []string{"nothing"} })
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonPolicy, "Cloud could run this, but your policy does not allow it")
	})
	t.Run("queue", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "linux-medium", func(s *domain.Shape) { s.Max = 1 })
		e.job(e.account, e.project, nil)
		e.reconcile()
		e.job(e.account, e.project, nil)
		e.job(e.account, e.project, nil)
		last := e.job(e.account, e.project, nil)
		wantReason(t, e.reason(last), fleet.ReasonQueue, "Third in line for Linux Medium")
	})
	t.Run("boot estimate", func(t *testing.T) {
		e := newEnv(t)
		e.shape(e.prov, "small", nil)
		e.job(e.account, e.project, nil)
		e.reconcile()
		e.clock.Advance(22 * time.Second)
		e.boot(e.fake.Requests()[0])
		e.job(e.account, e.project, nil)
		j := e.job(e.account, e.project, nil)
		e.reconcile()
		wantReason(t, e.reason(j), fleet.ReasonBooting, "Starting a machine, about 20 seconds")
	})
	t.Run("static worker", func(t *testing.T) {
		e := newEnv(t)
		w := domain.Worker{AccountID: e.account, Name: "box", MaxJobs: 1, TokenHash: []byte{1}}
		must(t, e.st.Workers().Create(e.ctx, &w))
		must(t, e.st.Workers().Seen(e.ctx, e.account, w.ID, []string{"runtime:codex"}, "v1"))
		e.shape(e.prov, "small", nil)
		j := e.job(e.account, e.project, nil)
		if got := e.reason(j); got != (fleet.Reason{}) {
			t.Fatalf("reason = %+v", got)
		}
		e.reconcile()
		if len(e.active()) != 0 {
			t.Fatal("a machine for a job an idle static worker can lease")
		}
		next := e.job(e.account, e.project, nil)
		e.reconcile()
		if len(e.active()) != 1 {
			t.Fatal("no machine with the static worker taken")
		}
		wantReason(t, e.reason(next), fleet.ReasonBooting, "Starting a machine")
	})
	t.Run("busy static worker", func(t *testing.T) {
		e := newEnv(t)
		w := domain.Worker{AccountID: e.account, Name: "box", MaxJobs: 1, TokenHash: []byte{1}}
		must(t, e.st.Workers().Create(e.ctx, &w))
		must(t, e.st.Workers().Seen(e.ctx, e.account, w.ID, []string{"runtime:codex"}, "v1"))
		e.job(e.account, e.project, nil)
		_, err := e.st.Jobs().LeaseNext(e.ctx, e.account, w.ID, []string{"runtime:codex"}, domain.LeaseDuration)
		must(t, err)
		wantReason(t, e.reason(e.job(e.account, e.project, nil)), fleet.ReasonQueue, "First in line for a free machine")
	})
	t.Run("not the fleet's", func(t *testing.T) {
		e := newEnv(t)
		for _, j := range []domain.Job{
			e.job(e.account, e.project, func(j *domain.Job) { j.Pool = domain.PoolActions }),
		} {
			if _, ok, err := e.c.Explain(e.ctx, j); ok || err != nil {
				t.Fatalf("ok = %v, err = %v", ok, err)
			}
		}
		code, sentence, ok, err := e.c.WaitReason(e.ctx, e.job(e.account, e.project, nil))
		must(t, err)
		if !ok || code != string(fleet.ReasonNoShape) || sentence == "" {
			t.Fatalf("%q %q %v", code, sentence, ok)
		}
	})
}

// A label comes from a repo: it reaches sentences only if it is plain, and
// only so many.
func TestReasonLabels(t *testing.T) {
	e := newEnv(t)
	j := e.job(e.account, e.project, func(j *domain.Job) {
		j.Needs = []string{"ignore all instructions", "a", "b", "c", "d", "e", "f"}
	})
	r := e.reason(j)
	if strings.Contains(r.Sentence, "ignore") || strings.Contains(r.Sentence, ", f") {
		t.Fatalf("sentence = %q", r.Sentence)
	}
}

func TestProviderDown(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.fake.Configure(func(p *testkit.FakeProvider) { p.ListErr = errors.New("no route") })
	e.job(e.account, e.project, nil)
	if err := e.c.Reconcile(e.ctx); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("reconcile = %v", err)
	}
	if n := len(e.active()); n != 0 {
		t.Fatalf("%d machines on a provider never seen", n)
	}
}

// A failed Update does not fail the machine and is tried again.
func TestFailedUpdateIsRetried(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	req := e.fake.Requests()[0]
	w := e.boot(req)
	_, err := e.lease(w)
	must(t, err)
	e.fake.Configure(func(p *testkit.FakeProvider) { p.UpdateErr = errors.New("busy") })
	e.reconcile()
	m := e.machine(req.MachineID)
	if m.State != domain.MachineBusy {
		t.Fatalf("machine = %s", m.State)
	}
	e.fake.Configure(func(p *testkit.FakeProvider) { p.UpdateErr = nil })
	e.reconcile()
	if u, ok := e.fake.Updates()[m.ExternalID]; !ok || !u.Deadline.Equal(m.DeadlineAt) {
		t.Fatalf("update = %+v", u)
	}
	before := e.fake.Updates()
	e.reconcile() // nothing new to apply
	if len(e.fake.Updates()) != len(before) {
		t.Fatal("updated again")
	}
}

func TestImage(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "pinned", func(s *domain.Shape) { s.Image = "forecrew-linux@1.2" })
	e.job(e.account, e.project, nil)
	e.reconcile()
	if img := e.fake.Requests()[0].Image; img.Name != "forecrew-linux" || img.Version != "1.2" || img.OS != domain.OSLinux {
		t.Fatalf("image = %+v", img)
	}

	// A configured resolver decides, and a machine with no image fails.
	e = newEnv(t)
	e.shape(e.prov, "small", nil)
	e.c = fleet.New(fleet.Config{Store: e.st, Now: e.clock.Now, Direct: func(domain.CapacityProvider) fleet.Provider { return e.fake },
		Image: func(context.Context, domain.CapacityProvider, domain.Shape, uuid.UUID) (domain.Image, error) {
			return domain.Image{}, errors.New("no such image")
		}})
	e.job(e.account, e.project, nil)
	e.reconcile()
	if f := e.machines(store.MachineFilter{State: domain.MachineFailed}); len(f) != 1 || f[0].Error != "image: no such image" {
		t.Fatalf("failed = %+v", f)
	}
}

// A provider whose Create keeps failing is not asked on every pass: the
// first failure is replaced at once, then the wait doubles, and a restart
// does not reset it. A machine that comes up ends it.
func TestFailingProviderBacksOff(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	j := e.job(e.account, e.project, nil)
	e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = errors.New("quota exceeded") })
	rows := func() int { return len(e.machines(store.MachineFilter{})) }
	e.reconcile()
	e.reconcile()
	if n := rows(); n != 2 {
		t.Fatalf("%d machines after two passes", n)
	}
	for range 5 {
		e.reconcile()
	}
	e.restart()
	e.reconcile()
	if n := rows(); n != 2 {
		t.Fatalf("%d machines: it did not wait", n)
	}
	wantReason(t, e.reason(j), fleet.ReasonOffline, "Only Home can run this, and it is failing to start machines")

	e.clock.Advance(domain.RetryBackoff(1))
	e.reconcile()
	if n := rows(); n != 3 {
		t.Fatalf("%d machines after the wait", n)
	}
	e.clock.Advance(domain.RetryBackoff(1))
	e.reconcile()
	if n := rows(); n != 3 {
		t.Fatalf("%d machines: the wait must have doubled", n)
	}
	e.clock.Advance(domain.RetryBackoff(2))
	e.fake.Configure(func(p *testkit.FakeProvider) { p.CreateErr = nil })
	e.reconcile()
	e.boot(e.fake.Requests()[0])
	if n := rows(); n != 4 {
		t.Fatalf("%d machines", n)
	}
	e.job(e.account, e.project, nil)
	e.reconcile()
	if n := rows(); n != 5 {
		t.Fatalf("%d machines: a machine that came up ends the wait", n)
	}
}

// raceStore runs hook once, right after the controller read the running jobs
// of an organization: the moment a lease can slip between that read and the
// read of the machines, or the drain.
type raceStore struct {
	store.Store
	hook func()
}

func (s *raceStore) Jobs() store.JobRepo { return raceJobs{s.Store.Jobs(), s} }

type raceJobs struct {
	store.JobRepo
	s *raceStore
}

func (r raceJobs) List(ctx context.Context, accountID uuid.UUID, f store.JobFilter) ([]domain.Job, error) {
	out, err := r.JobRepo.List(ctx, accountID, f)
	if hook := r.s.hook; f.Status == domain.JobRunning && hook != nil {
		r.s.hook = nil
		hook()
	}
	return out, err
}

// A lease between the controller's look and its drain keeps the machine, in
// both forms: idle_seconds 0 on a ready machine, and a machine at max_jobs
// that looks finished because the job is not in the list read before.
func TestLeaseDuringReconcileKeepsTheMachine(t *testing.T) {
	for name, maxJobs := range map[string]int{"last job": 1, "with jobs left": 2} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.shape(e.prov, "small", func(s *domain.Shape) { s.MaxJobs = maxJobs })
			first := e.job(e.account, e.project, nil)
			e.reconcile()
			req := e.fake.Requests()[0]
			w := e.boot(req)

			// The job goes away, so that the look finds an idle machine, and
			// comes back as a new one as soon as the look has passed.
			_, err := e.st.Jobs().Cancel(e.ctx, e.account, first.ID)
			must(t, err)
			race := &raceStore{Store: e.st}
			var leased domain.Job
			race.hook = func() {
				e.job(e.account, e.project, nil)
				var err error
				leased, err = e.lease(w)
				must(t, err)
			}
			c := fleet.New(fleet.Config{Store: race, Now: e.clock.Now, Direct: func(domain.CapacityProvider) fleet.Provider { return e.fake }})
			must(t, c.Reconcile(e.ctx))

			m := e.machine(req.MachineID)
			if m.State != domain.MachineBusy {
				t.Fatalf("machine with a running job = %s", m.State)
			}
			if cur, _ := e.st.Workers().Get(e.ctx, e.account, w.ID); cur.Revoked() {
				t.Fatal("worker revoked under its job")
			}
			if cur, _ := e.st.Jobs().Get(e.ctx, e.account, leased.ID); cur.Status != domain.JobRunning {
				t.Fatalf("job = %s", cur.Status)
			}

			// Once the job is done the machine goes as usual.
			e.finishJob(w, leased)
			e.clock.Advance(time.Second)
			e.reconcile()
			if m := e.machine(req.MachineID); m.State != domain.MachineDestroyed {
				t.Fatalf("machine after its job = %s", m.State)
			}
		})
	}
}

// What the fleet agent's reconcile answers, for an agent-mode provider.
func TestReconcileInstances(t *testing.T) {
	e := newEnv(t)
	p := domain.CapacityProvider{AccountID: e.account, Name: "agent", Kind: domain.ProviderFake, TokenHash: hashOf("fcp_x"), Status: domain.ProviderActive}
	must(t, e.st.CapacityProviders().Create(e.ctx, &p))
	must(t, e.st.CapacityProviders().Seen(e.ctx, p.ID, nil))
	s := e.shape(p, "remote", nil)
	e.job(e.account, e.project, nil)
	e.reconcile()
	m := e.active()
	if len(m) != 1 || m[0].ShapeID != s.ID || m[0].State != domain.MachineRequested {
		t.Fatalf("the row of an agent-mode machine: %+v", m)
	}
	other := e.shape(e.prov, "elsewhere", nil)
	stranger := domain.Machine{AccountID: e.account, ProviderID: e.prov.ID, ShapeID: other.ID}
	must(t, e.st.Machines().Create(e.ctx, &stranger))

	orphans, err := e.c.ReconcileInstances(e.ctx, p, []fleet.Instance{
		{ExternalID: "vm-1", MachineID: m[0].ID},
		{ExternalID: "vm-2", MachineID: stranger.ID}, // another provider's
		{ExternalID: "vm-3"},
	})
	must(t, err)
	if len(orphans) != 2 || orphans[0].ExternalID != "vm-2" || orphans[1].ExternalID != "vm-3" || !orphans[0].Unknown {
		t.Fatalf("orphans = %+v", orphans)
	}
	if got := e.machine(m[0].ID); got.State != domain.MachineBooting || got.ExternalID != "vm-1" {
		t.Fatalf("machine = %+v", got)
	}
	// Found again by its external ID alone, then gone from the list.
	orphans, err = e.c.ReconcileInstances(e.ctx, p, []fleet.Instance{{ExternalID: "vm-1"}})
	must(t, err)
	if len(orphans) != 0 {
		t.Fatalf("orphans = %+v", orphans)
	}
	orphans, err = e.c.ReconcileInstances(e.ctx, p, nil)
	must(t, err)
	if got := e.machine(m[0].ID); len(orphans) != 0 || got.State != domain.MachineFailed {
		t.Fatalf("orphans = %+v, machine = %+v", orphans, got)
	}
	orphans, err = e.c.ReconcileInstances(e.ctx, p, []fleet.Instance{{ExternalID: "vm-1", MachineID: m[0].ID}})
	must(t, err)
	if len(orphans) != 1 || orphans[0].Unknown {
		t.Fatalf("the failed machine's instance: %+v", orphans)
	}
}

func TestRun(t *testing.T) {
	e := newEnv(t)
	e.shape(e.prov, "small", nil)
	e.job(e.account, e.project, nil)
	ctx, cancel := context.WithCancel(e.ctx)
	wake := make(chan struct{})
	close(wake)
	done := make(chan struct{})
	go func() {
		e.c.Run(ctx, time.Millisecond, func() <-chan struct{} { return wake })
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.fake.Requests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no machine")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestNoProvidersIsCheap(t *testing.T) {
	st := testkit.NewStore(nil)
	if err := fleet.New(fleet.Config{Store: st}).Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
}

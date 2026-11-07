package fleetagent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetapi"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// safeBuffer is a log both sides write to.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// world is forecrewd's fleet agent API on a fake store, with an agent and a
// fake provider talking to it over HTTP.
type world struct {
	t        *testing.T
	st       *testkit.Store
	url      string
	down     atomic.Bool // forecrewd cannot be reached
	polls    atomic.Int64
	fake     *testkit.FakeProvider
	provider domain.CapacityProvider
	shape    domain.Shape
	token    string
	logs     *safeBuffer
	log      *slog.Logger
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := t.Context()
	w := &world{t: t, st: testkit.NewStore(func() time.Time { return time.Now().UTC() }), fake: testkit.NewFakeProvider(), logs: &safeBuffer{}}
	w.log = slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	account := domain.Account{Name: "acme"}
	must(t, w.st.Accounts().Create(ctx, &account))
	token, hash := fleet.NewProviderToken()
	w.token = token
	w.provider = domain.CapacityProvider{AccountID: account.ID, Name: "home", Kind: domain.ProviderFake, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash}
	must(t, w.st.CapacityProviders().Create(ctx, &w.provider))
	w.shape = domain.Shape{AccountID: account.ID, ProviderID: w.provider.ID, Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20, Image: "linux", Max: 9, Enabled: true}
	must(t, w.st.Shapes().Create(ctx, &w.shape))
	c := fleet.New(fleet.Config{Store: w.st, Logger: w.log, ServerURL: "https://forecrew.example.com"})
	api := fleetapi.New(fleetapi.Config{Store: w.st, Fleet: c, Logger: w.log, PollWait: 30 * time.Millisecond, PollRecheck: 3 * time.Millisecond}).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.down.Load() {
			http.Error(rw, "unavailable", http.StatusBadGateway)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/poll") {
			w.polls.Add(1)
		}
		api.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

func (w *world) config(p fleet.Provider, edit ...func(*fleetagent.Config)) fleetagent.Config {
	cfg := fleetagent.Config{
		Client: fleetagent.NewClient(w.url, w.token, nil), Provider: p, Version: "test", Logger: w.log,
		BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond, NoRoomWait: 20 * time.Millisecond,
		Detect: func(context.Context) domain.ProviderCapabilities {
			return domain.ProviderCapabilities{Host: "nas", MaxCPU: 8}
		},
	}
	for _, e := range edit {
		e(&cfg)
	}
	return cfg
}

// start runs an agent and returns what stops it and waits for it.
func (w *world) start(cfg fleetagent.Config) (stop func(), done <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 2) // one for the test, one for stop
	go func() {
		err := fleetagent.New(cfg).Run(ctx)
		ch <- err
		ch <- err
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-ch; err != nil && !errors.Is(err, fleetagent.ErrRejected) {
				w.t.Errorf("run: %v", err)
			}
		})
	}
	w.t.Cleanup(stop)
	return stop, ch
}

func (w *world) request() domain.Machine {
	w.t.Helper()
	m := domain.Machine{AccountID: w.provider.AccountID, ProviderID: w.provider.ID, ShapeID: w.shape.ID}
	must(w.t, w.st.Machines().Create(w.t.Context(), &m))
	return m
}

func (w *world) machine(id uuid.UUID) domain.Machine {
	w.t.Helper()
	m, err := w.st.Machines().Get(context.Background(), id)
	must(w.t, err)
	return m
}

func (w *world) waitState(id uuid.UUID, s domain.MachineState) {
	w.t.Helper()
	eventually(w.t, "machine "+string(s), func() bool { return w.machine(id).State == s })
}

func (w *world) drain(id uuid.UUID) {
	w.t.Helper()
	must(w.t, w.st.Machines().UpdateState(w.t.Context(), id, w.machine(id).State, domain.MachineDraining, ""))
}

func TestCreateAndDestroy(t *testing.T) {
	w := newWorld(t)
	stop, _ := w.start(w.config(w.fake))
	eventually(t, "the provider's capabilities", func() bool {
		p, err := w.st.CapacityProviders().Get(t.Context(), w.provider.ID)
		must(t, err)
		return p.Capabilities.Host == "nas" && !p.LastSeenAt.IsZero()
	})
	m := w.request()
	w.waitState(m.ID, domain.MachineBooting)
	reqs := w.fake.Requests()
	if len(reqs) != 1 || reqs[0].MachineID != m.ID || reqs[0].Bootstrap == nil || reqs[0].Shape.ID != w.shape.ID {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := w.machine(m.ID); got.ExternalID != "fake-1" {
		t.Fatalf("machine = %+v", got)
	}
	w.drain(m.ID)
	w.waitState(m.ID, domain.MachineDestroyed)
	if len(w.fake.Instances()) != 0 {
		t.Fatalf("left: %+v", w.fake.Instances())
	}
	stop()
	assertNoSecrets(t, w)
}

func assertNoSecrets(t *testing.T, w *world) {
	t.Helper()
	logs := w.logs.String()
	if strings.Contains(logs, w.token) {
		t.Error("the provider token is in the logs")
	}
	for _, r := range w.fake.Requests() {
		if strings.Contains(logs, r.Bootstrap.Token) {
			t.Error("a bootstrap token is in the logs")
		}
	}
	if domain.SecretPatterns.MatchString(logs) {
		t.Errorf("a secret is in the logs:\n%s", logs)
	}
}

// gated holds Create until it is let go, as a slow provider does.
type gated struct {
	*testkit.FakeProvider
	release chan struct{}
	entered atomic.Int64
}

func (g *gated) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	g.entered.Add(1)
	<-g.release
	return g.FakeProvider.Create(ctx, req)
}

// Polls go on during a slow Create and leave the token the machine got alone.
func TestSlowCreateKeepsItsToken(t *testing.T) {
	w := newWorld(t)
	g := &gated{FakeProvider: w.fake, release: make(chan struct{})}
	w.start(w.config(g))
	m := w.request()
	eventually(t, "Create to start", func() bool { return g.entered.Load() == 1 })
	before := w.polls.Load()
	eventually(t, "polls during Create", func() bool { return w.polls.Load() > before+3 })
	close(g.release)
	w.waitState(m.ID, domain.MachineBooting)
	reqs := w.fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d creates", len(reqs))
	}
	tok, err := w.st.BootstrapTokens().GetByTokenHash(t.Context(), mcp.HashToken(reqs[0].Bootstrap.Token))
	if err != nil || tok.MachineID != m.ID {
		t.Fatalf("the token booted with no longer stands: %+v, %v", tok, err)
	}
}

// A periodic reconcile during Create must not make forecrewd fail the machine.
func TestReconcileWaitsForCreate(t *testing.T) {
	w := newWorld(t)
	g := &gated{FakeProvider: w.fake, release: make(chan struct{})}
	w.start(w.config(g, func(c *fleetagent.Config) { c.ReconcileEvery = time.Millisecond }))
	m := w.request()
	eventually(t, "Create to start", func() bool { return g.entered.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	close(g.release)
	w.waitState(m.ID, domain.MachineBooting)
	time.Sleep(30 * time.Millisecond)
	if got := w.machine(m.ID); got.State != domain.MachineBooting {
		t.Fatalf("machine = %+v", got)
	}
}

func TestNoRoom(t *testing.T) {
	w := newWorld(t)
	w.fake.Configure(func(f *testkit.FakeProvider) { f.CreateErr = fleet.ErrCapacity })
	w.start(w.config(w.fake))
	m := w.request()
	time.Sleep(100 * time.Millisecond)
	if got := w.machine(m.ID); got.State != domain.MachineRequested {
		t.Fatalf("machine = %+v", got)
	}
	if n := w.polls.Load(); n > 12 {
		t.Fatalf("%d polls in 100 ms: it spins", n)
	}
	w.fake.Configure(func(f *testkit.FakeProvider) { f.CreateErr = nil })
	w.waitState(m.ID, domain.MachineBooting)
}

func TestCreateFailureIsReportedWithoutSecrets(t *testing.T) {
	w := newWorld(t)
	secret := "fcb_" + strings.Repeat("s", 43)
	w.fake.Configure(func(f *testkit.FakeProvider) { f.CreateErr = errors.New("boot with " + secret + " failed") })
	w.start(w.config(w.fake))
	m := w.request()
	w.waitState(m.ID, domain.MachineFailed)
	got := w.machine(m.ID)
	if !strings.HasPrefix(got.Error, "create: ") || strings.Contains(got.Error, secret) {
		t.Fatalf("error = %q", got.Error)
	}
	if strings.Contains(w.logs.String(), secret) {
		t.Fatal("the secret is in the logs")
	}
}

// After a restart the agent knows its machines from the provider: nothing is
// created twice, no new token goes out, and a machine started just before the
// crash is reported by the reconcile.
func TestRestartDoesNotDuplicate(t *testing.T) {
	w := newWorld(t)
	stop, _ := w.start(w.config(w.fake))
	m := w.request()
	w.waitState(m.ID, domain.MachineBooting)
	stop()

	// Started, and the crash came before the report.
	lost := w.request()
	inst, err := w.fake.Create(t.Context(), fleet.CreateRequest{MachineID: lost.ID, Bootstrap: &fleet.Bootstrap{Token: "fcb_x"}})
	must(t, err)

	stop2, _ := w.start(w.config(w.fake))
	w.waitState(lost.ID, domain.MachineBooting)
	if got := w.machine(lost.ID); got.ExternalID != inst.ExternalID {
		t.Fatalf("lost = %+v", got)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(w.fake.Instances()); n != 2 {
		t.Fatalf("%d instances", n)
	}
	if n := len(w.fake.Requests()); n != 2 {
		t.Fatalf("%d creates", n)
	}
	// And it can still destroy what it picked up.
	w.drain(m.ID)
	w.waitState(m.ID, domain.MachineDestroyed)
	if n := len(w.fake.Instances()); n != 1 {
		t.Fatalf("%d instances", n)
	}
	stop2()
}

func TestOutage(t *testing.T) {
	w := newWorld(t)
	w.start(w.config(w.fake))
	running := w.request()
	w.waitState(running.ID, domain.MachineBooting)
	gone := w.request()
	w.waitState(gone.ID, domain.MachineBooting)

	w.down.Store(true)
	eventually(t, "the agent to notice", func() bool { return strings.Contains(w.logs.String(), "no contact with forecrewd") })
	// Forecrewd decides things the agent cannot hear about yet.
	fresh := w.request()
	w.drain(gone.ID)
	time.Sleep(30 * time.Millisecond)
	if n := len(w.fake.Instances()); n != 2 {
		t.Fatalf("%d instances during the outage: it must neither create nor destroy", n)
	}
	if n := len(w.fake.Requests()); n != 2 {
		t.Fatalf("%d creates during the outage", n)
	}

	w.down.Store(false)
	w.waitState(fresh.ID, domain.MachineBooting)
	w.waitState(gone.ID, domain.MachineDestroyed)
	if got := w.machine(running.ID); got.State != domain.MachineBooting {
		t.Fatalf("running = %+v", got)
	}
	if n := len(w.fake.Requests()); n != 3 {
		t.Fatalf("%d creates: a duplicate", n)
	}
	if n := len(w.fake.Instances()); n != 2 {
		t.Fatalf("%d instances", n)
	}
	assertNoSecrets(t, w)
}

// A deadline the agent knows applies while forecrewd is away, and forecrewd
// hears of it afterwards.
func TestDeadlineDuringOutage(t *testing.T) {
	w := newWorld(t)
	w.start(w.config(w.fake))
	m := w.request()
	w.waitState(m.ID, domain.MachineBooting)
	ctx := t.Context()
	// A lease sets the deadline; the store has only TakeJob for that.
	project := domain.Project{AccountID: w.provider.AccountID, Name: "app", Repo: "acme/app"}
	must(t, w.st.Projects().Create(ctx, &project))
	j := domain.Job{AccountID: w.provider.AccountID, ProjectID: project.ID, Role: "dev", Kind: domain.JobKindAgent}
	must(t, w.st.Jobs().Create(ctx, &j))
	deadline := time.Now().Add(80 * time.Millisecond)
	_, err := w.st.Machines().TakeJob(ctx, m.ID, j.ID, deadline)
	must(t, err)
	eventually(t, "the deadline to reach the provider", func() bool {
		for _, u := range w.fake.Updates() {
			if u.Deadline.Equal(deadline) {
				return true
			}
		}
		return false
	})
	w.down.Store(true)
	eventually(t, "the machine to be destroyed at its deadline", func() bool { return len(w.fake.Instances()) == 0 })
	if w.machine(m.ID).State == domain.MachineDestroyed {
		t.Fatal("forecrewd heard during the outage")
	}
	w.down.Store(false)
	w.waitState(m.ID, domain.MachineDestroyed)
}

func TestRevokedTokenStopsDemand(t *testing.T) {
	w := newWorld(t)
	_, done := w.start(w.config(w.fake))
	m := w.request()
	w.waitState(m.ID, domain.MachineBooting)
	must(t, w.st.CapacityProviders().SetStatus(t.Context(), w.provider.ID, domain.ProviderRevoked))
	select {
	case err := <-done:
		if !errors.Is(err, fleetagent.ErrRejected) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent goes on")
	}
	w.request()
	time.Sleep(20 * time.Millisecond)
	if n := len(w.fake.Requests()); n != 1 {
		t.Fatalf("%d creates after the revocation", n)
	}
	if n := len(w.fake.Instances()); n != 1 {
		t.Fatalf("%d instances: stopping must not touch machines", n)
	}
}

func TestOrphanIsDestroyedAndUpdatesAreRetried(t *testing.T) {
	w := newWorld(t)
	stray, err := w.fake.Create(t.Context(), fleet.CreateRequest{MachineID: uuid.New(), Bootstrap: &fleet.Bootstrap{Token: "fcb_x"}})
	must(t, err)
	// A machine from before this agent gets its values once it can: Update
	// fails first, and that does not fail the machine.
	m := w.request()
	external, err := w.fake.Create(t.Context(), fleet.CreateRequest{MachineID: m.ID, Bootstrap: &fleet.Bootstrap{Token: "fcb_y"}})
	must(t, err)
	w.fake.Configure(func(f *testkit.FakeProvider) { f.UpdateErr = errors.New("busy") })
	w.start(w.config(w.fake, func(c *fleetagent.Config) { c.ReconcileEvery = 5 * time.Millisecond }))
	eventually(t, "the orphan to go", func() bool { return slices.Contains(w.fake.Destroyed(), stray.ExternalID) })
	w.waitState(m.ID, domain.MachineBooting)
	time.Sleep(30 * time.Millisecond)
	if w.machine(m.ID).State != domain.MachineBooting {
		t.Fatal("a failed Update failed the machine")
	}
	w.fake.Configure(func(f *testkit.FakeProvider) { f.UpdateErr = nil })
	eventually(t, "the update", func() bool {
		_, ok := w.fake.Updates()[external.ExternalID]
		return ok
	})
}

// Without a list from its provider the agent changes nothing.
func TestProviderListFailure(t *testing.T) {
	w := newWorld(t)
	w.fake.Configure(func(f *testkit.FakeProvider) { f.ListErr = errors.New("provider away") })
	w.start(w.config(w.fake))
	m := w.request()
	time.Sleep(30 * time.Millisecond)
	if w.polls.Load() != 0 || w.machine(m.ID).State != domain.MachineRequested {
		t.Fatalf("%d polls, machine %+v", w.polls.Load(), w.machine(m.ID))
	}
	w.fake.Configure(func(f *testkit.FakeProvider) { f.ListErr = nil })
	w.waitState(m.ID, domain.MachineBooting)
}

func TestCapabilityChangeIsRegistered(t *testing.T) {
	w := newWorld(t)
	var host atomic.Value
	host.Store("nas")
	w.start(w.config(w.fake, func(c *fleetagent.Config) {
		c.ReconcileEvery = time.Millisecond
		c.Detect = func(context.Context) domain.ProviderCapabilities {
			return domain.ProviderCapabilities{Host: host.Load().(string)}
		}
	}))
	stored := func() string {
		p, err := w.st.CapacityProviders().Get(context.Background(), w.provider.ID)
		must(t, err)
		return p.Capabilities.Host
	}
	eventually(t, "the first registration", func() bool { return stored() == "nas" })
	host.Store("nas-2")
	eventually(t, "the change", func() bool { return stored() == "nas-2" })
}

// A compromised or confused forecrewd cannot point the agent's credentials at
// something the provider did not list.
func TestReconcileDestroysOnlyWhatWasListed(t *testing.T) {
	listed := &listHiding{FakeProvider: testkit.NewFakeProvider()}
	hidden, err := listed.Create(t.Context(), fleet.CreateRequest{MachineID: uuid.New(), Bootstrap: &fleet.Bootstrap{Token: "fcb_x"}})
	must(t, err)
	listed.hide = hidden.ExternalID
	stray, err := listed.Create(t.Context(), fleet.CreateRequest{MachineID: uuid.New(), Bootstrap: &fleet.Bootstrap{Token: "fcb_y"}})
	must(t, err)
	var answered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reconcile"):
			fmt.Fprintf(rw, `{"destroy":[%q,%q]}`, hidden.ExternalID, stray.ExternalID)
			answered.Store(true)
		case strings.HasSuffix(r.URL.Path, "/poll"):
			time.Sleep(5 * time.Millisecond)
			fmt.Fprint(rw, `{"generation":"1","machines":[]}`)
		default:
			fmt.Fprint(rw, `{}`)
		}
	}))
	defer srv.Close()
	logs := &safeBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- fleetagent.New(fleetagent.Config{Client: fleetagent.NewClient(srv.URL, "fcp_t", nil), Provider: listed, Logger: slog.New(slog.NewTextHandler(logs, nil)),
			Detect: func(context.Context) domain.ProviderCapabilities { return domain.ProviderCapabilities{} }}).Run(ctx)
	}()
	eventually(t, "the listed orphan to go", func() bool { return slices.Contains(listed.Destroyed(), stray.ExternalID) })
	cancel()
	must(t, <-done)
	if slices.Contains(listed.Destroyed(), hidden.ExternalID) {
		t.Fatal("destroyed something the provider did not list")
	}
	if !answered.Load() || !strings.Contains(logs.String(), "did not list") {
		t.Fatalf("not logged:\n%s", logs.String())
	}
}

// listHiding leaves one instance out of List, as what is not Forecrew's.
type listHiding struct {
	*testkit.FakeProvider
	hide string
}

func (l *listHiding) List(ctx context.Context) ([]fleet.Instance, error) {
	all, err := l.FakeProvider.List(ctx)
	return slices.DeleteFunc(all, func(i fleet.Instance) bool { return i.ExternalID == l.hide }), err
}

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/worker"
)

// agentSite is a place with a fleet agent: a fake local provider that only the
// agent touches, and a link to forecrewd that can be cut.
type agentSite struct {
	w     *world
	local *testkit.FakeProvider
	url   string
	cut   atomic.Bool
	token string
	logs  *logBuffer
}

func (w *world) newSite() *agentSite {
	w.t.Helper()
	target, err := url.Parse(w.url)
	must(w.t, err)
	s := &agentSite{w: w, local: testkit.NewFakeProvider(), logs: &logBuffer{}}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if s.cut.Load() {
			http.Error(rw, "no route", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// addProvider adds the provider through the API and a shape of it.
func (s *agentSite) addProvider(name string, edit func(*domain.Shape)) domain.CapacityProvider {
	s.w.t.Helper()
	var out mcp.CreateProviderOutput
	if code := s.w.api(http.MethodPost, "/providers", mcp.CreateProviderInput{Name: name, Kind: domain.ProviderFake, Org: s.w.account.Slug}, &out); code != http.StatusCreated {
		s.w.t.Fatalf("add provider: status %d", code)
	}
	s.token = out.Token
	s.addShape(out.Provider.CapacityProvider, edit)
	return out.Provider.CapacityProvider
}

func (s *agentSite) addShape(p domain.CapacityProvider, edit func(*domain.Shape)) {
	s.w.t.Helper()
	shape := domain.Shape{
		AccountID: s.w.account.ID, ProviderID: p.ID, Name: p.Name + "-small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Pool: domain.PoolControl, Labels: []string{"runtime:claude-code"}, Image: "forecrew-linux", Max: 2, Enabled: true,
	}
	if edit != nil {
		edit(&shape)
	}
	must(s.w.t, s.w.st.Shapes().Create(s.w.t.Context(), &shape))
}

func (s *agentSite) facts() domain.ProviderCapabilities {
	return domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSLinux}, Arch: []domain.MachineArch{domain.ArchAMD64}, MaxCPU: 8, MaxMemoryMB: 16384, MaxDiskGB: 100, Host: "nas",
		ImageFormats: []domain.ImageFormat{domain.ImageRootfs},
	}
}

// startAgent runs an agent; stop ends it as a crash does, and done gives its
// result.
func (s *agentSite) startAgent() (stop func(), done <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 2)
	go func() {
		err := fleetagent.New(fleetagent.Config{
			Client: fleetagent.NewClient(s.url, s.token, nil), Provider: s.local, Version: "e2e",
			Logger:     slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
			Detect: func(context.Context) domain.ProviderCapabilities { return s.facts() },
		}).Run(ctx)
		ch <- err
		ch <- err
	}()
	stop = func() {
		cancel()
		<-ch
	}
	s.w.t.Cleanup(cancel)
	return stop, ch
}

// assertNoSecrets looks for every token of the site in the logs of both sides.
func (s *agentSite) assertNoSecrets() {
	s.w.t.Helper()
	for _, logs := range []string{s.logs.String(), s.w.logs.String()} {
		if strings.Contains(logs, s.token) {
			s.w.t.Error("the provider token is in the logs")
		}
		for _, r := range s.local.Requests() {
			if strings.Contains(logs, r.Bootstrap.Token) {
				s.w.t.Error("a bootstrap token is in the logs")
			}
		}
	}
}

func (s *agentSite) machines(state domain.MachineState) []domain.Machine {
	s.w.t.Helper()
	m, err := s.w.st.Machines().List(context.Background(), store.MachineFilter{AccountID: s.w.account.ID, State: state})
	must(s.w.t, err)
	return m
}

// bootWorkers makes the machines' guests real workers.
func (s *agentSite) bootWorkers() {
	w := s.w
	s.local.Configure(func(f *testkit.FakeProvider) {
		f.OnBoot = func(req fleet.CreateRequest) {
			client := worker.NewClient(w.url, req.Bootstrap.Token, nil)
			client.BootstrapWait = 10 * time.Millisecond
			g, err := client.Bootstrap(w.t.Context(), "e2e")
			if err != nil {
				w.t.Errorf("bootstrap: %v", err)
				return
			}
			w.runWorker(&workerProc{Name: g.Name}, g.WorkerToken, g.Labels)
		}
	})
}

// A job with no worker anywhere runs on a machine an agent made, through the
// real API, and the machine is destroyed afterwards.
func TestFleetAgentRunsAJob(t *testing.T) {
	w := newWorld(t, true)
	s := w.newSite()
	p := s.addProvider("home", nil)
	s.bootWorkers()
	s.startAgent()
	eventually(t, "the agent to register", func() bool {
		got, err := w.st.CapacityProviders().Get(t.Context(), p.ID)
		must(t, err)
		return got.Online(time.Now()) && got.Capabilities.Host == "nas"
	})

	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner"}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	eventually(t, "the job to succeed on a machine", func() bool {
		var info mcp.JobInfo
		w.api(http.MethodGet, "/jobs/"+job.Ref(), nil, &info)
		return info.Status == domain.JobSucceeded
	})
	eventually(t, "the machine to be destroyed", func() bool {
		return len(s.machines(domain.MachineDestroyed)) == 1 && len(s.local.Instances()) == 0
	})
	reqs := s.local.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d machines for one job", len(reqs))
	}
	if len(w.machines.Requests()) != 0 {
		t.Fatal("forecrewd started a machine itself")
	}
	s.assertNoSecrets()
}

// The link goes down and the agent restarts during the outage. The machine
// keeps running, nothing new starts, and afterwards there is still one machine.
func TestFleetAgentOutageAndRestart(t *testing.T) {
	w := newWorld(t, true)
	s := w.newSite()
	s.addProvider("home", func(sh *domain.Shape) { sh.WarmMin = 1 })
	stop, _ := s.startAgent()
	eventually(t, "the warm machine", func() bool { return len(s.machines(domain.MachineBooting)) == 1 })
	before := s.machines(domain.MachineBooting)[0]

	s.cut.Store(true)
	eventually(t, "the agent to lose contact", func() bool { return strings.Contains(s.logs.String(), "no contact with forecrewd") })
	stop()
	if n := len(s.local.Instances()); n != 1 {
		t.Fatalf("%d instances during the outage", n)
	}
	s.startAgent()
	time.Sleep(100 * time.Millisecond)
	s.cut.Store(false)
	eventually(t, "the agent to come back", func() bool {
		p, err := w.st.CapacityProviders().GetByName(t.Context(), w.account.ID, "home")
		must(t, err)
		return p.Online(time.Now())
	})
	time.Sleep(300 * time.Millisecond) // a poll or two, and a controller pass
	if n := len(s.local.Requests()); n != 1 {
		t.Fatalf("%d creates: a duplicate", n)
	}
	if n := len(s.local.Instances()); n != 1 {
		t.Fatalf("%d instances", n)
	}
	active, err := w.st.Machines().List(t.Context(), store.MachineFilter{AccountID: w.account.ID, Active: true})
	must(t, err)
	if len(active) != 1 || active[0].ID != before.ID || active[0].State != domain.MachineBooting {
		t.Fatalf("machines = %+v", active)
	}
	s.assertNoSecrets()
}

// A revoked token stops the agent, and demand goes nowhere near it.
func TestFleetAgentRevoked(t *testing.T) {
	w := newWorld(t, true)
	s := w.newSite()
	p := s.addProvider("home", func(sh *domain.Shape) { sh.WarmMin = 1 })
	_, done := s.startAgent()
	eventually(t, "the warm machine", func() bool { return len(s.machines(domain.MachineBooting)) == 1 })

	var info mcp.ProviderInfo
	if code := w.api(http.MethodPost, "/providers/home/revoke", nil, &info); code != http.StatusOK || info.Status != domain.ProviderRevoked {
		t.Fatalf("revoke: status %d, %+v", code, info)
	}
	select {
	case err := <-done:
		if !errors.Is(err, fleetagent.ErrRejected) {
			t.Fatalf("agent: %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("the agent goes on")
	}
	eventually(t, "the machine's row to end", func() bool { return len(s.machines(domain.MachineFailed)) == 1 })

	var job domain.Job
	if code := w.api(http.MethodPost, "/jobs", mcp.DispatchJobInput{Project: "app", Role: "product-owner"}, &job); code != http.StatusCreated {
		t.Fatalf("dispatch: status %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(s.local.Requests()); n != 1 {
		t.Fatalf("%d creates after the revocation", n)
	}
	m, err := w.st.Machines().List(t.Context(), store.MachineFilter{ProviderID: p.ID, Active: true})
	must(t, err)
	if len(m) != 0 {
		t.Fatalf("machines = %+v", m)
	}
	s.assertNoSecrets()
}

// A join code: the agent reports with no access to anything, a person
// confirms through the API, and the token reaches the agent alone.
func TestFleetAgentJoin(t *testing.T) {
	w := newWorld(t, true)
	s := w.newSite()
	var out mcp.CreateJoinCodeOutput
	if code := w.api(http.MethodPost, "/join-codes", mcp.CreateJoinCodeInput{Org: w.account.Slug}, &out); code != http.StatusCreated {
		t.Fatalf("join code: status %d", code)
	}
	tokens := make(chan string, 1)
	go func() {
		token, err := fleetagent.Join(w.t.Context(), fleetagent.NewClient(s.url, "", nil), out.Code, domain.ProviderFake, s.facts(), slog.New(slog.NewTextHandler(s.logs, nil)), 10*time.Millisecond)
		if err != nil {
			t.Errorf("join: %v", err)
		}
		tokens <- token
	}()
	var joins []mcp.JoinCodeInfo
	eventually(t, "the agent's report", func() bool {
		w.api(http.MethodGet, "/join-codes", nil, &joins)
		return len(joins) == 1 && joins[0].Status == mcp.JoinClaimed
	})
	if joins[0].Facts.Host != "nas" || joins[0].Kind != domain.ProviderFake {
		t.Fatalf("join = %+v", joins[0])
	}
	var p mcp.ProviderInfo
	if code := w.api(http.MethodPost, "/join-codes/"+joins[0].ID.String()+"/confirm", mcp.ConfirmJoinInput{Name: "joined"}, &p); code != http.StatusOK {
		t.Fatalf("confirm: status %d", code)
	}
	s.token = <-tokens
	if !strings.HasPrefix(s.token, "fcp_") {
		t.Fatalf("token of %d characters", len(s.token))
	}
	s.addShape(p.CapacityProvider, func(sh *domain.Shape) { sh.WarmMin = 1 })
	s.startAgent()
	eventually(t, "a machine on the joined location", func() bool { return len(s.local.Instances()) == 1 })
	var providers []mcp.ProviderInfo
	w.api(http.MethodGet, "/providers", nil, &providers)
	if i := slices.IndexFunc(providers, func(x mcp.ProviderInfo) bool { return x.Name == "joined" }); i < 0 || providers[i].Status != domain.ProviderActive {
		t.Fatalf("providers = %+v", providers)
	}
	for _, logs := range []string{s.logs.String(), w.logs.String()} {
		if strings.Contains(logs, out.Code) {
			t.Error("the join code is in the logs")
		}
	}
	s.assertNoSecrets()
}

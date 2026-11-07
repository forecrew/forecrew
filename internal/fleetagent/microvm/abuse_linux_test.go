package microvm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// abuseEnv is a provider with abuse checks whose clock, CPU times and mining
// counter the test sets.
type abuseEnv struct {
	*testEnv
	mu      sync.Mutex
	now     time.Time
	used    map[int]time.Duration
	counter string
}

func newAbuseEnv(t *testing.T, edit func(*AbuseConfig)) *abuseEnv {
	t.Helper()
	a := &abuseEnv{now: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), used: map[int]time.Duration{}, counter: `{"nftables":[{"metainfo":{}},{"counter":{"name":"mining","packets":0}}]}`}
	a.testEnv = newEnv(t, func(c *Config) {
		c.Abuse = AbuseConfig{CPUMinutes: 10, MiningMinutes: 2}
		if edit != nil {
			edit(&c.Abuse)
		}
	})
	a.p.now = func() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.now }
	a.p.cpuTime = func(pid int) (time.Duration, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.used[pid], nil
	}
	a.p.output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "nft" || !strings.Contains(strings.Join(args, " "), "list counter inet forecrew_vm") {
			t.Errorf("ran %s %v", name, args)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return []byte(a.counter), nil
	}
	return a
}

// tick moves the clock and gives the machine of pid cpus × d of CPU time.
func (a *abuseEnv) tick(d time.Duration, pid int, cpus float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = a.now.Add(d)
	a.used[pid] += time.Duration(float64(d) * cpus)
}

func TestAbuseMining(t *testing.T) {
	a := newAbuseEnv(t, nil)
	req := a.request() // 2 vCPUs
	if _, err := a.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	r, _ := a.p.load(req.MachineID.String())
	look := func() []string {
		t.Helper()
		found, err := a.p.CheckAbuse(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, f := range found {
			kinds = append(kinds, f.Kind)
		}
		return kinds
	}
	if got := look(); got != nil {
		t.Fatalf("first look: %v", got)
	}
	for range 3 {
		a.tick(time.Minute, r.PID, 2)
		if got := look(); got != nil {
			t.Fatalf("busy with no mining ports: %v", got)
		}
	}
	a.mu.Lock()
	a.counter = `{"nftables":[{"counter":{"name":"mining","packets":4}}]}`
	a.mu.Unlock()
	a.tick(time.Minute, r.PID, 2)
	found, err := a.p.CheckAbuse(t.Context())
	if err != nil || len(found) != 1 || found[0].Kind != domain.AbuseMining || found[0].MachineID != req.MachineID || !strings.Contains(found[0].Detail, "4 connections") {
		t.Fatalf("found %+v, %v", found, err)
	}
	r, _ = a.p.load(req.MachineID.String())
	if running(r) || r.Abuse != domain.AbuseMining {
		t.Fatalf("the machine runs on: %+v", r)
	}
	if got := look(); got != nil {
		t.Fatalf("a stopped machine again: %v", got)
	}
}

// Full CPUs stop a machine only after the limit, and a pause starts the count
// again.
func TestAbuseCPU(t *testing.T) {
	a := newAbuseEnv(t, func(c *AbuseConfig) { c.MiningMinutes = 0 })
	req := a.request()
	if _, err := a.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	r, _ := a.p.load(req.MachineID.String())
	_, _ = a.p.CheckAbuse(t.Context())
	for range 9 {
		a.tick(time.Minute, r.PID, 2)
		if found, _ := a.p.CheckAbuse(t.Context()); len(found) != 0 {
			t.Fatalf("stopped before the limit: %+v", found)
		}
	}
	a.tick(time.Minute, r.PID, 0.5) // a pause
	_, _ = a.p.CheckAbuse(t.Context())
	for range 9 {
		a.tick(time.Minute, r.PID, 2)
		if found, _ := a.p.CheckAbuse(t.Context()); len(found) != 0 {
			t.Fatalf("the pause did not count: %+v", found)
		}
	}
	a.tick(time.Minute, r.PID, 2)
	found, _ := a.p.CheckAbuse(t.Context())
	if len(found) != 1 || found[0].Kind != domain.AbuseCPU {
		t.Fatalf("found %+v", found)
	}
}

func TestAbuseOff(t *testing.T) {
	a := newAbuseEnv(t, func(c *AbuseConfig) { *c = AbuseConfig{} })
	if found, err := a.p.CheckAbuse(t.Context()); found != nil || err != nil {
		t.Fatalf("found %+v, %v", found, err)
	}
}

func TestAbuseCounterErrors(t *testing.T) {
	a := newAbuseEnv(t, nil)
	r := &record{Slot: 1}
	for _, out := range []string{"not json", `{"nftables":[]}`} {
		a.counter = out
		if _, err := a.p.miningConnections(t.Context(), r); err == nil {
			t.Errorf("%q: no error", out)
		}
	}
	a.p.output = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no nft") }
	if _, err := a.p.miningConnections(t.Context(), r); err == nil {
		t.Error("nft failed and no error")
	}
	a.p.cpuTime = func(int) (time.Duration, error) { return 0, errors.New("gone") }
	req := a.request()
	if _, err := a.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := a.p.CheckAbuse(t.Context()); err == nil {
		t.Error("an unreadable machine was not reported")
	}
}

func TestParseCPUTime(t *testing.T) {
	got, err := parseCPUTime("4242 (fire cracker) S 1 4242 4242 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 3 0")
	if err != nil || got != 3*time.Second {
		t.Fatalf("cpu time = %s, %v", got, err)
	}
	for _, bad := range []string{"no name", "1 (x) S 1 2", "1 (x) S 1 2 3 4 5 6 7 8 9 10 x 1 2"} {
		if _, err := parseCPUTime(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if d, err := cpuTime(1); err != nil || d < 0 {
		t.Errorf("own process: %s, %v", d, err)
	}
	if _, err := outputOf(t.Context(), "true", "x"); err != nil {
		t.Error(err)
	}
	if _, err := outputOf(t.Context(), "false", "x"); err == nil {
		t.Error("a failed command")
	}
}

// A location of hosted machines refuses them unless it keeps them to an
// allowlist, and doctor says what is missing.
func TestHostedLocation(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Hosted = true })
	req := e.request()
	req.Shape.AccountID = uuid.Nil() // instance-wide
	if _, err := e.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "egress: allowlist") {
		t.Fatalf("a hosted machine on open egress: %v", err)
	}
	status := func(p *Provider, name string) string {
		for _, c := range p.checks() {
			if c.Name == name {
				return c.Status
			}
		}
		return ""
	}
	if status(e.p, "hosted egress") != statusFail || status(e.p, "abuse checks") != statusWarn {
		t.Fatalf("checks = %+v", e.p.checks())
	}
	ok := newEnv(t, func(c *Config) {
		c.Hosted, c.Egress, c.Abuse = true, egressAllowlist, AbuseConfig{CPUMinutes: 120, MiningMinutes: 5}
	})
	if status(ok.p, "hosted egress") != statusOK || status(ok.p, "abuse checks") != statusOK {
		t.Fatalf("checks = %+v", ok.p.checks())
	}
	req = ok.request()
	req.Shape.AccountID = uuid.Nil() // instance-wide
	if _, err := ok.p.Create(t.Context(), req); err != nil {
		t.Fatalf("a hosted machine on an allowlist: %v", err)
	}
	if status(newEnv(t, nil).p, "hosted egress") != "" {
		t.Fatal("an own location checks hosted egress")
	}
}

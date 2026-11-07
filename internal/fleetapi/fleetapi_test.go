package fleetapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetapi"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type env struct {
	t       *testing.T
	st      *testkit.Store
	clock   *testkit.Clock
	account domain.Account
	h       http.Handler
	logs    *bytes.Buffer
	c       *fleet.Controller
	addr    string // RemoteAddr of the next requests
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	st := testkit.NewStore(clock.Now)
	e := &env{t: t, st: st, clock: clock, account: domain.Account{Name: "acme"}, logs: &bytes.Buffer{}, addr: "192.0.2.1:1234"}
	must(t, st.Accounts().Create(t.Context(), &e.account))
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.c = fleet.New(fleet.Config{Store: st, Now: clock.Now, Logger: log, ServerURL: "https://forecrew.example.com"})
	e.h = fleetapi.New(fleetapi.Config{Store: st, Fleet: e.c, Logger: log, Now: clock.Now, PollWait: 40 * time.Millisecond, PollRecheck: 5 * time.Millisecond}).Handler()
	return e
}

// provider adds an active agent provider and returns it with its token.
func (e *env) provider(name string) (domain.CapacityProvider, string) {
	e.t.Helper()
	token, hash := fleet.NewProviderToken()
	p := domain.CapacityProvider{AccountID: e.account.ID, Name: name, Kind: domain.ProviderFake, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, TokenHash: hash, Limits: domain.ProviderLimits{MaxMachines: 3}, Tags: map[string]string{"owner": "platform"}}
	must(e.t, e.st.CapacityProviders().Create(e.t.Context(), &p))
	return p, token
}

func (e *env) machine(p domain.CapacityProvider) domain.Machine {
	e.t.Helper()
	ctx := e.t.Context()
	s := domain.Shape{AccountID: p.AccountID, ProviderID: p.ID, Name: "s-" + uuid.New().String()[:8], OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20, Image: "linux", Max: 5, Enabled: true}
	must(e.t, e.st.Shapes().Create(ctx, &s))
	m := domain.Machine{AccountID: p.AccountID, ProviderID: p.ID, ShapeID: s.ID}
	must(e.t, e.st.Machines().Create(ctx, &m))
	return m
}

// call posts body, returning the status and the decoded answer.
func (e *env) call(path, token string, body any) (int, map[string]any) {
	e.t.Helper()
	raw, ok := body.([]byte)
	if !ok {
		var err error
		raw, err = json.Marshal(body)
		must(e.t, err)
	}
	r := httptest.NewRequestWithContext(e.t.Context(), http.MethodPost, path, bytes.NewReader(raw))
	r.RemoteAddr = e.addr
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	for _, path := range []string{"/fleet/v1/register", "/fleet/v1/poll", "/fleet/v1/reconcile", "/fleet/v1/machines/" + uuid.New().String() + "/state"} {
		for name, tok := range map[string]string{"none": "", "unknown": "fcp_nobody"} {
			if code, out := e.call(path, tok, map[string]any{}); code != 401 || errCode(out) != "unauthorized" {
				t.Errorf("%s, %s token: %d %v", path, name, code, out)
			}
		}
	}
	if code, _ := e.call("/fleet/v1/register", token, map[string]any{}); code != 200 {
		t.Fatalf("good token: %d", code)
	}
	must(t, e.st.CapacityProviders().SetStatus(t.Context(), p.ID, domain.ProviderRevoked))
	if code, _ := e.call("/fleet/v1/register", token, map[string]any{}); code != 401 {
		t.Fatalf("revoked: %d", code)
	}
}

func TestRegister(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	caps := domain.ProviderCapabilities{Host: "nas", MaxCPU: 16, OS: []domain.MachineOS{domain.OSLinux}}
	code, out := e.call("/fleet/v1/register", token, map[string]any{"capabilities": caps})
	if code != 200 || out["name"] != "home" || out["status"] != "active" || out["kind"] != "fake" || out["id"] != p.ID.String() {
		t.Fatalf("%d %v", code, out)
	}
	if out["limits"].(map[string]any)["max_machines"] != 3.0 || out["tags"].(map[string]any)["owner"] != "platform" {
		t.Fatalf("limits or tags: %v", out)
	}
	got, err := e.st.CapacityProviders().Get(t.Context(), p.ID)
	must(t, err)
	if got.Capabilities.Host != "nas" || got.LastSeenAt.IsZero() {
		t.Fatalf("stored %+v", got)
	}
	bad := []domain.ProviderCapabilities{
		{MaxCPU: -1},
		{Host: "two\nlines"},
		{Host: strings.Repeat("x", 300)},
		{Checks: make([]domain.ProviderCheck, 65)},
		{Checks: []domain.ProviderCheck{{Name: "kvm", Status: "ok", Detail: "a\x00b"}}},
	}
	for i := range bad[3].Checks {
		bad[3].Checks[i] = domain.ProviderCheck{Name: "c", Status: "ok"}
	}
	for _, c := range bad {
		if code, _ := e.call("/fleet/v1/register", token, map[string]any{"capabilities": c}); code != 400 {
			t.Errorf("%+v: %d", c, code)
		}
	}
}

func TestBodies(t *testing.T) {
	e := newEnv(t)
	_, token := e.provider("home")
	if code, out := e.call("/fleet/v1/poll", token, []byte("{")); code != 400 || errCode(out) != "bad_request" {
		t.Errorf("malformed: %d %v", code, out)
	}
	huge := []byte(`{"generation":"` + strings.Repeat("x", 2<<20) + `"}`)
	if code, out := e.call("/fleet/v1/poll", token, huge); code != 413 || errCode(out) != "too_large" {
		t.Errorf("huge: %d %v", code, out)
	}
}

func TestPollDelivers(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	p, token := e.provider("home")
	other, _ := e.provider("other")
	e.machine(other)
	m := e.machine(p)

	code, out := e.call("/fleet/v1/poll", token, map[string]any{})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	list := out["machines"].([]any)
	if len(list) != 1 {
		t.Fatalf("machines = %v", list)
	}
	first := list[0].(map[string]any)
	boot := first["bootstrap"].(map[string]any)
	if first["machine_id"] != m.ID.String() || boot["server_url"] != "https://forecrew.example.com" || !strings.HasPrefix(boot["token"].(string), "fcb_") {
		t.Fatalf("machine = %v", first)
	}
	if first["shape"].(map[string]any)["image"] != "linux" || first["image"].(map[string]any)["name"] != "linux" {
		t.Fatalf("shape or image: %v", first)
	}
	if strings.Contains(e.logs.String(), boot["token"].(string)) {
		t.Fatal("the token is in the logs")
	}
	got, err := e.st.CapacityProviders().Get(ctx, p.ID)
	must(t, err)
	if got.LastSeenAt.IsZero() {
		t.Fatal("no contact recorded")
	}

	// Listed in have, with the same generation: it waits and adds no token.
	gen := out["generation"]
	start := time.Now()
	code, out = e.call("/fleet/v1/poll", token, map[string]any{"generation": gen, "have": []string{m.ID.String()}})
	if code != 200 || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("%d after %s", code, time.Since(start))
	}
	again := out["machines"].([]any)[0].(map[string]any)
	if again["bootstrap"] != nil || out["generation"] != gen {
		t.Fatalf("%v", out)
	}

	// Not in have: at once, whatever the generation, with a new token.
	start = time.Now()
	_, out = e.call("/fleet/v1/poll", token, map[string]any{"generation": gen})
	if time.Since(start) > 30*time.Millisecond {
		t.Fatalf("waited %s", time.Since(start))
	}
	third := out["machines"].([]any)[0].(map[string]any)["bootstrap"].(map[string]any)
	if third["token"] == boot["token"] {
		t.Fatal("the same token twice")
	}

	// A change wakes a waiting poll, by a generation it did not send.
	if _, out = e.call("/fleet/v1/poll", token, map[string]any{"generation": "old", "have": []string{m.ID.String()}}); out["generation"] != gen {
		t.Fatalf("%v", out)
	}
}

// A poll that was waiting when its provider lost its token gets no demand.
func TestRevokedDuringPoll(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	m := e.machine(p)
	_, out := e.call("/fleet/v1/poll", token, map[string]any{"have": []string{m.ID.String()}})
	done := make(chan int)
	go func() {
		code, _ := e.call("/fleet/v1/poll", token, map[string]any{"generation": out["generation"], "have": []string{m.ID.String()}})
		done <- code
	}()
	must(t, e.st.CapacityProviders().SetStatus(t.Context(), p.ID, domain.ProviderRevoked))
	if code := <-done; code != 401 {
		t.Fatalf("status %d", code)
	}
}

func TestPausedGetsNoRequested(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	e.machine(p)
	must(t, e.st.CapacityProviders().SetStatus(t.Context(), p.ID, domain.ProviderPaused))
	if _, out := e.call("/fleet/v1/poll", token, map[string]any{}); len(out["machines"].([]any)) != 0 {
		t.Fatalf("%v", out)
	}
}

func TestState(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	_, otherToken := e.provider("other")
	m := e.machine(p)
	path := "/fleet/v1/machines/" + m.ID.String() + "/state"
	if code, out := e.call(path, otherToken, map[string]any{"state": "booting", "external_id": "vm"}); code != 404 || errCode(out) != "not_found" {
		t.Errorf("another provider: %d %v", code, out)
	}
	if code, _ := e.call("/fleet/v1/machines/nope/state", token, map[string]any{"state": "booting"}); code != 400 {
		t.Errorf("not a UUID: %d", code)
	}
	if code, _ := e.call(path, token, map[string]any{"state": "ready"}); code != 400 {
		t.Errorf("ready: %d", code)
	}
	if code, _ := e.call(path, token, map[string]any{"state": "booting"}); code != 400 {
		t.Errorf("no external ID: %d", code)
	}
	if code, _ := e.call(path, token, map[string]any{"state": "booting", "external_id": "a\nb"}); code != 400 {
		t.Errorf("bad external ID: %d", code)
	}
	if code, out := e.call(path, token, map[string]any{"state": "booting", "external_id": "vm-1"}); code != 200 || len(out) != 0 {
		t.Fatalf("booting: %d %v", code, out)
	}
	if code, _ := e.call(path, token, map[string]any{"state": "destroyed"}); code != 200 {
		t.Fatalf("destroyed: %d", code)
	}
	if code, out := e.call(path, token, map[string]any{"state": "booting", "external_id": "vm-1"}); code != 409 || errCode(out) != "conflict" {
		t.Errorf("after the end: %d %v", code, out)
	}
}

func TestReconcile(t *testing.T) {
	e := newEnv(t)
	p, token := e.provider("home")
	m := e.machine(p)
	code, out := e.call("/fleet/v1/reconcile", token, map[string]any{"instances": []fleet.Instance{{ExternalID: "vm-1", MachineID: m.ID}, {ExternalID: "vm-stray"}}})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if d := out["destroy"].([]any); len(d) != 1 || d[0] != "vm-stray" {
		t.Fatalf("destroy = %v", d)
	}
	got, err := e.st.Machines().Get(t.Context(), m.ID)
	must(t, err)
	if got.State != domain.MachineBooting || got.ExternalID != "vm-1" {
		t.Fatalf("machine = %+v", got)
	}
	if code, _ := e.call("/fleet/v1/reconcile", token, map[string]any{"instances": []fleet.Instance{{}}}); code != 400 {
		t.Errorf("no external ID: %d", code)
	}
	if code, out := e.call("/fleet/v1/reconcile", token, map[string]any{}); code != 200 || out["destroy"] == nil {
		t.Errorf("empty: %d %v", code, out)
	}
}

func claim(b byte) string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func (e *env) joinCode() (string, domain.ProviderJoinCode) {
	e.t.Helper()
	code, hash := fleet.NewJoinCode()
	c := domain.ProviderJoinCode{AccountID: e.account.ID, CodeHash: hash, ExpiresAt: e.clock.Now().Add(time.Hour)}
	must(e.t, e.st.ProviderJoinCodes().Create(e.t.Context(), &c))
	return code, c
}

func joinBody(code, claim string) map[string]any {
	return map[string]any{"code": code, "claim": claim, "kind": "microvm", "facts": domain.ProviderCapabilities{Host: "nas"}}
}

func TestJoin(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	code, c := e.joinCode()
	st, out := e.call("/fleet/v1/join", "", joinBody(code, claim(1)))
	if st != 200 || out["status"] != "waiting" || out["retry_after_seconds"] != 5.0 || out["token"] != nil {
		t.Fatalf("%d %v", st, out)
	}
	if st, out := e.call("/fleet/v1/join", "", joinBody(code, claim(2))); st != 409 || errCode(out) != "conflict" {
		t.Errorf("second agent: %d %v", st, out)
	}
	// The code alone reaches nothing else.
	if st, _ := e.call("/fleet/v1/poll", code, map[string]any{}); st != 401 {
		t.Errorf("poll with a code: %d", st)
	}
	p := domain.CapacityProvider{AccountID: e.account.ID, Name: "home", Kind: domain.ProviderMicroVM, Mode: domain.ProviderModeAgent}
	must(t, e.st.CapacityProviders().Create(ctx, &p))
	must(t, e.st.ProviderJoinCodes().Confirm(ctx, c.ID, p.ID))
	st, out = e.call("/fleet/v1/join", "", joinBody(code, claim(1)))
	token, _ := out["token"].(string)
	if st != 200 || out["status"] != "confirmed" || !strings.HasPrefix(token, "fcp_") {
		t.Fatalf("%d %v", st, out)
	}
	if st, _ := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st != 401 {
		t.Errorf("used: %d", st)
	}
	if st, _ := e.call("/fleet/v1/register", token, map[string]any{}); st != 200 {
		t.Errorf("the token does not work: %d", st)
	}
	if l := e.logs.String(); strings.Contains(l, token) || strings.Contains(l, code) || strings.Contains(l, claim(1)) {
		t.Fatalf("a secret in the logs:\n%s", l)
	}
}

func TestJoinBadRequests(t *testing.T) {
	e := newEnv(t)
	code, _ := e.joinCode()
	for name, body := range map[string]map[string]any{
		"no code":   joinBody("", claim(1)),
		"no claim":  joinBody(code, ""),
		"short":     joinBody(code, "abc"),
		"not b64":   joinBody(code, strings.Repeat("!", 43)),
		"bad kind":  {"code": code, "claim": claim(1), "kind": "vmware"},
		"bad facts": {"code": code, "claim": claim(1), "kind": "aws", "facts": domain.ProviderCapabilities{Host: "a\nb"}},
	} {
		if st, _ := e.call("/fleet/v1/join", "", body); st != 400 {
			t.Errorf("%s: %d", name, st)
		}
	}
}

// Past 5 unknown codes a second, over every address, every join stops before
// any lookup, a good code included; a good code uses up nothing.
func TestJoinGlobalThrottle(t *testing.T) {
	e := newEnv(t)
	code, _ := e.joinCode()
	for range 20 {
		if st, out := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st != 200 {
			t.Fatalf("a real code was limited: %d %v", st, out)
		}
		e.addr = "192.0.2.9:1"
	}
	for i := range 5 {
		e.addr = fmt.Sprintf("198.51.100.%d:1", i)
		if st, _ := e.call("/fleet/v1/join", "", joinBody("AAAA-AAAA", claim(2))); st != 401 {
			t.Fatalf("guess %d: %d", i, st)
		}
	}
	e.addr = "198.51.100.200:1"
	if st, out := e.call("/fleet/v1/join", "", joinBody("AAAA-AAAA", claim(2))); st != 429 || errCode(out) != "rate_limited" {
		t.Fatalf("sixth guess: %d %v", st, out)
	}
	if st, _ := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st != 429 {
		t.Fatalf("real code during a flood: %d", st)
	}
	e.clock.Advance(time.Second)
	if st, _ := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st != 200 {
		t.Fatalf("after backing off: %d", st)
	}
}

func TestJoinAddressLimit(t *testing.T) {
	e := newEnv(t)
	code, _ := e.joinCode()
	limited := 0
	for range 40 {
		if st, _ := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st == 429 {
			limited++
		}
	}
	if limited != 20 {
		t.Fatalf("%d calls limited", limited)
	}
	e.addr = "192.0.2.2:1"
	if st, _ := e.call("/fleet/v1/join", "", joinBody(code, claim(1))); st != 200 {
		t.Fatalf("another address: %d", st)
	}
}

// Each unknown instance costs a lookup and an audit row, so the lists are
// bounded, and a list at the bound still passes.
func TestListsAreBounded(t *testing.T) {
	e := newEnv(t)
	_, token := e.provider("home")
	instances := make([]fleet.Instance, 1001)
	for i := range instances {
		instances[i] = fleet.Instance{ExternalID: fmt.Sprint("vm-", i)}
	}
	if code, out := e.call("/fleet/v1/reconcile", token, map[string]any{"instances": instances}); code != 400 || errCode(out) != "bad_request" {
		t.Errorf("reconcile: %d %v", code, out)
	}
	if audit, err := e.st.Audit().List(t.Context(), e.account.ID, store.AuditFilter{}); err != nil || len(audit) != 0 {
		t.Errorf("refused, yet it wrote %d audit rows: %v", len(audit), err)
	}
	if code, out := e.call("/fleet/v1/reconcile", token, map[string]any{"instances": instances[:1000]}); code != 200 || len(out["destroy"].([]any)) != 1000 {
		t.Errorf("reconcile at the bound: %d", code)
	}
	have := make([]uuid.UUID, 1001)
	for i := range have {
		have[i] = uuid.New()
	}
	if code, out := e.call("/fleet/v1/poll", token, map[string]any{"have": have}); code != 400 || errCode(out) != "bad_request" {
		t.Errorf("poll: %d %v", code, out)
	}
	if code, _ := e.call("/fleet/v1/poll", token, map[string]any{"have": have[:1000]}); code != 200 {
		t.Errorf("poll at the bound: %d", code)
	}
}

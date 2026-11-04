package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestNewRejectsBadPlaybooks(t *testing.T) {
	const unknownAction = "description: x\nsteps:\n  - id: a\n    type: action\n    action: no.such.action\n"
	const unknownRole = "description: x\nsteps:\n  - id: j\n    type: job\n    with: {role: no-such-role}\n"
	const nestedUnknownRole = "description: x\nfinally:\n  - id: a\n    type: action\n    action: profile.feedback\nsteps:\n" +
		"  - id: l\n    type: loop\n    max_iterations: 2\n    until: steps.j.output.ok\n    steps:\n      - id: j\n        type: job\n        with: {role: ghost}\n"
	const roleFromInput = "description: x\ninputs: [role]\nsteps:\n  - id: j\n    type: job\n    with: {role: '${inputs.role}'}\n"
	tests := []struct {
		name, playbook, want string
	}{
		{"unknown action", unknownAction, "no.such.action"},
		{"unknown role", unknownRole, "no-such-role"},
		{"unknown role in a loop", nestedUnknownRole, "ghost"},
		{"invalid yaml", "steps: nope\n", "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			files := packs(t, func(m fstest.MapFS) {
				m["packs/playbooks/bad.yaml"] = &fstest.MapFile{Data: []byte(tt.playbook)}
			})
			_, err := New(Deps{Config: testConfig(), Store: f.st, GitHub: f.tracker, Files: files})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want it to mention %q", err, tt.want)
			}
			if tt.name == "unknown role" && !errors.Is(err, roles.ErrUnknownRole) {
				t.Errorf("error %v does not wrap ErrUnknownRole", err)
			}
		})
	}

	f := newFixture(t)
	files := packs(t, func(m fstest.MapFS) {
		m["packs/playbooks/dynamic.yaml"] = &fstest.MapFile{Data: []byte(roleFromInput)}
	})
	if _, err := New(Deps{Config: testConfig(), Store: f.st, GitHub: f.tracker, Files: files}); err != nil {
		t.Errorf("a role given by a reference is checked later: %v", err)
	}
}

func TestNewRejectsBadRoles(t *testing.T) {
	f := newFixture(t)
	empty := packs(t, func(m fstest.MapFS) {
		for name := range m {
			if strings.HasPrefix(name, "packs/roles/") {
				delete(m, name)
			}
		}
	})
	if _, err := New(Deps{Config: testConfig(), Store: f.st, GitHub: f.tracker, Files: empty}); err == nil || !strings.Contains(err.Error(), "load roles") {
		t.Errorf("no roles: %v", err)
	}
	broken := packs(t, func(m fstest.MapFS) { m["packs/playbooks/x.yaml"] = &fstest.MapFile{Data: []byte("not: [valid")} })
	if _, err := New(Deps{Config: testConfig(), Store: f.st, GitHub: f.tracker, Files: broken}); !errors.Is(err, workflow.ErrInvalidPlaybook) {
		t.Errorf("broken playbook: %v", err)
	}
}

func TestLoadsEveryEmbeddedPlaybook(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.engine.Start(t.Context(), "onboarding", workflow.StartRequest{
		AccountID: f.account.ID, ProjectID: f.project.ID, Inputs: map[string]any{"repo": "acme/app", "project": "app"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.engine.Start(t.Context(), "nope", workflow.StartRequest{}); !errors.Is(err, workflow.ErrUnknownPlaybook) {
		t.Errorf("unknown playbook: %v", err)
	}
}

func get(t *testing.T, h http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoutes(t *testing.T) {
	down := errors.New("down")
	var pingErr error
	f := newFixture(t, func(d *Deps) { d.Ping = func(context.Context) error { return pingErr } })
	h := f.app.Handler()
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/worker/v1/poll", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/me", http.StatusUnauthorized},
		{http.MethodPost, "/mcp", http.StatusUnauthorized},
		{http.MethodPost, "/job-mcp", http.StatusUnauthorized},
		{http.MethodPost, "/webhooks/github", http.StatusUnauthorized},
		{http.MethodGet, "/webhooks/github", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		if rec := get(t, h, tt.method, tt.path); rec.Code != tt.want {
			t.Errorf("%s %s: status %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
	// "/" is the web UI: the page itself when built, else the "not built" notice.
	if rec := get(t, h, http.MethodGet, "/inbox"); rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /inbox: status %d", rec.Code)
	}
	if rec := get(t, h, http.MethodGet, "/api/v1/nope"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "html") {
		t.Errorf("GET /api/v1/nope: status %d", rec.Code)
	}
	pingErr = down
	if rec := get(t, h, http.MethodGet, "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz with the database down: %d", rec.Code)
	}
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestOnboardingQueuesItsFirstJob(t *testing.T) {
	f := newFixture(t)
	f.tracker.Install("acme/new")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = f.app.engine.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	by := domain.Actor{AccountID: f.account.ID, Kind: domain.ActorMCP, Name: "test"}
	out, err := f.app.tools.OnboardProject(ctx, by, mcp.OnboardProjectInput{Repo: "acme/new"})
	if err != nil {
		t.Fatal(err)
	}
	var job domain.Job
	waitFor(t, "the analyst job", func() bool {
		jobs, err := f.st.Jobs().List(ctx, f.account.ID, store.JobFilter{ProjectID: out.Project.ID})
		if err != nil || len(jobs) == 0 {
			return false
		}
		job = jobs[0]
		return true
	})
	if job.Role != "onboarding-analyst" || job.Status != domain.JobQueued || job.Spec.Repo != "acme/new" ||
		job.Spec.Branch != "agent/onboarding" || job.Spec.Model != "claude-strong" || !job.Spec.IncludeRepoFacts {
		t.Errorf("job = %+v", job)
	}
	if len(job.Spec.WritePaths) == 0 || job.Spec.Prompt == "" {
		t.Errorf("spec = %+v", job.Spec)
	}
}

func TestRunServesAndStops(t *testing.T) {
	var logs bytes.Buffer
	ln := listen(t)
	f := newFixture(t, func(d *Deps) {
		d.Listener = ln
		d.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- f.app.Run(ctx) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	waitFor(t, "healthz", func() bool {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
	})
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop within 10 seconds")
	}
	if !strings.Contains(logs.String(), "forecrewd started") || !strings.Contains(logs.String(), "forecrewd stopped") {
		t.Errorf("logs = %s", logs.String())
	}
}

func TestRunListenErrors(t *testing.T) {
	busy := listen(t)
	f := newFixture(t, func(d *Deps) { d.Config.Server.Listen = busy.Addr().String() })
	if err := f.app.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("busy port: %v", err)
	}
}

func TestRunFailsWhenTheServerFails(t *testing.T) {
	ln := listen(t)
	f := newFixture(t, func(d *Deps) { d.Listener = ln })
	_ = ln.Close()
	if err := f.app.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "serve") {
		t.Errorf("closed listener: %v", err)
	}
}

func TestRunStopsWhenTheEngineFails(t *testing.T) {
	ln := listen(t)
	f := newFixture(t, func(d *Deps) { d.Listener = ln })
	boom := errors.New("engine broke")
	f.app.runEngine = func(context.Context) error { return boom }
	errc := make(chan error, 1)
	go func() { errc <- f.app.Run(t.Context()) }()
	select {
	case err := <-errc:
		if !errors.Is(err, boom) {
			t.Fatalf("Run = %v, want the engine's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept serving without an engine")
	}
}

func TestNewRefusesSameVendorReview(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig()
	cfg.Roles["reviewer"] = config.Role{Runtime: "claude", Tier: config.TierBest}
	if _, err := New(Deps{Config: cfg, Store: f.st, GitHub: f.tracker}); !errors.Is(err, roles.ErrSameVendorReview) {
		t.Errorf("New: %v, want ErrSameVendorReview", err)
	}
	cfg.Policy.AllowSameVendorReview = true
	if _, err := New(Deps{Config: cfg, Store: f.st, GitHub: f.tracker}); err != nil {
		t.Errorf("allowed by policy: %v", err)
	}
}

func TestJobMCPURL(t *testing.T) {
	cfg := &config.Config{}
	if got := jobMCPURL(cfg); got != "" {
		t.Errorf("without a public URL: %q", got)
	}
	cfg.Server.PublicBaseURL = "https://forecrew.example.com"
	if got := jobMCPURL(cfg); got != "https://forecrew.example.com/job-mcp" {
		t.Errorf("jobMCPURL = %q", got)
	}
}

func TestProviderOfPoolActions(t *testing.T) {
	without := newFixture(t)
	if without.app.provider != nil {
		t.Error("a provider runs without GitHub's Actions calls")
	}
	fake := testkit.NewFakeActions(t, nil)
	with := newFixture(t, func(d *Deps) { d.Actions = fake.Client })
	if with.app.provider == nil {
		t.Fatal("no provider with GitHub's Actions calls")
	}
}

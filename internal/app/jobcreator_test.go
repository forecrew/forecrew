package app

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0", PublicBaseURL: "http://localhost:8080"},
		GitHub: config.GitHub{AppID: 1, WebhookSecret: "hook-secret", Allowlist: []string{"octocat"}},
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic", Credentials: map[string]string{"ANTHROPIC_API_KEY": "k"}},
			"codex":  {Adapter: config.AdapterCodex, Vendor: "openai"},
		},
		ModelTiers: map[config.Tier]map[string]string{
			config.TierStrong: {"anthropic": "claude-strong", "openai": "gpt-strong"},
			config.TierBest:   {"anthropic": "claude-best", "openai": "gpt-best"},
		},
		Roles: map[string]config.Role{
			"onboarding-analyst": {Runtime: "claude", Tier: config.TierStrong},
			"product-owner":      {Runtime: "claude", Tier: config.TierStrong},
			"architect":          {Runtime: "claude", Tier: config.TierBest},
			"developer":          {Runtime: "claude", Tier: config.TierStrong},
			"reviewer":           {Runtime: "codex", Tier: config.TierBest},
		},
	}
}

// fixture is a job creator on the in-memory store with one project.
type fixture struct {
	creator *jobCreator
	catalog *roles.Catalog
	st      *testkit.Store
	account domain.Account
	project domain.Project
}

func newFixture(t *testing.T, edit ...func(*config.Config)) *fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	f := &fixture{st: st}
	f.account = domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &f.account))
	f.project = domain.Project{AccountID: f.account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &f.project))
	cfg := testConfig()
	for _, e := range edit {
		e(cfg)
	}
	catalog, err := roles.New(forecrew.Files, cfg)
	must(t, err)
	f.catalog = catalog
	f.creator = &jobCreator{
		store: st, roles: catalog, packs: contextpack.New(st, cfg.GitHub.Allowed, 0),
		jobs: jobs.New(st, testkit.NewSignalBus(st), slog.New(slog.DiscardHandler), clock.Now),
	}
	return f
}

func (f *fixture) request(role string) workflow.JobRequest {
	return workflow.JobRequest{AccountID: f.account.ID, ProjectID: f.project.ID, Role: role}
}

func TestJobCreatorWritingRole(t *testing.T) {
	f := newFixture(t)
	goal := domain.Goal{AccountID: f.account.ID, ProjectID: f.project.ID, Title: "Ship it", Body: "Add a thing", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(t.Context(), &goal))
	req := f.request("developer")
	req.ID, req.Branch, req.Needs, req.GoalID = domain.NewID(), "agent/task-1", []string{"Go", "linux"}, goal.ID
	req.IncludeRepoFacts, req.Prompt, req.Feedback = true, "mind the tests", "fix the lint"

	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	if id != req.ID {
		t.Fatalf("id = %s, want the request's %s", id, req.ID)
	}
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	role, err := f.catalog.Resolve(t.Context(), f.account.ID, f.project.ID, "developer")
	must(t, err)
	if j.Status != domain.JobQueued || j.Role != "developer" || j.Runtime != "claude-code" || j.GoalID != goal.ID {
		t.Errorf("job = %+v", j)
	}
	if want := []string{"go", "linux"}; !slices.Equal(j.Needs, want) {
		t.Errorf("needs = %v, want %v", j.Needs, want)
	}
	s := j.Spec
	if s.JobID != id || s.AccountID != f.account.ID || s.Repo != "acme/app" || s.BaseRef != "main" || s.Branch != "agent/task-1" ||
		s.Model != "claude-strong" || s.Runtime != "claude-code" || !s.IncludeRepoFacts {
		t.Errorf("spec = %+v", s)
	}
	if !slices.Equal(s.WritePaths, role.WritePaths) || len(s.WritePaths) == 0 {
		t.Errorf("write_paths = %v, want the role's %v", s.WritePaths, role.WritePaths)
	}
	if !slices.Equal(s.AllowedTools, role.AllowedTools) || s.MaxTurns != role.MaxTurns || time.Duration(s.Timeout) != role.Timeout {
		t.Errorf("limits = %v %d %v", s.AllowedTools, s.MaxTurns, s.Timeout)
	}
	if len(s.OutputSchema) == 0 || strings.Contains(string(s.OutputSchema), "$ref") {
		t.Errorf("output schema must be inline: %s", s.OutputSchema)
	}
	for _, want := range []string{"Add a thing", "mind the tests", "fix the lint"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestJobCreatorReadOnlyBranchAndOverrides(t *testing.T) {
	f := newFixture(t)
	req := f.request("reviewer")
	req.Repo, req.BaseRef, req.Branch = "ACME/App", "develop", "agent/task-2"
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	// A read-only role inspects the branch; the codex tier maps to the openai model.
	if j.Runtime != "codex" || j.Spec.Model != "gpt-best" || len(j.Spec.WritePaths) != 0 ||
		j.Spec.Repo != "acme/app" || j.Spec.BaseRef != "develop" || j.Spec.Branch != "agent/task-2" {
		t.Errorf("job = %+v", j)
	}
}

func TestJobCreatorGeneratesID(t *testing.T) {
	f := newFixture(t)
	id, err := f.creator.Create(t.Context(), f.request("reviewer"))
	must(t, err)
	if id == uuid.Nil() {
		t.Fatal("no job ID")
	}
}

func TestJobCreatorErrors(t *testing.T) {
	f := newFixture(t)
	other := f.request("developer")
	other.ProjectID = domain.NewID()
	unknown := f.request("nope")
	if _, err := f.creator.Create(t.Context(), other); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	foreign := f.request("developer")
	foreign.Repo = "acme/other"
	if _, err := f.creator.Create(t.Context(), foreign); !errors.Is(err, jobs.ErrInvalid) {
		t.Errorf("another repo: %v", err)
	}
	if _, err := f.creator.Create(t.Context(), unknown); !errors.Is(err, roles.ErrUnknownRole) {
		t.Errorf("unknown role: %v", err)
	}
	missing := f.request("developer")
	missing.TaskID = domain.NewID()
	if _, err := f.creator.Create(t.Context(), missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("context pack: %v", err)
	}
}

func TestSameVendorReviewAllowed(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.Roles["reviewer"] = config.Role{Runtime: "claude", Tier: config.TierBest}
		c.Policy.AllowSameVendorReview = true
	})
	id, err := f.creator.Create(t.Context(), f.request("reviewer"))
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	if j.Runtime != "claude-code" || j.Spec.Model != "claude-best" {
		t.Errorf("job = %+v", j)
	}
}

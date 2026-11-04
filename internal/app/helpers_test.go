package app

import (
	"io"
	"io/fs"
	"log/slog"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/testkit"
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
			"knowledge-curator":  {Runtime: "claude", Tier: config.TierStrong},
			"facilitator":        {Runtime: "claude", Tier: config.TierBest},
		},
	}
}

// fixture is an App on the in-memory store with one project.
type fixture struct {
	app     *App
	creator *jobCreator
	catalog *roles.Catalog
	st      *testkit.Store
	tracker *testkit.Tracker
	account domain.Account
	project domain.Project
}

func newFixture(t *testing.T, mutate ...func(*Deps)) *fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	f := &fixture{st: st, tracker: testkit.NewTracker(clock.Now)}
	f.account = domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &f.account))
	f.project = domain.Project{AccountID: f.account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &f.project))
	f.tracker.Install("acme/app")
	giveHead(f.tracker, "acme/app")
	d := Deps{Config: testConfig(), Store: st, GitHub: f.tracker, Logger: slog.New(slog.DiscardHandler), Version: "test", Now: clock.Now}
	for _, m := range mutate {
		m(&d)
	}
	a, err := New(d)
	must(t, err)
	f.app, f.creator, f.catalog = a, a.creator, a.catalog
	return f
}

// listen returns a listener on a free local port.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// packs returns a copy of the embedded packs that edit may change.
func packs(t *testing.T, edit func(fstest.MapFS)) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	must(t, fs.WalkDir(forecrew.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(forecrew.Files, path)
		m[path] = &fstest.MapFile{Data: data}
		return err
	}))
	edit(m)
	return m
}

// slogTo returns a logger writing JSON lines to w.
func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

// giveHead gives the repo a default branch with a commit: the knowledge
// policy is read from it, and a job is refused without the policy.
func giveHead(tr *testkit.Tracker, repo string) {
	if err := tr.SetFile(repo, "head0", "README.md", []byte("# app\n")); err != nil {
		panic(err)
	}
	if err := tr.SetHeadOfBranch(repo, "main", "head0"); err != nil {
		panic(err)
	}
}

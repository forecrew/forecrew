package models_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var project = domain.NewID()

func fileConfig() *config.Config {
	return &config.Config{
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic", CredentialEnv: []string{"ANTHROPIC_API_KEY"}, Credentials: map[string]string{"ANTHROPIC_API_KEY": "server"}},
			"codex":  {Adapter: config.AdapterCodex, Vendor: "openai"},
		},
		ModelTiers: map[config.Tier]map[string]string{config.TierStrong: {"anthropic": "file-claude", "openai": "file-gpt"}},
		Roles:      map[string]config.Role{"developer": {Runtime: "claude", Tier: config.TierStrong}, "reviewer": {Runtime: "codex", Tier: config.TierStrong}},
		Pools:      config.DefaultPools(),
	}
}

func hosted(c *config.Config) *config.Config {
	off := false
	c.Models.ServerKeysForOrganizations = &off
	return c
}

func conn(name, adapter, vendor string) domain.ModelConnection {
	return domain.ModelConnection{Name: name, Adapter: adapter, Vendor: vendor, SecretRef: "store:models/" + name, Enabled: true}
}

func org(role, connection string) domain.ModelAssignment {
	return domain.ModelAssignment{Roles: map[string]domain.ModelRole{role: {Connection: connection, Tier: "strong"}},
		Tiers: map[string]map[string]string{"strong": {"acme": "org-model"}}}
}

func TestResolutionOrder(t *testing.T) {
	off := false
	over := org("developer", "project-conn")
	over.ProjectID, over.Tiers = project, map[string]map[string]string{"strong": {"acme": "project-model"}}
	conns := []domain.ModelConnection{conn("org-conn", config.AdapterClaudeCode, "acme"), conn("project-conn", config.AdapterCodex, "acme")}
	closed := org("developer", "org-conn")
	closed.ProjectOverrides = &off
	tests := []struct {
		name     string
		rows     []domain.ModelAssignment
		role     string
		wantConn string
		source   string
		model    string
	}{
		{"project override first", []domain.ModelAssignment{org("developer", "org-conn"), over}, "developer", "project-conn", models.SourceOrganization, "project-model"},
		{"organization next", []domain.ModelAssignment{org("developer", "org-conn")}, "developer", "org-conn", models.SourceOrganization, "org-model"},
		{"server file last", []domain.ModelAssignment{org("architect", "org-conn")}, "developer", "claude", models.SourceServer, "file-claude"},
		{"policy turns overrides off", []domain.ModelAssignment{closed, over}, "developer", "org-conn", models.SourceOrganization, "org-model"},
		{"another project's role stays", []domain.ModelAssignment{org("developer", "org-conn"), over}, "reviewer", "codex", models.SourceServer, "file-gpt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := models.NewState(conns, tt.rows).Decide(fileConfig(), models.Request{ProjectID: project, Role: tt.role})
			if d.Waiting != "" || d.Connection != tt.wantConn || d.Source != tt.source || d.Model != tt.model {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestDecideWaits(t *testing.T) {
	off := conn("acme", config.AdapterClaudeCode, "acme")
	off.Enabled = false
	unlisted := conn("private", config.AdapterClaudeCode, "acme")
	plain := conn("oc", config.AdapterOpencode, "acme")
	over := org("developer", "gone")
	over.ProjectID = project
	tests := []struct {
		name string
		cfg  *config.Config
		st   models.State
		r    models.Request
		want string
	}{
		{"hosted, nothing connected", hosted(fileConfig()), models.NewState(nil, nil), models.Request{Role: "developer"}, "does not lend its keys"},
		{"assignment to nothing", fileConfig(), models.NewState(nil, []domain.ModelAssignment{org("developer", "gone")}), models.Request{Role: "developer"}, "does not exist"},
		{"hosted does not reach a file runtime", hosted(fileConfig()), models.NewState(nil, []domain.ModelAssignment{org("developer", "claude")}), models.Request{Role: "developer"}, "does not exist"},
		{"turned off", fileConfig(), models.NewState([]domain.ModelConnection{off}, []domain.ModelAssignment{org("developer", "acme")}), models.Request{Role: "developer"}, "turned off"},
		{"a bad override does not fall back", fileConfig(), models.NewState([]domain.ModelConnection{conn("acme", config.AdapterClaudeCode, "acme")}, []domain.ModelAssignment{org("developer", "acme"), over}), models.Request{ProjectID: project, Role: "developer"}, "does not exist"},
		{"no model", fileConfig(), models.NewState([]domain.ModelConnection{conn("acme", config.AdapterClaudeCode, "other")}, []domain.ModelAssignment{org("developer", "acme")}), models.Request{Role: "developer"}, "no strong model"},
		{"opencode form", fileConfig(), models.NewState([]domain.ModelConnection{plain}, []domain.ModelAssignment{org("developer", "oc")}), models.Request{Role: "developer"}, "provider/model"},
		{"pool rule on connection names", poolsOnly(fileConfig(), "other"), models.NewState([]domain.ModelConnection{unlisted}, []domain.ModelAssignment{org("developer", "private")}), models.Request{Role: "developer", Pool: "build"}, "may not receive"},
		{"role of an organization with settings", fileConfig(), models.NewState([]domain.ModelConnection{off}, nil), models.Request{Role: "architect"}, "has no model connection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.st.Decide(tt.cfg, tt.r)
			if !strings.Contains(d.Waiting, tt.want) || !strings.Contains(d.Waiting, tt.r.Role) || d.Connection != "" || d.Unset {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
	if d := models.NewState(nil, nil).Decide(fileConfig(), models.Request{Role: "architect"}); !d.Unset {
		t.Fatalf("a role nobody configured = %+v", d)
	}
}

func poolsOnly(c *config.Config, name string) *config.Config {
	p := c.Pools["build"]
	p.Credentials.Runtimes = []string{name}
	c.Pools = map[string]config.Pool{"build": p}
	return c
}

func TestFileRuntimes(t *testing.T) {
	c := fileConfig()
	c.Runtimes["claude-b"] = config.Runtime{Adapter: config.AdapterClaudeCode, Vendor: "anthropic"}
	c.Roles["architect"] = config.Role{Runtime: "claude-b", Tier: config.TierStrong}
	tests := []struct {
		name string
		r    models.Request
		want string
	}{
		{"role's runtime", models.Request{Role: "architect", Adapter: config.AdapterClaudeCode}, "claude-b"},
		{"first of the adapter for a role of another", models.Request{Role: "reviewer", Adapter: config.AdapterClaudeCode}, "claude"},
		{"first of the adapter for an unknown role", models.Request{Role: "other", Adapter: config.AdapterClaudeCode}, "claude"},
		{"seat", models.Request{Role: "developer", Pinned: config.AdapterCodex}, "codex"},
		{"adapter gone: back to the role's", models.Request{Role: "developer", Adapter: "gone"}, "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if d := models.NewState(nil, nil).Decide(c, tt.r); d.Connection != tt.want || d.Source != models.SourceServer {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestSeats(t *testing.T) {
	a, b := conn("a", config.AdapterClaudeCode, "acme"), conn("b", config.AdapterCodex, "acme")
	st := models.NewState([]domain.ModelConnection{a, b}, []domain.ModelAssignment{org("developer", "a")})
	if d := st.Decide(fileConfig(), models.Request{Role: "developer", Pinned: config.AdapterCodex}); d.Connection != "b" {
		t.Fatalf("seat on the organization's connection = %+v", d)
	}
	if d := st.Decide(fileConfig(), models.Request{Role: "developer", Pinned: config.AdapterOpencode}); d.Waiting == "" {
		t.Fatalf("no connection of the adapter = %+v", d)
	}
	// A project that keeps a role on one connection keeps its seats there.
	over := org("developer", "a")
	over.ProjectID = project
	st = models.NewState([]domain.ModelConnection{a, b}, []domain.ModelAssignment{org("developer", "a"), over})
	if d := st.Decide(fileConfig(), models.Request{ProjectID: project, Role: "developer", Pinned: config.AdapterCodex}); !strings.Contains(d.Waiting, "override") {
		t.Fatalf("seat against an override = %+v", d)
	}
	// With no connection of it, the server's serves, when it may.
	st = models.NewState([]domain.ModelConnection{a}, []domain.ModelAssignment{org("developer", "a")})
	if d := st.Decide(fileConfig(), models.Request{Role: "developer", Pinned: config.AdapterCodex}); d.Source != models.SourceServer || d.Connection != "codex" {
		t.Fatalf("seat on the server's runtime = %+v", d)
	}
	if d := st.Decide(hosted(fileConfig()), models.Request{Role: "developer", Pinned: config.AdapterCodex}); d.Waiting == "" {
		t.Fatalf("hosted seat = %+v", d)
	}
}

// world is an organization with a key on a fake store.
type world struct {
	st      *testkit.Store
	vault   *secrets.Encrypted
	account uuid.UUID
	project domain.Project
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st := testkit.NewStore(time.Now)
	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	w := &world{st: st, vault: vault, account: domain.NewID()}
	w.project = domain.Project{AccountID: w.account, Name: "app", Repo: "acme/app"}
	if err := st.Projects().Create(t.Context(), &w.project); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) connect(t *testing.T, c domain.ModelConnection, key, role string) {
	t.Helper()
	ctx := t.Context()
	c.AccountID, c.UpdatedBy = w.account, "user:ana"
	if err := w.st.ModelConnections().Put(ctx, &c); err != nil {
		t.Fatal(err)
	}
	name, _ := domain.StoreSecretName(c.SecretRef)
	if key != "" {
		if err := w.vault.Put(ctx, domain.Actor{}, w.account, name, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	a := org(role, c.Name)
	a.AccountID, a.UpdatedBy = w.account, "user:ana"
	if err := w.st.ModelAssignments().Put(ctx, &a); err != nil {
		t.Fatal(err)
	}
	// Authorized for every project: the private code policy has its own
	// tests.
	w.authorize(t, c, uuid.Nil())
}

// authorize lets c receive the private code of project, or of every project.
func (w *world) authorize(t *testing.T, c domain.ModelConnection, project uuid.UUID) {
	t.Helper()
	a := domain.AuthorizationOf(c, project, "user:ana")
	a.AccountID = w.account
	if err := w.st.ModelAuthorizations().Put(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
}

func (w *world) job(t *testing.T, role, adapter string) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: w.account, ProjectID: w.project.ID, Role: role, Runtime: adapter, Pool: domain.PoolBuild, Kind: domain.JobKindAgent,
		Spec: domain.JobSpec{Repo: "acme/app", Runtime: adapter, Model: "old"}}
	if err := w.st.Jobs().Create(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestGrantOfAConnection(t *testing.T) {
	w := newWorld(t)
	c := conn("acme", config.AdapterOpencode, "acme")
	c.KeyEnv, c.BaseURL = "ACME_KEY", "https://models.example.com/v1"
	w.connect(t, c, "sk-org", "developer")
	a := org("developer", "acme")
	a.AccountID, a.Tiers = w.account, map[string]map[string]string{"strong": {"acme": "acme/big"}}
	if err := w.st.ModelAssignments().Put(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	s := models.New(w.st, fileConfig(), w.vault, nil)
	g, err := s.Grant(t.Context(), w.job(t, "developer", config.AdapterOpencode))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Endpoint{BaseURL: c.BaseURL, KeyEnv: "ACME_KEY", Organization: true}
	if g.Waiting != "" || g.Connection != "acme" || g.Model != "acme/big" || !maps.Equal(g.Env, map[string]string{"ACME_KEY": "sk-org"}) || g.Endpoint == nil || *g.Endpoint != want {
		t.Fatalf("grant = %+v", g)
	}
}

func TestGrantKeyEnvOfTheAdapter(t *testing.T) {
	w := newWorld(t)
	w.connect(t, conn("acme", config.AdapterClaudeCode, "acme"), "sk-org", "developer")
	g, err := models.New(w.st, fileConfig(), w.vault, nil).Grant(t.Context(), w.job(t, "developer", config.AdapterClaudeCode))
	if err != nil || g.Endpoint != nil || !maps.Equal(g.Env, map[string]string{"ANTHROPIC_API_KEY": "sk-org"}) {
		t.Fatalf("grant = %+v, %v", g, err)
	}
}

func TestGrantOfTheServerFile(t *testing.T) {
	w := newWorld(t)
	c := fileConfig()
	s := models.New(w.st, c, nil, nil)
	g, err := s.Grant(t.Context(), w.job(t, "developer", config.AdapterClaudeCode))
	if err != nil || g.Connection != "claude" || g.Model != "file-claude" || g.Env["ANTHROPIC_API_KEY"] != "server" {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	g.Env["ANTHROPIC_API_KEY"] = "changed"
	if c.Runtimes["claude"].Credentials["ANTHROPIC_API_KEY"] != "server" {
		t.Fatal("the grant shares the config map")
	}
	if g, err := s.Grant(t.Context(), w.job(t, "reviewer", config.AdapterCodex)); err != nil || g.Env != nil || g.Connection != "codex" {
		t.Fatalf("no credentials = %+v, %v", g, err)
	}
	if g, err := s.Grant(t.Context(), w.job(t, "architect", "other")); err != nil || g.Connection != "" || g.Waiting != "" {
		t.Fatalf("role nobody configured = %+v, %v", g, err)
	}
}

func TestGrantWaits(t *testing.T) {
	w := newWorld(t)
	w.connect(t, conn("acme", config.AdapterClaudeCode, "acme"), "", "developer")
	j := w.job(t, "developer", config.AdapterClaudeCode)
	if g, err := models.New(w.st, fileConfig(), w.vault, nil).Grant(t.Context(), j); err != nil || !strings.Contains(g.Waiting, "no saved key") || g.Env != nil {
		t.Fatalf("no key = %+v, %v", g, err)
	}
	if g, err := models.New(w.st, fileConfig(), nil, nil).Grant(t.Context(), j); err != nil || !strings.Contains(g.Waiting, config.EnvSecretKey) {
		t.Fatalf("no store = %+v, %v", g, err)
	}
	other, err := secrets.New(w.st.Secrets(), append(make([]byte, secrets.KeySize-1), 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.vault.Put(t.Context(), domain.Actor{}, w.account, "models/acme", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if g, err := models.New(w.st, fileConfig(), other, nil).Grant(t.Context(), j); err != nil || !strings.Contains(g.Waiting, "does not read") {
		t.Fatalf("other key = %+v, %v", g, err)
	}
	if g, err := models.New(w.st, fileConfig(), w.vault, nil).Grant(t.Context(), w.job(t, "developer", config.AdapterCodex)); err != nil || !strings.Contains(g.Waiting, "moved") {
		t.Fatalf("job on another adapter = %+v, %v", g, err)
	}
}

type brokenVault struct{ secrets.Store }

var errVault = errors.New("vault down")

func (brokenVault) Get(context.Context, uuid.UUID, string) ([]byte, error) { return nil, errVault }

func TestErrorsArePassedOn(t *testing.T) {
	w := newWorld(t)
	w.connect(t, conn("acme", config.AdapterClaudeCode, "acme"), "k", "developer")
	j := w.job(t, "developer", config.AdapterClaudeCode)
	s := models.New(w.st, fileConfig(), brokenVault{w.vault}, nil)
	if _, err := s.Grant(t.Context(), j); !errors.Is(err, errVault) {
		t.Fatalf("grant = %v", err)
	}
	if err := s.Settle(t.Context(), w.account); !errors.Is(err, errVault) {
		t.Fatalf("settle = %v", err)
	}
	if _, _, _, err := s.WaitReason(t.Context(), j); !errors.Is(err, errVault) {
		t.Fatalf("wait reason = %v", err)
	}
}

func TestRotationAppliesToTheNextGrant(t *testing.T) {
	w := newWorld(t)
	w.connect(t, conn("acme", config.AdapterClaudeCode, "acme"), "old", "developer")
	s := models.New(w.st, fileConfig(), w.vault, nil)
	j := w.job(t, "developer", config.AdapterClaudeCode)
	c := conn("acme", config.AdapterClaudeCode, "acme")
	c.SecretRef = "store:models/acme-2"
	w.connect(t, c, "new", "developer")
	if g, err := s.Grant(t.Context(), j); err != nil || g.Env["ANTHROPIC_API_KEY"] != "new" {
		t.Fatalf("grant = %+v, %v", g, err)
	}
}

func TestSettle(t *testing.T) {
	w := newWorld(t)
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	ctx := t.Context()
	j := w.job(t, "developer", config.AdapterClaudeCode)
	get := func() domain.Job {
		t.Helper()
		got, err := w.st.Jobs().Get(ctx, w.account, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Runtime != domain.RuntimeNone || got.Spec.Runtime != domain.RuntimeNone || got.Spec.Model != "old" {
		t.Fatalf("no connection: job = %+v", got)
	}
	code, sentence, ok, err := s.WaitReason(ctx, get())
	if err != nil || !ok || code != models.ReasonNoConnection || !strings.Contains(sentence, "developer") {
		t.Fatalf("reason = %q %q %v %v", code, sentence, ok, err)
	}

	w.connect(t, conn("acme", config.AdapterCodex, "acme"), "k", "developer")
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Runtime != config.AdapterCodex || got.Spec.Runtime != config.AdapterCodex || got.Spec.Model != "org-model" {
		t.Fatalf("connected: job = %+v", got)
	}
	if _, _, ok, err := s.WaitReason(ctx, get()); err != nil || ok {
		t.Fatalf("still waiting: %v %v", ok, err)
	}
	if err := s.Settle(ctx, domain.NewID()); err != nil {
		t.Fatalf("an empty organization = %v", err)
	}
}

func TestSettleLeavesAnUnconfiguredRole(t *testing.T) {
	w := newWorld(t)
	j := w.job(t, "architect", "other")
	s := models.New(w.st, fileConfig(), w.vault, nil)
	if err := s.Settle(t.Context(), w.account); err != nil {
		t.Fatal(err)
	}
	got, _ := w.st.Jobs().Get(t.Context(), w.account, j.ID)
	if got.Runtime != "other" {
		t.Fatalf("job = %+v", got)
	}
	if _, _, ok, err := s.WaitReason(t.Context(), got); ok || err != nil {
		t.Fatalf("reason: %v %v", ok, err)
	}
}

func TestSeatKeepsItsAdapterThroughAWait(t *testing.T) {
	w := newWorld(t)
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	ctx := t.Context()
	j := domain.Job{AccountID: w.account, ProjectID: w.project.ID, Role: "developer", Runtime: config.AdapterCodex, Pool: domain.PoolBuild, Kind: domain.JobKindAgent,
		Spec: domain.JobSpec{Repo: "acme/app", Runtime: config.AdapterCodex, PinnedRuntime: config.AdapterCodex}}
	if err := w.st.Jobs().Create(ctx, &j); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	w.connect(t, conn("a", config.AdapterClaudeCode, "acme"), "k", "developer")
	c := conn("b", config.AdapterCodex, "acme")
	c.AccountID, c.UpdatedBy = w.account, "user:ana"
	if err := w.st.ModelConnections().Put(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if err := w.vault.Put(ctx, domain.Actor{}, w.account, "models/b", []byte("k")); err != nil {
		t.Fatal(err)
	}
	w.authorize(t, c, uuid.Nil())
	if err := s.Settle(ctx, w.account); err != nil {
		t.Fatal(err)
	}
	got, _ := w.st.Jobs().Get(ctx, w.account, j.ID)
	if got.Runtime != config.AdapterCodex {
		t.Fatalf("seat = %+v", got)
	}
	if g, err := s.Grant(ctx, got); err != nil || g.Connection != "b" {
		t.Fatalf("grant = %+v, %v", g, err)
	}
}

type fixedWaiter struct{}

func (fixedWaiter) WaitReason(context.Context, domain.Job) (string, string, bool, error) {
	return "queue", "next in line", true, nil
}

func TestBefore(t *testing.T) {
	w := newWorld(t)
	s := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	waiter := s.Before(fixedWaiter{})
	if code, _, ok, err := waiter.WaitReason(t.Context(), w.job(t, "developer", domain.RuntimeNone)); err != nil || !ok || code != models.ReasonNoConnection {
		t.Fatalf("connection first: %q %v %v", code, ok, err)
	}
	if code, _, ok, err := waiter.WaitReason(t.Context(), domain.Job{Kind: domain.JobKindCommand}); err != nil || !ok || code != "queue" {
		t.Fatalf("command job: %q %v %v", code, ok, err)
	}
}

func TestModelOnAndUsual(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := models.New(w.st, fileConfig(), w.vault, nil)
	if m, err := s.ModelOn(ctx, w.account, w.project.ID, "developer", config.AdapterCodex); err != nil || m != "file-gpt" {
		t.Fatalf("file seat = %q, %v", m, err)
	}
	if _, err := s.ModelOn(ctx, w.account, w.project.ID, "developer", config.AdapterOpencode); !errors.Is(err, models.ErrNoModel) {
		t.Fatalf("no such adapter = %v", err)
	}
	if a, err := s.Usual(ctx, w.account, w.project.ID, "developer"); err != nil || a != config.AdapterClaudeCode {
		t.Fatalf("usual = %q, %v", a, err)
	}
	h := models.New(w.st, hosted(fileConfig()), w.vault, nil)
	if _, err := h.ModelOn(ctx, w.account, w.project.ID, "developer", config.AdapterCodex); !errors.Is(err, models.ErrNoModel) {
		t.Fatalf("hosted = %v", err)
	}
	if a, err := h.Usual(ctx, w.account, w.project.ID, "developer"); err != nil || a != "" {
		t.Fatalf("hosted usual = %q, %v", a, err)
	}
}

type failingConnections struct{ store.ModelConnectionRepo }

var errStore = errors.New("store down")

func (failingConnections) List(context.Context, uuid.UUID) ([]domain.ModelConnection, error) {
	return nil, errStore
}

type failingStore struct{ *testkit.Store }

func (f failingStore) ModelConnections() store.ModelConnectionRepo {
	return failingConnections{f.Store.ModelConnections()}
}

func TestStoreErrors(t *testing.T) {
	w := newWorld(t)
	w.job(t, "developer", config.AdapterClaudeCode)
	s := models.New(failingStore{w.st}, fileConfig(), nil, nil)
	if _, err := s.Decide(t.Context(), w.account, models.Request{Role: "developer"}); !errors.Is(err, errStore) {
		t.Fatalf("decide = %v", err)
	}
	if _, err := s.Grant(t.Context(), domain.Job{}); !errors.Is(err, errStore) {
		t.Fatalf("grant = %v", err)
	}
	if err := s.Settle(t.Context(), w.account); !errors.Is(err, errStore) {
		t.Fatalf("settle = %v", err)
	}
	if _, _, _, err := s.WaitReason(t.Context(), domain.Job{}); !errors.Is(err, errStore) {
		t.Fatalf("reason = %v", err)
	}
}

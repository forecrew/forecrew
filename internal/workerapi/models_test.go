package workerapi

import (
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/secrets"
)

// modelsEnv is a server whose leases come from internal/models, with a server
// file that has a key of its own.
type modelsEnv struct {
	*env
	vault *secrets.Encrypted
	svc   *models.Service
}

func serverFile(hosted bool) *config.Config {
	c := &config.Config{
		Runtimes: map[string]config.Runtime{"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic",
			CredentialEnv: []string{"ANTHROPIC_API_KEY"}, Credentials: map[string]string{"ANTHROPIC_API_KEY": "server-key"}}},
		ModelTiers: map[config.Tier]map[string]string{config.TierStrong: {"anthropic": "file-model"}},
		Roles:      map[string]config.Role{"developer": {Runtime: "claude", Tier: config.TierStrong}},
		Pools:      config.DefaultPools(),
	}
	if hosted {
		off := false
		c.Models.ServerKeysForOrganizations = &off
	}
	return c
}

func newModelsEnv(t *testing.T, hosted bool) *modelsEnv {
	t.Helper()
	m := &modelsEnv{}
	cfg := serverFile(hosted)
	m.env = newEnv(t, func(c *Config) {
		vault, err := secrets.New(c.Store.Secrets(), make([]byte, secrets.KeySize))
		if err != nil {
			t.Fatal(err)
		}
		m.vault, m.svc = vault, models.New(c.Store, cfg, vault, nil)
		c.Grant, c.Settle, c.Pools = m.svc.Grant, m.svc.Settle, cfg.Pools
	})
	m.jobs.WithWaiting(m.svc)
	return m
}

// connect gives role developer of account a connection called main with key.
func (m *modelsEnv) connect(t *testing.T, account uuid.UUID, ref, key string) {
	t.Helper()
	ctx := t.Context()
	c := domain.ModelConnection{AccountID: account, Name: "main", Vendor: "acme", Adapter: config.AdapterClaudeCode, SecretRef: "store:models/" + ref, Enabled: true, UpdatedBy: "user:ana"}
	if err := m.st.ModelConnections().Put(ctx, &c); err != nil {
		t.Fatal(err)
	}
	if err := m.vault.Put(ctx, domain.Actor{}, account, "models/"+ref, []byte(key)); err != nil {
		t.Fatal(err)
	}
	a := domain.ModelAssignment{AccountID: account, UpdatedBy: "user:ana", Tiers: map[string]map[string]string{"strong": {"acme": "org-model"}},
		Roles: map[string]domain.ModelRole{"developer": {Connection: "main", Tier: "strong"}}}
	if err := m.st.ModelAssignments().Put(ctx, &a); err != nil {
		t.Fatal(err)
	}
}

// organization adds another account with a project and a worker.
func (m *modelsEnv) organization(t *testing.T, slug string) (uuid.UUID, domain.Project, string) {
	t.Helper()
	ctx := t.Context()
	acc := domain.Account{Name: slug, Slug: slug}
	if err := m.st.Accounts().Create(ctx, &acc); err != nil {
		t.Fatal(err)
	}
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: repo, DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := m.st.Projects().Create(ctx, &p); err != nil {
		t.Fatal(err)
	}
	token := "fcw_" + slug + "_" + domain.NewID().String()
	w := domain.Worker{AccountID: acc.ID, Name: "w", Pool: domain.PoolBuild, TokenHash: mcp.HashToken(token), MaxJobs: 1}
	if err := m.st.Workers().Create(ctx, &w); err != nil {
		t.Fatal(err)
	}
	return acc.ID, p, token
}

func (m *modelsEnv) developerJob(t *testing.T, account uuid.UUID, project domain.Project) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: account, ProjectID: project.ID, Pool: domain.PoolBuild, Role: "developer", Runtime: config.AdapterClaudeCode,
		Spec: domain.JobSpec{Repo: repo, Prompt: "do it", Model: "created-with"}}
	if err := m.st.Jobs().Create(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func (m *modelsEnv) lease(t *testing.T, token string) pollResponse {
	t.Helper()
	status, lease := m.poll(t, token)
	if status != http.StatusOK {
		t.Fatalf("poll: status %d", status)
	}
	return lease
}

// TestEachOrganizationGetsItsOwnKey: two organizations with the same role
// and connection name, and a server file with a key too, each lease only
// their own key.
func TestEachOrganizationGetsItsOwnKey(t *testing.T) {
	m := newModelsEnv(t, false)
	b, bProject, bToken := m.organization(t, "beta")
	_, aToken := m.addWorker(t, "a")
	m.connect(t, m.account, "main", "key-alpha")
	m.connect(t, b, "main", "key-beta")
	aJob := m.developerJob(t, m.account, m.project)
	bJob := m.developerJob(t, b, bProject)

	for _, tt := range []struct {
		token, own string
		job        uuid.UUID
	}{{aToken, "key-alpha", aJob.ID}, {bToken, "key-beta", bJob.ID}} {
		lease := m.lease(t, tt.token)
		_, body := m.call(t, tt.token, "/worker/v1/poll", map[string]any{"labels": []string{"runtime:claude-code"}, "free_slots": 1})
		got := lease.Job
		if got.JobID != tt.job || got.Env["ANTHROPIC_API_KEY"] != tt.own || len(got.Env) != 1 || got.Model != "org-model" {
			t.Fatalf("lease = %+v", got)
		}
		for _, other := range []string{"key-alpha", "key-beta", "server-key"} {
			if other != tt.own && (strings.Contains(string(body), other) || strings.Contains(got.Env["ANTHROPIC_API_KEY"], other)) {
				t.Fatalf("%s leased %s", tt.own, other)
			}
		}
	}
	if stored := m.job(t, aJob.ID); stored.Spec.Env != nil {
		t.Fatalf("stored env %v", stored.Spec.Env)
	}
}

// TestHostedJobWaitsForAConnection: with no connection a hosted job waits
// with the reason and never gets the server's key; once connected it runs,
// and a rotated key counts from the next lease.
func TestHostedJobWaitsForAConnection(t *testing.T) {
	m := newModelsEnv(t, true)
	_, token := m.addWorker(t, "a")
	j := m.developerJob(t, m.account, m.project)
	if status, _ := m.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("poll: status %d", status)
	}
	v, err := m.jobs.Status(t.Context(), m.account, j.ID)
	if err != nil || v.Status != domain.JobQueued || v.Runtime != domain.RuntimeNone || v.ReasonCode != models.ReasonNoConnection || !strings.Contains(v.Reason, "developer") {
		t.Fatalf("job = %+v, %v", v, err)
	}
	// A worker naming the runtime of waiting jobs gets nothing either.
	if status, _ := m.poll(t, token, domain.RuntimeLabel(domain.RuntimeNone)); status != http.StatusNoContent {
		t.Fatalf("poll none: status %d", status)
	}

	m.connect(t, m.account, "main", "key-one")
	if got := m.lease(t, token).Job; got.JobID != j.ID || got.Env["ANTHROPIC_API_KEY"] != "key-one" {
		t.Fatalf("lease = %+v", got)
	}
	m.connect(t, m.account, "main-2", "key-two")
	next := m.developerJob(t, m.account, m.project)
	_, other := m.addWorker(t, "b")
	if got := m.lease(t, other).Job; got.JobID != next.ID || got.Env["ANTHROPIC_API_KEY"] != "key-two" {
		t.Fatalf("after rotation = %+v", got)
	}
}

// TestServerFileSetupLeasesAsBefore: with only forecrew.yaml, a job gets the
// file's key and the model it was created with when the role is the file's.
func TestServerFileSetupLeasesAsBefore(t *testing.T) {
	m := newModelsEnv(t, false)
	_, token := m.addWorker(t, "a")
	j := m.developerJob(t, m.account, m.project)
	if got := m.lease(t, token).Job; got.JobID != j.ID || got.Env["ANTHROPIC_API_KEY"] != "server-key" || got.Model != "file-model" || got.Endpoint != nil {
		t.Fatalf("lease = %+v", got)
	}
}

// TestAJobWhoseConnectionWentIsGivenBack: a connection turned off between
// the settling and the lease is not handed out.
func TestAJobWhoseConnectionWentIsGivenBack(t *testing.T) {
	m := newModelsEnv(t, true)
	_, token := m.addWorker(t, "a")
	m.connect(t, m.account, "main", "key")
	j := m.developerJob(t, m.account, m.project)
	srv := New(Config{Store: m.st, Jobs: m.jobs, Tokens: m.tracker, Logger: m.server.cfg.Logger, Grant: m.svc.Grant, PollWait: m.server.cfg.PollWait, PollRecheck: m.server.cfg.PollRecheck})
	c := domain.ModelConnection{AccountID: m.account, Name: "main", Vendor: "acme", Adapter: config.AdapterClaudeCode, SecretRef: "store:models/main", UpdatedBy: "user:ana"}
	if err := m.st.ModelConnections().Put(t.Context(), &c); err != nil {
		t.Fatal(err)
	}
	m.server = srv
	m.http.Config.Handler = srv.Handler()
	if status, _ := m.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("poll: status %d", status)
	}
	if got := m.job(t, j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job = %+v", got)
	}
}

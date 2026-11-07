package workerapi

import (
	"context"
	"errors"
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
		c.Grant, c.CheckGrant, c.Settle, c.Pools = m.svc.Grant, m.svc.CheckGrant, m.svc.Settle, cfg.Pools
	})
	m.jobs.WithWaiting(m.svc)
	return m
}

// connect gives role developer of account a connection called main with key,
// authorized for the code of every project.
func (m *modelsEnv) connect(t *testing.T, account uuid.UUID, ref, key string) {
	t.Helper()
	c := m.connectTo(t, account, ref, key, "")
	m.authorize(t, c, uuid.Nil())
}

// authorize lets c receive the private code of project, or of every project.
func (m *modelsEnv) authorize(t *testing.T, c domain.ModelConnection, project uuid.UUID) {
	t.Helper()
	a := domain.ModelAuthorization{AccountID: c.AccountID, ProjectID: project, ConnectionID: c.ID, Connection: c.Name, Adapter: c.Adapter, BaseURL: c.BaseURL, AuthorizedBy: "user:ana"}
	if err := m.st.ModelAuthorizations().Put(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
}

// connectTo is connect without the authorization, to baseURL.
func (m *modelsEnv) connectTo(t *testing.T, account uuid.UUID, ref, key, baseURL string) domain.ModelConnection {
	t.Helper()
	ctx := t.Context()
	c := domain.ModelConnection{AccountID: account, Name: "main", Vendor: "acme", Adapter: config.AdapterClaudeCode, BaseURL: baseURL, SecretRef: "store:models/" + ref, Enabled: true, UpdatedBy: "user:ana"}
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
	return c
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

// TestAnOlderWorkerGetsNoOrganizationJob: a worker that does not say it keeps
// its own keys away gets no job on an organization's connection, which waits
// for one that does; a job of the server file still goes to it.
func TestAnOlderWorkerGetsNoOrganizationJob(t *testing.T) {
	m := newModelsEnv(t, false)
	_, token := m.addWorker(t, "old")
	m.connect(t, m.account, "main", "key-org")
	j := m.developerJob(t, m.account, m.project)
	old := func(labels ...string) int {
		status, _ := m.call(t, token, "/worker/v1/poll", map[string]any{"labels": append(labels, "runtime:claude-code"), "free_slots": 1, "version": "v0.1.0"})
		return status
	}
	if status := old(); status != http.StatusNoContent {
		t.Fatalf("old worker: status %d", status)
	}
	// It cannot claim the label either.
	if status := old(domain.OrganizationModelLabel); status != http.StatusNoContent {
		t.Fatalf("old worker with the label: status %d", status)
	}
	got := m.job(t, j.ID)
	if got.Status != domain.JobQueued || !got.Spec.OrganizationModel || got.Attempts != 0 {
		t.Fatalf("job = %+v", got)
	}
	// A server-file job still runs there.
	other := domain.Job{AccountID: m.account, ProjectID: m.project.ID, Pool: domain.PoolBuild, Role: "reviewer", Runtime: config.AdapterClaudeCode,
		Spec: domain.JobSpec{Repo: repo, Prompt: "do it"}}
	if err := m.st.Jobs().Create(t.Context(), &other); err != nil {
		t.Fatal(err)
	}
	if status := old(); status != http.StatusOK {
		t.Fatalf("old worker, file job: status %d", status)
	}
	_, current := m.addWorker(t, "new")
	if lease := m.lease(t, current); lease.Job.JobID != j.ID || !lease.Job.OrganizationModel {
		t.Fatalf("lease = %+v", lease.Job)
	}
}

// TestALeaseRaceDoesNotReachAnOlderWorker: a job leased before it was
// settled is given back when the worker lacks the capability.
func TestALeaseRaceDoesNotReachAnOlderWorker(t *testing.T) {
	m := newModelsEnv(t, false)
	w, _ := m.addWorker(t, "old")
	m.connect(t, m.account, "main", "key-org")
	j := m.developerJob(t, m.account, m.project)
	if _, err := m.server.withSecrets(t.Context(), w, j); !errors.Is(err, errNoConnection) {
		t.Fatalf("err = %v", err)
	}
}

// waitsForPrivateCode checks that a job of the project waits for an
// authorization: no worker gets it, and job_status says why.
func (m *modelsEnv) waitsForPrivateCode(t *testing.T, token string, j domain.Job) {
	t.Helper()
	if status, _ := m.poll(t, token); status != http.StatusNoContent {
		t.Fatalf("poll: status %d", status)
	}
	v, err := m.jobs.Status(t.Context(), m.account, j.ID)
	if err != nil || v.Status != domain.JobQueued || v.Runtime != domain.RuntimeNone || v.ReasonCode != models.ReasonPrivateCode || !strings.Contains(v.Reason, "main@app") {
		t.Fatalf("job = %+v, %v", v, err)
	}
}

// TestPrivateCodeNeedsAnAuthorization: a private repo's job does not run on
// an organization's connection that is not authorized for its project, at
// creation and at lease, and never falls back to the server's key; an
// authorization lets it run, and revoking it stops the next job.
func TestPrivateCodeNeedsAnAuthorization(t *testing.T) {
	m := newModelsEnv(t, false)
	ctx := t.Context()
	_, token := m.addWorker(t, "a")
	c := m.connectTo(t, m.account, "main", "key-org", "")

	// At creation, the decision of the job creator waits.
	d, err := m.svc.Decide(ctx, m.account, models.Request{ProjectID: m.project.ID, Role: "developer"})
	if err != nil || d.Waiting == "" || d.Code != models.ReasonPrivateCode || d.Adapter != "" {
		t.Fatalf("decide = %+v, %v", d, err)
	}
	j := m.developerJob(t, m.account, m.project)
	// At lease, the grant refuses whatever the job's runtime says.
	if g, err := m.svc.Grant(ctx, j); err != nil || g.Waiting == "" || g.Env != nil {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	m.waitsForPrivateCode(t, token, j)

	// Another project's authorization does not count.
	other := domain.Project{AccountID: m.account, Name: "web", Repo: "acme/web", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	if err := m.st.Projects().Create(ctx, &other); err != nil {
		t.Fatal(err)
	}
	m.authorize(t, c, other.ID)
	m.waitsForPrivateCode(t, token, j)

	m.authorize(t, c, m.project.ID)
	if got := m.lease(t, token).Job; got.JobID != j.ID || got.Env["ANTHROPIC_API_KEY"] != "key-org" {
		t.Fatalf("lease = %+v", got)
	}

	// Revoking stops new jobs.
	if err := m.st.ModelAuthorizations().Delete(ctx, m.account, c.ID, m.project.ID); err != nil {
		t.Fatal(err)
	}
	_, token2 := m.addWorker(t, "b")
	next := m.developerJob(t, m.account, m.project)
	m.waitsForPrivateCode(t, token2, next)

	// An authorization of another endpoint does not count either.
	m.authorize(t, c, m.project.ID)
	m.connectTo(t, m.account, "main", "key-org", "https://elsewhere.example.com")
	m.waitsForPrivateCode(t, token2, next)
}

// TestALocalConnectionNeedsNoAuthorization: code that stays on your own
// network needs no authorization.
func TestALocalConnectionNeedsNoAuthorization(t *testing.T) {
	m := newModelsEnv(t, false)
	_, token := m.addWorker(t, "a")
	m.connectTo(t, m.account, "main", "key-org", "http://10.1.2.3:8080")
	j := m.developerJob(t, m.account, m.project)
	if got := m.lease(t, token).Job; got.JobID != j.ID || got.Endpoint == nil || got.Endpoint.BaseURL != "http://10.1.2.3:8080" {
		t.Fatalf("lease = %+v", got)
	}
}

// TestAPublicRepoIsNotBlocked: GitHub says the repo is public, so its code
// goes anywhere; when GitHub does not answer, the repo counts as private.
func TestAPublicRepoIsNotBlocked(t *testing.T) {
	m := newModelsEnv(t, false)
	_, token := m.addWorker(t, "a")
	m.connectTo(t, m.account, "main", "key-org", "")
	down := errors.New("github down")
	answer := down
	m.svc.WithPublicRepo(func(_ context.Context, r string) (bool, error) { return r == repo, answer }, m.clock.Now)
	j := m.developerJob(t, m.account, m.project)
	m.waitsForPrivateCode(t, token, j)
	answer = nil
	if got := m.lease(t, token).Job; got.JobID != j.ID {
		t.Fatalf("lease = %+v", got)
	}
}

// pollChecks polls as a control worker that runs model checks, or not.
func (m *modelsEnv) pollChecks(t *testing.T, token string, checks bool) (int, pollResponse) {
	t.Helper()
	caps := []string{domain.OrganizationModelCapability}
	if checks {
		caps = append(caps, domain.ModelCheckCapability)
	}
	status, body := m.call(t, token, "/worker/v1/poll", map[string]any{"labels": []string{"runtime:claude-code", domain.ModelCheckLabel}, "free_slots": 1, "capabilities": caps})
	var out pollResponse
	if status == http.StatusOK {
		decodeBody(t, body, &out)
	}
	return status, out
}

// TestAKeyCheckGetsTheKeyAndItsAddressOnly: a model_check job is leased by a
// control worker that runs checks, with the connection's list address and
// key, and no repo token or job MCP token.
func TestAKeyCheckGetsTheKeyAndItsAddressOnly(t *testing.T) {
	m := newModelsEnv(t, false)
	ctx := t.Context()
	c := m.connectTo(t, m.account, "main", "key-org", "https://gateway.example.com")
	checker := models.NewChecker(m.st, m.jobs, nil)
	row, err := checker.Check(ctx, domain.Actor{AccountID: m.account, Kind: domain.ActorUser, Name: "ana"}, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, old := m.addPoolWorker(t, "old", domain.PoolControl)
	if status, _ := m.pollChecks(t, old, false); status != http.StatusNoContent {
		t.Fatalf("a worker without the capability: %d", status)
	}
	_, token := m.addPoolWorker(t, "ctl", domain.PoolControl)
	status, lease := m.pollChecks(t, token, true)
	got := lease.Job
	if status != http.StatusOK || got.JobID != row.JobID || got.ModelCheck == nil || got.ModelCheck.URL != "https://gateway.example.com/v1/models" ||
		got.Env["ANTHROPIC_API_KEY"] != "key-org" || len(got.Env) != 1 || got.RepoToken != "" || got.JobMCP != nil {
		t.Fatalf("lease = %d %+v", status, got)
	}
}

// TestASuitabilityTestGetsNoRepoToken: a test runs on the fixture with its
// connection's key, and gets no token of its job's repo.
func TestASuitabilityTestGetsNoRepoToken(t *testing.T) {
	m := newModelsEnv(t, false)
	ctx := t.Context()
	c := m.connectTo(t, m.account, "main", "key-org", "")
	checker := models.NewChecker(m.st, m.jobs, nil)
	row, err := checker.Test(ctx, domain.Actor{AccountID: m.account, Kind: domain.ActorUser, Name: "ana"}, c.Name, "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	_, token := m.addPoolWorker(t, "ctl", domain.PoolControl)
	status, lease := m.pollChecks(t, token, true)
	got := lease.Job
	if status != http.StatusOK || got.JobID != row.JobID || got.Suitability == nil || got.RepoToken != "" || got.Env["ANTHROPIC_API_KEY"] != "key-org" || got.JobMCP == nil {
		t.Fatalf("lease = %d %+v", status, got)
	}
}

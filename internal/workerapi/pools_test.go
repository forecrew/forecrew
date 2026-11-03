package workerapi

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// poolConfig has a claude and a codex runtime with credentials, a control
// pool that may only receive codex's and a pool without repo tokens.
func poolConfig() *config.Config {
	pools := config.DefaultPools()
	ctl := pools[domain.PoolControl]
	ctl.Credentials.Runtimes = []string{"codex"}
	pools[domain.PoolControl] = ctl
	pools["offline"] = config.Pool{Roles: []string{config.AnyRole}, Kinds: []domain.JobKind{domain.JobKindAgent},
		Credentials: config.PoolCredentials{Runtimes: []string{config.AnyRole}, RepoToken: config.RepoTokenNone}}
	return &config.Config{
		Runtimes: map[string]config.Runtime{
			"claude": {Adapter: config.AdapterClaudeCode, Credentials: map[string]string{"ANTHROPIC_API_KEY": apiSecret}},
			"codex":  {Adapter: config.AdapterCodex, Credentials: map[string]string{"OPENAI_API_KEY": "sk-openai"}},
		},
		Pools: pools,
	}
}

func withPools(cfg *config.Config) option {
	return func(c *Config) { c.Env, c.Pools = RuntimeEnv(cfg), cfg.Pools }
}

// queue stores a job directly, without the creation policy, as a job that
// was created before the policy changed would be.
func (e *env) queue(t *testing.T, j domain.Job) domain.Job {
	t.Helper()
	j.AccountID, j.ProjectID = e.account, e.project.ID
	j.Spec.Repo, j.Spec.Prompt = repo, "do it"
	if err := e.st.Jobs().Create(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// TestPoolCredentials: a leased job gets only the repo token scope and
// runtime keys its pool allows.
func TestPoolCredentials(t *testing.T) {
	tests := []struct {
		name    string
		pool    string
		job     domain.Job
		token   string // prefix; empty means no token
		wantEnv map[string]string
	}{
		{"build writer", domain.PoolBuild, domain.Job{Role: "developer", Runtime: config.AdapterClaudeCode,
			Spec: domain.JobSpec{WritePaths: []string{"**"}}}, "ghs_fake_write_", map[string]string{"ANTHROPIC_API_KEY": apiSecret}},
		{"control reader with an allowed runtime", domain.PoolControl, domain.Job{Pool: domain.PoolControl, Role: "reviewer", Runtime: config.AdapterCodex},
			"ghs_fake_read_", map[string]string{"OPENAI_API_KEY": "sk-openai"}},
		{"control runtime not allowed", domain.PoolControl, domain.Job{Pool: domain.PoolControl, Role: "reviewer", Runtime: config.AdapterClaudeCode},
			"ghs_fake_read_", nil},
		{"control job with write paths", domain.PoolControl, domain.Job{Pool: domain.PoolControl, Role: "developer", Runtime: config.AdapterCodex,
			Spec: domain.JobSpec{WritePaths: []string{"**"}}}, "ghs_fake_read_", map[string]string{"OPENAI_API_KEY": "sk-openai"}},
		{"pool without repo token", "offline", domain.Job{Pool: "offline", Role: "developer", Runtime: config.AdapterClaudeCode,
			Spec: domain.JobSpec{WritePaths: []string{"**"}}}, "", map[string]string{"ANTHROPIC_API_KEY": apiSecret}},
		{"command job", domain.PoolBuild, domain.Job{Kind: domain.JobKindCommand, Spec: domain.JobSpec{Kind: domain.JobKindCommand,
			WritePaths: []string{"**"}, Command: &domain.CommandSpec{Shell: "make test"}}}, "ghs_fake_read_", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, withPools(poolConfig()))
			_, token := e.addPoolWorker(t, "w", tt.pool)
			j := e.queue(t, tt.job)
			status, lease := e.poll(t, token, "runtime:"+tt.job.Runtime)
			if status != http.StatusOK || lease.Job.JobID != j.ID {
				t.Fatalf("poll: status %d, lease %+v", status, lease)
			}
			got := lease.Job
			if (tt.token == "") != (got.RepoToken == "") || !strings.HasPrefix(got.RepoToken, tt.token) {
				t.Errorf("repo token = %q, want prefix %q", got.RepoToken, tt.token)
			}
			if !maps.Equal(got.Env, tt.wantEnv) {
				t.Errorf("env keys = %v, want %v", slices.Collect(maps.Keys(got.Env)), slices.Collect(maps.Keys(tt.wantEnv)))
			}
		})
	}
}

// TestPoolNotConfigured: a job of a pool the configuration no longer has
// gets no secrets; the lease is released.
func TestPoolNotConfigured(t *testing.T) {
	e := newEnv(t, withPools(poolConfig()))
	_, token := e.addPoolWorker(t, "w", "gone")
	j := e.queue(t, domain.Job{Pool: "gone", Role: "reviewer", Runtime: config.AdapterCodex})
	if status, _ := e.poll(t, token, "runtime:codex"); status != http.StatusInternalServerError {
		t.Fatalf("poll status = %d, want 500", status)
	}
	if got := e.job(t, j.ID); got.Status != domain.JobQueued || got.Attempts != 1 || e.tracker.Tokens != 0 {
		t.Fatalf("job = %+v, tokens minted %d", got, e.tracker.Tokens)
	}
}

// TestPoolLabelIsJustALabel: a build worker that announces pool:control
// still leases only build jobs.
func TestPoolLabelIsJustALabel(t *testing.T) {
	e := newEnv(t)
	w, token := e.addWorker(t, "box")
	ctl := e.queue(t, domain.Job{Pool: domain.PoolControl, Role: "reviewer", Runtime: config.AdapterClaudeCode})
	if status, _ := e.poll(t, token, "pool:control", "pool=control"); status != http.StatusNoContent {
		t.Fatalf("build worker leased a control job: status %d", status)
	}
	if got := e.job(t, ctl.ID); got.Status != domain.JobQueued {
		t.Fatalf("control job = %+v", got)
	}
	if stored, err := e.st.Workers().Get(t.Context(), e.account, w.ID); err != nil || stored.Pool != domain.PoolBuild {
		t.Fatalf("worker = %+v, %v", stored, err)
	}
}

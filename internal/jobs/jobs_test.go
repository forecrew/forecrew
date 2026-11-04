package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	clock   *testkit.Clock
	st      *testkit.Store
	bus     *testkit.SignalBus
	svc     *Service
	logs    *bytes.Buffer
	account uuid.UUID
	project domain.Project
	worker  domain.Worker
}

var system = domain.Actor{Kind: domain.ActorSystem, Name: "test"}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	ctx := t.Context()
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(ctx, &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, st.Projects().Create(ctx, &p))
	w := domain.Worker{AccountID: acc.ID, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, st.Workers().Create(ctx, &w))
	bus := testkit.NewSignalBus(st)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &fixture{
		clock: clock, st: st, bus: bus, svc: New(st, bus, nil, logger, clock.Now), logs: &logs,
		account: acc.ID, project: p, worker: w,
	}
}

func (f *fixture) newJob(t *testing.T, needs ...string) domain.Job {
	t.Helper()
	j, err := f.svc.Create(t.Context(), system, domain.Job{
		AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code", Needs: needs,
		Spec: domain.JobSpec{Repo: "acme/app", BaseRef: "main", Prompt: "go", Env: map[string]string{"K": "secret"}, RepoToken: "ghs_x"},
	})
	must(t, err)
	return j
}

func (f *fixture) lease(t *testing.T, labels ...string) domain.Job {
	t.Helper()
	j, err := f.st.Jobs().LeaseNext(t.Context(), f.account, f.worker.ID, append(labels, "runtime:claude-code"), domain.LeaseDuration)
	must(t, err)
	return j
}

func (f *fixture) signals(t *testing.T, jobID uuid.UUID) []domain.JobStatus {
	t.Helper()
	sigs, err := f.bus.Lookup(t.Context(), f.account, domain.JobKey(jobID))
	must(t, err)
	var out []domain.JobStatus
	for _, s := range sigs {
		p, err := s.Decode()
		must(t, err)
		out = append(out, p.(*domain.JobCompleted).Status)
	}
	return out
}

func (f *fixture) audit(t *testing.T, jobID uuid.UUID) []string {
	t.Helper()
	entries, err := f.st.Audit().List(t.Context(), f.account, store.AuditFilter{SubjectID: jobID})
	must(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Actor+" "+e.Action)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func TestCreateKeepsID(t *testing.T) {
	f := newFixture(t)
	id := domain.NewID()
	j, err := f.svc.Create(t.Context(), system, domain.Job{
		ID: id, AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code",
		Spec: domain.JobSpec{Repo: "acme/app"},
	})
	must(t, err)
	if j.ID != id || j.Spec.JobID != id {
		t.Fatalf("id = %s, spec job id = %s, want %s", j.ID, j.Spec.JobID, id)
	}
	if _, err := f.st.Jobs().Get(t.Context(), f.account, id); err != nil {
		t.Fatal(err)
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	wake := f.svc.Wait()
	j := f.newJob(t, "Go", "go ")
	select {
	case <-wake:
	default:
		t.Fatal("create did not wake waiters")
	}
	got, err := f.st.Jobs().Get(t.Context(), f.account, j.ID)
	must(t, err)
	if got.Status != domain.JobQueued || got.Spec.JobID != j.ID || got.Spec.AccountID != f.account ||
		got.Spec.Role != "developer" || got.Spec.Runtime != "claude-code" || got.Spec.Env != nil || got.Spec.RepoToken != "" {
		t.Fatalf("stored = %+v", got)
	}
	if len(got.Needs) != 1 || got.Needs[0] != "go" {
		t.Fatalf("needs = %v", got.Needs)
	}
	if a := f.audit(t, j.ID); len(a) != 1 || a[0] != "system:test job.created" {
		t.Fatalf("audit = %v", a)
	}

	for _, bad := range []domain.Job{
		{ProjectID: f.project.ID, Role: "r", Runtime: "x", Spec: domain.JobSpec{Repo: "a/b"}},
		{AccountID: f.account, Role: "r", Runtime: "x", Spec: domain.JobSpec{Repo: "a/b"}},
		{AccountID: f.account, ProjectID: f.project.ID, Runtime: "x", Spec: domain.JobSpec{Repo: "a/b"}},
		{AccountID: f.account, ProjectID: f.project.ID, Role: "r", Spec: domain.JobSpec{Repo: "a/b"}},
		{AccountID: f.account, ProjectID: f.project.ID, Role: "r", Runtime: "x"},
	} {
		_, err := f.svc.Create(t.Context(), system, bad)
		wantErr(t, err, ErrInvalid)
	}
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))
	theirs := domain.Project{AccountID: other.ID, Name: "theirs", Repo: "them/app"}
	must(t, f.st.Projects().Create(t.Context(), &theirs))
	_, err = f.svc.Create(t.Context(), system, domain.Job{AccountID: f.account, ProjectID: theirs.ID, Role: "r", Runtime: "x", Spec: domain.JobSpec{Repo: "a/b"}})
	wantErr(t, err, store.ErrNotFound)
}

// TestCreatePools: the pool comes from the job (the role pack) or, for a
// command job, from its environment, and a job the pool's policy refuses is
// not stored.
func TestCreatePools(t *testing.T) {
	pools := config.DefaultPools()
	pools["gpu"] = config.Pool{Roles: []string{"reviewer"}, Kinds: []domain.JobKind{domain.JobKindAgent},
		Credentials: config.PoolCredentials{RepoToken: config.RepoTokenWrite}}
	f := newFixture(t)
	f.svc = New(f.st, f.bus, pools, f.svc.logger, f.clock.Now)
	agent := func(pool, role string, writes ...string) domain.Job {
		return domain.Job{AccountID: f.account, ProjectID: f.project.ID, Pool: pool, Role: role, Runtime: "codex",
			Spec: domain.JobSpec{Repo: "acme/app", WritePaths: writes}}
	}
	command := func(env *domain.Environment, pool string) domain.Job {
		return domain.Job{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Pool: pool,
			Spec: domain.JobSpec{Repo: "acme/app", Command: &domain.CommandSpec{Shell: "make test"}, Environment: env}}
	}
	actions := &domain.Environment{Name: "linux", Provider: domain.ProviderActions}
	docker := &domain.Environment{Name: "go", Provider: domain.ProviderDocker}
	tests := []struct {
		name     string
		job      domain.Job
		wantPool string
		refused  string
	}{
		{"default build", agent("", "developer", "**"), domain.PoolBuild, ""},
		{"control reader", agent(domain.PoolControl, "reviewer"), domain.PoolControl, ""},
		{"listed role", agent("gpu", "reviewer", "docs/**"), "gpu", ""},
		{"command on build", command(docker, ""), domain.PoolBuild, ""},
		{"command pool from environment", command(actions, domain.PoolControl), domain.PoolActions, ""},
		{"command without environment", command(nil, ""), domain.PoolBuild, ""},
		{"writer in control", agent(domain.PoolControl, "developer", "**"), "", "role developer writes to the repo and pool control gets no write token"},
		{"role not listed", agent("gpu", "developer"), "", "pool gpu does not run role developer"},
		{"kind not listed", agent(domain.PoolActions, "reviewer"), "", "pool actions does not run agent jobs"},
		{"unknown pool", agent("nope", "reviewer"), "", "pool nope is not configured"},
	}
	for _, tt := range tests {
		j, err := f.svc.Create(t.Context(), system, tt.job)
		if tt.refused != "" {
			if !errors.Is(err, ErrInvalid) || !errors.Is(err, config.ErrNotAllowed) || !strings.Contains(err.Error(), tt.refused) {
				t.Errorf("%s: err = %v, want %q", tt.name, err, tt.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		got, err := f.st.Jobs().Get(t.Context(), f.account, j.ID)
		must(t, err)
		if got.Pool != tt.wantPool {
			t.Errorf("%s: pool = %q, want %q", tt.name, got.Pool, tt.wantPool)
		}
		if tt.job.Kind == domain.JobKindCommand && (got.Kind != domain.JobKindCommand || got.Spec.Kind != domain.JobKindCommand) {
			t.Errorf("%s: kind = %q, spec kind = %q", tt.name, got.Kind, got.Spec.Kind)
		}
	}
	if list, _ := f.st.Jobs().List(t.Context(), f.account, store.JobFilter{}); len(list) != 6 {
		t.Fatalf("stored %d jobs, want 6", len(list))
	}
}

func TestCancel(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	got, err := f.svc.Cancel(ctx, system, f.account, j.ID)
	must(t, err)
	if got.Status != domain.JobCanceled {
		t.Fatalf("status = %s", got.Status)
	}
	if s := f.signals(t, j.ID); len(s) != 1 || s[0] != domain.JobCanceled {
		t.Fatalf("signals = %v", s)
	}
	if a := f.audit(t, j.ID); len(a) != 2 || a[0] != "system:test job.canceled" {
		t.Fatalf("audit = %v", a)
	}
	_, err = f.svc.Cancel(ctx, system, f.account, j.ID)
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.Cancel(ctx, system, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func schemaJob(t *testing.T, f *fixture) domain.Job {
	t.Helper()
	j, err := f.svc.Create(t.Context(), system, domain.Job{
		AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "claude-code",
		Spec: domain.JobSpec{Repo: "acme/app", OutputSchema: json.RawMessage(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`)},
	})
	must(t, err)
	return j
}

func TestComplete(t *testing.T) {
	tests := []struct {
		name      string
		result    store.JobResult
		wantState domain.JobStatus
		wantError string
	}{
		{"succeeded", store.JobResult{Status: domain.JobSucceeded, Output: json.RawMessage(`{"summary":"ok"}`), HeadSHA: "abc", CostUSD: 1.5}, domain.JobSucceeded, ""},
		{"bad output", store.JobResult{Status: domain.JobSucceeded, Output: json.RawMessage(`{"nope":1}`)}, domain.JobFailed, ErrSchemaMismatch},
		{"missing output", store.JobResult{Status: domain.JobSucceeded}, domain.JobFailed, ErrSchemaMismatch},
		{"failed", store.JobResult{Status: domain.JobFailed, Error: "boom"}, domain.JobFailed, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			j := schemaJob(t, f)
			f.lease(t)
			got, err := f.svc.Complete(ctx, f.worker, j.ID, tt.result)
			must(t, err)
			if got.Status != tt.wantState || got.Error != tt.wantError {
				t.Fatalf("job = %s %q", got.Status, got.Error)
			}
			if s := f.signals(t, j.ID); len(s) != 1 || s[0] != tt.wantState {
				t.Fatalf("signals = %v", s)
			}
			// A second complete changes nothing and publishes nothing new.
			again, err := f.svc.Complete(ctx, f.worker, j.ID, store.JobResult{Status: domain.JobFailed, Error: "late"})
			wantErr(t, err, store.ErrLeaseLost)
			if again.Status != tt.wantState || again.Error != tt.wantError {
				t.Fatalf("after second complete = %s %q", again.Status, again.Error)
			}
			if s := f.signals(t, j.ID); len(s) != 1 {
				t.Fatalf("signals after second complete = %v", s)
			}
			if a := f.audit(t, j.ID); len(a) != 2 || a[0] != "worker:box job.completed" {
				t.Fatalf("audit = %v", a)
			}
		})
	}
}

func TestCompleteErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, err := f.svc.Complete(ctx, f.worker, domain.NewID(), store.JobResult{Status: domain.JobSucceeded})
	wantErr(t, err, store.ErrNotFound)

	queued := f.newJob(t)
	_, err = f.svc.Complete(ctx, f.worker, queued.ID, store.JobResult{Status: domain.JobSucceeded})
	wantErr(t, err, store.ErrLeaseLost)
	if s := f.signals(t, queued.ID); len(s) != 0 {
		t.Fatalf("signals for a queued job = %v", s)
	}

	f.lease(t)
	_, err = f.svc.Complete(ctx, f.worker, queued.ID, store.JobResult{Status: domain.JobQueued})
	wantErr(t, err, domain.ErrInvalidTransition)
}

// failingBus fails every publish.
type failingBus struct{}

func (failingBus) Publish(context.Context, domain.Signal) (bool, error) {
	return false, errors.New("bus down")
}

func TestCompleteRepublishesLostSignal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	f.lease(t)
	broken := New(f.st, failingBus{}, nil, slog.New(slog.DiscardHandler), f.clock.Now)
	if _, err := broken.Complete(ctx, f.worker, j.ID, store.JobResult{Status: domain.JobSucceeded}); err == nil {
		t.Fatal("want publish error")
	}
	if s := f.signals(t, j.ID); len(s) != 0 {
		t.Fatalf("signals = %v", s)
	}
	// The worker retries; the job is already finished, so the retry gets
	// lease lost but the signal is published.
	_, err := f.svc.Complete(ctx, f.worker, j.ID, store.JobResult{Status: domain.JobSucceeded})
	wantErr(t, err, store.ErrLeaseLost)
	if s := f.signals(t, j.ID); len(s) != 1 || s[0] != domain.JobSucceeded {
		t.Fatalf("signals = %v", s)
	}
	_, err = broken.Complete(ctx, f.worker, j.ID, store.JobResult{Status: domain.JobSucceeded})
	if err == nil || errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("err = %v, want the publish error", err)
	}
}

func TestRelease(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	for attempt := 1; attempt <= domain.DefaultMaxAttempts; attempt++ {
		f.lease(t)
		got, err := f.svc.Release(ctx, f.worker, j.ID, "clone failed")
		must(t, err)
		if got.Attempts != attempt {
			t.Fatalf("attempts = %d, want %d", got.Attempts, attempt)
		}
		if attempt < domain.DefaultMaxAttempts {
			if got.Status != domain.JobQueued || !got.RunAfter.Equal(f.clock.Now().Add(domain.RetryBackoff(attempt))) {
				t.Fatalf("released = %s run after %s", got.Status, got.RunAfter)
			}
			f.clock.Advance(domain.RetryBackoff(attempt))
			continue
		}
		if got.Status != domain.JobFailed || got.Error != "attempts exhausted: clone failed" {
			t.Fatalf("last release = %s %q", got.Status, got.Error)
		}
	}
	if s := f.signals(t, j.ID); len(s) != 1 || s[0] != domain.JobFailed {
		t.Fatalf("signals = %v", s)
	}
	_, err := f.svc.Release(ctx, f.worker, j.ID, "again")
	wantErr(t, err, store.ErrLeaseLost)
}

type fakeRegistrar struct {
	jobs []domain.Job
	err  error
}

func (r *fakeRegistrar) JobCompleted(_ context.Context, _ store.Store, j domain.Job) error {
	r.jobs = append(r.jobs, j)
	return r.err
}

// A succeeded job is told to the registrar, in the transaction that finishes
// it: a failing registrar keeps the job running so that the worker retries.
func TestCompleteTellsTheRegistrar(t *testing.T) {
	f := newFixture(t)
	reg := &fakeRegistrar{}
	f.svc.WithRegistrar(reg)
	ctx := t.Context()

	failed := schemaJob(t, f)
	f.lease(t)
	_, err := f.svc.Complete(ctx, f.worker, failed.ID, store.JobResult{Status: domain.JobFailed, Error: "boom"})
	must(t, err)
	if len(reg.jobs) != 0 {
		t.Fatalf("a failed job was registered: %+v", reg.jobs)
	}

	ok := schemaJob(t, f)
	f.lease(t)
	reg.err = errors.New("register down")
	_, err = f.svc.Complete(ctx, f.worker, ok.ID, store.JobResult{Status: domain.JobSucceeded, Output: json.RawMessage(`{"summary":"ok"}`)})
	if err == nil || !strings.Contains(err.Error(), "register down") {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := f.st.Jobs().Get(ctx, f.account, ok.ID); cur.Status != domain.JobRunning {
		t.Fatalf("job = %s, want running", cur.Status)
	}
	reg.err = nil
	got, err := f.svc.Complete(ctx, f.worker, ok.ID, store.JobResult{Status: domain.JobSucceeded, Output: json.RawMessage(`{"summary":"ok"}`)})
	must(t, err)
	if last := reg.jobs[len(reg.jobs)-1]; last.ID != got.ID || last.Status != domain.JobSucceeded || string(last.Output) != `{"summary":"ok"}` {
		t.Fatalf("registrar saw %+v", reg.jobs)
	}
}

package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"
	"uuid"

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
		clock: clock, st: st, bus: bus, svc: New(st, bus, logger, clock.Now), logs: &logs,
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
	broken := New(f.st, failingBus{}, slog.New(slog.DiscardHandler), f.clock.Now)
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

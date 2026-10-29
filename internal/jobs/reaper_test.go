package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

var errDown = errors.New("store down")

// noAccounts is a store whose account list fails.
type noAccounts struct{ store.Store }

func (noAccounts) Accounts() store.AccountRepo { return failingAccounts{} }

type failingAccounts struct{ store.AccountRepo }

func (failingAccounts) List(context.Context) ([]domain.Account, error) { return nil, errDown }

// syncBuffer is a bytes.Buffer safe for a logger and a test reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.buf.Bytes(), []byte(s))
}

func TestReap(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t)
	for attempt := 1; attempt <= domain.DefaultMaxAttempts; attempt++ {
		f.lease(t)
		got, err := f.svc.Reap(ctx)
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("reaped a live lease: %+v", got)
		}
		f.clock.Advance(domain.LeaseDuration + time.Second)
		wake := f.svc.Wait()
		got, err = f.svc.Reap(ctx)
		must(t, err)
		if len(got) != 1 || got[0].ID != j.ID || got[0].Attempts != attempt {
			t.Fatalf("reaped = %+v", got)
		}
		if attempt < domain.DefaultMaxAttempts {
			if got[0].Status != domain.JobQueued {
				t.Fatalf("status = %s", got[0].Status)
			}
			select {
			case <-wake:
			default:
				t.Fatal("requeue did not wake waiters")
			}
			continue
		}
		if got[0].Status != domain.JobFailed || got[0].Error != "attempts exhausted: lease expired" {
			t.Fatalf("last = %s %q", got[0].Status, got[0].Error)
		}
	}
	if s := f.signals(t, j.ID); len(s) != 1 || s[0] != domain.JobFailed {
		t.Fatalf("signals = %v", s)
	}
	if !bytes.Contains(f.logs.Bytes(), []byte("job lease expired")) {
		t.Fatalf("logs = %s", f.logs)
	}
}

func TestReapPublishError(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var failed []domain.Job
	for range 2 {
		j, err := f.svc.Create(ctx, system, domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "r", Runtime: "claude-code", MaxAttempts: 1, Spec: domain.JobSpec{Repo: "a/b"}})
		must(t, err)
		f.lease(t)
		failed = append(failed, j)
	}
	requeued := f.newJob(t)
	f.lease(t)
	f.clock.Advance(domain.LeaseDuration + time.Second)

	var logs syncBuffer
	broken := New(f.st, failingBus{}, slog.New(slog.NewJSONHandler(&logs, nil)), f.clock.Now)
	wake := broken.Wait()
	got, err := broken.Reap(ctx)
	if err == nil {
		t.Fatal("want publish errors")
	}
	if len(got) != 3 {
		t.Fatalf("reaped %d jobs, want every expired one: %+v", len(got), got)
	}
	for _, j := range failed {
		if !strings.Contains(err.Error(), j.Ref()) || !logs.contains(`"ref":"`+j.Ref()+`"`) {
			t.Fatalf("error %v or logs miss %s: %s", err, j.Ref(), logs.buf.String())
		}
		if s := f.job(t, j.ID); s.Status != domain.JobFailed {
			t.Fatalf("%s = %s", j.Ref(), s.Status)
		}
	}
	if s := f.job(t, requeued.ID); s.Status != domain.JobQueued {
		t.Fatalf("%s = %s", requeued.Ref(), s.Status)
	}
	select {
	case <-wake:
	default:
		t.Fatal("reap did not wake waiting polls")
	}
}

// failingRequeue is a store whose RequeueExpired fails.
type failingRequeue struct{ store.Store }

func (s failingRequeue) Jobs() store.JobRepo { return failingRequeueJobs{s.Store.Jobs()} }

type failingRequeueJobs struct{ store.JobRepo }

func (failingRequeueJobs) RequeueExpired(context.Context, uuid.UUID) ([]domain.Job, error) {
	return nil, errDown
}

func TestReapAccountError(t *testing.T) {
	f := newFixture(t)
	broken := New(failingRequeue{f.st}, f.bus, slog.New(slog.DiscardHandler), f.clock.Now)
	_, err := broken.Reap(t.Context())
	wantErr(t, err, errDown)
	_, err = New(noAccounts{f.st}, f.bus, slog.New(slog.DiscardHandler), f.clock.Now).Reap(t.Context())
	wantErr(t, err, errDown)
}

func TestRunReaper(t *testing.T) {
	f := newFixture(t)
	f.newJob(t)
	f.lease(t)
	f.clock.Advance(domain.LeaseDuration + time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	wake := f.svc.Wait()
	go func() {
		f.svc.RunReaper(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not requeue")
	}
	cancel()
	<-done
}

func TestRunReaperLogsErrors(t *testing.T) {
	f := newFixture(t)
	f.newJob(t)
	f.lease(t)
	f.clock.Advance(domain.LeaseDuration + time.Second)
	var logs syncBuffer
	broken := New(noAccounts{f.st}, f.bus, slog.New(slog.NewJSONHandler(&logs, nil)), f.clock.Now)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		broken.RunReaper(ctx, time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !logs.contains("reap expired leases") {
		if time.Now().After(deadline) {
			t.Fatal("reaper did not log its error")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func (f *fixture) job(t *testing.T, id uuid.UUID) domain.Job {
	t.Helper()
	j, err := f.st.Jobs().Get(t.Context(), f.account, id)
	must(t, err)
	return j
}

package knowledge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/testkit"
)

type call struct{ before, head string }

// recordSyncer records its calls and answers with the next scripted result.
type recordSyncer struct {
	mu      sync.Mutex
	calls   []call
	results []syncAnswer
}

type syncAnswer struct {
	res knowledge.SyncResult
	err error
}

func (r *recordSyncer) Sync(_ context.Context, _ domain.Project, before, head string) (knowledge.SyncResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{before, head})
	if len(r.results) == 0 {
		return knowledge.SyncResult{Head: head}, nil
	}
	a := r.results[0]
	r.results = r.results[1:]
	return a.res, a.err
}

func (r *recordSyncer) got() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.calls...)
}

func schedulerEnv(t *testing.T) (*knowledge.Scheduler, *recordSyncer, *testkit.Clock, domain.Project) {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	rec := &recordSyncer{}
	return &knowledge.Scheduler{Syncer: rec, Now: clock.Now}, rec, clock, domain.Project{ID: domain.NewID(), Name: "app"}
}

// Pushes that arrive while a project waits become one sync from the oldest
// previous commit to the newest head, so drift covers all of them.
func TestSchedulerCoalescesPushes(t *testing.T) {
	s, rec, _, p := schedulerEnv(t)
	other := domain.Project{ID: domain.NewID(), Name: "other"}
	s.Request(p, "a", "b")
	s.Request(p, "b", "c")
	s.Request(p, "", "") // the poller knows neither
	s.Request(other, "x", "y")
	if s.Pending() != 2 {
		t.Fatalf("pending = %d", s.Pending())
	}
	s.Drain(t.Context())
	calls := rec.got()
	if len(calls) != 2 || s.Pending() != 0 {
		t.Fatalf("calls = %v, pending = %d", calls, s.Pending())
	}
	for _, c := range calls {
		if c != (call{"a", "c"}) && c != (call{"x", "y"}) {
			t.Errorf("call = %+v", c)
		}
	}
}

// A failed sync waits twice as long each time, whatever requests come in
// meanwhile, and keeps its range.
func TestSchedulerBacksOffOnFailure(t *testing.T) {
	s, rec, clock, p := schedulerEnv(t)
	boom := errors.New("snapshot too large")
	rec.results = []syncAnswer{{err: boom}, {err: boom}}
	s.Request(p, "a", "b")
	s.Drain(t.Context())
	if len(rec.got()) != 1 || s.Pending() != 1 {
		t.Fatalf("calls = %v, pending = %d", rec.got(), s.Pending())
	}
	// The poller asks again every few minutes: that must not retry early.
	s.Request(p, "", "")
	clock.Advance(knowledge.BackoffMin - time.Second)
	s.Drain(t.Context())
	if len(rec.got()) != 1 {
		t.Fatalf("retried before the backoff: %v", rec.got())
	}
	clock.Advance(time.Second)
	s.Drain(t.Context())
	if got := rec.got(); len(got) != 2 || got[1] != (call{"a", "b"}) {
		t.Fatalf("calls = %v", got)
	}
	clock.Advance(2*knowledge.BackoffMin - time.Second)
	s.Drain(t.Context())
	if len(rec.got()) != 2 {
		t.Fatalf("the second wait is twice the first: %v", rec.got())
	}
	clock.Advance(time.Second)
	s.Drain(t.Context()) // succeeds
	s.Drain(t.Context()) // nothing left to do
	if len(rec.got()) != 3 || s.Pending() != 0 {
		t.Errorf("calls = %v, pending = %d", rec.got(), s.Pending())
	}
}

func TestSchedulerBackoffIsCapped(t *testing.T) {
	s, rec, clock, p := schedulerEnv(t)
	boom := errors.New("down")
	for range 14 {
		rec.results = append(rec.results, syncAnswer{err: boom})
	}
	s.Request(p, "", "")
	for range 12 {
		s.Drain(t.Context())
		clock.Advance(knowledge.BackoffMax)
	}
	s.Drain(t.Context())
	n := len(rec.got())
	clock.Advance(knowledge.BackoffMax - time.Second)
	s.Drain(t.Context())
	if len(rec.got()) != n {
		t.Error("retried before BackoffMax")
	}
}

// A sync that stopped at its read limit goes on at once, without drift.
func TestSchedulerContinuesAfterTheReadLimit(t *testing.T) {
	s, rec, _, p := schedulerEnv(t)
	rec.results = []syncAnswer{{res: knowledge.SyncResult{Head: "h2", More: true}}, {res: knowledge.SyncResult{Head: "h2"}}}
	s.Request(p, "h1", "h2")
	s.Drain(t.Context())
	if got := rec.got(); len(got) != 2 || got[0] != (call{"h1", "h2"}) || got[1] != (call{"", "h2"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestSchedulerRunWorksOffTheCallersGoroutine(t *testing.T) {
	s, rec, _, p := schedulerEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	s.Request(p, "a", "b")
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not sync")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

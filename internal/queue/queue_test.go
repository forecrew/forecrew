package queue_test

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestQueueLifecycle(t *testing.T) {
	ctx := t.Context()
	clock := testkit.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st := testkit.NewStore(clock.Now)
	q := queue.New(st.Jobs())
	acc, proj, worker := domain.NewID(), domain.NewID(), domain.NewID()
	labels := []string{"runtime:claude-code"}
	newJob := func() *domain.Job {
		return &domain.Job{AccountID: acc, ProjectID: proj, Role: "dev", Runtime: "claude-code", MaxAttempts: 2}
	}

	j := newJob()
	if err := q.Enqueue(ctx, j); err != nil || j.Number != 1 {
		t.Fatalf("enqueue = %+v, %v", j, err)
	}
	leased, err := q.Lease(ctx, acc, worker, labels)
	if err != nil || leased.ID != j.ID || !leased.LeaseExpiresAt.Equal(clock.Now().Add(domain.LeaseDuration)) {
		t.Fatalf("lease = %+v, %v", leased, err)
	}
	clock.Advance(time.Minute)
	ext, err := q.Extend(ctx, acc, j.ID, worker)
	if err != nil || !ext.LeaseExpiresAt.Equal(clock.Now().Add(domain.LeaseDuration)) {
		t.Fatalf("extend = %v, %v", ext.LeaseExpiresAt, err)
	}
	nacked, err := q.Nack(ctx, acc, j.ID, worker, "disk full")
	if err != nil || nacked.Status != domain.JobQueued || nacked.Attempts != 1 {
		t.Fatalf("nack = %+v, %v", nacked, err)
	}
	if _, err := q.Extend(ctx, acc, j.ID, worker); err == nil {
		t.Fatal("extend after nack succeeded")
	}
	if _, err := q.Lease(ctx, acc, worker, labels); err != nil {
		t.Fatal(err)
	}
	clock.Advance(domain.LeaseDuration)
	expired, err := q.RequeueExpired(ctx, acc)
	if err != nil || len(expired) != 1 || expired[0].Status != domain.JobFailed {
		t.Fatalf("requeue expired = %+v, %v", expired, err)
	}
	if _, err := q.Lease(ctx, acc, worker, labels); err != store.ErrNoJob {
		t.Fatalf("lease on empty queue = %v", err)
	}

	k := newJob()
	if err := q.Enqueue(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Lease(ctx, acc, worker, labels); err != nil {
		t.Fatal(err)
	}
	acked, err := q.Ack(ctx, acc, k.ID, worker, store.JobResult{Status: domain.JobSucceeded})
	if err != nil || acked.Status != domain.JobSucceeded {
		t.Fatalf("ack = %+v, %v", acked, err)
	}
}

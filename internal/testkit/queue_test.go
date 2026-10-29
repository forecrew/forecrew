package testkit

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func TestQueue(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	q := NewQueue(f.st)
	worker := domain.NewID()
	labels := []string{"runtime:claude-code"}

	for range 3 {
		j := newJob(f)
		j.MaxAttempts = 1
		must(t, q.Enqueue(ctx, &j))
	}
	a, err := q.Lease(ctx, f.account, worker, labels)
	must(t, err)
	if !a.LeaseExpiresAt.Equal(t0.Add(domain.LeaseDuration)) {
		t.Fatalf("lease = %v", a.LeaseExpiresAt)
	}
	f.clock.Advance(time.Minute)
	ext, err := q.Extend(ctx, f.account, a.ID, worker)
	if err != nil || !ext.LeaseExpiresAt.Equal(t0.Add(time.Minute+domain.LeaseDuration)) {
		t.Fatalf("extend = %v, %v", ext.LeaseExpiresAt, err)
	}
	acked, err := q.Ack(ctx, f.account, a.ID, worker, store.JobResult{Status: domain.JobFailed, Error: "tests failed"})
	if err != nil || acked.Status != domain.JobFailed || acked.Error != "tests failed" {
		t.Fatalf("ack = %+v, %v", acked, err)
	}

	b, err := q.Lease(ctx, f.account, worker, labels)
	must(t, err)
	nacked, err := q.Nack(ctx, f.account, b.ID, worker, "disk full")
	if err != nil || nacked.Status != domain.JobFailed {
		t.Fatalf("nack with one attempt = %+v, %v", nacked, err)
	}

	c, err := q.Lease(ctx, f.account, worker, labels)
	must(t, err)
	f.clock.Advance(domain.LeaseDuration)
	expired, err := q.RequeueExpired(ctx, f.account)
	if err != nil || len(expired) != 1 || expired[0].ID != c.ID {
		t.Fatalf("expired = %+v, %v", expired, err)
	}
	_, err = q.Lease(ctx, f.account, worker, labels)
	wantErr(t, err, store.ErrNoJob)
}

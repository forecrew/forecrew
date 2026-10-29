package testkit

import (
	"context"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

var _ queue.Queue = (*Queue)(nil)

// Queue is a queue.Queue on top of a store's job repository, with leases of
// domain.LeaseDuration.
type Queue struct {
	jobs store.JobRepo
}

// NewQueue returns a queue that keeps its jobs in st.
func NewQueue(st store.Store) *Queue {
	return &Queue{jobs: st.Jobs()}
}

// Enqueue implements queue.Queue.
func (q *Queue) Enqueue(ctx context.Context, j *domain.Job) error {
	return q.jobs.Create(ctx, j)
}

// Lease implements queue.Queue.
func (q *Queue) Lease(ctx context.Context, accountID, workerID uuid.UUID, labels []string) (domain.Job, error) {
	return q.jobs.LeaseNext(ctx, accountID, workerID, labels, domain.LeaseDuration)
}

// Extend implements queue.Queue.
func (q *Queue) Extend(ctx context.Context, accountID, jobID, workerID uuid.UUID) (domain.Job, error) {
	return q.jobs.ExtendLease(ctx, accountID, jobID, workerID, domain.LeaseDuration)
}

// Ack implements queue.Queue.
func (q *Queue) Ack(ctx context.Context, accountID, jobID, workerID uuid.UUID, r store.JobResult) (domain.Job, error) {
	return q.jobs.Complete(ctx, accountID, jobID, workerID, r)
}

// Nack implements queue.Queue.
func (q *Queue) Nack(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error) {
	return q.jobs.Release(ctx, accountID, jobID, workerID, reason)
}

// RequeueExpired implements queue.Queue.
func (q *Queue) RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error) {
	return q.jobs.RequeueExpired(ctx, accountID)
}

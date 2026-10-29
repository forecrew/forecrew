// Package queue defines the work queue that leases jobs to workers. The
// Postgres implementation sits on store.JobRepo (SKIP LOCKED leasing) with a
// fixed lease of domain.LeaseDuration; internal/testkit has a fake.
package queue

import (
	"context"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Queue errors are the store's: store.ErrNoJob when Lease finds nothing,
// store.ErrLeaseLost when the caller no longer holds the lease, and
// store.ErrNotFound for unknown jobs.

// Queue leases jobs to workers by label.
type Queue interface {
	// Enqueue stores a new queued job (see store.JobRepo.Create).
	Enqueue(ctx context.Context, j *domain.Job) error
	// Lease leases the oldest queued job whose required labels are all among
	// labels, for domain.LeaseDuration.
	Lease(ctx context.Context, accountID, workerID uuid.UUID, labels []string) (domain.Job, error)
	// Extend renews the lease for another domain.LeaseDuration. A returned
	// job with Status canceled tells the worker to stop.
	Extend(ctx context.Context, accountID, jobID, workerID uuid.UUID) (domain.Job, error)
	// Ack finishes the job with the worker's result (succeeded or failed).
	Ack(ctx context.Context, accountID, jobID, workerID uuid.UUID, r store.JobResult) (domain.Job, error)
	// Nack gives up the attempt: queued again with Attempts+1, held back by
	// domain.RetryBackoff, or failed when attempts are exhausted.
	Nack(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error)
	// RequeueExpired handles every expired lease of the account like Nack,
	// but without the backoff.
	RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error)
}

// New returns the Queue that leases from jobs for domain.LeaseDuration. With
// the Postgres job repository it is the production queue; with the testkit
// store it is the fake.
func New(jobs store.JobRepo) Queue { return jobQueue{jobs} }

// jobQueue is a Queue on top of a store.JobRepo, which owns the SKIP LOCKED
// leasing and the lease timing.
type jobQueue struct{ jobs store.JobRepo }

func (q jobQueue) Enqueue(ctx context.Context, j *domain.Job) error { return q.jobs.Create(ctx, j) }

func (q jobQueue) Lease(ctx context.Context, accountID, workerID uuid.UUID, labels []string) (domain.Job, error) {
	return q.jobs.LeaseNext(ctx, accountID, workerID, labels, domain.LeaseDuration)
}

func (q jobQueue) Extend(ctx context.Context, accountID, jobID, workerID uuid.UUID) (domain.Job, error) {
	return q.jobs.ExtendLease(ctx, accountID, jobID, workerID, domain.LeaseDuration)
}

func (q jobQueue) Ack(ctx context.Context, accountID, jobID, workerID uuid.UUID, r store.JobResult) (domain.Job, error) {
	return q.jobs.Complete(ctx, accountID, jobID, workerID, r)
}

func (q jobQueue) Nack(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error) {
	return q.jobs.Release(ctx, accountID, jobID, workerID, reason)
}

func (q jobQueue) RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error) {
	return q.jobs.RequeueExpired(ctx, accountID)
}

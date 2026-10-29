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
	// Nack gives up the attempt: queued again with Attempts+1, or failed when
	// attempts are exhausted.
	Nack(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error)
	// RequeueExpired handles every expired lease of the account like Nack.
	RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error)
}

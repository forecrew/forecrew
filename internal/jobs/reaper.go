package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

// ReapInterval is how often RunReaper looks for expired leases.
const ReapInterval = 30 * time.Second

// Reap requeues the running jobs of every account whose lease expired: back
// to queued with one more attempt, or failed when attempts are exhausted,
// which publishes job.completed. It returns the jobs it changed. An error
// of one account or one signal does not stop the others; each is logged and
// all are returned joined.
func (s *Service) Reap(ctx context.Context) ([]domain.Job, error) {
	accounts, err := s.st.Accounts().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("reap: list accounts: %w", err)
	}
	// Wake waiting polls whatever happens: some jobs may be queued again.
	defer s.notify()
	var out []domain.Job
	var errs []error
	for _, a := range accounts {
		jobs, err := queue.New(s.st.Jobs()).RequeueExpired(ctx, a.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "requeue expired jobs", "account_id", a.ID, "error", err)
			errs = append(errs, fmt.Errorf("reap account %s: %w", a.ID, err))
			continue
		}
		for _, j := range jobs {
			s.logger.WarnContext(ctx, "job lease expired", append(logAttrs(j), "status", j.Status, "attempts", j.Attempts)...)
			out = append(out, j)
			if !j.Status.Terminal() {
				continue
			}
			// RequeueExpired changed the job outside a transaction of ours: tell
			// the registrars in one, so its proposals do not stay queued.
			if err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error { return s.failedJob(ctx, tx, j) }); err != nil {
				s.logger.ErrorContext(ctx, "settle a failed job", append(logAttrs(j), "error", err)...)
				errs = append(errs, err)
			}
			if err := s.completed(ctx, j); err != nil {
				s.logger.ErrorContext(ctx, "publish job completed", append(logAttrs(j), "error", err)...)
				errs = append(errs, err)
			}
		}
	}
	return out, errors.Join(errs...)
}

// RunReaper calls Reap every interval until ctx ends. Errors are logged.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Reap(ctx); err != nil && ctx.Err() == nil {
				s.logger.ErrorContext(ctx, "reap expired leases", "error", err)
			}
		}
	}
}

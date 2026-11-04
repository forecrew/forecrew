package actionsprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
)

// endTimeout bounds the calls that end a job, which run even when the
// provider is shutting down.
const endTimeout = 30 * time.Second

// complete ends the job with res.
func (r *jobRun) complete(ctx context.Context, res store.JobResult) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	j, err := r.p.cfg.Jobs.Complete(ctx, r.w, r.j.ID, res)
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		r.log.WarnContext(ctx, "job ended before its result was stored")
	case err != nil:
		r.log.ErrorContext(ctx, "complete job", "error", err)
	default:
		r.log.InfoContext(ctx, "actions job completed", "status", j.Status, "test_runs", len(res.TestRuns))
	}
}

// fail ends the job as failed.
func (r *jobRun) fail(ctx context.Context, format string, args ...any) {
	r.complete(ctx, store.JobResult{Status: domain.JobFailed, Error: testreport.CleanText(fmt.Sprintf(format, args...))})
}

// release gives the attempt up: the job is queued again, or failed when its
// attempts are used.
func (r *jobRun) release(ctx context.Context, format string, args ...any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	reason := testreport.CleanText(fmt.Sprintf(format, args...))
	if _, err := r.p.cfg.Jobs.Release(ctx, r.w, r.j.ID, reason); err != nil && !errors.Is(err, store.ErrLeaseLost) {
		r.log.ErrorContext(ctx, "release job", "error", err)
	}
}

// event adds a line to the job log; a failure is only logged.
func (r *jobRun) event(ctx context.Context, typ domain.JobEventType, text string) {
	ev := domain.JobEvent{Time: r.p.cfg.Now(), Type: typ, Text: testreport.CleanText(text)}
	if _, err := r.p.cfg.Store.JobEvents().AppendLeased(ctx, r.j.AccountID, r.j.ID, r.w.ID, []domain.JobEvent{ev}); err != nil && ctx.Err() == nil {
		r.log.WarnContext(ctx, "append job event", "error", err)
	}
}

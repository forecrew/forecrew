package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errShutdown is the cause of jobs canceled when the grace period ends.
var errShutdown = errors.New("worker shutting down")

// Run polls for jobs and runs them, up to MaxJobs at once, until ctx is done
// or the server rejects the worker token. On shutdown it stops polling, lets
// running jobs finish for the grace period and then cancels them; their
// leases expire and forecrewd queues the jobs again.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.cleanWorkDir(); err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelRun(nil)
	var jobs sync.WaitGroup

	err := w.pollLoop(ctx, runCtx, &jobs)
	if err != nil {
		cancelRun(err) // without a valid token the jobs cannot report
	}
	done := make(chan struct{})
	go func() { jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.GracePeriod):
		w.cfg.Logger.Warn("grace period over, canceling running jobs", "grace_period", w.cfg.GracePeriod.String())
		cancelRun(errShutdown)
		<-done
	}
	return err
}

// pollLoop leases jobs while slots are free until ctx is done (nil) or the
// token is rejected.
func (w *Worker) pollLoop(ctx, runCtx context.Context, jobs *sync.WaitGroup) error {
	slots := make(chan struct{}, w.cfg.MaxJobs)
	failures := 0
	for {
		// Reserve a slot first; a worker with all slots busy does not poll.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		free := cap(slots) - len(slots) + 1
		lease, err := w.cfg.Client.Poll(ctx, w.cfg.Labels, free, w.cfg.Version)
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrUnauthorized) {
				return fmt.Errorf("the server rejected the worker token, it was probably revoked: %w", err)
			}
			failures++
			wait := w.cfg.Client.backoff(failures)
			w.cfg.Logger.Warn("poll failed, retrying", "error", err, "retry_in", wait.String())
			if sleep(ctx, wait) != nil {
				return nil
			}
			continue
		}
		failures = 0
		if lease == nil {
			<-slots
			continue
		}
		jobs.Go(func() {
			defer func() { <-slots }()
			w.handleJob(runCtx, lease)
		})
	}
}

// cleanWorkDir creates work_dir and removes the job workspaces a crashed
// worker left behind.
func (w *Worker) cleanWorkDir() error {
	if err := os.RemoveAll(filepath.Join(w.cfg.WorkDir, "jobs")); err != nil {
		return fmt.Errorf("clean work dir: %w", err)
	}
	if err := os.MkdirAll(w.cfg.WorkDir, 0o750); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	return nil
}

package jobs

import (
	"context"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ReasonNoWorker is the reason of a queued job that no online worker can run.
const ReasonNoWorker = "no matching worker"

// Log page sizes of Logs.
const (
	DefaultLogLimit = 200
	MaxLogLimit     = 1000
)

// JobView is a job with what a human needs to know about its state.
type JobView struct {
	domain.Job
	// Reason explains the state: ReasonNoWorker for a queued job that no
	// online worker has all the labels for, the error of a failed job.
	Reason string `json:"reason,omitempty"`
	// WorkerName is the worker running the job.
	WorkerName string `json:"worker_name,omitempty"`
}

// Logs is a page of job events.
type Logs struct {
	Events []domain.JobEvent `json:"events"`
	// NextSeq is the afterSeq of the next call.
	NextSeq int64 `json:"next_seq"`
}

// WorkerView is a worker with its online state and running jobs.
type WorkerView struct {
	domain.Worker
	// Online means the worker polled or heartbeated within
	// domain.OnlineWindow and is not revoked.
	Online bool `json:"online"`
	// RunningJobs holds the refs of the jobs it runs, oldest first.
	RunningJobs []string `json:"running_jobs"`
}

// Status returns a job with its reason and worker name.
func (s *Service) Status(ctx context.Context, accountID, jobID uuid.UUID) (JobView, error) {
	j, err := s.st.Jobs().Get(ctx, accountID, jobID)
	if err != nil {
		return JobView{}, fmt.Errorf("job %s: %w", jobID, err)
	}
	v := JobView{Job: j}
	switch j.Status {
	case domain.JobQueued:
		workers, err := s.st.Workers().List(ctx, accountID)
		if err != nil {
			return JobView{}, fmt.Errorf("job %s: list workers: %w", j.Ref(), err)
		}
		if !s.anyCanRun(j, workers) {
			v.Reason = ReasonNoWorker
		}
	case domain.JobRunning:
		w, err := s.st.Workers().Get(ctx, accountID, j.WorkerID)
		if err != nil {
			return JobView{}, fmt.Errorf("job %s: worker: %w", j.Ref(), err)
		}
		v.WorkerName = w.Name
	case domain.JobFailed:
		v.Reason = j.Error
	}
	return v, nil
}

func (s *Service) anyCanRun(j domain.Job, workers []domain.Worker) bool {
	now := s.now()
	for _, w := range workers {
		if w.Online(now) && domain.MatchLabels(j.RequiredLabels(), w.Labels) {
			return true
		}
	}
	return false
}

// Logs returns up to limit events of a job after the cursor afterSeq.
// limit defaults to DefaultLogLimit and is capped at MaxLogLimit.
func (s *Service) Logs(ctx context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) (Logs, error) {
	if _, err := s.st.Jobs().Get(ctx, accountID, jobID); err != nil {
		return Logs{}, fmt.Errorf("job %s: %w", jobID, err)
	}
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	limit = min(limit, MaxLogLimit)
	events, err := s.st.JobEvents().List(ctx, accountID, jobID, afterSeq, limit)
	if err != nil {
		return Logs{}, fmt.Errorf("job %s logs: %w", jobID, err)
	}
	out := Logs{Events: events, NextSeq: afterSeq}
	if n := len(events); n > 0 {
		out.NextSeq = events[n-1].Seq
	}
	return out, nil
}

// Workers returns the workers of an account with their online state and
// running jobs.
func (s *Service) Workers(ctx context.Context, accountID uuid.UUID) ([]WorkerView, error) {
	workers, err := s.st.Workers().List(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	running, err := s.st.Jobs().List(ctx, accountID, store.JobFilter{Status: domain.JobRunning})
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	byWorker := map[uuid.UUID][]string{}
	for _, j := range slices.Backward(running) { // List is newest first
		byWorker[j.WorkerID] = append(byWorker[j.WorkerID], j.Ref())
	}
	now := s.now()
	out := make([]WorkerView, 0, len(workers))
	for _, w := range workers {
		refs := byWorker[w.ID]
		if refs == nil {
			refs = []string{}
		}
		out = append(out, WorkerView{Worker: w, Online: w.Online(now), RunningJobs: refs})
	}
	return out, nil
}

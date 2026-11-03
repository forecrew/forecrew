package workerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

type pollRequest struct {
	Labels    []string `json:"labels"`
	FreeSlots int      `json:"free_slots"`
	Version   string   `json:"version"`
}

type pollResponse struct {
	Job               domain.JobSpec `json:"job"`
	LeaseExpiresAt    time.Time      `json:"lease_expires_at"`
	HeartbeatInterval int            `json:"heartbeat_interval_seconds"`
}

func (s *Server) poll(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w, logger := s.worker(r)
	var req pollRequest
	if !decode(rw, r, &req, false) {
		return
	}
	if req.FreeSlots < 0 {
		writeError(rw, http.StatusBadRequest, "bad_request", "free_slots must not be negative")
		return
	}
	labels := domain.NormalizeLabels(req.Labels)
	if err := s.cfg.Store.Workers().Seen(ctx, w.AccountID, w.ID, labels, req.Version); err != nil {
		fail(ctx, rw, logger, "record worker seen", err)
		return
	}
	if req.FreeSlots == 0 {
		rw.WriteHeader(http.StatusNoContent)
		return
	}
	j, err := s.waitForJob(ctx, w, labels)
	if err != nil {
		if errors.Is(err, store.ErrNoJob) || ctx.Err() != nil {
			// No job, the worker went away or forecrewd is shutting down.
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		fail(ctx, rw, logger, "lease job", err)
		return
	}
	logger = logger.With("job_id", j.ID, "ref", j.Ref(), "project_id", j.ProjectID)
	spec, err := s.handOut(ctx, w, j)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		s.giveBack(ctx, w, j, "worker went away before receiving the job", logger)
		rw.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, errRevoked):
		s.giveBack(ctx, w, j, "worker revoked", logger)
		writeError(rw, http.StatusUnauthorized, "unauthorized", "unknown or revoked worker token")
		return
	default:
		s.giveBack(ctx, w, j, "lease: "+err.Error(), logger)
		fail(ctx, rw, logger, "complete job spec", err)
		return
	}
	logger.InfoContext(ctx, "job leased", "spec", spec, "lease_expires_at", j.LeaseExpiresAt)
	writeJSON(rw, http.StatusOK, pollResponse{
		Job: spec, LeaseExpiresAt: j.LeaseExpiresAt, HeartbeatInterval: int(HeartbeatInterval / time.Second),
	})
}

// waitForJob leases a job for w, waiting up to PollWait for one. It returns
// store.ErrNoJob when none came.
func (s *Server) waitForJob(ctx context.Context, w domain.Worker, labels []string) (domain.Job, error) {
	deadline := time.NewTimer(s.cfg.PollWait)
	defer deadline.Stop()
	recheck := time.NewTicker(s.cfg.PollRecheck)
	defer recheck.Stop()
	q := queue.New(s.cfg.Store.Jobs())
	for {
		wake := s.cfg.Jobs.Wait() // before leasing, so no new job is missed
		j, err := q.Lease(ctx, w.AccountID, w.ID, labels)
		if !errors.Is(err, store.ErrNoJob) {
			return j, err
		}
		select {
		case <-wake:
		case <-recheck.C:
		case <-deadline.C:
			return domain.Job{}, store.ErrNoJob
		case <-ctx.Done():
			return domain.Job{}, ctx.Err()
		}
	}
}

// errRevoked means the worker was revoked while its poll waited.
var errRevoked = errors.New("worker revoked")

// handOut checks that the worker may still receive the leased job j and
// returns its spec with secrets.
func (s *Server) handOut(ctx context.Context, w domain.Worker, j domain.Job) (domain.JobSpec, error) {
	if err := ctx.Err(); err != nil {
		return domain.JobSpec{}, err
	}
	cur, err := s.cfg.Store.Workers().Get(ctx, w.AccountID, w.ID)
	if err != nil {
		return domain.JobSpec{}, fmt.Errorf("check worker: %w", err)
	}
	if cur.Revoked() {
		return domain.JobSpec{}, errRevoked
	}
	return s.withSecrets(ctx, j)
}

// giveBack releases a leased job the worker will not run, so another poll
// can lease it. It works even when the request was canceled.
func (s *Server) giveBack(ctx context.Context, w domain.Worker, j domain.Job, reason string, logger *slog.Logger) {
	ctx = context.WithoutCancel(ctx)
	if _, err := s.cfg.Jobs.Release(ctx, w, j.ID, reason); err != nil {
		logger.ErrorContext(ctx, "release job", "error", err)
		return
	}
	logger.WarnContext(ctx, "job released before hand-out", "reason", reason)
}

// withSecrets returns the job's spec with the secrets its pool allows
// (config.md, pools): a fresh repo token (contents write for writing agent
// jobs, read otherwise, never wider than the pool's repo_token, none for
// none) and the runtime secrets when the pool lists the runtime. Command
// jobs get no runtime secrets.
func (s *Server) withSecrets(ctx context.Context, j domain.Job) (domain.JobSpec, error) {
	spec := j.Spec
	spec.JobID, spec.AccountID = j.ID, j.AccountID
	pool, ok := s.cfg.Pools[j.Pool]
	if !ok {
		return spec, fmt.Errorf("pool %s is not configured", j.Pool)
	}
	if access, ok := repoAccess(spec, pool.Credentials.RepoToken); ok {
		tok, err := s.cfg.Tokens.InstallationToken(ctx, spec.Repo, access)
		if err != nil {
			return spec, fmt.Errorf("repo token for %s: %w", spec.Repo, err)
		}
		spec.RepoToken = tok.Value
	}
	if !spec.IsCommand() {
		if runtime, env := s.cfg.Env(j); pool.Credentials.AllowsRuntime(runtime) {
			spec.Env = env
		}
	}
	return spec, nil
}

// repoAccess returns the repo token access of a job in a pool whose widest
// scope is scope; false means no token.
func repoAccess(spec domain.JobSpec, scope string) (github.TokenAccess, bool) {
	switch scope {
	case config.RepoTokenWrite:
		if len(spec.WritePaths) > 0 && !spec.IsCommand() {
			return github.AccessWrite, true
		}
		return github.AccessRead, true
	case config.RepoTokenRead:
		return github.AccessRead, true
	}
	return "", false
}

// jobID parses the job_id path value; an invalid one answers 404.
func jobID(rw http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("job_id"))
	if err != nil {
		writeError(rw, http.StatusNotFound, "not_found", "job not found")
		return uuid.Nil(), false
	}
	return id, true
}

// Heartbeat actions.
const (
	actionContinue = "continue"
	actionCancel   = "cancel"
)

type heartbeatResponse struct {
	Action         string    `json:"action"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitzero"`
}

func (s *Server) heartbeat(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w, logger := s.worker(r)
	id, ok := jobID(rw, r)
	if !ok {
		return
	}
	var body struct{}
	if !decode(rw, r, &body, true) {
		return
	}
	if err := s.cfg.Store.Workers().Seen(ctx, w.AccountID, w.ID, w.Labels, w.Version); err != nil {
		fail(ctx, rw, logger, "record worker seen", err)
		return
	}
	j, err := queue.New(s.cfg.Store.Jobs()).Extend(ctx, w.AccountID, id, w.ID)
	switch {
	case err == nil:
		writeJSON(rw, http.StatusOK, heartbeatResponse{Action: actionContinue, LeaseExpiresAt: j.LeaseExpiresAt})
	case errors.Is(err, store.ErrLeaseLost) && j.Status == domain.JobRunning:
		// Leased to another worker: refuse rather than tell it to stop.
		fail(ctx, rw, logger, "heartbeat", err)
	case errors.Is(err, store.ErrLeaseLost):
		// Canceled, finished, or expired and requeued: stop.
		logger.InfoContext(ctx, "job lease ended, telling worker to stop", "job_id", j.ID, "ref", j.Ref(), "status", j.Status)
		writeJSON(rw, http.StatusOK, heartbeatResponse{Action: actionCancel})
	default:
		fail(ctx, rw, logger, "heartbeat", err)
	}
}

type eventsRequest struct {
	Events []domain.JobEvent `json:"events"`
}

func (s *Server) events(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w, logger := s.worker(r)
	id, ok := jobID(rw, r)
	if !ok {
		return
	}
	var req eventsRequest
	if !decode(rw, r, &req, false) {
		return
	}
	if msg := checkEvents(req.Events); msg != "" {
		writeError(rw, http.StatusBadRequest, "bad_request", msg)
		return
	}
	_, err := s.leasedJob(ctx, w, id)
	if err == nil && len(req.Events) > 0 {
		_, err = s.cfg.Store.JobEvents().Append(ctx, w.AccountID, id, req.Events)
	}
	if err != nil {
		fail(ctx, rw, logger, "append job events", err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// checkEvents returns why a batch of events is invalid, or "".
func checkEvents(events []domain.JobEvent) string {
	if len(events) > MaxEvents {
		return fmt.Sprintf("at most %d events per request", MaxEvents)
	}
	types := domain.JobEventTypes()
	for i, e := range events {
		if !slices.Contains(types, e.Type) {
			return fmt.Sprintf("events[%d]: unknown type %q", i, e.Type)
		}
		if e.Time.IsZero() {
			return fmt.Sprintf("events[%d]: missing time", i)
		}
	}
	return ""
}

// Complete statuses a worker reports.
const (
	completeSucceeded = "succeeded"
	completeFailed    = "failed"
	completeRetry     = "retry"
)

type completeRequest struct {
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	HeadSHA string          `json:"head_sha"`
	Error   string          `json:"error"`
	Usage   domain.Usage    `json:"usage"`
	CostUSD float64         `json:"cost_usd"`
}

type completeResponse struct {
	Status domain.JobStatus `json:"status"`
}

func (s *Server) complete(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w, logger := s.worker(r)
	id, ok := jobID(rw, r)
	if !ok {
		return
	}
	var req completeRequest
	if !decode(rw, r, &req, false) {
		return
	}
	if string(req.Output) == "null" {
		req.Output = nil
	}
	var j domain.Job
	var err error
	switch {
	case req.Status != completeSucceeded && req.Status != completeFailed && req.Status != completeRetry:
		writeError(rw, http.StatusBadRequest, "bad_request", fmt.Sprintf("status must be succeeded, failed or retry, got %q", req.Status))
		return
	case req.Status != completeSucceeded && req.Error == "":
		writeError(rw, http.StatusBadRequest, "bad_request", "error is required for status "+req.Status)
		return
	case req.Status == completeRetry:
		j, err = s.cfg.Jobs.Release(ctx, w, id, req.Error)
	default:
		j, err = s.cfg.Jobs.Complete(ctx, w, id, store.JobResult{
			Status: domain.JobStatus(req.Status), Output: req.Output, HeadSHA: req.HeadSHA,
			Error: req.Error, Usage: req.Usage, CostUSD: req.CostUSD,
		})
	}
	if err != nil {
		fail(ctx, rw, logger.With("job_id", id), "complete job", err)
		return
	}
	writeJSON(rw, http.StatusOK, completeResponse{Status: j.Status})
}

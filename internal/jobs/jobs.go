// Package jobs is the job service of forecrewd: it creates and cancels
// jobs, finishes them with a worker's result, requeues jobs whose lease
// expired, and serves the job and worker read models. Every terminal state
// publishes a job.completed signal.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrInvalid means a job request misses a required field or its pool does
// not allow it (then it also wraps config.ErrNotAllowed).
var ErrInvalid = errors.New("invalid job")

// ErrSchemaMismatch is the error stored on a job whose output does not match
// its spec's output schema.
const ErrSchemaMismatch = "output does not match schema"

// Publisher publishes signals; workflow.SignalBus implements it.
type Publisher interface {
	Publish(ctx context.Context, s domain.Signal) (bool, error)
}

// Registrar captures what a finished job states: the assumption register
// registers the assumptions in its output.
type Registrar interface {
	// JobCompleted is called in the transaction that finishes j, with the job
	// as stored. An error fails the completion, which the worker retries.
	JobCompleted(ctx context.Context, tx store.Store, j domain.Job) error
}

// Service runs the job lifecycle. Safe for concurrent use.
type Service struct {
	st     store.Store
	bus    Publisher
	reg    Registrar
	pools  map[string]config.Pool
	logger *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	wake chan struct{} // closed and replaced when a job may have become leasable
}

// New returns a service on st that publishes signals on bus. pools is the
// pool policy jobs are created under (config.Config.Pools); nil means
// config.DefaultPools. now is the clock of the read models (online workers).
func New(st store.Store, bus Publisher, pools map[string]config.Pool, logger *slog.Logger, now func() time.Time) *Service {
	if pools == nil {
		pools = config.DefaultPools()
	}
	return &Service{st: st, bus: bus, pools: pools, logger: logger, now: now, wake: make(chan struct{})}
}

// WithRegistrar makes the service tell r of every job that succeeds.
func (s *Service) WithRegistrar(r Registrar) *Service {
	s.reg = r
	return s
}

// Wait returns a channel that is closed the next time a job may have become
// leasable (created or requeued). Take it before looking for a job so that
// no wake-up is missed.
func (s *Service) Wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

func (s *Service) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.wake)
	s.wake = make(chan struct{})
}

// Create stores j as a queued job and audits it. AccountID, ProjectID,
// Spec.Repo and, for agent jobs, Role and Runtime are required; ID is kept
// when set (the workflow engine relies on it), otherwise a new one is made;
// Needs, GoalID, SpecID, TaskID and the rest of Spec are kept as given. The
// job's pool is j.Pool (the role pack's, default build) for agent jobs and
// comes from the environment for command jobs (domain.CommandPool); a job
// the pool's policy does not allow is refused with an error wrapping
// ErrInvalid and config.ErrNotAllowed. The spec gets the job's ID, account,
// kind, role and runtime and is stored without secrets.
func (s *Service) Create(ctx context.Context, by domain.Actor, j domain.Job) (domain.Job, error) {
	if j.Kind == "" {
		j.Kind = j.Spec.Kind
	}
	j.Kind = j.Kind.OrDefault()
	agent := j.Kind == domain.JobKindAgent
	if !agent {
		j.Spec.Kind = j.Kind
	}
	if j.AccountID == uuid.Nil() || j.ProjectID == uuid.Nil() || j.Spec.Repo == "" || (agent && (j.Role == "" || j.Runtime == "")) {
		return domain.Job{}, fmt.Errorf("create job: %w: account, project, repo and, for agent jobs, role and runtime are required", ErrInvalid)
	}
	if err := s.place(&j); err != nil {
		return domain.Job{}, fmt.Errorf("create job: %w: %w", ErrInvalid, err)
	}
	if j.ID == uuid.Nil() {
		j.ID = domain.NewID()
	}
	j.Spec.JobID, j.Spec.AccountID, j.Spec.Role, j.Spec.Runtime = j.ID, j.AccountID, j.Role, j.Runtime
	j.Spec = j.Spec.Redacted()
	j.Needs = domain.NormalizeLabels(j.Needs)
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := queue.New(tx.Jobs()).Enqueue(ctx, &j); err != nil {
			return err
		}
		return audit(ctx, tx, by, j, "job.created", map[string]any{"role": j.Role, "runtime": j.Runtime, "needs": j.Needs, "pool": j.Pool})
	})
	if err != nil {
		return domain.Job{}, fmt.Errorf("create job: %w", err)
	}
	s.notify()
	s.logger.InfoContext(ctx, "job created", logAttrs(j)...)
	return j, nil
}

// place sets the job's pool and checks that the pool's policy allows it.
func (s *Service) place(j *domain.Job) error {
	if j.Kind == domain.JobKindCommand {
		j.Pool = domain.CommandPool(j.Spec.Environment)
	}
	j.Pool = domain.OrDefaultPool(j.Pool)
	if j.Pool == domain.PoolActions && j.Kind == domain.JobKindCommand {
		// forecrewd serves this pool and has no labels, so a need would keep
		// the job queued for ever.
		j.Needs = nil
	}
	p, ok := s.pools[j.Pool]
	if !ok {
		return fmt.Errorf("pool %s is not configured: %w", j.Pool, config.ErrNotAllowed)
	}
	return p.Check(j.Pool, j.Role, j.Kind, len(j.Spec.WritePaths) > 0)
}

// Cancel cancels a queued or running job, audits it and publishes
// job.completed. A running job's worker learns it on its next heartbeat.
func (s *Service) Cancel(ctx context.Context, by domain.Actor, accountID, jobID uuid.UUID) (domain.Job, error) {
	var j domain.Job
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if j, err = tx.Jobs().Cancel(ctx, accountID, jobID); err != nil {
			return err
		}
		return audit(ctx, tx, by, j, "job.canceled", nil)
	})
	if err != nil {
		return domain.Job{}, fmt.Errorf("cancel job %s: %w", jobID, err)
	}
	s.logger.InfoContext(ctx, "job canceled", logAttrs(j)...)
	return j, s.completed(ctx, j)
}

// Complete finishes a job running under worker w with its result (status
// succeeded or failed), audits it and publishes job.completed. A succeeded
// result whose output does not match the spec's output schema is stored as
// failed with ErrSchemaMismatch. When the job is no longer running under w
// it returns an error wrapping store.ErrLeaseLost and changes nothing, except
// that it publishes job.completed again (deduplicated) for a finished job, so
// a retried complete never loses the signal.
func (s *Service) Complete(ctx context.Context, w domain.Worker, jobID uuid.UUID, r store.JobResult) (domain.Job, error) {
	cur, err := s.st.Jobs().Get(ctx, w.AccountID, jobID)
	if err != nil {
		return domain.Job{}, fmt.Errorf("complete job %s: %w", jobID, err)
	}
	if r.Status == domain.JobSucceeded && len(cur.Spec.OutputSchema) > 0 {
		if err := roles.ValidateOutput(cur.Spec.OutputSchema, r.Output); err != nil {
			s.logger.WarnContext(ctx, "job output does not match schema", append(logAttrs(cur), "error", err)...)
			r.Status, r.Error = domain.JobFailed, ErrSchemaMismatch
		}
	}
	var j domain.Job
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if j, err = queue.New(tx.Jobs()).Ack(ctx, w.AccountID, jobID, w.ID, r); err != nil {
			return err
		}
		if err = storeTestRuns(ctx, tx, cur, j, r.TestRuns); err != nil {
			return err
		}
		if s.reg != nil && j.Status == domain.JobSucceeded {
			if err := s.reg.JobCompleted(ctx, tx, j); err != nil {
				return err
			}
		}
		return audit(ctx, tx, workerActor(w), j, "job.completed", map[string]any{
			"status": j.Status, "head_sha": j.HeadSHA, "error": j.Error, "usage": j.Usage, "cost_usd": j.CostUSD,
		})
	})
	if errors.Is(err, store.ErrLeaseLost) {
		return j, s.leaseLost(ctx, jobID, j, err)
	}
	if err != nil {
		return domain.Job{}, fmt.Errorf("complete job %s: %w", jobID, err)
	}
	s.logger.InfoContext(ctx, "job completed", append(logAttrs(j), "status", j.Status, "worker_id", w.ID)...)
	return j, s.completed(ctx, j)
}

// storeTestRuns stores the test runs a command job reported, linked to the
// change whose head is the job's SHA. cur is the job before it was
// completed, j after. Runs on an agent job, or without a head SHA, are
// ErrInvalid.
func storeTestRuns(ctx context.Context, tx store.Store, cur, j domain.Job, runs []store.TestRunReport) error {
	if len(runs) == 0 {
		return nil
	}
	if cur.Kind.OrDefault() != domain.JobKindCommand {
		return fmt.Errorf("%w: test_runs are only for command jobs", ErrInvalid)
	}
	if j.HeadSHA == "" {
		return fmt.Errorf("%w: test_runs need a head_sha", ErrInvalid)
	}
	var changeID uuid.UUID
	cs, err := tx.Changes().List(ctx, j.AccountID, store.ChangeFilter{ProjectID: j.ProjectID, HeadSHA: j.HeadSHA})
	if err != nil {
		return fmt.Errorf("find change of %s: %w", j.HeadSHA, err)
	}
	for _, c := range cs { // prefer an open PR
		if changeID == uuid.Nil() || c.State == domain.ChangeOpen {
			changeID = c.ID
		}
	}
	for _, rep := range runs {
		run := rep.Run
		run.AccountID, run.ProjectID, run.JobID, run.ChangeID = j.AccountID, j.ProjectID, j.ID, changeID
		run.SHA, run.Attempt = j.HeadSHA, cur.Attempts+1
		if run.Suite == "" {
			run.Suite = cur.Spec.Suite
		}
		run.Cell = cur.Spec.Cell
		if env := cur.Spec.Environment; env != nil {
			run.Environment = env.Name
		}
		if err := tx.TestRuns().Create(ctx, &run, rep.Results); err != nil {
			return fmt.Errorf("store test run %q: %w", run.Suite, err)
		}
	}
	return nil
}

// leaseLost republishes job.completed for a finished job and returns err
// with context.
func (s *Service) leaseLost(ctx context.Context, jobID uuid.UUID, j domain.Job, err error) error {
	if j.Status.Terminal() {
		if perr := s.completed(ctx, j); perr != nil {
			return perr
		}
	}
	return fmt.Errorf("complete job %s: %w", jobID, err)
}

// Release gives up the attempt of worker w on a job (an infrastructure
// failure): the job is queued again after domain.RetryBackoff, or failed
// when its attempts are exhausted, which publishes job.completed.
func (s *Service) Release(ctx context.Context, w domain.Worker, jobID uuid.UUID, reason string) (domain.Job, error) {
	var j domain.Job
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if j, err = queue.New(tx.Jobs()).Nack(ctx, w.AccountID, jobID, w.ID, reason); err != nil {
			return err
		}
		return audit(ctx, tx, workerActor(w), j, "job.released", map[string]any{"status": j.Status, "reason": reason, "attempts": j.Attempts})
	})
	if err != nil {
		return j, fmt.Errorf("release job %s: %w", jobID, err)
	}
	s.logger.WarnContext(ctx, "job released", append(logAttrs(j), "status", j.Status, "reason", reason, "attempts", j.Attempts, "worker_id", w.ID)...)
	if j.Status.Terminal() {
		return j, s.completed(ctx, j)
	}
	return j, nil
}

// completed publishes job.completed for a job in a terminal state.
func (s *Service) completed(ctx context.Context, j domain.Job) error {
	sig, err := domain.NewSignal(j.AccountID, j.ProjectID, domain.JobCompleted{JobID: j.ID, Status: j.Status}, s.now())
	if err != nil {
		return fmt.Errorf("job %s completed signal: %w", j.Ref(), err)
	}
	if _, err := s.bus.Publish(ctx, sig); err != nil {
		return fmt.Errorf("publish job %s completed: %w", j.Ref(), err)
	}
	return nil
}

func workerActor(w domain.Worker) domain.Actor {
	return domain.Actor{AccountID: w.AccountID, Kind: domain.ActorWorker, Name: w.Name}
}

func audit(ctx context.Context, tx store.Store, by domain.Actor, j domain.Job, action string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["ref"] = j.Ref()
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode audit data: %w", err)
	}
	return tx.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: j.AccountID, ProjectID: j.ProjectID, Actor: by.String(), Action: action,
		SubjectType: "job", SubjectID: j.ID, Data: raw,
	})
}

func logAttrs(j domain.Job) []any {
	return []any{"account_id", j.AccountID, "project_id", j.ProjectID, "job_id", j.ID, "ref", j.Ref()}
}

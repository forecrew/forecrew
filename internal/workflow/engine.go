package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

// Engine defaults.
const (
	DefaultWorkers = 4
	DefaultSweep   = 30 * time.Second
)

// ErrUnknownPlaybook means Start was given a playbook the engine does not have.
var ErrUnknownPlaybook = errors.New("unknown playbook")

// ErrMissingInput means Start was not given an input the playbook declares.
var ErrMissingInput = errors.New("missing playbook input")

// Action is a Go function an action step calls. It must be idempotent for
// call.Key: after a crash between the call and the step being recorded as
// done, the step calls it again with the same key. Its result, which must
// marshal to JSON, is the step's output; an error fails the step.
type Action func(ctx context.Context, call ActionCall) (any, error)

// ActionCall is the input of an action.
type ActionCall struct {
	Key  string // idempotency key: the step run ID
	Run  domain.WorkflowRun
	With map[string]any // the step's with, references resolved
}

// Actions is the action registry, by name.
type Actions map[string]Action

// Config holds the engine's dependencies.
type Config struct {
	Store     store.Store
	Bus       SignalBus
	Jobs      JobCreator
	Gates     gates.Service
	Actions   Actions
	Playbooks map[string]*Playbook
	Logger    *slog.Logger     // defaults to discarding
	Now       func() time.Time // defaults to the real UTC time
	// Workers is the number of runs advanced at once (DefaultWorkers).
	Workers int
	// Sweep is how often every active run is advanced, which recovers
	// missed signals and ends timed-out waits (DefaultSweep).
	Sweep time.Duration
}

// Engine runs playbooks durably. Each run is advanced by replaying its
// playbook against the step runs recorded so far: a step run is recorded
// before its side effect and marked done after it, and side effects are
// keyed by the step run ID (job and gate IDs, action keys), so a run
// resumed after a crash never repeats one. One Engine per database: runs
// are serialized in process.
type Engine struct {
	cfg Config
	q   *workQueue
}

// New returns an engine. It fails when a playbook calls an action that is
// not registered.
func New(cfg Config) (*Engine, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Workers < 1 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = DefaultSweep
	}
	for _, p := range cfg.Playbooks {
		if err := checkActions(append(slices.Clone(p.Steps), p.Finally...), cfg.Actions); err != nil {
			return nil, fmt.Errorf("playbook %q: %w", p.Name, err)
		}
	}
	return &Engine{cfg: cfg, q: newWorkQueue()}, nil
}

func checkActions(steps []Step, actions Actions) error {
	for _, s := range steps {
		if s.Type == StepAction && actions[s.Action] == nil {
			return fmt.Errorf("step %q: action %q is not registered: %w", s.ID, s.Action, ErrInvalidPlaybook)
		}
		if err := checkActions(s.Steps, actions); err != nil {
			return err
		}
	}
	return nil
}

// StartRequest starts a run.
type StartRequest struct {
	AccountID uuid.UUID
	ProjectID uuid.UUID
	GoalID    uuid.UUID
	TaskID    uuid.UUID
	Inputs    map[string]any
}

// Start stores a new run of the named playbook and queues it. The run is
// advanced by Run.
func (e *Engine) Start(ctx context.Context, playbook string, req StartRequest) (domain.WorkflowRun, error) {
	p := e.cfg.Playbooks[playbook]
	if p == nil {
		return domain.WorkflowRun{}, fmt.Errorf("start %q: %w", playbook, ErrUnknownPlaybook)
	}
	for _, name := range p.Inputs {
		if req.Inputs[name] == nil {
			return domain.WorkflowRun{}, fmt.Errorf("start %q: %s: %w", playbook, name, ErrMissingInput)
		}
	}
	inputs, err := json.Marshal(req.Inputs)
	if err != nil {
		return domain.WorkflowRun{}, fmt.Errorf("start %q: inputs: %w", playbook, err)
	}
	run := domain.WorkflowRun{
		AccountID: req.AccountID, ProjectID: req.ProjectID, GoalID: req.GoalID, TaskID: req.TaskID,
		Playbook: playbook, Status: domain.RunRunning, Inputs: inputs,
	}
	if err := e.cfg.Store.WorkflowRuns().Create(ctx, &run); err != nil {
		return domain.WorkflowRun{}, fmt.Errorf("start %q: %w", playbook, err)
	}
	e.cfg.Logger.Info("workflow run started", "account_id", run.AccountID, "project_id", run.ProjectID, "run_id", run.ID, "playbook", playbook)
	e.q.push(runKey{run.AccountID, run.ID})
	return run, nil
}

// Run advances runs with a pool of workers until ctx is done. It resumes
// every active run first, wakes runs waiting on each published signal and
// sweeps the active runs periodically.
func (e *Engine) Run(ctx context.Context) error {
	signals := e.cfg.Bus.Subscribe(ctx)
	var wg sync.WaitGroup
	for range e.cfg.Workers {
		wg.Go(func() { e.work(ctx) })
	}
	defer wg.Wait()
	e.sweep(ctx)
	ticker := time.NewTicker(e.cfg.Sweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case s, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			e.wake(ctx, s)
		case <-ticker.C:
			e.sweep(ctx)
		}
	}
}

// work advances queued runs until ctx is done.
func (e *Engine) work(ctx context.Context) {
	for {
		k, ok := e.q.pop(ctx)
		if !ok {
			return
		}
		if err := e.advance(ctx, k); err != nil && ctx.Err() == nil {
			e.cfg.Logger.Error("advance workflow run", "account_id", k.account, "run_id", k.run, "error", err)
		}
		e.q.done(k)
	}
}

// wake queues the runs with a step waiting for s.
func (e *Engine) wake(ctx context.Context, s domain.Signal) {
	steps, err := e.cfg.Store.StepRuns().ListWaiting(ctx, s.AccountID, s.Key)
	if err != nil {
		e.cfg.Logger.Error("list waiting steps", "account_id", s.AccountID, "key", s.Key, "error", err)
		return
	}
	for _, st := range steps {
		e.q.push(runKey{st.AccountID, st.RunID})
	}
}

// sweep queues every active run of every account.
func (e *Engine) sweep(ctx context.Context) {
	accounts, err := e.cfg.Store.Accounts().List(ctx)
	if err != nil {
		e.cfg.Logger.Error("list accounts", "error", err)
		return
	}
	for _, a := range accounts {
		runs, err := e.cfg.Store.WorkflowRuns().ListActive(ctx, a.ID)
		if err != nil {
			e.cfg.Logger.Error("list active workflow runs", "account_id", a.ID, "error", err)
			continue
		}
		for _, r := range runs {
			e.q.push(runKey{r.AccountID, r.ID})
		}
	}
}

// advance replays a run as far as it goes and records its new status.
func (e *Engine) advance(ctx context.Context, k runKey) error {
	run, err := e.cfg.Store.WorkflowRuns().Get(ctx, k.account, k.run)
	if err != nil {
		return err
	}
	if run.Status != domain.RunRunning && run.Status != domain.RunWaiting {
		return nil
	}
	x, err := e.newExec(ctx, run)
	if err != nil {
		return err
	}
	err = x.replay(ctx)
	var end *runEnd
	if errors.As(err, &end) && end.status != domain.RunSucceeded {
		if err := x.finally(ctx); err != nil {
			return err
		}
	}
	run = x.run // resume may have changed its status
	switch {
	case err == nil:
		end = &runEnd{status: domain.RunSucceeded}
	case errors.Is(err, errWaiting):
		if run.Status == domain.RunWaiting {
			return nil
		}
		run.Status = domain.RunWaiting
		return e.cfg.Store.WorkflowRuns().Update(ctx, &run)
	case !errors.As(err, &end):
		return err
	}
	run.Status, run.Error, run.FinishedAt = end.status, end.reason, e.cfg.Now()
	if err := e.cfg.Store.WorkflowRuns().Update(ctx, &run); err != nil {
		return err
	}
	e.cfg.Logger.Info("workflow run finished", "account_id", run.AccountID, "project_id", run.ProjectID, "run_id", run.ID,
		"playbook", run.Playbook, "status", run.Status, "reason", run.Error)
	return nil
}

// errWaiting means the run waits for a signal.
var errWaiting = errors.New("waiting for a signal")

// runEnd ends a run with a status other than succeeded.
type runEnd struct {
	status domain.RunStatus
	reason string
}

func (r *runEnd) Error() string { return string(r.status) + ": " + r.reason }

// stepFailed is a step failure that on_failure handles.
type stepFailed struct{ reason string }

func (f *stepFailed) Error() string { return f.reason }

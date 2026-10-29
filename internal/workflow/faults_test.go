package workflow_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var errDB = errors.New("db down")

// faultyStore fails the calls named in fail.
type faultyStore struct {
	store.Store
	mu   sync.Mutex
	fail map[string]bool
}

func (f *faultyStore) failing(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[name] {
		return errDB
	}
	return nil
}

func (f *faultyStore) set(name string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[name] = on
}

func (f *faultyStore) Accounts() store.AccountRepo { return faultyAccounts{f.Store.Accounts(), f} }
func (f *faultyStore) WorkflowRuns() store.WorkflowRunRepo {
	return faultyRuns{f.Store.WorkflowRuns(), f}
}
func (f *faultyStore) StepRuns() store.StepRunRepo { return faultySteps{f.Store.StepRuns(), f} }
func (f *faultyStore) Projects() store.ProjectRepo { return faultyProjects{f.Store.Projects(), f} }

type faultyAccounts struct {
	store.AccountRepo
	f *faultyStore
}

func (a faultyAccounts) List(ctx context.Context) ([]domain.Account, error) {
	if err := a.f.failing("accounts"); err != nil {
		return nil, err
	}
	return a.AccountRepo.List(ctx)
}

type faultyRuns struct {
	store.WorkflowRunRepo
	f *faultyStore
}

func (r faultyRuns) ListActive(ctx context.Context, accountID uuid.UUID) ([]domain.WorkflowRun, error) {
	if err := r.f.failing("active"); err != nil {
		return nil, err
	}
	return r.WorkflowRunRepo.ListActive(ctx, accountID)
}

func (r faultyRuns) Get(ctx context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error) {
	if err := r.f.failing("run"); err != nil {
		return domain.WorkflowRun{}, err
	}
	return r.WorkflowRunRepo.Get(ctx, accountID, id)
}

type faultySteps struct {
	store.StepRunRepo
	f *faultyStore
}

func (s faultySteps) ListWaiting(ctx context.Context, accountID uuid.UUID, key string) ([]domain.StepRun, error) {
	if err := s.f.failing("waiting"); err != nil {
		return nil, err
	}
	return s.StepRunRepo.ListWaiting(ctx, accountID, key)
}

func (s faultySteps) ListByRun(ctx context.Context, accountID, runID uuid.UUID) ([]domain.StepRun, error) {
	if err := s.f.failing("steps"); err != nil {
		return nil, err
	}
	return s.StepRunRepo.ListByRun(ctx, accountID, runID)
}

func (s faultySteps) Create(ctx context.Context, sr *domain.StepRun) error {
	if err := s.f.failing("create"); err != nil {
		return err
	}
	return s.StepRunRepo.Create(ctx, sr)
}

type faultyProjects struct {
	store.ProjectRepo
	f *faultyStore
}

func (p faultyProjects) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Project, error) {
	if err := p.f.failing("project"); err != nil {
		return domain.Project{}, err
	}
	return p.ProjectRepo.Get(ctx, accountID, id)
}

// syncBuffer is a log destination safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestStoreFaults checks that store errors are logged and retried: the run
// completes once the store recovers.
func TestStoreFaults(t *testing.T) {
	v := newEnv(t)
	fs := &faultyStore{Store: v.st, fail: map[string]bool{"accounts": true, "active": true, "waiting": true, "run": true, "steps": true, "create": true, "project": true}}
	var logs syncBuffer
	cfg := v.config(t, map[string]string{"pb": `
steps:
  - {id: merged, type: wait, with: {signal: pr.merged, repo: acme/app, number: 1}}
  - {id: a, type: action, action: record, on_failure: escalate}
`})
	cfg.Store = fs
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	v.actions["record"] = v.record(func(_ workflow.ActionCall, n int) (any, error) {
		if n == 1 {
			return nil, errors.New("flaked")
		}
		return nil, nil
	})
	e, _ := run(t, cfg)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	must(t, err)
	v.publish(t, &domain.PRMerged{Repo: "acme/app", Number: 1, SHA: "s"})
	for _, msg := range []string{"list accounts", "advance workflow run"} {
		eventually(t, "log "+msg, func() bool { return strings.Contains(logs.String(), msg) })
	}
	// Run subscribes asynchronously: publish until a signal is seen.
	n := 1
	eventually(t, "log list waiting steps", func() bool {
		n++
		v.publish(t, &domain.PRClosed{Repo: "acme/app", Number: n})
		return strings.Contains(logs.String(), "list waiting steps")
	})
	for _, name := range []string{"accounts", "run", "steps"} {
		fs.set(name, false)
	}
	eventually(t, "log list active", func() bool { return strings.Contains(logs.String(), "list active workflow runs") })
	fs.set("active", false)
	time.Sleep(20 * time.Millisecond)
	fs.set("create", false)
	// the escalation needs the project's repo
	v.waitStep(t, r.ID, "a", 1, domain.StepFailed)
	time.Sleep(20 * time.Millisecond)
	fs.set("project", false)
	v.decide(t, r.ID, "a:escalate", 1, domain.GateApproved, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
}

func TestNewDefaults(t *testing.T) {
	e, err := workflow.New(workflow.Config{})
	if err != nil || e == nil {
		t.Fatalf("New = %v, %v", e, err)
	}
}

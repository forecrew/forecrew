package testkit

import (
	"context"
	"errors"
	"sync"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrCrash is what a CrashStore fails with.
var ErrCrash = errors.New("crash")

// CrashStore fails every step run update once a step run with its step ID
// is about to be marked waiting or done: the engine is killed after the
// step's side effect and before the step is recorded as done.
type CrashStore struct {
	store.Store
	id      string
	crashed chan struct{}
	once    sync.Once
}

// NewCrashStore returns a store that crashes at step id.
func NewCrashStore(st store.Store, id string) *CrashStore {
	return &CrashStore{Store: st, id: id, crashed: make(chan struct{})}
}

// Crashed is closed by the crash.
func (c *CrashStore) Crashed() <-chan struct{} { return c.crashed }

// StepRuns implements store.Store.
func (c *CrashStore) StepRuns() store.StepRunRepo { return crashSteps{c.Store.StepRuns(), c} }

// WorkflowRuns fails reads after the crash, so the dead engine does not
// advance the run again before it is stopped.
func (c *CrashStore) WorkflowRuns() store.WorkflowRunRepo {
	return crashRuns{c.Store.WorkflowRuns(), c}
}

type crashRuns struct {
	store.WorkflowRunRepo
	c *CrashStore
}

func (r crashRuns) Get(ctx context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error) {
	select {
	case <-r.c.crashed:
		return domain.WorkflowRun{}, ErrCrash
	default:
	}
	return r.WorkflowRunRepo.Get(ctx, accountID, id)
}

type crashSteps struct {
	store.StepRunRepo
	c *CrashStore
}

func (s crashSteps) Update(ctx context.Context, sr *domain.StepRun) error {
	select {
	case <-s.c.crashed:
		return ErrCrash
	default:
	}
	if sr.StepID == s.c.id {
		s.c.once.Do(func() { close(s.c.crashed) })
		return ErrCrash
	}
	return s.StepRunRepo.Update(ctx, sr)
}

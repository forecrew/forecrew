package testkit

import (
	"context"
	"slices"
	"sync"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ workflow.JobCreator = (*JobCreator)(nil)

// JobCreator is a workflow.JobCreator that records requests. With a store
// it also creates a queued job from each request; without one it returns
// req.ID or a new ID. Err, when set, is returned instead.
type JobCreator struct {
	Store store.Store
	Err   error

	mu       sync.Mutex
	requests []workflow.JobRequest
	canceled []uuid.UUID
}

// Create implements workflow.JobCreator.
func (c *JobCreator) Create(ctx context.Context, req workflow.JobRequest) (uuid.UUID, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	if c.Err != nil {
		return uuid.Nil(), c.Err
	}
	if c.Store == nil {
		return newID(req.ID), nil
	}
	j := domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Pool: req.Pool, Role: req.Role, Needs: req.Needs,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, Role: req.Role, IncludeRepoFacts: req.IncludeRepoFacts,
			Command: req.Command, Environment: req.Environment, Cell: req.Cell, Suite: req.Suite, Overlay: req.Overlay,
		},
	}
	if req.Kind == domain.JobKindCommand {
		j.Kind, j.Spec.Kind = req.Kind, req.Kind
	}
	if err := c.Store.Jobs().Create(ctx, &j); err != nil {
		return uuid.Nil(), err
	}
	return j.ID, nil
}

// Cancel implements workflow.JobCreator: it cancels the job in the store and
// records its ID. Without a store it only records the ID.
func (c *JobCreator) Cancel(ctx context.Context, accountID, jobID uuid.UUID) error {
	c.mu.Lock()
	c.canceled = append(c.canceled, jobID)
	c.mu.Unlock()
	if c.Store == nil {
		return nil
	}
	_, err := c.Store.Jobs().Cancel(ctx, accountID, jobID)
	return err
}

// Canceled returns the job IDs Cancel was called with.
func (c *JobCreator) Canceled() []uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.canceled)
}

// Requests returns the requests Create was called with.
func (c *JobCreator) Requests() []workflow.JobRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

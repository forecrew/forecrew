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
// new IDs. Err, when set, is returned instead.
type JobCreator struct {
	Store store.Store
	Err   error

	mu       sync.Mutex
	requests []workflow.JobRequest
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
		return domain.NewID(), nil
	}
	j := domain.Job{
		AccountID: req.AccountID, ProjectID: req.ProjectID, Role: req.Role, Needs: req.Needs,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, Role: req.Role, IncludeRepoFacts: req.IncludeRepoFacts,
		},
	}
	if err := c.Store.Jobs().Create(ctx, &j); err != nil {
		return uuid.Nil(), err
	}
	return j.ID, nil
}

// Requests returns the requests Create was called with.
func (c *JobCreator) Requests() []workflow.JobRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

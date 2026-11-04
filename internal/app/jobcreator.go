// Package app assembles forecrewd from its components. It holds the job
// creator that turns a role and its inputs into a queued job.
package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ workflow.JobCreator = (*jobCreator)(nil)

// jobCreator is the workflow.JobCreator of forecrewd: it resolves the role,
// builds the prompt with the context pack and queues the job.
type jobCreator struct {
	store store.Store
	roles roles.Resolver
	packs contextpack.Builder
	jobs  *jobs.Service
}

// Create implements workflow.JobCreator. The job gets req.ID when it is set
// and runs in req.Pool, or else the role pack's pool; jobs.Service refuses
// a job the pool's policy does not allow.
func (c *jobCreator) Create(ctx context.Context, req workflow.JobRequest) (uuid.UUID, error) {
	if req.Kind.OrDefault() == domain.JobKindCommand {
		return c.createCommand(ctx, req)
	}
	if req.Command != nil || req.Environment != nil || len(req.Cell) > 0 || req.Suite != "" || req.Overlay != nil || req.Timeout != 0 {
		return uuid.Nil(), fmt.Errorf("create %s job: command, environment, cell, suite, overlay and timeout are only for command jobs: %w", req.Role, jobs.ErrInvalid)
	}
	project, err := c.store.Projects().Get(ctx, req.AccountID, req.ProjectID)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: project %s: %w", req.Role, req.ProjectID, err)
	}
	role, err := c.roles.Resolve(ctx, req.AccountID, req.ProjectID, req.Role)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	// The lease mints a token for the job's repo: it must be the project's.
	if req.Repo != "" && !strings.EqualFold(req.Repo, project.Repo) {
		return uuid.Nil(), fmt.Errorf("create %s job: repo %s is not the repo of project %s: %w", req.Role, req.Repo, project.Name, jobs.ErrInvalid)
	}
	req.Repo = project.Repo
	if req.BaseRef == "" {
		req.BaseRef = project.DefaultBranch
	}
	pack, err := c.packs.Build(ctx, req, role)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	pool := req.Pool
	if pool == "" {
		pool = role.Pool
	}
	j, err := c.jobs.Create(ctx, domain.SystemActor(req.AccountID, domain.ForecrewdName), domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Pool: pool, Role: role.Name, Runtime: role.Runtime, Needs: req.Needs,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, WritePaths: role.WritePaths, Model: role.Model,
			Prompt: pack.Prompt, OutputSchema: role.OutputSchema, AllowedTools: role.AllowedTools, MaxTurns: role.MaxTurns,
			Timeout: domain.Duration(role.Timeout), IncludeRepoFacts: req.IncludeRepoFacts,
		},
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	return j.ID, nil
}

// defaultCommandTimeout bounds a command job whose request has no timeout.
const defaultCommandTimeout = 30 * time.Minute

// createCommand queues a command job: no role resolution, no prompt. Its
// pool comes from its environment (jobs.Service).
func (c *jobCreator) createCommand(ctx context.Context, req workflow.JobRequest) (uuid.UUID, error) {
	project, err := c.store.Projects().Get(ctx, req.AccountID, req.ProjectID)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create command job: project %s: %w", req.ProjectID, err)
	}
	if req.Repo != "" && !strings.EqualFold(req.Repo, project.Repo) {
		return uuid.Nil(), fmt.Errorf("create command job: repo %s is not the repo of project %s: %w", req.Repo, project.Name, jobs.ErrInvalid)
	}
	if req.Role != "" || req.Pool != "" || req.Prompt != "" || req.Feedback != "" {
		return uuid.Nil(), fmt.Errorf("create command job: role, pool, prompt and feedback are not for command jobs: %w", jobs.ErrInvalid)
	}
	if req.BaseRef == "" {
		req.BaseRef = project.DefaultBranch
	}
	if req.Timeout == 0 {
		req.Timeout = domain.Duration(defaultCommandTimeout)
	}
	spec := domain.JobSpec{
		Kind: domain.JobKindCommand, Repo: project.Repo, BaseRef: req.BaseRef, Branch: req.Branch,
		Timeout: req.Timeout, IncludeRepoFacts: req.IncludeRepoFacts, Command: req.Command, Environment: req.Environment,
		Cell: req.Cell, Suite: req.Suite, Overlay: req.Overlay,
	}
	if err := spec.Validate(); err != nil {
		return uuid.Nil(), fmt.Errorf("create command job: %w: %w", jobs.ErrInvalid, err)
	}
	needs := slices.Clone(req.Needs)
	if env := req.Environment; env != nil {
		needs = append(needs, env.Needs...)
		if env.Provider == domain.ProviderDocker || len(env.Services) > 0 {
			needs = append(needs, "docker")
		}
	}
	j, err := c.jobs.Create(ctx, domain.SystemActor(req.AccountID, domain.ForecrewdName), domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Kind: domain.JobKindCommand, Needs: needs,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID, Spec: spec,
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create command job: %w", err)
	}
	return j.ID, nil
}

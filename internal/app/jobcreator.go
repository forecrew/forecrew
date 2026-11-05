// Package app assembles forecrewd from its components. It holds the job
// creator that turns a role and its inputs into a queued job.
package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/councils"
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
	// knowledge gives the paths of the project's curated knowledge; nil
	// leaves the role's write paths as they are.
	knowledge pathSource
	// councils and models make a council turn's job: the step's prompt and
	// schema, on the runtime its member is pinned to.
	councils *councils.Set
	models   func(role, adapter string) (string, error)
}

// pathSource lists the repo paths of a project's curated knowledge, which
// writing roles except the knowledge curator may not change: curated
// knowledge changes only through proposals.
type pathSource interface {
	MappedPaths(ctx context.Context, p domain.Project) ([]string, error)
}

// CuratorRole is the role that writes the fallback knowledge/** folder.
const CuratorRole = "knowledge-curator"

// ReviewerRole is the role whose verdict schema reviews of knowledge
// proposals answer with.
const ReviewerRole = "reviewer"

// writePaths returns the role's write paths with the project's knowledge
// paths excluded, for every writing role but the curator. When the
// project's knowledge policy is unknown the job is refused (fail closed):
// without it the curated paths cannot be excluded.
func (c *jobCreator) writePaths(ctx context.Context, role roles.Role, p domain.Project) ([]string, error) {
	if c.knowledge == nil || role.ReadOnly() || role.Name == CuratorRole {
		return role.WritePaths, nil
	}
	mapped, err := c.knowledge.MappedPaths(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("create %s job: %w", role.Name, err)
	}
	out := slices.Clone(role.WritePaths)
	for _, g := range mapped {
		if ex := "!" + g; !slices.Contains(out, ex) {
			out = append(out, ex)
		}
	}
	return out, nil
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
	role, err := c.jobRole(ctx, req)
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
	writePaths, err := c.writePaths(ctx, role, project)
	if err != nil {
		return uuid.Nil(), err
	}
	pool := req.Pool
	if pool == "" {
		pool = role.Pool
	}
	for _, l := range req.LockedPaths {
		// They come from model output and are shown in the prompt: no
		// control characters or backticks.
		if strings.HasPrefix(l, "!") || !domain.RelativePath(l) || strings.ContainsFunc(l, func(r rune) bool { return unicode.IsControl(r) || r == '`' }) {
			return uuid.Nil(), fmt.Errorf("create %s job: locked_paths: %q must be a glob relative to the repo root, without !, control characters or backticks: %w", req.Role, l, jobs.ErrInvalid)
		}
	}
	j, err := c.jobs.Create(ctx, domain.SystemActor(req.AccountID, domain.ForecrewdName), domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Pool: pool, Role: role.Name, Runtime: role.Runtime, Needs: req.Needs, Resources: role.Resources,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID, ContextManifest: pack.Manifest,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, WritePaths: writePaths, Model: role.Model,
			Prompt: pack.Prompt, OutputSchema: role.OutputSchema, AllowedTools: role.AllowedTools, MaxTurns: role.MaxTurns,
			Timeout: domain.Duration(role.Timeout), IncludeRepoFacts: req.IncludeRepoFacts, LockedPaths: req.LockedPaths,
		},
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	return j.ID, nil
}

// jobRole resolves the role of req, as a council turn or the review of a
// knowledge proposal when req is one.
func (c *jobCreator) jobRole(ctx context.Context, req workflow.JobRequest) (roles.Role, error) {
	role, err := c.roles.Resolve(ctx, req.AccountID, req.ProjectID, req.Role)
	if err != nil {
		return roles.Role{}, err
	}
	if req.Council != nil {
		return c.councilRole(ctx, req, role)
	}
	if req.ProposalReview {
		reviewer, err := c.roles.Resolve(ctx, req.AccountID, req.ProjectID, ReviewerRole)
		if err != nil {
			return roles.Role{}, err
		}
		return role.ProposalReviewer(reviewer.OutputSchema), nil
	}
	return role, nil
}

// councilRole turns role into the job of req's council turn. The pool stays
// the role's.
func (c *jobCreator) councilRole(ctx context.Context, req workflow.JobRequest, role roles.Role) (roles.Role, error) {
	t := req.Council
	council, err := c.store.Councils().Get(ctx, req.AccountID, t.CouncilID)
	if err != nil {
		return roles.Role{}, fmt.Errorf("council %s: %w", t.CouncilID, err)
	}
	if council.ProjectID != req.ProjectID {
		return roles.Role{}, fmt.Errorf("%s is not a council of this project: %w", council.Ref(), jobs.ErrInvalid)
	}
	if t.Runtime != "" && t.Runtime != role.Runtime {
		if role.Model, err = c.models(role.Name, t.Runtime); err != nil {
			return roles.Role{}, err
		}
		role.Runtime = t.Runtime
	}
	return c.councils.Job(councils.Turn{
		Kind: council.Kind, Step: councils.Step(t.Step), Round: t.Round, Roles: t.Roles,
		Seat: councils.Seat{Role: role.Name, Priority: t.Priority}, BudgetExhausted: t.BudgetExhausted,
	}, role)
}

// Cancel implements workflow.JobCreator.
func (c *jobCreator) Cancel(ctx context.Context, accountID, jobID uuid.UUID) error {
	_, err := c.jobs.Cancel(ctx, domain.SystemActor(accountID, domain.ForecrewdName), accountID, jobID)
	return err
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
	if req.Role != "" || req.Pool != "" || req.Prompt != "" || req.Feedback != "" || len(req.LockedPaths) > 0 || req.Council != nil {
		return uuid.Nil(), fmt.Errorf("create command job: role, pool, prompt, feedback, locked_paths and council are not for command jobs: %w", jobs.ErrInvalid)
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
		if env.Provider == domain.ProviderDocker {
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

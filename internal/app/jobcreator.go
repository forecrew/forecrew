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
}

// pathSource lists the repo paths of a project's curated knowledge, which
// writing roles except the knowledge curator may not change: curated
// knowledge changes only through proposals.
type pathSource interface {
	MappedPaths(p domain.Project) []string
}

// CuratorRole is the role that writes the fallback knowledge/** folder.
const CuratorRole = "knowledge-curator"

// writePaths returns the role's write paths with the project's knowledge
// paths excluded, for every writing role but the curator.
func (c *jobCreator) writePaths(role roles.Role, p domain.Project) []string {
	if c.knowledge == nil || role.ReadOnly() || role.Name == CuratorRole {
		return role.WritePaths
	}
	out := slices.Clone(role.WritePaths)
	for _, g := range c.knowledge.MappedPaths(p) {
		if ex := "!" + g; !slices.Contains(out, ex) {
			out = append(out, ex)
		}
	}
	return out
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
	for _, l := range req.LockedPaths {
		// They come from model output and are shown in the prompt: no
		// control characters or backticks.
		if strings.HasPrefix(l, "!") || !domain.RelativePath(l) || strings.ContainsFunc(l, func(r rune) bool { return unicode.IsControl(r) || r == '`' }) {
			return uuid.Nil(), fmt.Errorf("create %s job: locked_paths: %q must be a glob relative to the repo root, without !, control characters or backticks: %w", req.Role, l, jobs.ErrInvalid)
		}
	}
	j, err := c.jobs.Create(ctx, domain.SystemActor(req.AccountID, domain.ForecrewdName), domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Pool: pool, Role: role.Name, Runtime: role.Runtime, Needs: req.Needs,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, WritePaths: c.writePaths(role, project), Model: role.Model,
			Prompt: pack.Prompt, OutputSchema: role.OutputSchema, AllowedTools: role.AllowedTools, MaxTurns: role.MaxTurns,
			Timeout: domain.Duration(role.Timeout), IncludeRepoFacts: req.IncludeRepoFacts, LockedPaths: req.LockedPaths,
		},
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	return j.ID, nil
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
	if req.Role != "" || req.Pool != "" || req.Prompt != "" || req.Feedback != "" || len(req.LockedPaths) > 0 {
		return uuid.Nil(), fmt.Errorf("create command job: role, pool, prompt, feedback and locked_paths are not for command jobs: %w", jobs.ErrInvalid)
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

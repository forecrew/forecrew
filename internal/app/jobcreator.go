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
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/runtime"
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
	// tasks are the tasks of the role packs, by role.name.
	tasks map[string]roles.Task
	// councils and models make a council turn's job: the step's prompt and
	// schema, on the runtime its member is pinned to.
	councils *councils.Set
	models   func(role, adapter string) (string, error)
}

// pathSource says what a role may do to the repo files of a project's
// curated knowledge: curated knowledge changes only through proposals,
// except where a section's access.write gives the role its files.
type pathSource interface {
	MappedPaths(ctx context.Context, p domain.Project, role string, curator bool) (knowledge.PathRules, error)
}

// CuratorRole is the role that writes the fallback knowledge/** folder.
const CuratorRole = knowledge.CuratorRole

// ReviewerRole is the role whose verdict schema reviews of knowledge
// proposals answer with.
const ReviewerRole = "reviewer"

// writePaths returns the role's write paths with the project's curated
// knowledge excluded (knowledge.PathRules), for every writing role. The
// curator keeps knowledge/** but not the protected sections or the policy
// file. Granted globs are added, and the pack's own !knowledge/** goes when
// there are any, so that access.write works. When the project's knowledge
// policy is unknown the job is refused (fail closed): without it the
// curated paths cannot be excluded.
func (c *jobCreator) writePaths(ctx context.Context, role roles.Role, p domain.Project) ([]string, error) {
	if c.knowledge == nil || role.ReadOnly() {
		return role.WritePaths, nil
	}
	rules, err := c.knowledge.MappedPaths(ctx, p, role.Name, role.Name == CuratorRole)
	if err != nil {
		return nil, fmt.Errorf("create %s job: %w", role.Name, err)
	}
	out := slices.Clone(role.WritePaths)
	if len(rules.Granted) > 0 {
		out = slices.DeleteFunc(out, func(w string) bool { return w == "!knowledge/**" })
	}
	for _, g := range rules.Granted {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	for _, g := range rules.Denied {
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
	if req.Task != "" && (req.ProposalReview || req.Council != nil) {
		return uuid.Nil(), fmt.Errorf("create %s job: a task, a proposal review and a council turn exclude each other: %w", req.Role, jobs.ErrInvalid)
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
	needs, allowed := toolboxJob(role, req.Needs)
	j, err := c.jobs.Create(ctx, domain.SystemActor(req.AccountID, domain.ForecrewdName), domain.Job{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Pool: pool, Role: role.Name, Runtime: role.Runtime, Needs: needs, Resources: role.Resources,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID, ContextManifest: pack.Manifest,
		Spec: domain.JobSpec{
			Repo: req.Repo, BaseRef: req.BaseRef, Branch: req.Branch, WritePaths: writePaths, Model: role.Model,
			Prompt: pack.Prompt, OutputSchema: role.OutputSchema, AllowedTools: allowed, MaxTurns: role.MaxTurns,
			Timeout: domain.Duration(role.Timeout), IncludeRepoFacts: req.IncludeRepoFacts, LockedPaths: req.LockedPaths,
			Toolbox: role.Toolbox,
		},
	})
	if err != nil {
		return uuid.Nil(), fmt.Errorf("create %s job: %w", req.Role, err)
	}
	return j.ID, nil
}

// withWritePaths returns role with the paths of req added to its write paths.
func withWritePaths(role roles.Role, req workflow.JobRequest) (roles.Role, error) {
	if err := checkWritePaths(req); err != nil {
		return role, err
	}
	role.WritePaths = slices.Concat(role.WritePaths, req.WritePaths)
	return role, nil
}

// checkWritePaths refuses a write path that is not a plain glob under the
// repo root: like the locked ones it may come from a playbook's action.
func checkWritePaths(req workflow.JobRequest) error {
	for _, w := range req.WritePaths {
		if strings.HasPrefix(w, "!") || !domain.RelativePath(w) || strings.ContainsFunc(w, func(r rune) bool { return unicode.IsControl(r) || r == '`' }) {
			return fmt.Errorf("create %s job: write_paths: %q must be a glob relative to the repo root, without !, control characters or backticks: %w", req.Role, w, jobs.ErrInvalid)
		}
	}
	return nil
}

// jobRole resolves the role of req, as a council turn, the review of a
// knowledge proposal or a task of the role when req is one.
func (c *jobCreator) jobRole(ctx context.Context, req workflow.JobRequest) (roles.Role, error) {
	role, err := c.roles.Resolve(ctx, req.AccountID, req.ProjectID, req.Role)
	if err != nil {
		return roles.Role{}, err
	}
	if req.Council != nil {
		return c.councilRole(ctx, req, role)
	}
	if req.Task != "" {
		t, ok := c.tasks[role.Name+"."+req.Task]
		if !ok {
			return roles.Role{}, fmt.Errorf("role %s has no task %q: %w", role.Name, req.Task, jobs.ErrInvalid)
		}
		return role.ForTask(t), nil
	}
	if req.ProposalReview {
		reviewer, err := c.roles.Resolve(ctx, req.AccountID, req.ProjectID, ReviewerRole)
		if err != nil {
			return roles.Role{}, err
		}
		return role.ProposalReviewer(reviewer.OutputSchema), nil
	}
	// The added write paths are the job's: the role's prompt and the curated
	// knowledge rules see them. Tasks, councils and reviews stay read-only.
	return withWritePaths(role, req)
}

// toolboxJob returns the needs and the allowed tools of a job of role: with a
// toolbox the job needs a worker with Docker and the toolbox's labels, and
// may use every tool of the toolbox's MCP servers.
func toolboxJob(role roles.Role, needs []string) (labels, allowed []string) {
	t := role.Toolbox
	if t == nil {
		return needs, role.AllowedTools
	}
	labels = append(slices.Clone(needs), "docker")
	labels = append(labels, t.Needs...)
	allowed = slices.Clone(role.AllowedTools)
	for _, s := range t.MCPServers {
		allowed = append(allowed, runtime.MCPServerTool(s.Name))
	}
	return labels, allowed
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
	if req.Role != "" || req.Pool != "" || req.Prompt != "" || req.Feedback != "" || len(req.LockedPaths) > 0 || len(req.WritePaths) > 0 || req.Council != nil || req.Task != "" {
		return uuid.Nil(), fmt.Errorf("create command job: role, pool, prompt, feedback, locked_paths, write_paths and council are not for command jobs: %w", jobs.ErrInvalid)
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

// Package tools implements mcp.ToolService once. The MCP server, /api/v1 and
// the web UI all call it, so a rule lives in exactly one place. Every method
// works for the account of the actor, never sees another account's rows and
// answers with the errors of internal/mcp (ErrNotFound, ErrInvalidInput, ...).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Playbooks started by the tools.
const (
	// PlaybookOnboarding takes the inputs repo and project (its name).
	PlaybookOnboarding = "onboarding"
	// PlaybookGoal takes the inputs repo, goal_id and platforms.
	PlaybookGoal = "goal"
)

// List sizes of list_jobs.
const (
	DefaultJobLimit = 50
	MaxJobLimit     = 500
)

// Starter starts playbook runs; *workflow.Engine implements it.
type Starter interface {
	Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error)
}

// JobReader serves the job and worker read models; *jobs.Service implements it.
type JobReader interface {
	Status(ctx context.Context, accountID, jobID uuid.UUID) (jobs.JobView, error)
	Logs(ctx context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) (jobs.Logs, error)
	Workers(ctx context.Context, accountID uuid.UUID) ([]jobs.WorkerView, error)
}

// Checker reads from GitHub; github.Tracker implements it.
type Checker interface {
	IsInstalled(ctx context.Context, repo string) (bool, error)
	CheckStatus(ctx context.Context, repo, sha string) (github.CheckStatus, error)
}

// Config holds the dependencies of a Service.
type Config struct {
	Store     store.Store
	Gates     gates.Service
	Jobs      JobReader
	Creator   workflow.JobCreator
	Playbooks Starter
	Roles     roles.Resolver
	GitHub    Checker
	Logger    *slog.Logger // defaults to discarding
}

// Service is the mcp.ToolService.
type Service struct {
	cfg Config
}

var _ mcp.ToolService = (*Service)(nil)

// New returns the tool service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg}
}

// lookup finds an entity by human ref (GOAL-3) or UUID. A ref that is
// neither wraps mcp.ErrInvalidInput, a missing entity mcp.ErrNotFound.
func lookup[T any](ref string, kind domain.RefKind, byNumber func(int64) (T, error), byID func(uuid.UUID) (T, error)) (T, error) {
	var v T
	number, id, err := domain.ParseRefOrID(kind, ref)
	if err != nil {
		return v, fmt.Errorf("%q is not a %s ref or UUID: %w", ref, kind, mcp.ErrInvalidInput)
	}
	if number != 0 {
		v, err = byNumber(number)
	} else {
		v, err = byID(id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return v, fmt.Errorf("%s %s: %w", strings.ToLower(string(kind)), ref, mcp.ErrNotFound)
	}
	return v, err
}

func (s *Service) project(ctx context.Context, accountID uuid.UUID, nameOrRepo string) (domain.Project, error) {
	if nameOrRepo == "" {
		return domain.Project{}, fmt.Errorf("project is required: %w", mcp.ErrInvalidInput)
	}
	p, err := s.cfg.Store.Projects().GetByName(ctx, accountID, nameOrRepo)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Project{}, fmt.Errorf("project %s: %w", nameOrRepo, mcp.ErrNotFound)
	}
	return p, err
}

// optionalProject resolves a project filter; an empty one is the zero ID.
func (s *Service) optionalProject(ctx context.Context, accountID uuid.UUID, nameOrRepo string) (uuid.UUID, error) {
	if nameOrRepo == "" {
		return uuid.Nil(), nil
	}
	p, err := s.project(ctx, accountID, nameOrRepo)
	return p.ID, err
}

func (s *Service) goal(ctx context.Context, accountID uuid.UUID, ref string) (domain.Goal, error) {
	return lookup(ref, domain.RefGoal,
		func(n int64) (domain.Goal, error) { return s.cfg.Store.Goals().GetByNumber(ctx, accountID, n) },
		func(id uuid.UUID) (domain.Goal, error) { return s.cfg.Store.Goals().Get(ctx, accountID, id) })
}

func (s *Service) task(ctx context.Context, accountID uuid.UUID, ref string) (domain.Task, error) {
	return lookup(ref, domain.RefTask,
		func(n int64) (domain.Task, error) { return s.cfg.Store.Tasks().GetByNumber(ctx, accountID, n) },
		func(id uuid.UUID) (domain.Task, error) { return s.cfg.Store.Tasks().Get(ctx, accountID, id) })
}

func (s *Service) job(ctx context.Context, accountID uuid.UUID, ref string) (domain.Job, error) {
	return lookup(ref, domain.RefJob,
		func(n int64) (domain.Job, error) { return s.cfg.Store.Jobs().GetByNumber(ctx, accountID, n) },
		func(id uuid.UUID) (domain.Job, error) { return s.cfg.Store.Jobs().Get(ctx, accountID, id) })
}

// audit records what by did.
func (s *Service) audit(ctx context.Context, st store.Store, by domain.Actor, projectID uuid.UUID, action, subject string, subjectID uuid.UUID, data map[string]any) error {
	e := domain.AuditEntry{AccountID: by.AccountID, ProjectID: projectID, Actor: by.String(), Action: action, SubjectType: subject, SubjectID: subjectID}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		e.Data = raw
	}
	return st.Audit().Append(ctx, &e)
}

// issueURL is the GitHub link of an issue or PR; GitHub redirects /issues/N
// to the PR when N is one.
func issueURL(repo string, number int) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
}

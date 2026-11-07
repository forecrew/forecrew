// Package tools implements mcp.ToolService once. The MCP server, /api/v1 and
// the web UI all call it, so a rule lives in exactly one place. Every method
// works for the account of the actor, never sees another account's rows,
// checks the actor's permission on the project it finds (internal/authz) and
// answers with the errors of internal/mcp (ErrNotFound, ErrInvalidInput, ...).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	cfgres "github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Playbooks started by the tools.
const (
	// PlaybookOnboarding takes the inputs repo and project (its name).
	PlaybookOnboarding = "onboarding"
	// PlaybookOnboardingTests takes the same inputs: the tests part of
	// onboarding, which onboarding starts and a refresh starts again.
	PlaybookOnboardingTests = "onboarding-tests"
	// PlaybookGoal takes the inputs repo, goal_id and platforms.
	PlaybookGoal = "goal"
	// PlaybookGoalTesting is PlaybookGoal with the testing system (tests
	// from the spec, a matrix per head, a verification gate). It takes the
	// same inputs and runs only for the projects of Config.TestingProjects.
	PlaybookGoalTesting = "goal-v2"
	// PlaybookGoalCouncils is PlaybookGoalTesting with triage, councils and
	// the learn step. It takes the same inputs and runs only for the
	// projects of Config.GoalV3Projects.
	PlaybookGoalCouncils = "goal-v3"
	// PlaybookBugfix fixes a bug: a regression test, the fix, verification,
	// review and a merge gate. It takes the inputs of PlaybookGoal and runs
	// only for the projects of Config.TestingProjects.
	PlaybookBugfix = "bugfix"
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

// JobService serves the job and worker read models and cancels jobs;
// *jobs.Service implements it.
type JobService interface {
	Cancel(ctx context.Context, by domain.Actor, accountID, jobID uuid.UUID) (domain.Job, error)
	Status(ctx context.Context, accountID, jobID uuid.UUID) (jobs.JobView, error)
	Logs(ctx context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) (jobs.Logs, error)
	Workers(ctx context.Context, accountID uuid.UUID, visible func(project uuid.UUID) bool) ([]jobs.WorkerView, error)
}

// FleetService serves the fleet's read model; *fleet.Controller implements
// it.
type FleetService interface {
	Overview(ctx context.Context, accountID uuid.UUID, visible func(project uuid.UUID) bool) (fleet.Overview, error)
	Usage(ctx context.Context, accountID uuid.UUID, from, to time.Time, records bool) (fleet.UsageReport, error)
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
	Jobs      JobService
	Creator   workflow.JobCreator
	Playbooks Starter
	Roles     roles.Resolver
	GitHub    Checker
	// TestingProjects are the names of the projects whose new goals run
	// PlaybookGoalTesting instead of PlaybookGoal (forecrew.yaml testing).
	TestingProjects []string
	// GoalV3Projects are the names of the projects whose new goals run
	// PlaybookGoalCouncils (forecrew.yaml goal_v3).
	GoalV3Projects []string
	// Councils holds the council packs.
	Councils *councils.Set
	// Evidence serves artifact content; nil when no evidence storage is
	// configured.
	Evidence evidence.Store
	// Registers is the assumption and decision register; the tools that
	// read or resolve it answer unavailable without one.
	Registers *registers.Service
	// Search and Notes serve search_knowledge and delete_note; nil answers
	// unavailable.
	Search *knowledge.Search
	Notes  *knowledge.Notes
	// Proposals serves propose_knowledge; nil answers unavailable.
	Proposals *proposals.Service
	// Fleet serves fleet_overview and fleet_usage; nil answers unavailable.
	Fleet FleetService
	// Trackers serves the tracker tools; nil answers unavailable.
	Trackers *tracker.Service
	Logger   *slog.Logger // defaults to discarding
	// GitHubUserID turns a GitHub login into its stable ID, wrapping
	// auth.ErrNoGitHubUser for an unknown login; invitations by login answer
	// unavailable without it.
	GitHubUserID func(ctx context.Context, login string) (int64, error)
	// Enterprise serves the enterprise tools; nil answers unavailable.
	Enterprise mcp.EnterpriseTools
	// PublicURL starts the links of invitations.
	PublicURL string
	// TokenTTL is auth.token_default_ttl; it defaults to
	// config.DefaultTokenTTL.
	TokenTTL time.Duration
	Now      func() time.Time // defaults to the real time
	// Server is forecrew.yaml as read; nil leaves server settings out of the
	// configuration resources. Getenv reads what it refers to.
	Server *config.Config
	Getenv func(string) string
	// CheckSchedule refuses a schedule forecrewd would not start.
	CheckSchedule func(config.Schedule) error
	// Repo reads repos and opens the pull requests of repo plans.
	Repo cfgres.Repo
	// Secrets is the secret store; nil refuses secure fields.
	Secrets secrets.Store
	// Roles says whether a role runs in a toolbox and whether it exists, for
	// plans that assign models; nil checks nothing.
	RoleToolbox func(role string) (toolbox, known bool)
	// RoleWrites says whether a role writes code, for plans that assign a
	// model its test did not pass; nil counts every role as writing.
	RoleWrites func(role string) bool
	// RoleNeeds lists the roles a suggestion of models fills; nil fills none.
	RoleNeeds func() []models.RoleNeed
	// Models runs key checks and suitability tests; nil answers unavailable.
	Models *models.Checker
}

// Service is the mcp.ToolService.
type Service struct {
	mcp.EnterpriseTools
	cfg    Config
	authz  *authz.Engine
	config *cfgres.Engine
	// people remembers who waiting tickets are from, per connection.
	people *peopleCache
}

var _ mcp.ToolService = (*Service)(nil)

// New returns the tool service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = config.DefaultTokenTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	engine := authz.New(cfg.Store, cfg.Logger)
	if cfg.Proposals != nil {
		engine.WithProtection(cfg.Proposals)
	}
	if cfg.Enterprise == nil {
		cfg.Enterprise = mcp.NoEnterprise{}
	}
	s := &Service{EnterpriseTools: cfg.Enterprise, cfg: cfg, authz: engine, people: &peopleCache{}}
	s.config = cfgres.NewEngine(cfgres.Config{
		Store: cfg.Store, Access: func(st store.Store) cfgres.Access { return s.in(st) }, Server: cfg.Server, Getenv: cfg.Getenv, CheckSchedule: cfg.CheckSchedule,
		Repo: cfg.Repo, Secrets: cfg.Secrets, Roles: cfg.RoleToolbox, RoleWrites: cfg.RoleWrites, Logger: cfg.Logger, Now: cfg.Now,
	})
	return s
}

// in returns the service working in the transaction st.
func (s *Service) in(st store.Store) *Service {
	c := *s
	c.cfg.Store = st
	c.authz = authz.New(st, s.cfg.Logger)
	return &c
}

// allow checks that by holds p on a project.
func (s *Service) allow(ctx context.Context, by domain.Actor, p domain.Permission, projectID uuid.UUID) error {
	return s.authz.Authorize(ctx, by, p, authz.Project(by.AccountID, projectID))
}

// allowOrg checks that by holds p on the organization.
func (s *Service) allowOrg(ctx context.Context, by domain.Actor, p domain.Permission) error {
	return s.authz.Authorize(ctx, by, p, authz.Organization(by.AccountID))
}

// scope returns where a list may look: the project it is filtered on, which
// by must hold p on, or every project where by holds p.
func (s *Service) scope(ctx context.Context, by domain.Actor, p domain.Permission, projectID uuid.UUID) (authz.Visible, error) {
	if projectID == uuid.Nil() {
		return s.authz.Projects(ctx, by, p)
	}
	if err := s.allow(ctx, by, p, projectID); err != nil {
		return authz.Visible{}, err
	}
	return authz.Visible{IDs: []uuid.UUID{projectID}}, nil
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
	e := domain.AuditEntry{AccountID: by.AccountID, ProjectID: projectID, Actor: by.String(), Credential: by.Credential.String(), Action: action, SubjectType: subject, SubjectID: subjectID}
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

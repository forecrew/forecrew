package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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

// defaultBranch is the branch of a newly registered project unless the
// caller names one.
const defaultBranch = "main"

// OnboardProject implements mcp.ToolService.
func (s *Service) OnboardProject(ctx context.Context, by domain.Actor, in mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error) {
	if err := s.allowOrg(ctx, by, domain.PermProjectCreate); err != nil {
		return mcp.OnboardProjectOutput{}, err
	}
	repo := strings.TrimSpace(in.Repo)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("repo %q is not owner/name: %w", in.Repo, mcp.ErrInvalidInput)
	}
	installed, err := s.cfg.GitHub.IsInstalled(ctx, repo)
	if err != nil {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("check GitHub App on %s: %w", repo, err)
	}
	if !installed {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("the GitHub App is not installed on %s: %w", repo, mcp.ErrInvalidInput)
	}
	if in.Refresh {
		return s.refreshTests(ctx, by, repo)
	}
	p, err := s.registerProject(ctx, by, repo, cmp.Or(strings.TrimSpace(in.Name), name), cmp.Or(strings.TrimSpace(in.DefaultBranch), defaultBranch))
	if err != nil {
		return mcp.OnboardProjectOutput{}, err
	}
	if p.Status != domain.ProjectRegistered {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("project %s is %s, only a registered project can be onboarded: %w", p.Name, p.Status, mcp.ErrInvalidInput)
	}
	projects := s.cfg.Store.Projects()
	err = projects.UpdateStatus(ctx, by.AccountID, p.ID, domain.ProjectRegistered, domain.ProjectOnboarding)
	if errors.Is(err, store.ErrConflict) {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("project %s is already being onboarded: %w", p.Name, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("start onboarding of %s: %w", p.Name, err)
	}
	p.Status = domain.ProjectOnboarding
	// The audit entry comes first: once the playbook runs there is no way back.
	if err := s.audit(ctx, s.cfg.Store, by, p.ID, "project.onboarding", "project", p.ID, nil); err != nil {
		s.revertOnboarding(ctx, by, p)
		return mcp.OnboardProjectOutput{}, fmt.Errorf("audit onboarding of %s: %w", p.Name, err)
	}
	run, err := s.cfg.Playbooks.Start(ctx, PlaybookOnboarding, workflow.StartRequest{
		AccountID: by.AccountID, ProjectID: p.ID, Inputs: map[string]any{"repo": p.Repo, "project": p.Name},
	})
	if err != nil {
		s.revertOnboarding(ctx, by, p)
		return mcp.OnboardProjectOutput{}, fmt.Errorf("start onboarding of %s: %w", p.Name, err)
	}
	return mcp.OnboardProjectOutput{Project: p, RunID: run.ID}, nil
}

// refreshTests starts the tests part of onboarding again for the project of
// repo, which must be onboarded and have no such run in flight. The project
// stays onboarded throughout.
func (s *Service) refreshTests(ctx context.Context, by domain.Actor, repo string) (mcp.OnboardProjectOutput, error) {
	p, err := s.cfg.Store.Projects().GetByName(ctx, by.AccountID, repo)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("%s is not a project: onboard it first: %w", repo, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.OnboardProjectOutput{}, err
	}
	if p.Status != domain.ProjectOnboarded {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("project %s is %s, only an onboarded project can refresh its tests: %w", p.Name, p.Status, mcp.ErrInvalidInput)
	}
	for _, role := range []string{"test-architect", "environment-engineer"} {
		if s.cfg.Roles == nil {
			return mcp.OnboardProjectOutput{}, fmt.Errorf("the role %s is not configured: add it under roles in forecrew.yaml to refresh the tests: %w", role, mcp.ErrInvalidInput)
		}
		_, err := s.cfg.Roles.Resolve(ctx, by.AccountID, p.ID, role)
		if errors.Is(err, roles.ErrNotConfigured) || errors.Is(err, roles.ErrUnknownRole) {
			return mcp.OnboardProjectOutput{}, fmt.Errorf("the role %s is not configured: add it under roles in forecrew.yaml to refresh the tests: %w", role, mcp.ErrInvalidInput)
		}
		if err != nil {
			return mcp.OnboardProjectOutput{}, fmt.Errorf("resolve the role %s: %w", role, err)
		}
	}
	active, err := s.cfg.Store.WorkflowRuns().ListActive(ctx, by.AccountID)
	if err != nil {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("list the runs of %s: %w", p.Name, err)
	}
	if i := slices.IndexFunc(active, func(r domain.WorkflowRun) bool {
		return r.ProjectID == p.ID && r.Playbook == PlaybookOnboardingTests
	}); i >= 0 {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("the tests of %s are already being onboarded (run %s): %w", p.Name, active[i].ID, mcp.ErrInvalidInput)
	}
	if err := s.audit(ctx, s.cfg.Store, by, p.ID, "project.tests_refresh", "project", p.ID, nil); err != nil {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("audit the refresh of the tests of %s: %w", p.Name, err)
	}
	run, err := s.cfg.Playbooks.Start(ctx, PlaybookOnboardingTests, workflow.StartRequest{
		AccountID: by.AccountID, ProjectID: p.ID, Inputs: map[string]any{"repo": p.Repo, "project": p.Name},
	})
	if err != nil {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("start the refresh of the tests of %s: %w", p.Name, err)
	}
	return mcp.OnboardProjectOutput{Project: p, RunID: run.ID}, nil
}

// revertOnboarding puts a project back to registered after onboarding could
// not start.
func (s *Service) revertOnboarding(ctx context.Context, by domain.Actor, p domain.Project) {
	ctx = context.WithoutCancel(ctx)
	err := s.cfg.Store.Projects().UpdateStatus(ctx, by.AccountID, p.ID, domain.ProjectOnboarding, domain.ProjectRegistered)
	if err == nil {
		err = s.audit(ctx, s.cfg.Store, by, p.ID, "project.onboarding_reverted", "project", p.ID, nil)
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "revert project status", "account_id", by.AccountID, "project_id", p.ID, "error", err)
	}
}

// registerProject returns the project of repo, creating it when needed.
func (s *Service) registerProject(ctx context.Context, by domain.Actor, repo, name, branch string) (domain.Project, error) {
	p, err := s.cfg.Store.Projects().GetByName(ctx, by.AccountID, repo)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return p, err
	}
	p = domain.Project{AccountID: by.AccountID, Name: name, Repo: repo, DefaultBranch: branch, Status: domain.ProjectRegistered}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Projects().Create(ctx, &p); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, p.ID, "project.registered", "project", p.ID, map[string]any{"repo": repo})
	})
	if errors.Is(err, store.ErrConflict) {
		return domain.Project{}, fmt.Errorf("a project named %q already exists: %w", name, mcp.ErrInvalidInput)
	}
	return p, err
}

// CreateGoal implements mcp.ToolService.
func (s *Service) CreateGoal(ctx context.Context, by domain.Actor, in mcp.CreateGoalInput) (domain.Goal, error) {
	if strings.TrimSpace(in.Title) == "" {
		return domain.Goal{}, fmt.Errorf("title is required: %w", mcp.ErrInvalidInput)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return domain.Goal{}, err
	}
	if err := s.allow(ctx, by, domain.PermGoalCreate, p.ID); err != nil {
		return domain.Goal{}, err
	}
	if p.Status != domain.ProjectOnboarded {
		return domain.Goal{}, fmt.Errorf("project %s is %s, onboard it first: %w", p.Name, p.Status, mcp.ErrInvalidInput)
	}
	councils, err := overrideCouncils(in.Councils)
	if err != nil {
		return domain.Goal{}, err
	}
	g := domain.Goal{
		AccountID: by.AccountID, ProjectID: p.ID, Title: strings.TrimSpace(in.Title), Body: in.Body,
		Platforms: domain.NormalizeLabels(in.Platforms), Status: domain.GoalDraft, CreatedBy: by.String(),
	}
	if councils != nil {
		// The override is the goal's size: the goal playbook skips the triage job for it.
		g.Size = &domain.GoalSize{Councils: councils, Reason: "Councils set by " + by.String() + "."}
	}
	return s.startGoal(ctx, by, p, g, s.goalPlaybook(p))
}

// ReportBug implements mcp.ToolService.
func (s *Service) ReportBug(ctx context.Context, by domain.Actor, in mcp.ReportBugInput) (domain.Goal, error) {
	if strings.TrimSpace(in.Title) == "" {
		return domain.Goal{}, fmt.Errorf("title is required: %w", mcp.ErrInvalidInput)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return domain.Goal{}, err
	}
	if err := s.allow(ctx, by, domain.PermGoalCreate, p.ID); err != nil {
		return domain.Goal{}, err
	}
	return s.startBug(ctx, by, p, strings.TrimSpace(in.Title), in.Body, in.Platforms, 0)
}

// ReportIssueAsBug makes the GitHub issue of p's repo the goal issue of a
// new bug goal and starts the bug-fix playbook, for the @forecrew bug
// command. The caller has checked that by may create goals: a GitHub login
// on the allowlist holds no role to check. An issue that is a goal's issue
// already is not reported again; the goal is returned with
// mcp.ErrInvalidInput. Nor is an issue that is a ticket waiting, queued or at
// work: one issue has one goal.
func (s *Service) ReportIssueAsBug(ctx context.Context, by domain.Actor, p domain.Project, issue github.Issue) (domain.Goal, error) {
	goals, err := s.cfg.Store.Goals().List(ctx, p.AccountID, store.GoalFilter{ProjectID: p.ID})
	if err != nil {
		return domain.Goal{}, fmt.Errorf("list the goals of %s: %w", p.Name, err)
	}
	if i := slices.IndexFunc(goals, func(g domain.Goal) bool { return g.IssueNumber == issue.Number }); i >= 0 {
		return goals[i], fmt.Errorf("issue #%d is %s already: %w", issue.Number, goals[i].Ref(), mcp.ErrInvalidInput)
	}
	switch l, err := s.cfg.Store.TicketLinks().GetByIssue(ctx, p.AccountID, p.ID, issue.Number); {
	case err == nil:
		return domain.Goal{}, fmt.Errorf("issue #%d is a ticket Forecrew has, %s: %w", issue.Number, l.Status, mcp.ErrInvalidInput)
	case !errors.Is(err, store.ErrNotFound):
		return domain.Goal{}, fmt.Errorf("find the ticket of issue #%d: %w", issue.Number, err)
	}
	return s.startBug(ctx, by, p, strings.TrimSpace(issue.Title), issue.Body, nil, issue.Number)
}

// startBug creates a goal of kind bug and starts the bug-fix playbook. The
// bug-fix playbook writes its regression test with the testing system, so
// only the projects of Config.TestingProjects have it. issue is the goal's
// GitHub issue, or 0 for the playbook to open one.
func (s *Service) startBug(ctx context.Context, by domain.Actor, p domain.Project, title, body string, platforms []string, issue int) (domain.Goal, error) {
	if title == "" {
		return domain.Goal{}, fmt.Errorf("title is required: %w", mcp.ErrInvalidInput)
	}
	if p.Status != domain.ProjectOnboarded {
		return domain.Goal{}, fmt.Errorf("project %s is %s, onboard it first: %w", p.Name, p.Status, mcp.ErrInvalidInput)
	}
	if !slices.Contains(s.cfg.TestingProjects, p.Name) {
		return domain.Goal{}, fmt.Errorf("the bug-fix playbook needs the testing system: add %s to testing.projects: %w", p.Name, mcp.ErrInvalidInput)
	}
	g := domain.Goal{
		AccountID: by.AccountID, ProjectID: p.ID, Kind: domain.GoalBug, Title: title, Body: body,
		Platforms: domain.NormalizeLabels(platforms), Status: domain.GoalDraft, IssueNumber: issue, CreatedBy: by.String(),
	}
	return s.startGoal(ctx, by, p, g, PlaybookBugfix)
}

// startGoal stores the goal g and starts playbook for it. A goal whose
// playbook cannot start is canceled.
func (s *Service) startGoal(ctx context.Context, by domain.Actor, p domain.Project, g domain.Goal, playbook string) (domain.Goal, error) {
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Goals().Create(ctx, &g); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, p.ID, "goal.created", "goal", g.ID, map[string]any{"title": g.Title, "kind": g.Kind.OrDefault()})
	})
	if err != nil {
		return domain.Goal{}, fmt.Errorf("create goal: %w", err)
	}
	_, err = s.cfg.Playbooks.Start(ctx, playbook, workflow.StartRequest{
		AccountID: by.AccountID, ProjectID: p.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": p.Repo, "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	if err != nil {
		if cerr := s.cfg.Store.Goals().UpdateStatus(context.WithoutCancel(ctx), by.AccountID, g.ID, domain.GoalDraft, domain.GoalCanceled); cerr != nil {
			s.cfg.Logger.ErrorContext(ctx, "cancel goal", "account_id", by.AccountID, "goal_id", g.ID, "error", cerr)
		}
		return domain.Goal{}, fmt.Errorf("start the plan of %s: %w", g.Ref(), err)
	}
	return g, nil
}

// GoalPlaybook is the playbook a goal of kind runs in p, given the names of
// the projects with the testing system and those with councils and learning
// (which have the testing system too). The bug-fix playbook writes its
// regression test with that system, so a bug elsewhere runs the goal playbook.
func GoalPlaybook(testing, goalV3 []string, p domain.Project, kind domain.GoalKind) string {
	v3 := slices.Contains(goalV3, p.Name)
	switch {
	case !v3 && !slices.Contains(testing, p.Name):
		return PlaybookGoal
	case kind == domain.GoalBug:
		return PlaybookBugfix
	case v3:
		return PlaybookGoalCouncils
	}
	return PlaybookGoalTesting
}

// goalPlaybook is the playbook the goals of p run.
func (s *Service) goalPlaybook(p domain.Project) string {
	return GoalPlaybook(s.cfg.TestingProjects, s.cfg.GoalV3Projects, p, domain.GoalFeature)
}

// overrideCouncils returns the councils a human chose for a goal, in the
// order of domain.GoalSize; nil when none were given (triage sizes the goal).
// An empty list is no council.
func overrideCouncils(names *[]string) ([]domain.CouncilKind, error) {
	if names == nil {
		return nil, nil
	}
	chosen := map[domain.CouncilKind]bool{}
	for _, name := range *names {
		c := domain.CouncilKind(strings.ToLower(strings.TrimSpace(name)))
		if c != domain.CouncilProduct && c != domain.CouncilTechnical {
			return nil, fmt.Errorf("council %q must be product or technical: %w", name, mcp.ErrInvalidInput)
		}
		chosen[c] = true
	}
	out := []domain.CouncilKind{}
	for _, c := range []domain.CouncilKind{domain.CouncilProduct, domain.CouncilTechnical} {
		if chosen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// DispatchJob implements mcp.ToolService. Only read-only roles can be
// dispatched: writing roles run inside playbooks, on their own branches.
func (s *Service) DispatchJob(ctx context.Context, by domain.Actor, in mcp.DispatchJobInput) (domain.Job, error) {
	if in.Role == "" {
		return domain.Job{}, fmt.Errorf("role is required: %w", mcp.ErrInvalidInput)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return domain.Job{}, err
	}
	if err := s.allow(ctx, by, domain.PermJobDispatch, p.ID); err != nil {
		return domain.Job{}, err
	}
	role, err := s.cfg.Roles.Resolve(ctx, by.AccountID, p.ID, in.Role)
	if errors.Is(err, roles.ErrUnknownRole) {
		return domain.Job{}, fmt.Errorf("role %q: %w", in.Role, mcp.ErrInvalidInput)
	}
	if err != nil {
		return domain.Job{}, fmt.Errorf("resolve role %s: %w", in.Role, err)
	}
	if !role.ReadOnly() {
		return domain.Job{}, fmt.Errorf("role %s writes to the repo and runs only inside playbooks, dispatch a read-only role: %w", in.Role, mcp.ErrInvalidInput)
	}
	req := workflow.JobRequest{
		AccountID: by.AccountID, ProjectID: p.ID, Repo: p.Repo, BaseRef: in.BaseRef, Branch: in.Branch, Role: in.Role,
		Needs: in.Needs, Pool: in.Pool, IncludeRepoFacts: in.IncludeRepoFacts, Feedback: in.Feedback, Prompt: in.Prompt,
	}
	if in.Goal != "" {
		g, err := s.goal(ctx, by.AccountID, in.Goal)
		if err != nil {
			return domain.Job{}, err
		}
		if g.ProjectID != p.ID {
			return domain.Job{}, fmt.Errorf("%s belongs to another project: %w", g.Ref(), mcp.ErrInvalidInput)
		}
		req.GoalID = g.ID
	}
	if in.Task != "" {
		t, err := s.task(ctx, by.AccountID, in.Task)
		if err != nil {
			return domain.Job{}, err
		}
		if t.ProjectID != p.ID {
			return domain.Job{}, fmt.Errorf("%s belongs to another project: %w", t.Ref(), mcp.ErrInvalidInput)
		}
		req.TaskID, req.SpecID = t.ID, t.SpecID
		if req.GoalID == uuid.Nil() {
			req.GoalID = t.GoalID
		}
	}
	j, err := s.createJob(ctx, req)
	if err != nil {
		return domain.Job{}, fmt.Errorf("dispatch %s job: %w", in.Role, err)
	}
	if err := s.audit(ctx, s.cfg.Store, by, p.ID, "job.dispatched", "job", j.ID, map[string]any{"role": j.Role}); err != nil {
		return domain.Job{}, fmt.Errorf("audit dispatch of %s: %w", j.Ref(), err)
	}
	return j, nil
}

// createJob creates the job of req and returns it. A request the job
// service refuses, such as a pool that does not run the role, is invalid
// input.
func (s *Service) createJob(ctx context.Context, req workflow.JobRequest) (domain.Job, error) {
	id, err := s.cfg.Creator.Create(ctx, req)
	if errors.Is(err, jobs.ErrInvalid) {
		return domain.Job{}, fmt.Errorf("%w: %w", mcp.ErrInvalidInput, err)
	}
	if err != nil {
		return domain.Job{}, err
	}
	return s.cfg.Store.Jobs().Get(ctx, req.AccountID, id)
}

// Approve implements mcp.ToolService.
func (s *Service) Approve(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return s.decide(ctx, by, mcp.ToolApprove, in)
}

// RequestChanges implements mcp.ToolService.
func (s *Service) RequestChanges(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return s.decide(ctx, by, mcp.ToolRequestChanges, in)
}

// Reject implements mcp.ToolService.
func (s *Service) Reject(ctx context.Context, by domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
	return s.decide(ctx, by, mcp.ToolReject, in)
}

// decide checks that by may resolve the gate and lets the gate service apply
// the decision rules, which also answer a call that names no gate.
func (s *Service) decide(ctx context.Context, by domain.Actor, tool string, in mcp.GateDecisionInput) (domain.Gate, error) {
	var err error
	if in.Gate != "" {
		var g domain.Gate
		if g, err = s.cfg.Gates.Find(ctx, by.AccountID, in.Gate); err == nil {
			err = s.authz.AuthorizeGate(ctx, by, g)
			if err == nil && in.SaveAsLesson {
				// The feedback becomes a proposal, sent as the caller's own.
				err = s.allow(ctx, by, domain.PermKnowledgePropose, g.ProjectID)
			}
		}
	}
	var resolved domain.Gate
	if err == nil {
		resolved, err = s.cfg.Gates.Decide(ctx, by, tool, in)
	}
	if errors.Is(err, gates.ErrNotFound) {
		return domain.Gate{}, fmt.Errorf("gate %s: %w", in.Gate, mcp.ErrNotFound)
	}
	return resolved, err
}

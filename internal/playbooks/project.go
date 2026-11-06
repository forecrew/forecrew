package playbooks

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// projectOutput is the output of project.register.
type projectOutput struct {
	ProjectID     uuid.UUID `json:"project_id"`
	Repo          string    `json:"repo"`
	DefaultBranch string    `json:"default_branch"`
}

// registerProject is the action project.register. It checks that the GitHub
// App is installed on with.repo and that the run's project is that repo, and
// moves the project from registered to onboarding (a project already
// onboarding stays as it is, so a repeated call succeeds). With with.keep
// the status is not touched and an onboarding or onboarded project is
// accepted: the test part of onboarding, which a refresh runs on a project
// that is onboarded. It outputs the project's ID, repo and default branch.
func (a *actions) registerProject(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo string `json:"repo"`
		Keep bool   `json:"keep"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if !strings.EqualFold(p.Repo, with.Repo) {
		return nil, fmt.Errorf("project %s is %s, not %s", p.Name, p.Repo, with.Repo)
	}
	installed, err := a.Tracker.IsInstalled(ctx, p.Repo)
	if err != nil {
		return nil, fmt.Errorf("check the GitHub App on %s: %w", p.Repo, err)
	}
	if !installed {
		return nil, fmt.Errorf("the Forecrew GitHub App is not installed on %s: install it on the repo and onboard again", p.Repo)
	}
	switch {
	case with.Keep && (p.Status == domain.ProjectOnboarding || p.Status == domain.ProjectOnboarded):
	case with.Keep:
		return nil, fmt.Errorf("project %s is %s, not onboarding or onboarded", p.Name, p.Status)
	default:
		if err := a.startOnboarding(ctx, p); err != nil {
			return nil, err
		}
	}
	return projectOutput{ProjectID: p.ID, Repo: p.Repo, DefaultBranch: p.DefaultBranch}, nil
}

// startOnboarding moves a registered project to onboarding; one already
// onboarding stays.
func (a *actions) startOnboarding(ctx context.Context, p domain.Project) error {
	switch p.Status {
	case domain.ProjectRegistered:
		if err := a.Store.Projects().UpdateStatus(ctx, p.AccountID, p.ID, domain.ProjectRegistered, domain.ProjectOnboarding); err != nil {
			return fmt.Errorf("start onboarding of %s: %w", p.Name, err)
		}
	case domain.ProjectOnboarding:
	default:
		return fmt.Errorf("project %s is %s, not registered", p.Name, p.Status)
	}
	return nil
}

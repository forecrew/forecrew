package playbooks

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Labels the goal playbook puts on GitHub issues; needs labels are
// "needs:<worker label>".
const (
	labelGoal  = "forecrew:goal"
	labelTask  = "forecrew:task"
	labelNeeds = "needs:"
)

// ensureLabel creates a label of the goal playbook when it is missing.
func (a *actions) ensureLabel(ctx context.Context, repo, name, color, description string) error {
	if err := a.Tracker.EnsureLabel(ctx, repo, github.Label{Name: name, Color: color, Description: description}); err != nil {
		return fmt.Errorf("ensure label %s in %s: %w", name, repo, err)
	}
	return nil
}

// loadGoal returns the goal of the run's project with the given ID.
func (a *actions) loadGoal(ctx context.Context, run domain.WorkflowRun, id uuid.UUID) (domain.Goal, error) {
	g, err := a.Store.Goals().Get(ctx, run.AccountID, id)
	if err != nil {
		return domain.Goal{}, fmt.Errorf("get goal %s: %w", id, err)
	}
	if g.ProjectID != run.ProjectID {
		return domain.Goal{}, fmt.Errorf("goal %s belongs to another project", g.Ref())
	}
	return g, nil
}

// intakeOutput is the output of goal.intake.
type intakeOutput struct {
	IssueNumber   int    `json:"issue_number"`
	IssueURL      string `json:"issue_url"`
	DefaultBranch string `json:"default_branch"`
}

// goalIntake is the action goal.intake. It makes sure the labels
// forecrew:goal and forecrew:task exist in with.repo and opens the GitHub
// issue that mirrors the goal with.goal_id, labeled forecrew:goal, unless
// the goal has one already. It outputs the issue's number and URL and the
// project's default branch.
func (a *actions) goalIntake(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string    `json:"repo"`
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if !strings.EqualFold(p.Repo, with.Repo) {
		return nil, fmt.Errorf("project %s is %s, not %s", p.Name, p.Repo, with.Repo)
	}
	if err := a.ensureLabel(ctx, p.Repo, labelGoal, "0e8a16", "A goal Forecrew works on"); err != nil {
		return nil, err
	}
	if err := a.ensureLabel(ctx, p.Repo, labelTask, "1d76db", "A task of a Forecrew goal"); err != nil {
		return nil, err
	}
	var issue github.Issue
	if g.IssueNumber > 0 {
		issue, err = a.Tracker.GetIssue(ctx, p.Repo, g.IssueNumber)
		if err != nil {
			return nil, fmt.Errorf("get issue %s#%d: %w", p.Repo, g.IssueNumber, err)
		}
	} else {
		issue, err = a.Tracker.CreateIssue(ctx, p.Repo, github.IssueInput{
			Title: g.Ref() + ": " + g.Title, Body: g.Body, Labels: []string{labelGoal},
		})
		if err != nil {
			return nil, fmt.Errorf("open the issue of %s: %w", g.Ref(), err)
		}
		g.IssueNumber = issue.Number
		if err := a.Store.Goals().Update(ctx, &g); err != nil {
			return nil, fmt.Errorf("record the issue of %s: %w", g.Ref(), err)
		}
	}
	return intakeOutput{IssueNumber: issue.Number, IssueURL: issue.URL, DefaultBranch: p.DefaultBranch}, nil
}

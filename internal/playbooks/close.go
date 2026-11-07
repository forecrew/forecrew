package playbooks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// closeOutput is the output of goal.close.
type closeOutput struct {
	Tasks   int     `json:"tasks"`
	CostUSD float64 `json:"cost_usd"`
}

// closeGoal is the action goal.close. It comments the summary of the goal
// (tasks with their PRs, developer runs and cost, and the cost of all jobs of
// the goal) on the goal issue, closes the issue and moves the goal from in
// progress to done. It fails while a task of the goal is not merged. A goal
// that is done already is left as it is.
func (a *actions) closeGoal(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	tasks, err := a.Store.Tasks().List(ctx, g.AccountID, store.TaskFilter{GoalID: g.ID})
	if err != nil {
		return nil, fmt.Errorf("list the tasks of %s: %w", g.Ref(), err)
	}
	for _, t := range tasks {
		if t.Status != domain.TaskMerged {
			return nil, fmt.Errorf("%s is %s, not merged: %s is not done", t.Ref(), t.Status, g.Ref())
		}
	}
	jobs, err := a.Store.Jobs().List(ctx, g.AccountID, store.JobFilter{GoalID: g.ID})
	if err != nil {
		return nil, fmt.Errorf("list the jobs of %s: %w", g.Ref(), err)
	}
	total := 0.0
	for _, j := range jobs {
		total += j.CostUSD
	}
	if g.Status == domain.GoalDone {
		return closeOutput{Tasks: len(tasks), CostUSD: total}, nil
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	summary, err := a.goalSummary(ctx, g, tasks, jobs, total)
	if err != nil {
		return nil, err
	}
	if err := a.commentOnce(ctx, p.Repo, g.IssueNumber, summaryMarker(g), summary); err != nil {
		return nil, err
	}
	closed := "closed"
	if _, err := a.Tracker.UpdateIssue(ctx, p.Repo, g.IssueNumber, github.IssueUpdate{State: &closed}); err != nil {
		return nil, fmt.Errorf("close issue %s#%d: %w", p.Repo, g.IssueNumber, err)
	}
	if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, g.Status, domain.GoalDone); err != nil {
		return nil, fmt.Errorf("move %s to done: %w", g.Ref(), err)
	}
	return closeOutput{Tasks: len(tasks), CostUSD: total}, nil
}

// goalSummary is the closing comment: a line per task with its PR, the
// developer runs it took and what its jobs cost, then the total cost.
func (a *actions) goalSummary(ctx context.Context, g domain.Goal, tasks []domain.Task, jobs []domain.Job, total float64) (string, error) {
	changes, err := a.Store.Changes().List(ctx, g.AccountID, store.ChangeFilter{ProjectID: g.ProjectID})
	if err != nil {
		return "", fmt.Errorf("list the pull requests of %s: %w", g.Ref(), err)
	}
	var lines []string
	for _, t := range tasks {
		runs, cost := 0, 0.0
		for _, j := range jobs {
			if j.TaskID == t.ID {
				cost += j.CostUSD
				if j.Role == "developer" {
					runs++
				}
			}
		}
		pr := "no PR"
		if i := slices.IndexFunc(changes, func(c domain.Change) bool { return c.TaskID == t.ID }); i >= 0 {
			pr = fmt.Sprintf("PR #%d (%s)", changes[i].Number, changes[i].URL)
		}
		lines = append(lines, fmt.Sprintf("#%d %s %s: %s, %d developer %s, $%.2f", t.IssueNumber, t.Ref(), t.Title, pr, runs, plural(runs, "run"), cost))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Done\n\n%s is delivered: %d %s merged.\n", summaryMarker(g), g.Ref(), len(tasks), plural(len(tasks), "task"))
	section(&b, "Tasks", lines, "- ")
	if g.Kind == domain.GoalBug {
		named, err := a.regressionTests(ctx, tasks)
		if err != nil {
			return "", err
		}
		section(&b, "Regression tests", named, "- ")
	}
	fmt.Fprintf(&b, "\nCost of the %d jobs of the goal: $%.2f.\n", len(jobs), total)
	return b.String(), nil
}

// summaryMarker marks the closing comment of a goal.
func summaryMarker(g domain.Goal) string { return fmt.Sprintf("<!-- forecrew:summary:%s -->", g.ID) }

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// settleGoal is the action goal.settle, which the goal playbook runs when it
// ends failed or canceled. It puts the goal in a consistent state: a goal
// that is not done is canceled, its tasks that are not merged are closed,
// and the issues of both are closed with a comment on the goal's, unless the
// goal's issue is a person's ticket, which the route's canceled mapping
// decides on. A task
// whose pull request is merged, on GitHub too, is moved to merged instead:
// merged pull requests are never undone. A goal that is done is left alone,
// and a repeated call completes what a failed one left open.
func (a *actions) settleGoal(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	if g.Status == domain.GoalDone {
		return map[string]string{"status": string(g.Status)}, nil
	}
	if err := a.cancelCouncils(ctx, g); err != nil {
		return nil, err
	}
	// What agents proposed for the knowledge base during the goal is not
	// kept: the learn step that would have sent it will not run.
	if a.Proposals != nil {
		b := proposals.LearnBatch{AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.ID}
		if err := a.Proposals.LearnSettle(ctx, b, true); err != nil {
			return nil, fmt.Errorf("reject the proposals of %s: %w", g.Ref(), err)
		}
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	tasks, err := a.Store.Tasks().List(ctx, g.AccountID, store.TaskFilter{GoalID: g.ID})
	if err != nil {
		return nil, fmt.Errorf("list the tasks of %s: %w", g.Ref(), err)
	}
	issues, err := a.settleTasks(ctx, tasks)
	if err != nil {
		return nil, err
	}
	if g.Status != domain.GoalCanceled {
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, g.Status, domain.GoalCanceled); err != nil {
			return nil, fmt.Errorf("cancel %s: %w", g.Ref(), err)
		}
	}
	for _, n := range issues {
		if err := a.closeIssue(ctx, p.Repo, n, "", ""); err != nil {
			return nil, err
		}
	}
	ticket, err := a.issueIsTicket(ctx, g)
	if err != nil {
		return nil, err
	}
	if ticket { // a person's issue: the route's canceled mapping decides
		return map[string]string{"status": string(domain.GoalCanceled)}, nil
	}
	reason := "Forecrew stopped working on this goal: the run failed or was canceled. Pull requests that were merged stay merged."
	if err := a.closeIssue(ctx, p.Repo, g.IssueNumber, fmt.Sprintf("<!-- forecrew:settle:%s -->", g.ID), reason); err != nil {
		return nil, err
	}
	return map[string]string{"status": string(domain.GoalCanceled)}, nil
}

// issueIsTicket reports whether the goal's issue is the ticket it came from,
// live or just ended.
func (a *actions) issueIsTicket(ctx context.Context, g domain.Goal) (bool, error) {
	l, err := a.Store.TicketLinks().GetByGoal(ctx, g.AccountID, g.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("find the ticket of %s: %w", g.Ref(), err)
	}
	return g.IssueNumber != 0 && l.IssueNumber == g.IssueNumber, nil
}

// settleTasks moves the tasks that are not merged to merged when their pull
// request is, else to closed, and returns the issue numbers of the closed.
func (a *actions) settleTasks(ctx context.Context, tasks []domain.Task) ([]int, error) {
	var issues []int
	for _, t := range tasks {
		if t.Status == domain.TaskMerged {
			continue
		}
		merged, err := a.prMerged(ctx, t)
		if err != nil {
			return nil, err
		}
		if merged {
			// The PR merged, by a human or before a later step failed: the
			// task is merged whatever happens to the goal.
			if _, err := a.moveTask(ctx, t, domain.TaskMerged); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := a.moveTask(ctx, t, domain.TaskClosed); err != nil {
			return nil, err
		}
		issues = append(issues, t.IssueNumber)
	}
	return issues, nil
}

// prMerged reports whether a pull request of the task is merged: the change
// says so, or GitHub does for a change still recorded as open (a human may
// have merged it).
func (a *actions) prMerged(ctx context.Context, t domain.Task) (bool, error) {
	changes, err := a.Store.Changes().List(ctx, t.AccountID, store.ChangeFilter{TaskID: t.ID})
	if err != nil {
		return false, fmt.Errorf("list the pull requests of %s: %w", t.Ref(), err)
	}
	for _, c := range changes {
		switch c.State {
		case domain.ChangeMerged:
			return true, nil
		case domain.ChangeOpen:
			pr, err := a.Tracker.GetPR(ctx, c.Repo, c.Number)
			if err != nil {
				return false, fmt.Errorf("get pull request %s#%d: %w", c.Repo, c.Number, err)
			}
			if pr.Merged {
				return true, nil
			}
		}
	}
	return false, nil
}

// closeIssue closes issue number of repo unless it is closed already, with
// the comment first when it is not empty; a comment that holds marker is
// posted once. An issue number of 0 (no issue) is skipped.
func (a *actions) closeIssue(ctx context.Context, repo string, number int, marker, comment string) error {
	if number == 0 {
		return nil
	}
	issue, err := a.Tracker.GetIssue(ctx, repo, number)
	if err != nil {
		return fmt.Errorf("get issue %s#%d: %w", repo, number, err)
	}
	if issue.State == "closed" {
		return nil
	}
	if comment != "" {
		if err := a.commentOnce(ctx, repo, number, marker, marker+"\n"+comment); err != nil {
			return err
		}
	}
	closed := "closed"
	if _, err := a.Tracker.UpdateIssue(ctx, repo, number, github.IssueUpdate{State: &closed}); err != nil {
		return fmt.Errorf("close issue %s#%d: %w", repo, number, err)
	}
	return nil
}

// regressionTests names the tests the test engineer wrote for the tasks, as
// "`id` (suite, file)".
func (a *actions) regressionTests(ctx context.Context, tasks []domain.Task) ([]string, error) {
	var named []string
	for _, t := range tasks {
		docs, err := a.testerOutputs(ctx, t)
		if err != nil {
			return nil, err
		}
		var out testsMapOutput
		unionTests(docs, &out)
		for _, x := range out.Tests {
			named = append(named, fmt.Sprintf("`%s` (%s, %s)", strings.NewReplacer("`", "'", "\n", " ").Replace(x.ID), x.Suite, x.File))
		}
	}
	return slices.Compact(named), nil
}

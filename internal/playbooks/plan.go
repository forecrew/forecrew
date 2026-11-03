package playbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// planTask is a task of the architect's breakdown.
type planTask struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Criteria    []string `json:"criteria"`
	Needs       []string `json:"needs"`
	Size        string   `json:"size"`
	DependsOn   []int    `json:"depends_on"`
}

// planDoc is the output of the architect role (packs/schemas/task-breakdown.json).
type planDoc struct {
	Summary     string                    `json:"summary"`
	Tasks       []planTask                `json:"tasks"`
	Assumptions []domain.StatedAssumption `json:"assumptions"`
	Escalation  *string                   `json:"escalation"`
}

// check reports why the breakdown cannot be used for spec: the architect
// escalated, there is no task, a task has no title, names a criterion the
// spec does not have, depends on a task that does not come before it, or a
// criterion of the spec is left without a task.
func (p planDoc) check(spec specDoc) error {
	if p.Escalation != nil && strings.TrimSpace(*p.Escalation) != "" {
		return fmt.Errorf("the architect needs a human: %s", strings.TrimSpace(*p.Escalation))
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("the plan has no task")
	}
	known := map[string]bool{}
	for _, c := range spec.AcceptanceCriteria {
		known[c.ID] = true
	}
	for i, t := range p.Tasks {
		n := i + 1
		if strings.TrimSpace(t.Title) == "" {
			return fmt.Errorf("task %d has no title", n)
		}
		for _, c := range t.Criteria {
			if !known[c] {
				return fmt.Errorf("task %d names criterion %s, which the spec does not have", n, c)
			}
		}
		for _, d := range t.DependsOn {
			if d < 1 || d >= n {
				return fmt.Errorf("task %d depends on task %d, which does not come before it", n, d)
			}
		}
	}
	if u := p.uncovered(spec); len(u) > 0 {
		return fmt.Errorf("the plan does not cover these acceptance criteria: %s", strings.Join(u, ", "))
	}
	return nil
}

// uncovered returns the criteria of spec that no task delivers.
func (p planDoc) uncovered(spec specDoc) []string {
	var out []string
	for _, c := range spec.AcceptanceCriteria {
		if !slices.ContainsFunc(p.Tasks, func(t planTask) bool { return slices.Contains(t.Criteria, c.ID) }) {
			out = append(out, c.ID)
		}
	}
	return out
}

// taskRef is a task as the playbook's per-task steps see it.
type taskRef struct {
	ID          uuid.UUID `json:"id"`
	Ref         string    `json:"ref"`
	Number      int64     `json:"number"`
	Seq         int       `json:"seq"`
	Title       string    `json:"title"`
	IssueNumber int       `json:"issue_number"`
	Branch      string    `json:"branch"`
	// Needs are the worker labels of the developer job: the task's own and
	// the goal's platforms.
	Needs    []string `json:"needs"`
	Criteria []string `json:"criteria"`
}

// planOutput is the output of goal.plan.
type planOutput struct {
	// Valid is false, with Errors for the architect's rerun, when the plan
	// cannot be used; no task is stored then.
	Valid  bool      `json:"valid"`
	Errors string    `json:"errors"`
	Tasks  []taskRef `json:"tasks"`
}

// taskBranch is the branch a task is developed on.
func taskBranch(t domain.Task) string { return "agent/" + t.Ref() }

// goalPlan is the action goal.plan. From with.plan, the architect's output
// for the approved spec with.spec_id, it stores one task per entry in order
// and a GitHub issue for each (labeled forecrew:task and needs:<label>, a
// sub-issue of the goal's), comments the plan on the goal issue and moves
// the goal from spec_review to planned. with.spec is the product owner's
// output the criteria are taken from. A task or issue that exists already is
// kept, so a repeated call completes the plan. A plan that cannot be used
// (see planDoc.check) stores nothing: the output is not valid and its errors
// are the feedback of the architect's rerun, and are commented on the goal
// issue once per call. It outputs the tasks in order.
// The goal's with.platforms are added to the needs of every task.
func (a *actions) goalPlan(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
		SpecID uuid.UUID `json:"spec_id"`
		// Platforms are the goal's worker labels, added to the needs of
		// every task.
		Platforms []string        `json:"platforms"`
		Plan      json.RawMessage `json:"plan"`
		Spec      json.RawMessage `json:"spec"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var plan planDoc
	if err := parseOutput(with.Plan, &plan); err != nil {
		return nil, fmt.Errorf("with: plan: %w", err)
	}
	var spec specDoc
	if err := parseOutput(with.Spec, &spec); err != nil {
		return nil, fmt.Errorf("with: spec: %w", err)
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if err := plan.check(spec); err != nil {
		// The human who is asked after two bad plans sees why on the issue.
		marker := fmt.Sprintf("<!-- forecrew:plan-problem:%s -->", call.Key)
		note := marker + "\nThe architect's plan cannot be used: " + err.Error() + "."
		if err := a.commentOnce(ctx, p.Repo, g.IssueNumber, marker, note); err != nil {
			return nil, err
		}
		return planOutput{Errors: err.Error() + ". Fix the plan.", Tasks: []taskRef{}}, nil
	}
	switch g.Status {
	case domain.GoalSpecReview, domain.GoalPlanned:
	default:
		return nil, fmt.Errorf("%s is %s, not in spec review", g.Ref(), g.Status)
	}
	existing, err := a.Store.Tasks().List(ctx, g.AccountID, store.TaskFilter{GoalID: g.ID, SpecID: with.SpecID})
	if err != nil {
		return nil, fmt.Errorf("list the tasks of %s: %w", g.Ref(), err)
	}
	out := planOutput{Valid: true}
	refs := make([]string, 0, len(plan.Tasks)) // TASK-n by position
	for i, pt := range plan.Tasks {
		t, err := a.planTask(ctx, p, g, with.SpecID, existing, i+1, pt, spec, refs)
		if err != nil {
			return nil, err
		}
		refs = append(refs, t.Ref())
		out.Tasks = append(out.Tasks, taskRef{
			ID: t.ID, Ref: t.Ref(), Number: t.Number, Seq: t.Seq, Title: t.Title, IssueNumber: t.IssueNumber, Branch: taskBranch(t),
			Needs: domain.NormalizeLabels(append(slices.Clone(pt.Needs), with.Platforms...)), Criteria: pt.Criteria,
		})
	}
	if err := a.commentPlan(ctx, p.Repo, g, with.SpecID, plan, out.Tasks); err != nil {
		return nil, err
	}
	if g.Status == domain.GoalSpecReview {
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, domain.GoalSpecReview, domain.GoalPlanned); err != nil {
			return nil, fmt.Errorf("move %s to planned: %w", g.Ref(), err)
		}
	}
	return out, nil
}

// planTask returns the task at position seq, creating the task and its issue
// when they do not exist yet. refs holds the refs of the earlier tasks.
func (a *actions) planTask(ctx context.Context, p domain.Project, g domain.Goal, specID uuid.UUID, existing []domain.Task, seq int, pt planTask, spec specDoc, refs []string) (domain.Task, error) {
	idx := slices.IndexFunc(existing, func(t domain.Task) bool { return t.Seq == seq })
	var t domain.Task
	if idx >= 0 {
		t = existing[idx]
	} else {
		t = domain.Task{
			AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.ID, SpecID: specID, Seq: seq,
			Title: strings.TrimSpace(pt.Title), Body: taskBody(pt, spec, refs), Status: domain.TaskOpen,
		}
		if err := a.Store.Tasks().Create(ctx, &t); err != nil {
			return domain.Task{}, fmt.Errorf("store task %d of %s: %w", seq, g.Ref(), err)
		}
	}
	if t.IssueNumber > 0 {
		return t, nil
	}
	labels := []string{labelTask}
	for _, need := range domain.NormalizeLabels(pt.Needs) {
		if err := a.ensureLabel(ctx, p.Repo, labelNeeds+need, "fbca04", "Worker label the task needs"); err != nil {
			return domain.Task{}, err
		}
		labels = append(labels, labelNeeds+need)
	}
	issue, err := a.Tracker.CreateIssue(ctx, p.Repo, github.IssueInput{
		Title: t.Ref() + ": " + t.Title, Body: t.Body + fmt.Sprintf("\n\nPart of #%d.", g.IssueNumber), Labels: labels,
	})
	if err != nil {
		return domain.Task{}, fmt.Errorf("open the issue of %s: %w", t.Ref(), err)
	}
	t.IssueNumber = issue.Number
	if err := a.Store.Tasks().Update(ctx, &t); err != nil {
		return domain.Task{}, fmt.Errorf("record the issue of %s: %w", t.Ref(), err)
	}
	if err := a.Tracker.AddSubIssue(ctx, p.Repo, g.IssueNumber, issue.Number); err != nil {
		return domain.Task{}, fmt.Errorf("add issue #%d to #%d: %w", issue.Number, g.IssueNumber, err)
	}
	return t, nil
}

// taskBody is the markdown of a task: its description, the criteria it
// delivers with their texts, the tasks it follows and the size estimate.
func taskBody(pt planTask, spec specDoc, refs []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(pt.Description) + "\n")
	var criteria []string
	for _, id := range pt.Criteria {
		criteria = append(criteria, criterionLine(spec, id))
	}
	section(&b, "Acceptance criteria", criteria, "- ")
	var deps []string
	for _, d := range pt.DependsOn {
		deps = append(deps, refs[d-1])
	}
	section(&b, "Depends on", deps, "- ")
	if pt.Size != "" {
		fmt.Fprintf(&b, "\nSize: %s\n", pt.Size)
	}
	return b.String()
}

// criterionLine is the line of a criterion: its ID and text.
func criterionLine(spec specDoc, id string) string {
	for _, c := range spec.AcceptanceCriteria {
		if c.ID == id {
			return fmt.Sprintf("**%s** %s", c.ID, c.Text)
		}
	}
	return "**" + id + "**"
}

// commentPlan comments the plan on the goal issue unless it did already.
func (a *actions) commentPlan(ctx context.Context, repo string, g domain.Goal, specID uuid.UUID, plan planDoc, tasks []taskRef) error {
	marker := fmt.Sprintf("<!-- forecrew:plan:%s -->", specID)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Plan\n\n%s\n", marker, strings.TrimSpace(plan.Summary))
	var lines []string
	for _, t := range tasks {
		lines = append(lines, fmt.Sprintf("%d. #%d %s", t.Seq, t.IssueNumber, t.Title))
	}
	section(&b, "Tasks, in order", lines, "")
	section(&b, "Assumptions", assumptionLines(plan.Assumptions), "- ")
	return a.commentOnce(ctx, repo, g.IssueNumber, marker, b.String())
}

// commentOnce comments body on issue number unless a comment with marker is
// there already.
func (a *actions) commentOnce(ctx context.Context, repo string, number int, marker, body string) error {
	comments, err := a.Tracker.ListComments(ctx, repo, number, time.Time{})
	if err != nil {
		return fmt.Errorf("list the comments of %s#%d: %w", repo, number, err)
	}
	if slices.ContainsFunc(comments, func(c github.Comment) bool { return strings.Contains(c.Body, marker) }) {
		return nil
	}
	if _, err := a.Tracker.CreateComment(ctx, repo, number, body); err != nil {
		return fmt.Errorf("comment on %s#%d: %w", repo, number, err)
	}
	return nil
}

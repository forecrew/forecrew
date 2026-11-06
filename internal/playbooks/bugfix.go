package playbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// bugCriterion is the one acceptance criterion of a bug-fix task.
var bugCriterion = criterion{
	ID:   "AC-1",
	Text: "The failure in the bug report no longer happens. A regression test that failed before the fix passes after it.",
}

// bugSpec is the spec of a bug goal: its report and the one criterion.
func bugSpec(g domain.Goal) specDoc {
	return specDoc{
		Summary:            "Bug report: " + g.Title + "\n\n" + strings.TrimSpace(g.Body),
		AcceptanceCriteria: []criterion{bugCriterion},
	}
}

// bugSpecAction is the action bug.spec. It stores the report of the bug goal
// with.goal_id as the goal's spec, with one acceptance criterion, and moves
// the goal from draft to spec review. A bug has no product owner and no spec
// approval: the report is the spec. A repeated call returns the same spec.
// It outputs spec_id and revision, as goal.spec does.
func (a *actions) bugSpecAction(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.bugGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	specs, err := a.Store.Specs().ListByGoal(ctx, g.AccountID, g.ID)
	if err != nil {
		return nil, fmt.Errorf("list the specs of %s: %w", g.Ref(), err)
	}
	var spec domain.Spec
	if len(specs) > 0 {
		spec = specs[0]
	} else {
		spec = domain.Spec{AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.ID, Revision: 1, Body: bugSpec(g).markdown(1)}
		if err := a.Store.Specs().Create(ctx, &spec); err != nil {
			return nil, fmt.Errorf("store the spec of %s: %w", g.Ref(), err)
		}
	}
	if g.Status == domain.GoalDraft {
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, domain.GoalDraft, domain.GoalSpecReview); err != nil {
			return nil, fmt.Errorf("move %s to spec review: %w", g.Ref(), err)
		}
	}
	return specOutput{SpecID: spec.ID, Revision: spec.Revision}, nil
}

// bugGoal loads the goal of the run's project, which must be a bug.
func (a *actions) bugGoal(ctx context.Context, run domain.WorkflowRun, id uuid.UUID) (domain.Goal, error) {
	g, err := a.loadGoal(ctx, run, id)
	if err != nil {
		return domain.Goal{}, err
	}
	if g.Kind != domain.GoalBug {
		return domain.Goal{}, fmt.Errorf("%s is a %s, not a bug", g.Ref(), g.Kind.OrDefault())
	}
	return g, nil
}

// bugPlanAction is the action bug.plan. It stores the one task of the bug
// goal with.goal_id for the spec with.spec_id, with the GitHub issue of
// goal.plan, and moves the goal to planned. The task is the unit the test
// engineer, the developer and the verification work on, as in goal-v2. It
// outputs the task as goal.plan does. A repeated call completes what an
// earlier one left open.
func (a *actions) bugPlanAction(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID    uuid.UUID `json:"goal_id"`
		SpecID    uuid.UUID `json:"spec_id"`
		Platforms []string  `json:"platforms"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.bugGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
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
	pt := planTask{
		Title: "Fix " + g.Ref(),
		Description: "Fix the bug reported in " + g.Ref() + ". A regression test that reproduces it is written first and locked: " +
			"make it pass without changing it, and keep every other test green. Change only what the bug needs.",
		Criteria: []string{bugCriterion.ID}, Size: "s",
	}
	t, err := a.planTask(ctx, p, g, with.SpecID, existing, 1, pt, bugSpec(g), nil)
	if err != nil {
		return nil, err
	}
	if g.Status == domain.GoalSpecReview {
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, domain.GoalSpecReview, domain.GoalPlanned); err != nil {
			return nil, fmt.Errorf("move %s to planned: %w", g.Ref(), err)
		}
	}
	return planOutput{Valid: true, Tasks: []taskRef{{
		ID: t.ID, Ref: t.Ref(), Number: t.Number, Seq: t.Seq, Title: t.Title, IssueNumber: t.IssueNumber, Branch: taskBranch(t),
		Needs: domain.NormalizeLabels(with.Platforms), Criteria: pt.Criteria,
	}}}, nil
}

// bugBrief is the action bug.brief: the feedback of the next attempt of the
// test engineer, with.problem (what bug.reproduced found wrong in the last
// one) and with.note (what a person added at the gate), separated by a blank
// line. Both are empty in the first attempt.
func (a *actions) bugBrief(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Problem string `json:"problem"`
		Note    string `json:"note"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var parts []string
	for _, p := range []string{with.Problem, with.Note} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return map[string]string{"feedback": strings.Join(parts, "\n\n")}, nil
}

// reproducedOutput is the output of bug.reproduced.
type reproducedOutput struct {
	// Done is set when the engineer's tests are ready, there is at least one,
	// and every one failed on the base branch.
	Done bool `json:"done"`
	// Feedback says what is wrong when Done is false; it goes to the test
	// engineer's next attempt.
	Feedback string `json:"feedback"`
	// IfStuck holds the text of a needs_you gate, or is empty when Done: a
	// foreach over it asks a human for more detail.
	IfStuck []string `json:"if_stuck"`
}

// bugReproduced is the action bug.reproduced. It decides whether the
// regression test of a bug is good: with.ready is tests.map's verdict,
// with.tests its tests and with.base the output of tests.base_check_result.
// A test that passes on the base branch does not reproduce the bug; no test
// at all, or a base check that did not run everywhere (with.missing says
// where), is not good either. In both cases if_stuck has the text for a
// human, who can add detail, and feedback the same for the next attempt.
func (a *actions) bugReproduced(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Ready    bool      `json:"ready"`
		Feedback string    `json:"feedback"`
		Tests    []newTest `json:"tests"`
		// Base and Missing are empty when the tests were not ready and the
		// base check did not run.
		Base    json.RawMessage `json:"base"`
		Missing string          `json:"missing"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var base baseCheckResult
	if with.Ready {
		if err := json.Unmarshal(with.Base, &base); err != nil {
			return nil, fmt.Errorf("with: base: %w", err)
		}
	}
	var problems []string
	var passed []string
	for _, t := range base.NewTests {
		if !t.FailedOnBase {
			passed = append(passed, contextpack.OneLine(t.ID))
		}
	}
	switch {
	case !with.Ready:
		problems = append(problems, "The test engineer has no regression test yet.\n\n"+with.Feedback)
	case len(with.Tests) == 0:
		problems = append(problems, "The test engineer wrote no test that reproduces the bug.")
	}
	if len(passed) > 0 {
		problems = append(problems, "These tests passed on the base branch, so they do not reproduce the bug:\n"+contextpack.WrapUntrusted("test ids", strings.Join(passed, "\n")))
	} else if with.Ready && len(with.Tests) > 0 && !base.OK {
		problems = append(problems, "The tests were not checked on the base branch everywhere, so it is not known that they reproduce the bug."+
			strings.TrimRight("\n"+with.Missing, "\n"))
	}
	out := reproducedOutput{Done: len(problems) == 0, IfStuck: []string{}}
	if !out.Done {
		out.Feedback = strings.Join(problems, "\n\n")
		out.IfStuck = []string{out.Feedback}
	}
	return out, nil
}

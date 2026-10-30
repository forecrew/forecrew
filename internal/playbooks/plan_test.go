package playbooks_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// planOutput is an architect output with the given tasks.
func planOutput(tasks ...map[string]any) map[string]any {
	ts := make([]any, len(tasks))
	for i, t := range tasks {
		ts[i] = t
	}
	return map[string]any{
		"summary": "Payments first, then the form.", "tasks": ts, "escalation": nil,
		"assumptions": []any{map[string]any{"text": "One PSP", "confidence": "high", "impact": "low"}},
	}
}

func planTask(title string, criteria []string, needs []string, depends ...int) map[string]any {
	return map[string]any{
		"title": title, "description": "Do " + title + ".", "criteria": criteria, "needs": needs, "size": "s", "depends_on": depends,
	}
}

// planned is an env whose goal has an issue and an approved spec.
type planned struct {
	*env
	goal  domain.Goal
	issue int
	spec  domain.Spec
}

func newPlanned(t *testing.T, platforms ...string) *planned {
	t.Helper()
	v := newEnv(t)
	g := v.newGoal(t, platforms...)
	n := v.intake(t, g)
	out, err := v.call(t, "goal.spec", map[string]any{
		"goal_id": g.ID.String(), "job_id": v.newJob(t, "product-owner", g).String(),
		"spec": specOutput("A payment succeeds.", "A declined card shows an error."),
	})
	must(t, err)
	spec, err := v.st.Specs().Get(t.Context(), v.account, uuidOf(t, toMap(t, out)["spec_id"]))
	must(t, err)
	return &planned{env: v, goal: g, issue: n, spec: spec}
}

func (p *planned) with(plan map[string]any) map[string]any {
	return map[string]any{
		"goal_id": p.goal.ID.String(), "spec_id": p.spec.ID.String(), "plan": plan, "platforms": p.goal.Platforms,
		"spec": specOutput("A payment succeeds.", "A declined card shows an error."),
	}
}

func TestGoalPlan(t *testing.T) {
	p := newPlanned(t, "Web")
	plan := planOutput(
		planTask("Charge cards", []string{"AC-1"}, []string{"go", "linux"}),
		planTask("Show declines", []string{"AC-2"}, []string{"node"}, 1),
	)
	out, err := p.callKey(t, "goal.plan", "k", p.with(plan))
	must(t, err)
	m := toMap(t, out)["tasks"].([]any)
	if len(m) != 2 {
		t.Fatalf("output = %v", toMap(t, out))
	}
	first, second := m[0].(map[string]any), m[1].(map[string]any)
	if first["ref"] != "TASK-1" || first["branch"] != "agent/TASK-1" || first["seq"] != 1.0 || first["title"] != "Charge cards" ||
		!slices.Equal(anyStrings(first["needs"]), []string{"go", "linux", "web"}) || !slices.Equal(anyStrings(first["criteria"]), []string{"AC-1"}) {
		t.Errorf("first task = %v", first)
	}
	if !slices.Equal(anyStrings(second["needs"]), []string{"node", "web"}) || second["seq"] != 2.0 {
		t.Errorf("second task = %v", second)
	}

	tasks, err := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	must(t, err)
	if len(tasks) != 2 || tasks[0].Status != domain.TaskOpen || tasks[1].SpecID != p.spec.ID || tasks[1].Seq != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}
	for _, want := range []string{"Do Show declines.", "**AC-2** A declined card shows an error.", "## Depends on\n\n- TASK-1"} {
		if !strings.Contains(tasks[1].Body, want) {
			t.Errorf("task body lacks %q:\n%s", want, tasks[1].Body)
		}
	}
	issue, err := p.tracker.GetIssue(t.Context(), "acme/app", tasks[0].IssueNumber)
	must(t, err)
	wantLabels := []string{"forecrew:task", "needs:go", "needs:linux"}
	if issue.Title != "TASK-1: Charge cards" || !slices.Equal(issue.Labels, wantLabels) || !strings.Contains(issue.Body, "Part of #"+itoa(p.issue)) {
		t.Errorf("issue = %+v", issue)
	}
	subs, err := p.tracker.ListSubIssues(t.Context(), "acme/app", p.issue)
	must(t, err)
	if len(subs) != 2 || subs[0].Number != tasks[0].IssueNumber || subs[1].Number != tasks[1].IssueNumber {
		t.Errorf("sub-issues = %+v", subs)
	}
	if _, err := p.tracker.Label("acme/app", "needs:node"); err != nil {
		t.Errorf("needs label: %v", err)
	}
	comments, _ := p.tracker.ListComments(t.Context(), "acme/app", p.issue, t0.Add(-1))
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "## Plan") || !strings.Contains(comments[0].Body, "1. #") || strings.Contains(comments[0].Body, "No task delivers") {
		t.Errorf("comments = %+v", comments)
	}
	if got := p.goalNow(t, p.goal.ID).Status; got != domain.GoalPlanned {
		t.Errorf("goal status = %s", got)
	}

	// A repeated call changes nothing.
	again, err := p.callKey(t, "goal.plan", "k", p.with(plan))
	must(t, err)
	tasks2, _ := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	subs, _ = p.tracker.ListSubIssues(t.Context(), "acme/app", p.issue)
	comments, _ = p.tracker.ListComments(t.Context(), "acme/app", p.issue, t0.Add(-1))
	if len(tasks2) != 2 || len(subs) != 2 || len(comments) != 1 || toMap(t, again)["tasks"].([]any)[0].(map[string]any)["id"] != first["id"] {
		t.Errorf("after the second call: %d tasks, %d sub-issues, %d comments", len(tasks2), len(subs), len(comments))
	}
}

// A call that stopped after the first task completes the plan.
func TestGoalPlanResumes(t *testing.T) {
	p := newPlanned(t)
	plan := planOutput(planTask("One", []string{"AC-1"}, nil), planTask("Two", []string{"AC-2"}, nil))
	p.wrapped = &failIssue{Tracker: p.tracker, n: 2}
	p.register()
	_, err := p.call(t, "goal.plan", p.with(plan))
	wantErr(t, err, "open the issue of TASK-2")
	p.wrapped = nil
	p.register()
	_, err = p.call(t, "goal.plan", p.with(plan))
	must(t, err)
	tasks, _ := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	subs, _ := p.tracker.ListSubIssues(t.Context(), "acme/app", p.issue)
	if len(tasks) != 2 || len(subs) != 2 || tasks[1].IssueNumber == 0 {
		t.Errorf("%d tasks, %d sub-issues", len(tasks), len(subs))
	}
}

// failIssue fails the n-th CreateIssue call, then works again.
type failIssue struct {
	github.Tracker
	n, calls int
}

func (f *failIssue) CreateIssue(ctx context.Context, repo string, in github.IssueInput) (github.Issue, error) {
	f.calls++
	if f.calls == f.n {
		return github.Issue{}, errors.New("boom")
	}
	return f.Tracker.CreateIssue(ctx, repo, in)
}

// A plan that leaves a criterion uncovered is sent back to the architect
// with the list, every time; nothing is stored.
func TestGoalPlanUncoveredCriteria(t *testing.T) {
	p := newPlanned(t)
	with := p.with(planOutput(planTask("One", []string{"AC-1"}, nil)))
	for range 3 {
		out, err := p.call(t, "goal.plan", with)
		must(t, err)
		m := toMap(t, out)
		if m["valid"] != false || !strings.Contains(m["errors"].(string), "does not cover these acceptance criteria: AC-2") || len(m["tasks"].([]any)) != 0 {
			t.Fatalf("output = %v", m)
		}
	}
	tasks, _ := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	if len(tasks) != 0 || p.goalNow(t, p.goal.ID).Status != domain.GoalSpecReview {
		t.Errorf("%d tasks stored, goal %s", len(tasks), p.goalNow(t, p.goal.ID).Status)
	}
	with["plan"] = planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil))
	out, err := p.call(t, "goal.plan", with)
	must(t, err)
	if toMap(t, out)["valid"] != true || toMap(t, out)["errors"] != "" {
		t.Errorf("covering plan = %v", toMap(t, out))
	}
}

func TestGoalPlanErrors(t *testing.T) {
	p := newPlanned(t)
	escalated := planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil))
	escalated["escalation"] = "AC-2 is ambiguous."
	tests := []struct {
		name string
		plan map[string]any
		want string
	}{
		{"escalation", escalated, "the architect needs a human: AC-2 is ambiguous."},
		{"no task", planOutput(), "no task"},
		{"no title", planOutput(planTask(" ", []string{"AC-1"}, nil)), "task 1 has no title"},
		{"unknown criterion", planOutput(planTask("One", []string{"AC-9"}, nil)), "criterion AC-9, which the spec does not have"},
		{"forward dependency", planOutput(planTask("One", []string{"AC-1"}, nil, 2), planTask("Two", []string{"AC-2"}, nil)), "task 1 depends on task 2"},
		{"self dependency", planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil), planTask("Two", []string{"AC-2"}, nil, 2)), "task 2 depends on task 2"},
		{"zero dependency", planOutput(planTask("One", []string{"AC-1"}, nil, 0)), "task 1 depends on task 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := p.call(t, "goal.plan", p.with(tt.plan))
			must(t, err)
			m := toMap(t, out)
			if m["valid"] != false || !strings.Contains(m["errors"].(string), tt.want) {
				t.Errorf("output = %v, want errors with %q", m, tt.want)
			}
		})
	}
	tasks, _ := p.st.Tasks().List(t.Context(), p.account, store.TaskFilter{GoalID: p.goal.ID})
	if len(tasks) != 0 {
		t.Errorf("%d tasks stored by invalid plans", len(tasks))
	}

	good := p.with(planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil)))
	other := p.with(planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil)))
	other["goal_id"] = domain.NewID().String()
	_, err := p.call(t, "goal.plan", other)
	wantErr(t, err, "get goal")
	draft := p.newGoal(t)
	other["goal_id"] = draft.ID.String()
	_, err = p.call(t, "goal.plan", other)
	wantErr(t, err, "not in spec review")
	for _, field := range []string{"plan", "spec"} {
		bad := p.with(planOutput(planTask("One", []string{"AC-1", "AC-2"}, nil)))
		delete(bad, field)
		_, err = p.call(t, "goal.plan", bad)
		wantErr(t, err, "with: "+field+": is empty")
		bad[field] = "text"
		_, err = p.call(t, "goal.plan", bad)
		wantErr(t, err, "with: "+field+": json:")
	}
	good["x"] = 1
	_, err = p.call(t, "goal.plan", good)
	wantErr(t, err, "unknown field")
}

func TestGoalPlanGitHubErrors(t *testing.T) {
	plan := planOutput(planTask("One", []string{"AC-1", "AC-2"}, []string{"go"}))
	for _, method := range []string{"EnsureLabel", "AddSubIssue", "ListComments", "CreateComment"} {
		t.Run(method, func(t *testing.T) {
			p := newPlanned(t)
			p.wrapped = failing{Tracker: p.tracker, errs: map[string]error{method: errors.New("boom")}}
			p.register()
			_, err := p.call(t, "goal.plan", p.with(plan))
			wantErr(t, err, "boom")
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func uuidOf(t *testing.T, v any) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(v.(string))
	must(t, err)
	return id
}

func anyStrings(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}

package playbooks_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

func newJobCreator(v *env) *testkit.JobCreator { return &testkit.JobCreator{Store: v.st} }

// newJob stores a queued job of role for the goal and returns its ID.
func (v *env) newJob(t *testing.T, role string, goal domain.Goal) uuid.UUID {
	t.Helper()
	id, err := newJobCreator(v).Create(t.Context(), workflow.JobRequest{
		AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Role: role, GoalID: goal.ID,
	})
	must(t, err)
	return id
}

// specOutput is a product-owner output; the criterion texts differ per call.
func specOutput(texts ...string) map[string]any {
	var criteria []any
	for i, text := range texts {
		criteria = append(criteria, map[string]any{"id": "AC-" + string(rune('1'+i)), "text": text})
	}
	return map[string]any{
		"summary":             "Customers pay by card at checkout.",
		"acceptance_criteria": criteria,
		"scope":               map[string]any{"in": []any{"card payments"}, "out": []any{"refunds"}},
		"assumptions":         []any{map[string]any{"statement": "Stripe is the processor", "confidence": "medium", "impact": "high", "verify_by": "test"}},
		"open_questions":      []any{"Which currencies?"},
	}
}

// intake runs goal.intake for the goal and returns its issue number.
func (v *env) intake(t *testing.T, g domain.Goal) int {
	t.Helper()
	out, err := v.call(t, "goal.intake", map[string]any{"repo": "acme/app", "goal_id": g.ID.String()})
	must(t, err)
	return int(toMap(t, out)["issue_number"].(float64))
}

func TestGoalSpec(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	n := v.intake(t, g)
	job := v.newJob(t, "product-owner", g)
	with := map[string]any{"goal_id": g.ID.String(), "job_id": job.String(), "spec": specOutput("A card payment succeeds.", "A declined card shows an error.")}

	out, err := v.callKey(t, "goal.spec", "k", with)
	must(t, err)
	m := toMap(t, out)
	if m["revision"] != 1.0 || m["spec_id"] == "" {
		t.Fatalf("output = %v", m)
	}
	summary := m["summary"].(string)
	for _, want := range []string{"Customers pay by card", "2 acceptance criteria, 1 assumptions", "Which currencies?", "/issues/"} {
		if !strings.Contains(summary, want) {
			t.Errorf("gate summary lacks %q: %s", want, summary)
		}
	}
	issue, err := v.tracker.GetIssue(t.Context(), "acme/app", n)
	must(t, err)
	for _, want := range []string{g.Body, "## Spec (revision 1)", "- **AC-1** A card payment succeeds.", "- **AC-2** A declined card shows an error.",
		"## In scope\n\n- card payments", "## Out of scope\n\n- refunds", "Stripe is the processor (confidence medium, impact high)", "## Open questions\n\n- Which currencies?"} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("issue body lacks %q:\n%s", want, issue.Body)
		}
	}
	if got := v.goalNow(t, g.ID).Status; got != domain.GoalSpecReview {
		t.Errorf("goal status = %s", got)
	}

	// The same job gives the same revision, a later job the next one, and
	// the issue shows the latest.
	again, err := v.callKey(t, "goal.spec", "k", with)
	must(t, err)
	if toMap(t, again)["spec_id"] != m["spec_id"] {
		t.Errorf("second call output = %v", toMap(t, again))
	}
	with["job_id"], with["spec"] = v.newJob(t, "product-owner", g).String(), specOutput("Only cards.")
	out, err = v.call(t, "goal.spec", with)
	must(t, err)
	if toMap(t, out)["revision"] != 2.0 {
		t.Errorf("third output = %v", toMap(t, out))
	}
	specs, err := v.st.Specs().ListByGoal(t.Context(), v.account, g.ID)
	must(t, err)
	issue, _ = v.tracker.GetIssue(t.Context(), "acme/app", n)
	if len(specs) != 2 || strings.Contains(issue.Body, "revision 1") || !strings.Contains(issue.Body, "revision 2") {
		t.Errorf("%d specs, issue body:\n%s", len(specs), issue.Body)
	}
}

func TestGoalSpecErrors(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	v.intake(t, g)
	job := v.newJob(t, "product-owner", g).String()
	noIssue := v.newGoal(t)
	started := v.newGoal(t)
	v.intake(t, started)
	must(t, v.st.Goals().UpdateStatus(t.Context(), v.account, started.ID, domain.GoalDraft, domain.GoalCanceled))

	dup := specOutput("a", "b")
	dup["acceptance_criteria"].([]any)[1].(map[string]any)["id"] = "AC-1"
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"no job", map[string]any{"goal_id": g.ID.String(), "spec": specOutput("a")}, "job_id is required"},
		{"no criteria", map[string]any{"goal_id": g.ID.String(), "job_id": job, "spec": specOutput()}, "no acceptance criteria"},
		{"repeated criterion", map[string]any{"goal_id": g.ID.String(), "job_id": job, "spec": dup}, "repeated criterion id"},
		{"unknown goal", map[string]any{"goal_id": domain.NewID().String(), "job_id": job, "spec": specOutput("a")}, "get goal"},
		{"goal without an issue", map[string]any{"goal_id": noIssue.ID.String(), "job_id": v.newJob(t, "product-owner", noIssue).String(), "spec": specOutput("a")}, "write the spec into issue"},
		{"canceled goal", map[string]any{"goal_id": started.ID.String(), "job_id": v.newJob(t, "product-owner", started).String(), "spec": specOutput("a")}, "not in spec review"},
		{"unknown key", map[string]any{"goal_id": g.ID.String(), "job_id": job, "spec": specOutput("a"), "x": 1}, "unknown field"},
		{"no spec", map[string]any{"goal_id": g.ID.String(), "job_id": job}, "with: spec: is empty"},
		{"spec of the wrong type", map[string]any{"goal_id": g.ID.String(), "job_id": job, "spec": "text"}, "with: spec: json:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.call(t, "goal.spec", tt.with)
			wantErr(t, err, tt.want)
		})
	}
}

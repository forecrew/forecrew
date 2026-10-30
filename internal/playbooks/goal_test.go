package playbooks_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

// newGoal stores a draft goal of the env's project.
func (v *env) newGoal(t *testing.T, platforms ...string) domain.Goal {
	t.Helper()
	g := domain.Goal{
		AccountID: v.account, ProjectID: v.project.ID, Title: "Add checkout", Body: "Customers can pay by card.",
		Platforms: platforms, Status: domain.GoalDraft, CreatedBy: "user:ana",
	}
	must(t, v.st.Goals().Create(t.Context(), &g))
	return g
}

func (v *env) goalNow(t *testing.T, id uuid.UUID) domain.Goal {
	t.Helper()
	g, err := v.st.Goals().Get(t.Context(), v.account, id)
	must(t, err)
	return g
}

func TestGoalIntake(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	with := map[string]any{"repo": "acme/app", "goal_id": g.ID.String()}
	out, err := v.callKey(t, "goal.intake", "k", with)
	must(t, err)
	m := toMap(t, out)
	n := int(m["issue_number"].(float64))
	if m["default_branch"] != "main" || n == 0 || m["issue_url"] == "" {
		t.Errorf("output = %v", m)
	}
	issue, err := v.tracker.GetIssue(t.Context(), "acme/app", n)
	must(t, err)
	if issue.Title != g.Ref()+": Add checkout" || issue.Body != g.Body || len(issue.Labels) != 1 || issue.Labels[0] != "forecrew:goal" {
		t.Errorf("issue = %+v", issue)
	}
	for _, l := range []string{"forecrew:goal", "forecrew:task"} {
		if _, err := v.tracker.Label("acme/app", l); err != nil {
			t.Errorf("label %s: %v", l, err)
		}
	}
	if got := v.goalNow(t, g.ID).IssueNumber; got != n {
		t.Errorf("goal issue = %d, want %d", got, n)
	}

	// A repeated call finds the issue instead of opening another.
	again, err := v.callKey(t, "goal.intake", "k", with)
	must(t, err)
	if toMap(t, again)["issue_number"] != m["issue_number"] || toMap(t, again)["issue_url"] != m["issue_url"] {
		t.Errorf("second output = %v, first %v", toMap(t, again), m)
	}
}

func TestGoalIntakeErrors(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	other := domain.Project{AccountID: v.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, v.st.Projects().Create(t.Context(), &other))
	foreign := domain.Goal{AccountID: v.account, ProjectID: other.ID, Title: "x", Status: domain.GoalDraft}
	must(t, v.st.Goals().Create(t.Context(), &foreign))

	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"unknown goal", map[string]any{"repo": "acme/app", "goal_id": domain.NewID().String()}, "get goal"},
		{"goal of another project", map[string]any{"repo": "acme/app", "goal_id": foreign.ID.String()}, "belongs to another project"},
		{"other repo", map[string]any{"repo": "acme/other", "goal_id": g.ID.String()}, "not acme/other"},
		{"unknown key", map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "x": 1}, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.call(t, "goal.intake", tt.with)
			wantErr(t, err, tt.want)
		})
	}

	t.Run("GitHub fails", func(t *testing.T) {
		w := newEnv(t)
		g := w.newGoal(t)
		w.wrapped = failing{Tracker: w.tracker, errs: map[string]error{"EnsureLabel": errors.New("boom")}}
		w.register()
		_, err := w.call(t, "goal.intake", map[string]any{"repo": "acme/app", "goal_id": g.ID.String()})
		wantErr(t, err, "ensure label forecrew:goal")
		w.wrapped = failing{Tracker: w.tracker, errs: map[string]error{"CreateIssue": errors.New("boom")}}
		w.register()
		_, err = w.call(t, "goal.intake", map[string]any{"repo": "acme/app", "goal_id": g.ID.String()})
		wantErr(t, err, "open the issue")
		if got := w.goalNow(t, g.ID).IssueNumber; got != 0 {
			t.Errorf("issue number = %d", got)
		}
	})
}

// failing is a tracker whose listed methods fail with the error under their
// name.
type failing struct {
	github.Tracker
	errs map[string]error
}

func (f failing) EnsureLabel(ctx context.Context, repo string, l github.Label) error {
	if err := f.errs["EnsureLabel"]; err != nil {
		return err
	}
	return f.Tracker.EnsureLabel(ctx, repo, l)
}

func (f failing) CreateIssue(ctx context.Context, repo string, in github.IssueInput) (github.Issue, error) {
	if err := f.errs["CreateIssue"]; err != nil {
		return github.Issue{}, err
	}
	return f.Tracker.CreateIssue(ctx, repo, in)
}

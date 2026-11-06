package playbooks_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

func (v *env) newBug(t *testing.T) domain.Goal {
	t.Helper()
	g := domain.Goal{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.GoalBug, Title: "Crash", Body: "On save.", Status: domain.GoalDraft, CreatedBy: "user:ana"}
	must(t, v.st.Goals().Create(t.Context(), &g))
	return g
}

func TestBugSpecAndPlan(t *testing.T) {
	v := newEnv(t)
	g := v.newBug(t)
	goal := map[string]any{"goal_id": g.ID.String()}
	out, err := v.callKey(t, "bug.spec", "k", goal)
	must(t, err)
	spec := toMap(t, out)
	if spec["revision"] != float64(1) || v.goalNow(t, g.ID).Status != domain.GoalSpecReview {
		t.Fatalf("spec = %v, goal %s", spec, v.goalNow(t, g.ID).Status)
	}
	again, err := v.callKey(t, "bug.spec", "k2", goal)
	must(t, err)
	if toMap(t, again)["spec_id"] != spec["spec_id"] {
		t.Errorf("a repeated call made another spec: %v", again)
	}
	stored, err := v.st.Specs().ListByGoal(t.Context(), v.account, g.ID)
	must(t, err)
	if len(stored) != 1 || !strings.Contains(stored[0].Body, "**AC-1**") || !strings.Contains(stored[0].Body, "On save.") {
		t.Fatalf("specs = %+v", stored)
	}

	// The plan needs the goal issue, which the intake opens.
	_, err = v.callKey(t, "goal.intake", "i", map[string]any{"repo": "acme/app", "goal_id": g.ID.String()})
	must(t, err)
	with := map[string]any{"goal_id": g.ID.String(), "spec_id": spec["spec_id"], "platforms": []string{"Web"}}
	out, err = v.callKey(t, "bug.plan", "p", with)
	must(t, err)
	plan := toMap(t, out)
	tasks, _ := plan["tasks"].([]any)
	if plan["valid"] != true || len(tasks) != 1 || v.goalNow(t, g.ID).Status != domain.GoalPlanned {
		t.Fatalf("plan = %v", plan)
	}
	task := tasks[0].(map[string]any)
	if task["branch"] != "agent/TASK-1" || !slices.Equal(toStrings(task["needs"]), []string{"web"}) || !slices.Equal(toStrings(task["criteria"]), []string{"AC-1"}) {
		t.Errorf("task = %v", task)
	}
	if _, err := v.callKey(t, "bug.plan", "p2", with); err != nil {
		t.Errorf("a repeated plan failed: %v", err)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestBugActionsRefuseOtherGoals(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	for _, name := range []string{"bug.spec", "bug.plan"} {
		_, err := v.call(t, name, map[string]any{"goal_id": g.ID.String()})
		if name == "bug.plan" {
			_, err = v.call(t, name, map[string]any{"goal_id": g.ID.String(), "spec_id": g.ID.String()})
		}
		wantErr(t, err, "not a bug")
	}
	b := v.newBug(t)
	_, err := v.call(t, "bug.plan", map[string]any{"goal_id": b.ID.String(), "spec_id": b.ID.String()})
	wantErr(t, err, "not in spec review")
}

func TestGoalIntakeLabelsABug(t *testing.T) {
	v := newEnv(t)
	g := v.newBug(t)
	out, err := v.call(t, "goal.intake", map[string]any{"repo": "acme/app", "goal_id": g.ID.String()})
	must(t, err)
	issue, err := v.tracker.GetIssue(t.Context(), "acme/app", int(toMap(t, out)["issue_number"].(float64)))
	must(t, err)
	if !slices.Contains(issue.Labels, "forecrew:bug") || !slices.Contains(issue.Labels, "forecrew:goal") {
		t.Errorf("labels = %v", issue.Labels)
	}

	// A person's issue gets the label once.
	own, err := v.tracker.CreateIssue(t.Context(), "acme/app", github.IssueInput{Title: "Crash", Body: "x"})
	must(t, err)
	g2 := v.newBug(t)
	g2.IssueNumber = own.Number
	must(t, v.st.Goals().Update(t.Context(), &g2))
	for range 2 {
		_, err = v.call(t, "goal.intake", map[string]any{"repo": "acme/app", "goal_id": g2.ID.String()})
		must(t, err)
	}
	got, err := v.tracker.GetIssue(t.Context(), "acme/app", own.Number)
	must(t, err)
	if !slices.Contains(got.Labels, "forecrew:bug") {
		t.Errorf("labels = %v", got.Labels)
	}
}

func TestBugReproduced(t *testing.T) {
	v := newEnv(t)
	ok := map[string]any{"new_tests": []any{map[string]any{"id": "p::T", "failed_on_base": true}}, "ok": true}
	passed := map[string]any{"new_tests": []any{map[string]any{"id": "p::T", "failed_on_base": false}}, "ok": false}
	unchecked := map[string]any{"new_tests": []any{}, "ok": false}
	test := []any{map[string]any{"suite": "unit", "id": "p::T", "file": "a_test.go"}}
	for _, tt := range []struct {
		name  string
		with  map[string]any
		done  bool
		wants []string
	}{
		{"failed on base", map[string]any{"ready": true, "tests": test, "base": ok}, true, nil},
		{"passed on base", map[string]any{"ready": true, "tests": test, "base": passed}, false, []string{"passed on the base branch", "p::T"}},
		{"not checked", map[string]any{"ready": true, "tests": test, "base": unchecked, "missing": "No worker for windows."}, false, []string{"not checked", "No worker for windows."}},
		{"not ready", map[string]any{"ready": false, "feedback": "cannot_reproduce: nothing", "tests": []any{}, "base": ok}, false, []string{"no regression test yet", "cannot_reproduce: nothing"}},
		{"no test", map[string]any{"ready": true, "tests": []any{}, "base": ok}, false, []string{"wrote no test"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := v.call(t, "bug.reproduced", tt.with)
			must(t, err)
			m := toMap(t, out)
			stuck, _ := m["if_stuck"].([]any)
			if m["done"] != tt.done || (len(stuck) == 1) == tt.done {
				t.Fatalf("output = %v", m)
			}
			for _, want := range tt.wants {
				if !strings.Contains(m["feedback"].(string), want) || !strings.Contains(stuck[0].(string), want) {
					t.Errorf("output lacks %q: %v", want, m)
				}
			}
		})
	}
	_, err := v.call(t, "bug.reproduced", map[string]any{"nope": 1})
	wantErr(t, err, "nope")
}

func TestBugBrief(t *testing.T) {
	v := newEnv(t)
	for _, tt := range []struct{ problem, note, want string }{
		{"", "", ""},
		{"It passed.", "", "It passed."},
		{"", " More detail. ", "More detail."},
		{"It passed.\n", "More detail.", "It passed.\n\nMore detail."},
	} {
		out, err := v.call(t, "bug.brief", map[string]any{"problem": tt.problem, "note": tt.note})
		must(t, err)
		if got := toMap(t, out)["feedback"]; got != tt.want {
			t.Errorf("brief(%q, %q) = %q, want %q", tt.problem, tt.note, got, tt.want)
		}
	}
	_, err := v.call(t, "bug.brief", map[string]any{"nope": 1})
	wantErr(t, err, "nope")
}

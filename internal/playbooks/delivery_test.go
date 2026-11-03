package playbooks_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

func approveReview() map[string]any {
	return map[string]any{
		"verdict": "approve", "summary": "Looks right.", "comments": []any{},
		"criteria_check": []any{map[string]any{"criterion": "AC-1", "status": "met", "evidence": "TestCharge"}},
		"assumptions":    []any{map[string]any{"statement": "Cards only", "confidence": "low", "impact": "high", "verify_by": "test"}},
	}
}

// assessWith returns the with of goal.assess for the PR.
func assessWith(pr github.PullRequest, checks string, review map[string]any) map[string]any {
	return map[string]any{"repo": "acme/app", "number": pr.Number, "checks": checks, "review": review, "result": developerOutput("Done."), "sha": pr.HeadSHA}
}

func TestGoalAssessReady(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	for _, checks := range []string{"success", "none"} {
		out, err := p.call(t, "goal.assess", assessWith(pr, checks, approveReview()))
		must(t, err)
		m := toMap(t, out)
		if m["done"] != true || m["merged"] != false || m["feedback"] != "" || !slices.Equal(toInts(m["if_ready"]), []int{pr.Number}) {
			t.Fatalf("output = %v", m)
		}
		summary := m["gate_summary"].(string)
		want := []string{pr.URL, pr.HeadSHA, "- **AC-1** met: TestCharge", "Review: approve. Looks right.", "## High-impact assumptions", "- Amounts are in cents", "- Cards only", "squash-merge"}
		if checks == "none" {
			want = append(want, "Checks: none, the repo has no checks.")
		} else {
			want = append(want, "Checks: success.")
		}
		for _, w := range want {
			if !strings.Contains(summary, w) {
				t.Errorf("%s: summary lacks %q:\n%s", checks, w, summary)
			}
		}
	}
}

// The gate names the commit the developer pushed, which the checks and the
// review are about.
func TestGoalAssessNamesTheDevelopersCommit(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	with := assessWith(pr, "success", approveReview())
	with["sha"] = "abc123"
	out, err := p.call(t, "goal.assess", with)
	must(t, err)
	if s := toMap(t, out)["gate_summary"].(string); !strings.Contains(s, " at abc123.") || strings.Contains(s, pr.HeadSHA) {
		t.Errorf("summary:\n%s", s)
	}
}

func TestGoalAssessRework(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	tests := []struct {
		name   string
		checks string
		review map[string]any
		want   []string
		not    []string
	}{
		{"failing checks", "failure", approveReview(), []string{"The checks of " + pr.URL + " failed"}, []string{"reviewer"}},
		{"request changes", "success", reviewOutput("request_changes"), []string{
			"The reviewer asks for changes: The charge is not retried.", "- `pay.go:12` **major**: Retry on timeout.",
			"- `pay.go` **minor**: Split this file.", "- **nit**: Typo in the PR text.", "AC-1 is not met: No retry test."}, []string{"checks of"}},
		{"both", "failure", reviewOutput("request_changes"), []string{"checks of", "reviewer asks"}, nil},
		{"odd verdict", "success", map[string]any{"verdict": "maybe", "summary": "?", "comments": []any{}}, []string{"reviewer asks for changes"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := p.call(t, "goal.assess", assessWith(pr, tt.checks, tt.review))
			must(t, err)
			m := toMap(t, out)
			fb := m["feedback"].(string)
			if m["done"] != false || len(toInts(m["if_ready"])) != 0 || m["gate_summary"] != "" {
				t.Errorf("output = %v", m)
			}
			for _, w := range tt.want {
				if !strings.Contains(fb, w) {
					t.Errorf("feedback lacks %q:\n%s", w, fb)
				}
			}
			for _, w := range tt.not {
				if strings.Contains(fb, w) {
					t.Errorf("feedback has %q:\n%s", w, fb)
				}
			}
		})
	}
}

func TestGoalAssessInvalidInput(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	_, err := p.call(t, "goal.assess", assessWith(pr, "pending", approveReview()))
	wantErr(t, err, `checks "pending" must be`)
	bad := assessWith(pr, "success", approveReview())
	delete(bad, "review")
	_, err = p.call(t, "goal.assess", bad)
	wantErr(t, err, "with: review: is empty")
	bad = assessWith(pr, "success", approveReview())
	delete(bad, "result")
	_, err = p.call(t, "goal.assess", bad)
	wantErr(t, err, "with: result: is empty")
	bad["x"] = 1
	_, err = p.call(t, "goal.assess", bad)
	wantErr(t, err, "unknown field")
	_, err = p.call(t, "goal.assess", map[string]any{"repo": "acme/app", "number": 999})
	wantErr(t, err, "get pull request acme/app#999")
}

func TestGoalAssessMergedOrClosedOnGitHub(t *testing.T) {
	p := newPlanned(t)
	pr := p.openPR(t)
	must(t, p.tracker.ClosePR("acme/app", pr.Number))
	_, err := p.call(t, "goal.assess", map[string]any{"repo": "acme/app", "number": pr.Number})
	wantErr(t, err, "was closed on GitHub")

	q := newPlanned(t)
	pr = q.openPR(t)
	_, err = q.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	out, err := q.call(t, "goal.assess", map[string]any{"repo": "acme/app", "number": pr.Number})
	must(t, err)
	m := toMap(t, out)
	if m["done"] != true || m["merged"] != true || len(toInts(m["if_ready"])) != 0 {
		t.Errorf("output = %v", m)
	}
}

func toInts(v any) []int {
	var out []int
	for _, e := range v.([]any) {
		out = append(out, int(e.(float64)))
	}
	return out
}

func TestGoalIfApproved(t *testing.T) {
	p := newEnv(t)
	tests := []struct {
		decision string
		want     []int
	}{
		{"approved", []int{7}},
		{"changes_requested", nil},
		{"rejected", nil},
		{"", nil},
	}
	for _, tt := range tests {
		out, err := p.call(t, "goal.if_approved", map[string]any{"decision": tt.decision, "number": 7})
		must(t, err)
		if got := toInts(toMap(t, out)["if_approved"]); !slices.Equal(got, tt.want) {
			t.Errorf("%q: if_approved = %v, want %v", tt.decision, got, tt.want)
		}
	}
	_, err := p.call(t, "goal.if_approved", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func TestGoalTaskDone(t *testing.T) {
	setup := func(t *testing.T) (*planned, domain.Task, github.PullRequest) {
		p := newPlanned(t)
		task := p.plan(t, "One")[0]
		must(t, p.st.Tasks().UpdateStatus(t.Context(), p.account, task.ID, domain.TaskOpen, domain.TaskInProgress))
		return p, task, p.openPR(t)
	}
	with := func(task domain.Task, pr github.PullRequest, feedback string) map[string]any {
		return map[string]any{"task_id": task.ID.String(), "repo": "acme/app", "number": pr.Number, "feedback": feedback}
	}

	t.Run("merged", func(t *testing.T) {
		p, task, pr := setup(t)
		sha, err := p.tracker.MergePR(t.Context(), "acme/app", pr.Number, github.MergeInput{})
		must(t, err)
		out, err := p.call(t, "goal.task_done", with(task, pr, ""))
		must(t, err)
		m := toMap(t, out)
		if m["done"] != true || m["merged"] != true || m["sha"] != sha || p.taskNow(t, task.ID).Status != domain.TaskMerged {
			t.Errorf("output = %v, task %s", m, p.taskNow(t, task.ID).Status)
		}
		if _, err := p.call(t, "goal.task_done", with(task, pr, "")); err != nil {
			t.Errorf("repeated call: %v", err)
		}
	})

	t.Run("changes requested", func(t *testing.T) {
		p, task, pr := setup(t)
		out, err := p.call(t, "goal.task_done", with(task, pr, "  Use cents. "))
		must(t, err)
		m := toMap(t, out)
		if m["done"] != false || m["merged"] != false || m["feedback"] != "Use cents." || p.taskNow(t, task.ID).Status != domain.TaskInProgress {
			t.Errorf("output = %v", m)
		}
		out, err = p.call(t, "goal.task_done", with(task, pr, ""))
		must(t, err)
		if !strings.Contains(toMap(t, out)["feedback"].(string), "without saying which") {
			t.Errorf("output = %v", out)
		}
	})

	t.Run("errors", func(t *testing.T) {
		p, task, pr := setup(t)
		must(t, p.tracker.ClosePR("acme/app", pr.Number))
		_, err := p.call(t, "goal.task_done", with(task, pr, ""))
		wantErr(t, err, "was closed without merging")
		_, err = p.call(t, "goal.task_done", map[string]any{"task_id": task.ID.String(), "repo": "acme/app", "number": 999})
		wantErr(t, err, "get pull request acme/app#999")
		_, err = p.call(t, "goal.task_done", map[string]any{"task_id": domain.NewID().String()})
		wantErr(t, err, "get task")
		_, err = p.call(t, "goal.task_done", map[string]any{"x": 1})
		wantErr(t, err, "unknown field")
	})
}

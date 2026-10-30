package playbooks_test

import (
	"maps"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

func prWith(over map[string]any) map[string]any {
	with := map[string]any{"repo": "acme/app", "title": "Add profile", "body": "First body", "head": "agent/onboarding", "base": "main"}
	maps.Copy(with, over)
	return with
}

func TestOpenOrUpdatePR(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	run := func(with map[string]any) map[string]any {
		t.Helper()
		out, err := v.call(t, "github.open_or_update_pr", with)
		must(t, err)
		return toMap(t, out)
	}

	first := run(prWith(nil))
	number := int(first["number"].(float64))
	pr, err := v.tracker.GetPR(ctx, "acme/app", number)
	must(t, err)
	if pr.Title != "Add profile" || pr.Body != "First body" || pr.Head != "agent/onboarding" || pr.Base != "main" {
		t.Errorf("pr = %+v", pr)
	}
	if first["head_sha"] != pr.HeadSHA || first["url"] != pr.URL {
		t.Errorf("output = %v", first)
	}
	c, err := v.st.Changes().GetByPR(ctx, v.account, "acme/app", number)
	must(t, err)
	if c.ProjectID != v.project.ID || c.HeadSHA != pr.HeadSHA || c.Branch != "agent/onboarding" || c.BaseRef != "main" || c.State != domain.ChangeOpen || c.URL != pr.URL {
		t.Errorf("change = %+v", c)
	}

	// A new push and a second call update the same PR and change.
	must(t, v.tracker.SetHeadSHA("acme/app", number, "abc123"))
	second := run(prWith(map[string]any{"title": "Add profile v2", "body": "Second body"}))
	if int(second["number"].(float64)) != number || second["head_sha"] != "abc123" {
		t.Errorf("second output = %v", second)
	}
	pr, _ = v.tracker.GetPR(ctx, "acme/app", number)
	if pr.Title != "Add profile v2" || pr.Body != "Second body" {
		t.Errorf("updated pr = %+v", pr)
	}
	changes, err := v.st.Changes().List(ctx, v.account, store.ChangeFilter{ProjectID: v.project.ID})
	must(t, err)
	if len(changes) != 1 || changes[0].HeadSHA != "abc123" {
		t.Errorf("changes = %+v", changes)
	}
}

func TestOpenOrUpdatePRErrors(t *testing.T) {
	v := newEnv(t)
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"unknown key", prWith(map[string]any{"draft": true}), "unknown field"},
		{"no title", prWith(map[string]any{"title": ""}), "title is required"},
		{"no base", prWith(map[string]any{"base": ""}), "base is required"},
		{"app not installed", prWith(map[string]any{"repo": "acme/other"}), "not installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.call(t, "github.open_or_update_pr", tt.with)
			wantErr(t, err, tt.want)
		})
	}
}

func TestOpenOrUpdatePRAfterAnEarlierRound(t *testing.T) {
	tests := []struct {
		name    string
		finish  func(t *testing.T, v *env, number int)
		wantErr string
		merged  bool
	}{
		{"still open", func(*testing.T, *env, int) {}, "", false},
		{"closed on GitHub", func(t *testing.T, v *env, n int) { must(t, v.tracker.ClosePR("acme/app", n)) }, "pull request acme/app#1 was closed on GitHub: not opening another", false},
		{"merged on GitHub", func(t *testing.T, v *env, n int) {
			_, err := v.tracker.MergePR(t.Context(), "acme/app", n, github.MergeInput{})
			must(t, err)
		}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			out, err := v.call(t, "github.open_or_update_pr", prWith(nil))
			must(t, err)
			n := int(toMap(t, out)["number"].(float64))
			tt.finish(t, v, n)
			out, err = v.call(t, "github.open_or_update_pr", prWith(map[string]any{"previous": n, "title": "Round 2"}))
			if tt.wantErr != "" {
				wantErr(t, err, tt.wantErr)
				if _, err := v.tracker.FindPR(t.Context(), "acme/app", "agent/onboarding"); err == nil {
					t.Error("a new PR was opened")
				}
				return
			}
			must(t, err)
			m := toMap(t, out)
			if got := int(m["number"].(float64)); got != n {
				t.Errorf("number = %d, want %d", got, n)
			}
			ifOpen, _ := m["if_open"].([]any)
			if tt.merged != (m["merged"] == true) || tt.merged == (len(ifOpen) == 1) || (tt.merged && m["merge_sha"] != "merge-1") {
				t.Errorf("output = %v", m)
			}
			if pr, _ := v.tracker.GetPR(t.Context(), "acme/app", n); tt.merged && pr.Title == "Round 2" {
				t.Errorf("a PR merged on GitHub was updated: %+v", pr)
			}
		})
	}
	v := newEnv(t)
	_, err := v.call(t, "github.open_or_update_pr", prWith(map[string]any{"previous": 99}))
	wantErr(t, err, "get pull request acme/app#99")
}

// A PR opened for a task of the goal playbook is recorded with that task.
func TestOpenOrUpdatePRForATask(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	out, err := p.call(t, "github.open_or_update_pr", prWith(map[string]any{"task_id": task.ID.String()}))
	must(t, err)
	c, err := p.st.Changes().GetByPR(t.Context(), p.account, "acme/app", int(toMap(t, out)["number"].(float64)))
	must(t, err)
	if c.TaskID != task.ID {
		t.Errorf("change task = %s, want %s", c.TaskID, task.ID)
	}
}

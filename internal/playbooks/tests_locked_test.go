package playbooks_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// finishJob runs the queued job id to the end with output.
func (v *env) finishJob(t *testing.T, id uuid.UUID, output string) {
	t.Helper()
	ctx := t.Context()
	w := domain.Worker{AccountID: v.account, Name: "w-" + id.String(), TokenHash: []byte(id.String())}
	must(t, v.st.Workers().Create(ctx, &w))
	for {
		j, err := v.st.Jobs().LeaseNext(ctx, v.account, w.ID, []string{domain.RuntimeLabel("")}, time.Hour)
		must(t, err)
		_, err = v.st.Jobs().Complete(ctx, v.account, j.ID, w.ID, store.JobResult{Status: domain.JobSucceeded, Output: []byte(output)})
		must(t, err)
		if j.ID == id {
			return
		}
	}
}

func (v *env) testerJob(t *testing.T, role string, task domain.Task, output string) {
	t.Helper()
	id, err := newJobCreator(v).Create(t.Context(), workflow.JobRequest{
		AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Role: role, TaskID: task.ID, GoalID: task.GoalID,
	})
	must(t, err)
	v.finishJob(t, id, output)
}

func TestTestsLockedPaths(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One", "Two")
	with := func(task domain.Task) map[string]any { return map[string]any{"task_id": task.ID.String()} }

	out, err := p.call(t, "tests.locked_paths", with(task[0]))
	must(t, err)
	if got := toMap(t, out)["locked_paths"]; len(got.([]any)) != 0 {
		t.Errorf("locked_paths without a test engineer job = %v, want []", got)
	}

	p.testerJob(t, "developer", task[0], `{"files":["ignored.go"]}`)
	p.testerJob(t, "test-engineer", task[0], `{"files":["old_test.go"],"tests":[{"suite":"unit","id":"p::T","file":"pkg/c_test.go"}]}`)
	// A retry reports only what it changed itself: the first job still counts.
	p.testerJob(t, "test-engineer", task[0], `{"files":["pkg/b_test.go","pkg/a_test.go","web/[id]/x{1}*?.test.ts","pkg/a_test.go"]}`)
	p.testerJob(t, "test-engineer", task[1], `{"files":["other_test.go"]}`)
	// The rerun a human approved runs without the lock; the first round has it.
	for iteration, locked := range map[int]bool{0: true, 1: true, 2: false, 3: false} {
		out, err = p.call(t, "tests.locked_paths", map[string]any{"task_id": task[0].ID.String(), "iteration": iteration})
		must(t, err)
		if n := len(toMap(t, out)["locked_paths"].([]any)); (n > 0) != locked {
			t.Errorf("iteration %d: %d locked paths, locked = %v", iteration, n, locked)
		}
	}
	out, err = p.call(t, "tests.locked_paths", with(task[0]))
	must(t, err)
	var got struct {
		LockedPaths []string `json:"locked_paths"`
	}
	b, _ := json.Marshal(out)
	must(t, json.Unmarshal(b, &got))
	if want := []string{"old_test.go", "pkg/a_test.go", "pkg/b_test.go", "pkg/c_test.go", "web/[[]id]/x[{]1[}][*][?].test.ts"}; !slices.Equal(got.LockedPaths, want) {
		t.Errorf("locked_paths = %v, want %v", got.LockedPaths, want)
	}
}

func TestTestsLockedPathsErrors(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	_, err := p.call(t, "tests.locked_paths", map[string]any{"task_id": domain.NewID().String()})
	wantErr(t, err, "get task")
	_, err = p.call(t, "tests.locked_paths", map[string]any{"task_id": task.ID.String(), "x": 1})
	wantErr(t, err, "unknown field")
	p.testerJob(t, "test-engineer", task, `{"files":"nope"}`)
	_, err = p.call(t, "tests.locked_paths", map[string]any{"task_id": task.ID.String()})
	wantErr(t, err, "output of the test-engineer job")

	other := newPlanned(t)
	task = other.plan(t, "One")[0]
	for _, file := range []string{`a\b_test.go`, "!x_test.go"} {
		other.testerJob(t, "test-engineer", task, `{"files":["`+strings.ReplaceAll(file, `\`, `\\`)+`"]}`)
		_, err = other.call(t, "tests.locked_paths", map[string]any{"task_id": task.ID.String()})
		wantErr(t, err, "cannot be locked")
	}
}

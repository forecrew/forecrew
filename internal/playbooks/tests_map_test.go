package playbooks_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

type mapOut struct {
	Ready       bool `json:"ready"`
	IfArchitect []struct {
		TaskID string   `json:"task_id"`
		Branch string   `json:"branch"`
		Needs  []string `json:"needs"`
	} `json:"if_architect"`
	Feedback string `json:"feedback"`
	Tests    []struct {
		Suite string `json:"suite"`
		ID    string `json:"id"`
		File  string `json:"file"`
	} `json:"tests"`
	Files  []string `json:"files"`
	Suites []string `json:"suites"`
}

func (p *planned) testsMap(t *testing.T, task domain.Task) mapOut {
	t.Helper()
	out, err := p.call(t, "tests.map", map[string]any{"task_id": task.ID.String(), "needs": []string{"go", "linux"}})
	must(t, err)
	b, _ := json.Marshal(out)
	var got mapOut
	must(t, json.Unmarshal(b, &got))
	if got.IfArchitect == nil || got.Tests == nil || got.Files == nil || got.Suites == nil {
		t.Fatalf("a list is null: %s", b)
	}
	return got
}

func (p *planned) tester(t *testing.T, task domain.Task, output string) {
	t.Helper()
	p.clock.Advance(time.Second)
	p.testerJob(t, "test-engineer", task, output)
}

func TestTestsMap(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]

	got := p.testsMap(t, task)
	if got.Ready || !strings.Contains(got.Feedback, "No test engineer job") || len(got.Tests) != 0 {
		t.Errorf("without a job = %+v", got)
	}

	// The first job covers AC-1 only; a retry adds AC-2 and reports only its
	// own files.
	p.tester(t, task, `{"files":["pkg/a_test.go","pkg/testdata/in.json"],"tests":[{"criterion":"AC-1","suite":"unit","id":"pkg::TestAC1","file":"pkg/a_test.go"}],"untestable":[],"escalation":null}`)
	got = p.testsMap(t, task)
	if got.Ready || !strings.Contains(got.Feedback, "Missing: AC-2.") || strings.Contains(got.Feedback, "stopped") || len(got.IfArchitect) != 0 {
		t.Errorf("one criterion = %+v", got)
	}
	p.tester(t, task, `{"files":["e2e/flow_test.go"],"tests":[{"criterion":"AC-2","suite":"e2e","id":"e2e::Flow","file":"e2e/flow_test.go"},`+
		`{"criterion":"AC-1","suite":"unit","id":"pkg::TestAC1","file":"pkg/a_test.go"}],"untestable":[],"escalation":null}`)
	got = p.testsMap(t, task)
	if !got.Ready || got.Feedback != "" || len(got.IfArchitect) != 0 {
		t.Errorf("both criteria = %+v", got)
	}
	if want := []string{"e2e/flow_test.go", "pkg/a_test.go", "pkg/testdata/in.json"}; !slices.Equal(got.Files, want) {
		t.Errorf("files = %v, want %v", got.Files, want)
	}
	if !slices.Equal(got.Suites, []string{"e2e", "unit"}) || len(got.Tests) != 2 || got.Tests[0].ID != "pkg::TestAC1" || got.Tests[1].File != "e2e/flow_test.go" {
		t.Errorf("tests = %+v, suites %v", got.Tests, got.Suites)
	}
}

func TestTestsMapUntestableAndEscalation(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	p.tester(t, task, `{"files":[],"tests":[{"criterion":"AC-1","suite":"unit","id":"T","file":"a_test.go"}],"untestable":[{"criterion":"AC-2","reason":"a human reads the page"}],"escalation":null}`)
	if got := p.testsMap(t, task); !got.Ready {
		t.Errorf("untestable with a reason = %+v", got)
	}
	// An untestable entry without a reason does not cover the criterion.
	q := newPlanned(t)
	task = q.plan(t, "One")[0]
	q.tester(t, task, `{"files":[],"tests":[{"criterion":"AC-1","suite":"unit","id":"T","file":"a_test.go"}],"untestable":[{"criterion":"AC-2","reason":""}],"escalation":null}`)
	if got := q.testsMap(t, task); got.Ready || !strings.Contains(got.Feedback, "AC-2") {
		t.Errorf("untestable without a reason = %+v", got)
	}

	for _, tt := range []struct {
		escalation string
		architect  bool
	}{
		{"AC-2 needs a Windows suite the matrix lacks", true},
		{"Testing AC-2 needs a new SUITE", true},
		{"AC-2 contradicts the spec", false},
	} {
		r := newPlanned(t)
		task = r.plan(t, "One")[0]
		raw, _ := json.Marshal(map[string]any{"files": []string{}, "tests": []any{}, "untestable": []any{}, "escalation": tt.escalation})
		r.tester(t, task, string(raw))
		got := r.testsMap(t, task)
		if got.Ready || len(got.IfArchitect) == 1 != tt.architect || !strings.Contains(got.Feedback, "<untrusted-data") || !strings.Contains(got.Feedback, tt.escalation) {
			t.Errorf("escalation %q = %+v", tt.escalation, got)
		}
		if tt.architect && (got.IfArchitect[0].TaskID != task.ID.String() || got.IfArchitect[0].Branch != "agent/TASK-1" || !slices.Equal(got.IfArchitect[0].Needs, []string{"go", "linux"})) {
			t.Errorf("if_architect = %+v", got.IfArchitect)
		}
		// A later job that does not escalate makes it ready again.
		r.tester(t, task, `{"files":[],"tests":[{"criterion":"AC-1","suite":"unit","id":"T","file":"a_test.go"},{"criterion":"AC-2","suite":"unit","id":"U","file":"a_test.go"}],"untestable":[],"escalation":null}`)
		if got := r.testsMap(t, task); !got.Ready {
			t.Errorf("after the retry = %+v", got)
		}
	}
}

func TestTestsMapErrors(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	_, err := p.call(t, "tests.map", map[string]any{"task_id": domain.NewID().String()})
	wantErr(t, err, "get task")
	_, err = p.call(t, "tests.map", map[string]any{"task_id": task.ID.String(), "x": 1})
	wantErr(t, err, "unknown field")
	p.tester(t, task, `{"files":"nope"}`)
	_, err = p.call(t, "tests.map", map[string]any{"task_id": task.ID.String()})
	wantErr(t, err, "output of the test-engineer job")
}

// Jobs created in the same instant are told apart by their numbers: the later
// one is the latest.
func TestTestsMapLatestJobOfOneInstant(t *testing.T) {
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	p.testerJob(t, "test-engineer", task, `{"files":[],"tests":[],"untestable":[],"escalation":"AC-1 contradicts the spec"}`)
	p.testerJob(t, "test-engineer", task, `{"files":[],"tests":[{"criterion":"AC-1","suite":"unit","id":"T","file":"a_test.go"},{"criterion":"AC-2","suite":"unit","id":"U","file":"a_test.go"}],"untestable":[],"escalation":null}`)
	if got := p.testsMap(t, task); !got.Ready {
		t.Errorf("map = %+v", got)
	}
}

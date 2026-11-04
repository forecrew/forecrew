package playbooks_test

import (
	"encoding/json"
	"slices"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/testreport"
	"github.com/forecrew/forecrew/internal/workflow"
)

const matrixYAML = `version: 1
axes:
  os: [linux, windows]
environments:
  - {match: {os: windows}, environment: win}
  - {match: {}, environment: linux-go}
suites:
  - name: unit
    command: [go, test, ./...]
    level: unit
    reports: [reports/unit.xml]
  - name: e2e
    command: make e2e
    level: e2e
    reports: [reports/e2e.xml]
    artifacts: [out/**]
    axes: {os: [linux]}
`

const linuxGoYAML = "version: 1\nname: linux-go\nprovider: docker\nimage: golang:1.27\n"
const winYAML = "version: 1\nname: win\nprovider: machine\nneeds: [windows]\n"

func (v *env) matrixFiles(t *testing.T, ref string) {
	t.Helper()
	must(t, v.tracker.SetFile("acme/app", ref, ".forecrew/matrix.yaml", []byte(matrixYAML)))
	must(t, v.tracker.SetFile("acme/app", ref, ".forecrew/environments/linux-go.yaml", []byte(linuxGoYAML)))
	must(t, v.tracker.SetFile("acme/app", ref, ".forecrew/environments/win.yaml", []byte(winYAML)))
}

func newTests(entries ...[3]string) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		out = append(out, map[string]any{"suite": e[0], "id": e[1], "file": e[2]})
	}
	return out
}

func TestTestsBaseCheckRequest(t *testing.T) {
	v := newEnv(t)
	v.matrixFiles(t, "agent/task-1")
	out, err := v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "agent/task-1",
		"tests": newTests([3]string{"unit", "pkg::TestA", "pkg/a_test.go"}, [3]string{"e2e", "e2e::Flow", "e2e/flow_test.go"}, [3]string{"unit", "pkg::TestB", "pkg/a_test.go"}),
		"files": []string{"pkg/a_test.go", "e2e/flow_test.go", "pkg/testdata/in.json"},
	})
	must(t, err)
	b, _ := json.Marshal(out)
	var got struct {
		Requests []workflow.JobRequest `json:"requests"`
	}
	must(t, json.Unmarshal(b, &got))
	type key struct{ suite, env, os string }
	var keys []key
	for _, r := range got.Requests {
		keys = append(keys, key{r.Suite, r.Environment.Name, r.Cell["os"]})
		if r.Kind != domain.JobKindCommand || r.Repo != "acme/app" || r.BaseRef != "main" || r.Branch != "" ||
			r.Overlay == nil || r.Overlay.Ref != "agent/task-1" ||
			!slices.Equal(r.Overlay.Paths, []string{"e2e/flow_test.go", "pkg/a_test.go", "pkg/testdata/in.json"}) {
			t.Errorf("request = %+v overlay = %+v", r, r.Overlay)
		}
	}
	want := []key{{"unit", "linux-go", "linux"}, {"e2e", "linux-go", "linux"}, {"unit", "win", "windows"}}
	if !slices.Equal(keys, want) {
		t.Fatalf("requests = %v, want %v", keys, want)
	}
	if !slices.Contains(got.Requests[2].Needs, "windows") {
		t.Errorf("needs = %v, want the cell's os", got.Requests[2].Needs)
	}
	e2e := got.Requests[1]
	if e2e.Command.Shell != "make e2e" || !slices.Equal(e2e.Command.Reports, []string{"reports/e2e.xml"}) || !slices.Equal(e2e.Command.Artifacts, []string{"out/**"}) {
		t.Errorf("e2e command = %+v", e2e.Command)
	}
	if unit := got.Requests[0]; !slices.Equal(unit.Command.Argv, []string{"go", "test", "./..."}) || unit.Environment.Image != "golang:1.27" {
		t.Errorf("unit request = %+v %+v", unit.Command, unit.Environment)
	}
}

func TestTestsBaseCheckRequestOverlayFromTests(t *testing.T) {
	v := newEnv(t)
	v.matrixFiles(t, "abc")
	out, err := v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"e2e", "e2e::Flow", "e2e/flow_test.go"}),
	})
	must(t, err)
	reqs := toMap(t, out)["requests"].([]any)
	if len(reqs) != 1 || !slices.Equal(toMap(t, reqs[0])["overlay"].(map[string]any)["paths"].([]any), []any{"e2e/flow_test.go"}) {
		t.Errorf("requests = %v", reqs)
	}
	out, err = v.call(t, "tests.base_check_request", map[string]any{"repo": "acme/app", "base": "main", "head": "abc", "tests": []any{}})
	must(t, err)
	if reqs := toMap(t, out)["requests"].([]any); len(reqs) != 0 {
		t.Errorf("requests without tests = %v", reqs)
	}
}

func TestTestsBaseCheckRequestRefusesNonTestFiles(t *testing.T) {
	v := newEnv(t)
	v.matrixFiles(t, "abc")
	_, err := v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"unit", "p::T", "a_test.go"}),
		"files": []string{"a_test.go", "Makefile", "go.mod", ".forecrew/matrix.yaml"},
	})
	wantErr(t, err, "Makefile, go.mod, .forecrew/matrix.yaml")
	_, err = v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"unit", "p::T", "scripts/run.sh"}),
	})
	wantErr(t, err, "scripts/run.sh")
	_, err = v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"unit", "p::T", "a\\b_test.go"}),
	})
	wantErr(t, err, "cannot be locked")
}

func TestTestsBaseCheckRequestNeedsRoles(t *testing.T) {
	v := newEnv(t)
	v.matrixFiles(t, "abc")
	playbooks.Register(v.actions, playbooks.Deps{Store: v.st, Tracker: v.tracker, Signals: v.bus})
	_, err := v.call(t, "tests.base_check_request", map[string]any{
		"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"unit", "p::T", "a_test.go"}),
	})
	wantErr(t, err, "no role resolver")
}

func TestTestsBaseCheckRequestErrors(t *testing.T) {
	v := newEnv(t)
	base := func() map[string]any {
		return map[string]any{"repo": "acme/app", "base": "main", "head": "abc", "tests": newTests([3]string{"unit", "pkg::TestA", "a_test.go"})}
	}
	with := func(mut func(map[string]any)) map[string]any {
		w := base()
		mut(w)
		return w
	}
	_, err := v.call(t, "tests.base_check_request", with(func(w map[string]any) { delete(w, "head") }))
	wantErr(t, err, "repo, base and head are required")
	_, err = v.call(t, "tests.base_check_request", with(func(w map[string]any) { w["x"] = 1 }))
	wantErr(t, err, "unknown field")
	_, err = v.call(t, "tests.base_check_request", base())
	wantErr(t, err, ".forecrew/matrix.yaml does not exist")
	must(t, v.tracker.SetFile("acme/app", "abc", ".forecrew/matrix.yaml", []byte("version: 2")))
	_, err = v.call(t, "tests.base_check_request", base())
	wantErr(t, err, "matrix.yaml")
	v.matrixFiles(t, "abc")
	_, err = v.call(t, "tests.base_check_request", with(func(w map[string]any) { w["tests"] = newTests([3]string{"nope", "x", "x_test.go"}) }))
	wantErr(t, err, `no suite "nope"`)
	must(t, v.tracker.SetFile("acme/app", "abc", ".forecrew/environments/win.yaml", nil))
	_, err = v.call(t, "tests.base_check_request", base())
	wantErr(t, err, "win.yaml")
	must(t, v.tracker.SetFile("acme/app", "abd", ".forecrew/matrix.yaml", []byte(matrixYAML)))
	_, err = v.call(t, "tests.base_check_request", with(func(w map[string]any) { w["head"] = "abd" }))
	wantErr(t, err, ".forecrew/environments/linux-go.yaml does not exist")
}

// baseRun stores a command job with one test run of suite.
func (v *env) baseRun(t *testing.T, suite string, status domain.TestRunStatus, exit int, results ...domain.TestResult) uuid.UUID {
	t.Helper()
	j := domain.Job{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, v.st.Jobs().Create(t.Context(), &j))
	r := domain.TestRun{AccountID: v.account, ProjectID: v.project.ID, JobID: j.ID, SHA: "main", Suite: suite, Status: status, ExitCode: exit, Attempt: 1}
	must(t, v.st.TestRuns().Create(t.Context(), &r, results))
	return j.ID
}

func res(class, name string, st domain.TestStatus) domain.TestResult {
	return domain.TestResult{Classname: class, Name: name, Status: st}
}

func TestTestsBaseCheckResult(t *testing.T) {
	v := newEnv(t)
	failedUnit := v.baseRun(t, "unit", domain.TestRunFailed, 1, res("pkg", "TestA", domain.TestFailed), res("pkg", "TestB", domain.TestPassed), res("", "plain", domain.TestErrored))
	erroredE2E := v.baseRun(t, "e2e", domain.TestRunErrored, 2)
	tests := newTests([3]string{"unit", "pkg::TestA", "a"}, [3]string{"unit", "pkg::TestA", "b"}, [3]string{"unit", "plain", "a"}, [3]string{"e2e", "e2e::Flow", "a"})

	out, err := v.call(t, "tests.base_check_result", map[string]any{
		"runs": []any{failedUnit.String(), map[string]any{"job_id": erroredE2E.String(), "output": map[string]any{}}}, "tests": tests,
	})
	must(t, err)
	m := toMap(t, out)
	if m["ok"] != true {
		t.Errorf("output = %v, want ok", m)
	}
	nt := m["new_tests"].([]any)
	if len(nt) != 3 {
		t.Fatalf("new_tests = %v, want 3 (a test with two criteria once)", nt)
	}
	for _, n := range nt {
		if toMap(t, n)["failed_on_base"] != true {
			t.Errorf("new test %v did not fail on base", n)
		}
	}

	// A passing test, a skipped one and a suite with no run are not failed on base.
	tests = newTests([3]string{"unit", "pkg::TestB", "a"}, [3]string{"integration", "i::T", "a"})
	out, err = v.call(t, "tests.base_check_result", map[string]any{"runs": []any{failedUnit.String()}, "tests": tests})
	must(t, err)
	m = toMap(t, out)
	if m["ok"] != false {
		t.Errorf("output = %v, want not ok", m)
	}
	for _, n := range m["new_tests"].([]any) {
		if toMap(t, n)["failed_on_base"] != false {
			t.Errorf("new test %v failed on base", n)
		}
	}
}

func TestTestsBaseCheckResultSeveralEnvironments(t *testing.T) {
	v := newEnv(t)
	linux := v.baseRun(t, "unit", domain.TestRunFailed, 1, res("pkg", "TestA", domain.TestFailed))
	windows := v.baseRun(t, "unit", domain.TestRunPassed, 0, res("pkg", "TestA", domain.TestSkipped))
	tests := newTests([3]string{"unit", "pkg::TestA", "a"})
	out, err := v.call(t, "tests.base_check_result", map[string]any{"runs": []any{linux.String(), windows.String()}, "tests": tests})
	must(t, err)
	if toMap(t, out)["ok"] != false {
		t.Errorf("a test that did not fail in every environment is ok: %v", out)
	}
	out, err = v.call(t, "tests.base_check_result", map[string]any{"runs": []any{linux.String()}, "tests": tests})
	must(t, err)
	if toMap(t, out)["ok"] != true {
		t.Errorf("output = %v, want ok", out)
	}
}

func TestTestsBaseCheckResultMissingTests(t *testing.T) {
	v := newEnv(t)
	cmd := domain.TestResult{Classname: "forecrew", Name: "(command)", Status: domain.TestErrored}
	rep := domain.TestResult{Classname: "forecrew", Name: "(report)", Status: domain.TestErrored}
	big := rep
	big.Name, big.Message = testreport.OversizedResult, "reworded: the results were too many" // matched by name, not by text
	tests := []struct {
		name   string
		status domain.TestRunStatus
		exit   int
		res    []domain.TestResult
		want   bool // the missing test failed on base
	}{
		{"compile failure, errored run", domain.TestRunErrored, 2, []domain.TestResult{rep}, true},
		{"errored run, exit 0: wrong report path", domain.TestRunErrored, 0, []domain.TestResult{rep}, false},
		{"errored run, timeout", domain.TestRunErrored, -1, []domain.TestResult{rep}, false},
		{"oversized run", domain.TestRunErrored, 1, []domain.TestResult{big}, false},
		{"partial report, one package did not compile", domain.TestRunFailed, 1, []domain.TestResult{res("a", "TestOther", domain.TestFailed)}, true},
		{"partial report, exit 0", domain.TestRunFailed, 0, []domain.TestResult{res("a", "TestOther", domain.TestFailed)}, false},
		{"all passed under a failing command", domain.TestRunFailed, 1, []domain.TestResult{res("a", "TestOther", domain.TestPassed), cmd}, true},
		{"passed", domain.TestRunPassed, 0, []domain.TestResult{res("a", "TestOther", domain.TestPassed)}, false},
		{"passed run with a failing exit", domain.TestRunPassed, 1, []domain.TestResult{res("a", "TestOther", domain.TestPassed)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := v.baseRun(t, "unit", tt.status, tt.exit, tt.res...)
			out, err := v.call(t, "tests.base_check_result", map[string]any{"runs": []any{job.String()}, "tests": newTests([3]string{"unit", "p::Missing", "f"})})
			must(t, err)
			if got := toMap(t, out)["ok"]; got != tt.want {
				t.Errorf("failed on base = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTestsBaseCheckResultDuplicateIDs(t *testing.T) {
	v := newEnv(t)
	for name, tt := range map[string]struct {
		results []domain.TestResult
		want    bool
	}{
		"failure then pass": {[]domain.TestResult{res("p", "T", domain.TestFailed), res("p", "T", domain.TestPassed)}, false},
		"pass then failure": {[]domain.TestResult{res("p", "T", domain.TestPassed), res("p", "T", domain.TestFailed)}, false},
		"both fail":         {[]domain.TestResult{res("p", "T", domain.TestFailed), res("p", "T", domain.TestErrored)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			job := v.baseRun(t, "unit", domain.TestRunFailed, 1, tt.results...)
			out, err := v.call(t, "tests.base_check_result", map[string]any{"runs": []any{job.String()}, "tests": newTests([3]string{"unit", "p::T", "f"})})
			must(t, err)
			if got := toMap(t, out)["ok"]; got != tt.want {
				t.Errorf("ok = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTestsBaseCheckResultRunShapes(t *testing.T) {
	v := newEnv(t)
	v.baseRun(t, "unit", domain.TestRunPassed, 0, res("p", "T", domain.TestPassed)) // must never be read
	job := v.baseRun(t, "unit", domain.TestRunFailed, 1, res("p", "T", domain.TestFailed))
	tests := newTests([3]string{"unit", "p::T", "f"})
	// A round of a foreach over a job step: the step id holds the job output.
	out, err := v.call(t, "tests.base_check_result", map[string]any{
		"runs": []any{map[string]any{"run_unit": map[string]any{"job_id": job.String(), "output": map[string]any{}}}}, "tests": tests,
	})
	must(t, err)
	if toMap(t, out)["ok"] != true {
		t.Errorf("foreach round shape: %v", out)
	}
	for name, runs := range map[string][]any{
		"no job_id":    {map[string]any{"output": map[string]any{}}},
		"empty object": {map[string]any{}},
		"nil job_id":   {map[string]any{"job_id": uuid.Nil().String()}},
		"nil string":   {uuid.Nil().String()},
		"nested no id": {map[string]any{"step": map[string]any{"output": 1}}},
		"null":         {nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.call(t, "tests.base_check_result", map[string]any{"runs": runs, "tests": tests})
			wantErr(t, err, "with:")
		})
	}
}

func TestTestsBaseCheckResultEmptyAndErrors(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "tests.base_check_result", map[string]any{"runs": []any{}, "tests": []any{}})
	must(t, err)
	if m := toMap(t, out); m["ok"] != true || len(m["new_tests"].([]any)) != 0 {
		t.Errorf("output = %v", m)
	}
	_, err = v.call(t, "tests.base_check_result", map[string]any{"runs": []any{"nope"}, "tests": []any{}})
	wantErr(t, err, "with:")
	_, err = v.call(t, "tests.base_check_result", map[string]any{"runs": []any{map[string]any{"job_id": 3}}, "tests": []any{}})
	wantErr(t, err, "with:")
	_, err = v.call(t, "tests.base_check_result", map[string]any{"runs": 1, "tests": []any{}})
	wantErr(t, err, "with:")
}

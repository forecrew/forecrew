package playbooks_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

const headSHA = "abc1234def"

// verified is a planned goal with a task, a PR on its head and the matrix at
// the head.
type verified struct {
	*planned
	task   domain.Task
	change domain.Change
	ev     *testkit.Evidence
}

func newVerified(t *testing.T) *verified {
	t.Helper()
	p := newPlanned(t)
	task := p.plan(t, "One")[0]
	p.matrixFiles(t, headSHA)
	must(t, p.tracker.AddBranch("acme/app", "agent/TASK-1"))
	pr, err := p.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "T", Head: "agent/TASK-1", Base: "main"})
	must(t, err)
	ch := domain.Change{AccountID: p.account, ProjectID: p.project.ID, TaskID: task.ID, Repo: "acme/app", Number: pr.Number, Branch: "agent/TASK-1", BaseRef: "main",
		HeadSHA: headSHA, State: domain.ChangeOpen, URL: pr.URL}
	must(t, p.st.Changes().Upsert(t.Context(), &ch))
	ev := testkit.NewEvidence()
	playbooks.Register(p.actions, playbooks.Deps{Store: p.st, Tracker: p.tracker, Signals: p.bus, Sizing: config.DefaultSizing(), Roles: testRoles(), Now: p.clock.Now, Evidence: ev})
	return &verified{planned: p, task: task, change: ch, ev: ev}
}

// addRun stores a command job with a test run for the head.
func (v *verified) addRun(t *testing.T, env string, os string, suite string, results ...domain.TestResult) {
	t.Helper()
	v.clock.Advance(time.Minute)
	j := domain.Job{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, v.st.Jobs().Create(t.Context(), &j))
	status := domain.TestRunPassed
	var counts domain.TestCounts
	for _, r := range results {
		switch r.Status {
		case domain.TestPassed:
			counts.Passed++
		case domain.TestFailed:
			counts.Failed++
			status = domain.TestRunFailed
		}
	}
	run := domain.TestRun{AccountID: v.account, ProjectID: v.project.ID, JobID: j.ID, ChangeID: v.change.ID, SHA: headSHA, Suite: suite, Environment: env,
		Cell: map[string]string{"os": os}, Status: status, Counts: counts, Attempt: 1, Versions: map[string]string{"go": "go1.27"}}
	must(t, v.st.TestRuns().Create(t.Context(), &run, results))
}

func testResults(status domain.TestStatus, ids ...string) []domain.TestResult {
	var out []domain.TestResult
	for _, id := range ids {
		class, name, _ := strings.Cut(id, "::")
		out = append(out, domain.TestResult{Classname: class, Name: name, Status: status})
	}
	return out
}

func (v *verified) comments(t *testing.T) []github.Comment {
	t.Helper()
	all, err := v.tracker.ListComments(t.Context(), "acme/app", v.change.Number, time.Time{})
	must(t, err)
	var out []github.Comment
	for _, c := range all {
		if strings.Contains(c.Body, verification.Marker) {
			out = append(out, c)
		}
	}
	return out
}

func (v *verified) with(extra map[string]any) map[string]any {
	with := map[string]any{"change": v.change.Number, "sha": headSHA}
	maps.Copy(with, extra)
	return with
}

type buildOut struct {
	Status   string   `json:"status"`
	Blocking []string `json:"blocking"`
	Summary  string   `json:"summary"`
	ReportID string   `json:"report_id"`
}

func (v *verified) build(t *testing.T, extra map[string]any) buildOut {
	t.Helper()
	out, err := v.call(t, "verification.build", v.with(extra))
	must(t, err)
	b, _ := json.Marshal(out)
	var got buildOut
	must(t, json.Unmarshal(b, &got))
	return got
}

func TestVerificationBuild(t *testing.T) {
	v := newVerified(t)
	v.testerJob(t, "test-engineer", v.task, `{"files":["pkg/a_test.go"],"tests":[{"criterion":"AC-1","suite":"unit","id":"pkg::TestA","file":"pkg/a_test.go"}],`+
		`"untestable":[{"criterion":"AC-2","reason":"a human reads the page"}]}`)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	baseCheck := map[string]any{"base_check": map[string]any{"ok": true, "new_tests": []any{map[string]any{"id": "pkg::TestA", "failed_on_base": true}}}}

	// The windows cell and the e2e suite have not run: incomplete.
	got := v.build(t, baseCheck)
	if got.Status != "incomplete" || len(got.Blocking) != 3 || !strings.HasPrefix(got.Summary, "Incomplete: ") || got.ReportID == "" {
		t.Fatalf("first build = %+v", got)
	}
	if cs := v.comments(t); len(cs) != 1 || !strings.Contains(cs[0].Body, "incomplete") {
		t.Fatalf("comments = %+v", cs)
	}
	if run, ok := v.tracker.CheckRun("acme/app", headSHA, verification.CheckName); !ok || run.Conclusion != github.ConclusionNeutral {
		t.Fatalf("check run = %+v, %v", run, ok)
	}

	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	got2 := v.build(t, baseCheck)
	if got2.Status != "passed" || len(got2.Blocking) != 0 || got2.ReportID != got.ReportID {
		t.Fatalf("second build = %+v", got2)
	}
	cs := v.comments(t)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "passed") || strings.Contains(cs[0].Body, "incomplete") {
		t.Fatalf("the comment was not updated in place: %+v", cs)
	}
	if run, _ := v.tracker.CheckRun("acme/app", headSHA, verification.CheckName); run.Conclusion != github.ConclusionSuccess {
		t.Errorf("check run = %+v", run)
	}
	stored, err := v.st.VerificationReports().GetBySHA(t.Context(), v.account, v.change.ID, headSHA)
	must(t, err)
	rep, err := verification.Parse(stored.Report)
	must(t, err)
	if c := rep.Criteria["AC-1"]; c.Status != "passed" || c.Tests["pkg::TestA"] != "passed" {
		t.Errorf("AC-1 = %+v", c)
	}
	if c := rep.Criteria["AC-2"]; c.Untestable != "a human reads the page" || len(rep.Criteria) != 2 {
		t.Errorf("criteria = %+v", rep.Criteria)
	}
	if !rep.NewTests["pkg::TestA"] || len(rep.Results) != 3 || stored.ArtifactID == uuid.Nil() || len(v.ev.Keys()) != 1 {
		t.Errorf("report = %+v, artifact %v, objects %v", rep, stored.ArtifactID, v.ev.Keys())
	}

	// A call with the same key again is a no-op.
	again := v.build(t, baseCheck)
	if !reflect.DeepEqual(again, got2) || len(v.comments(t)) != 1 || len(v.ev.Keys()) != 1 {
		t.Errorf("repeat = %+v", again)
	}
}

func TestVerificationBuildBlocks(t *testing.T) {
	v := newVerified(t)
	// AC-2 has neither a test nor an untestable entry; the base check says a
	// new test passed on base; a unit test fails on windows.
	v.testerJob(t, "test-engineer", v.task, `{"tests":[{"criterion":"AC-1","suite":"unit","id":"pkg::TestA","file":"a_test.go"}],"untestable":[]}`)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	got := v.build(t, map[string]any{"base_check": map[string]any{"new_tests": []any{map[string]any{"id": "pkg::TestA", "failed_on_base": false}}}})
	if got.Status != "failed" || len(got.Blocking) != 4 {
		t.Fatalf("build = %+v", got)
	}
	for _, want := range []string{"unit in win (os=windows) failed", "AC-2 has no test", "AC-1: pkg::TestA failed", "new test pkg::TestA passed on the base branch"} {
		if !slices.ContainsFunc(got.Blocking, func(b string) bool { return strings.Contains(b, want) }) {
			t.Errorf("blocking %q lacks %q", got.Blocking, want)
		}
	}
	if run, _ := v.tracker.CheckRun("acme/app", headSHA, verification.CheckName); run.Conclusion != github.ConclusionFailure {
		t.Errorf("check run = %+v", run)
	}
}

// TestVerificationBuildCriteriaFromTheAcceptanceSection: only the criteria of
// the task's Acceptance criteria section count, not a mention in its text.
func TestVerificationBuildCriteriaFromTheAcceptanceSection(t *testing.T) {
	v := newVerified(t)
	task := v.task
	task.Body = "Do it, see AC-7.\n\n- **AC-8** not a criterion\n\n## Acceptance criteria\n\n- **AC-1** one\n- **AC-3** three\n- AC-4 plain\n\n## Depends on\n\n- **AC-9** no\n"
	must(t, v.st.Tasks().Update(t.Context(), &task))
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	got := v.build(t, nil)
	for _, want := range []string{"AC-1 has no test", "AC-3 has no test"} {
		if !slices.Contains(got.Blocking, want) {
			t.Errorf("blocking %q lacks %q", got.Blocking, want)
		}
	}
	for _, bad := range []string{"AC-4", "AC-7", "AC-8", "AC-9"} {
		if slices.ContainsFunc(got.Blocking, func(b string) bool { return strings.Contains(b, bad) }) {
			t.Errorf("blocking %q names %s", got.Blocking, bad)
		}
	}
	stored, _ := v.st.VerificationReports().GetBySHA(t.Context(), v.account, v.change.ID, headSHA)
	rep, err := verification.Parse(stored.Report)
	must(t, err)
	if len(rep.Criteria) != 2 || rep.Criteria["AC-1"].Status == "" || rep.Criteria["AC-3"].Status == "" {
		t.Errorf("criteria = %v", rep.Criteria)
	}
}

func TestVerificationBuildWithoutATask(t *testing.T) {
	v := newVerified(t)
	must(t, v.tracker.AddBranch("acme/app", "agent/other"))
	pr, err := v.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "T2", Head: "agent/other", Base: "main"})
	must(t, err)
	ch := domain.Change{AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Number: pr.Number, Branch: "agent/other", BaseRef: "main", HeadSHA: headSHA, State: domain.ChangeOpen, URL: pr.URL}
	must(t, v.st.Changes().Upsert(t.Context(), &ch))
	v.change = ch
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	if got := v.build(t, nil); got.Status != "passed" {
		t.Fatalf("build = %+v", got)
	}
	// An explicit task wins over the change's.
	v.testerJob(t, "test-engineer", v.task, `{"tests":[],"untestable":[]}`)
	if got := v.build(t, map[string]any{"task_id": v.task.ID.String()}); got.Status != "failed" {
		t.Fatalf("build with a task = %+v", got)
	}
}

func TestVerificationBuildErrors(t *testing.T) {
	v := newVerified(t)
	other := newPlanned(t)
	foreign := other.plan(t, "Other")[0]
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"no change", map[string]any{"sha": headSHA}, "change and sha are required"},
		{"no sha", map[string]any{"change": 1}, "change and sha are required"},
		{"unknown field", v.with(map[string]any{"x": 1}), "unknown field"},
		{"unknown change", map[string]any{"change": 99, "sha": headSHA}, "get pull request"},
		{"no matrix at the sha", map[string]any{"change": v.change.Number, "sha": "deadbeef"}, "test matrix at deadbeef"},
		{"unknown task", v.with(map[string]any{"task_id": domain.NewID().String()}), "get task"},
		{"task of another project", v.with(map[string]any{"task_id": foreign.ID.String()}), "get task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.call(t, "verification.build", tt.with)
			wantErr(t, err, tt.want)
		})
	}
	// A bad test map is reported with the job.
	v.testerJob(t, "test-engineer", v.task, `{"tests":"nope"}`)
	_, err := v.call(t, "verification.build", v.with(nil))
	wantErr(t, err, "output of the test-engineer job")
}

func TestVerificationRerunCells(t *testing.T) {
	v := newVerified(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestFailed, "e2e::Flow")...)
	got := v.build(t, nil)
	if got.Status != "failed" {
		t.Fatalf("build = %+v", got)
	}
	requests := func() []workflow.JobRequest {
		out, err := v.call(t, "verification.rerun_cells", map[string]any{"report_id": got.ReportID})
		must(t, err)
		b, _ := json.Marshal(out)
		var res struct {
			Requests []workflow.JobRequest `json:"requests"`
		}
		must(t, json.Unmarshal(b, &res))
		return res.Requests
	}
	reqs := requests()
	type key struct{ suite, env, os string }
	var keys []key
	for _, r := range reqs {
		keys = append(keys, key{r.Suite, r.Environment.Name, r.Cell["os"]})
		if r.Kind != domain.JobKindCommand || r.Repo != "acme/app" || r.BaseRef != headSHA || r.Overlay != nil || r.Command == nil || len(r.Command.Reports) == 0 {
			t.Errorf("request = %+v", r)
		}
	}
	if want := []key{{"unit", "linux-go", "linux"}, {"e2e", "linux-go", "linux"}}; !slices.Equal(keys, want) {
		t.Fatalf("requests = %v, want %v", keys, want)
	}

	// The e2e rerun passes, the unit rerun fails again: only cells that ran
	// once are rerun, so after the reruns nothing is left.
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	got = v.build(t, nil)
	if got.Status != "failed" || len(requests()) != 0 {
		t.Fatalf("after the reruns: %+v, %d requests", got, len(requests()))
	}
	stored, err := v.st.VerificationReports().GetBySHA(t.Context(), v.account, v.change.ID, headSHA)
	must(t, err)
	rep, _ := verification.Parse(stored.Report)
	if rep.Results[1].Suite != "e2e" || rep.Results[1].Status != "passed" || rep.Results[1].Attempts != 2 {
		t.Errorf("results = %+v", rep.Results)
	}
}

func TestVerificationRerunCellsNothingToRerun(t *testing.T) {
	v := newVerified(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	got := v.build(t, nil) // incomplete: cells missing, none failed
	out, err := v.call(t, "verification.rerun_cells", map[string]any{"report_id": got.ReportID})
	must(t, err)
	if reqs := toMap(t, out)["requests"].([]any); len(reqs) != 0 {
		t.Errorf("requests = %v", reqs)
	}
}

func TestVerificationRerunCellsErrors(t *testing.T) {
	v := newVerified(t)
	_, err := v.call(t, "verification.rerun_cells", map[string]any{"report_id": domain.NewID().String()})
	wantErr(t, err, "get verification report")
	_, err = v.call(t, "verification.rerun_cells", map[string]any{"report_id": uuid.Nil().String(), "x": 1})
	wantErr(t, err, "unknown field")

	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	got := v.build(t, nil)
	// The matrix changed at the head: the suite is gone.
	must(t, v.tracker.SetFile("acme/app", headSHA, ".forecrew/matrix.yaml", []byte(strings.Replace(matrixYAML, "name: unit", "name: other", 1))))
	_, err = v.call(t, "verification.rerun_cells", map[string]any{"report_id": got.ReportID})
	wantErr(t, err, "unit")
	must(t, v.tracker.SetFile("acme/app", headSHA, ".forecrew/matrix.yaml", []byte("version: 2")))
	_, err = v.call(t, "verification.rerun_cells", map[string]any{"report_id": got.ReportID})
	wantErr(t, err, "test matrix at")
}

// TestVerificationBuildWarnsOnceWithoutChecksPermission: one publisher serves
// every call, so the warning is logged once for the process.
func TestVerificationBuildWarnsOnceWithoutChecksPermission(t *testing.T) {
	v := newVerified(t)
	var logs bytes.Buffer
	playbooks.Register(v.actions, playbooks.Deps{Store: v.st, Tracker: v.tracker, Signals: v.bus, Sizing: config.DefaultSizing(), Roles: testRoles(), Now: v.clock.Now,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	must(t, v.tracker.ForbidChecks("acme/app"))
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	for range 3 {
		v.build(t, nil)
		v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	}
	if n := strings.Count(logs.String(), "Checks: write"); n != 1 {
		t.Errorf("the warning was logged %d times:\n%s", n, logs.String())
	}
	if len(v.comments(t)) != 1 {
		t.Errorf("comments = %+v", v.comments(t))
	}
}

// TestVerificationBuildNewTestsNeedTheBaseCheck: without base_check the new
// tests of the test map are unchecked and the report is incomplete.
func TestVerificationBuildNewTestsNeedTheBaseCheck(t *testing.T) {
	v := newVerified(t)
	v.testerJob(t, "test-engineer", v.task, `{"tests":[{"criterion":"AC-1","suite":"unit","id":"pkg::TestA","file":"a_test.go"}],"untestable":[{"criterion":"AC-2","reason":"visual"}]}`)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	got := v.build(t, nil)
	if got.Status != "incomplete" || len(got.Blocking) != 1 || !strings.Contains(got.Blocking[0], "pkg::TestA has not been checked on the base branch") {
		t.Fatalf("build = %+v", got)
	}
}

// TestVerificationBuildTaskWithoutCriteriaBlocks: a task whose body lists no
// criteria cannot be verified.
func TestVerificationBuildTaskWithoutCriteriaBlocks(t *testing.T) {
	v := newVerified(t)
	task := v.task
	task.Body = "Do it, see AC-1."
	must(t, v.st.Tasks().Update(t.Context(), &task))
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	got := v.build(t, nil)
	if got.Status != "failed" || !slices.Contains(got.Blocking, "the task lists no acceptance criteria") {
		t.Fatalf("build = %+v", got)
	}
}

// A baseline report has no criteria and no base check, never blocks a
// merge, and shows nothing on GitHub: no comment, no check run.
func TestVerificationBuildBaseline(t *testing.T) {
	v := newVerified(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	// The task of the change has acceptance criteria that the baseline ignores.
	got := v.build(t, map[string]any{"baseline": true})
	if got.Status != "failed" || len(got.Blocking) != 1 || !strings.Contains(got.Blocking[0], "unit in linux-go") {
		t.Fatalf("baseline = %+v", got)
	}
	if len(v.comments(t)) != 0 {
		t.Errorf("comments = %+v", v.comments(t))
	}
	if _, ok := v.tracker.CheckRun("acme/app", headSHA, verification.CheckName); ok {
		t.Error("a check run was set")
	}
	stored, err := v.st.VerificationReports().GetBySHA(t.Context(), v.account, v.change.ID, headSHA)
	must(t, err)
	rep, err := verification.Parse(stored.Report)
	must(t, err)
	if stored.ID.String() != got.ReportID || len(rep.Criteria) != 0 || len(rep.NewTests) != 0 {
		t.Errorf("report = %+v, stored %s", rep, stored.ID)
	}
	if again := v.build(t, map[string]any{"baseline": true}); !reflect.DeepEqual(again, got) {
		t.Errorf("repeat = %+v", again)
	}
}

// Without a change the baseline is kept under the latest merged pull request.
func TestVerificationBuildBaselineWithoutAChange(t *testing.T) {
	v := newVerified(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	with := map[string]any{"sha": headSHA, "baseline": true}
	_, err := v.call(t, "verification.build", with)
	wantErr(t, err, "has no merged pull request")

	older := v.change
	older.State = domain.ChangeMerged
	must(t, v.st.Changes().Upsert(t.Context(), &older))
	must(t, v.tracker.AddBranch("acme/app", "agent/later"))
	pr, err := v.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "Later", Head: "agent/later", Base: "main"})
	must(t, err)
	later := domain.Change{AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Number: pr.Number, Branch: "agent/later", BaseRef: "main", HeadSHA: "f00", State: domain.ChangeMerged, URL: pr.URL}
	must(t, v.st.Changes().Upsert(t.Context(), &later))
	out, err := v.call(t, "verification.build", with)
	must(t, err)
	stored, err := v.st.VerificationReports().Get(t.Context(), v.account, uuid.MustParse(toMap(t, out)["report_id"].(string)))
	must(t, err)
	if stored.ChangeID != later.ID || stored.ProjectID != v.project.ID || stored.SHA != headSHA {
		t.Errorf("stored = %+v, want change %s", stored, later.ID)
	}
}

func TestVerificationBuildBaselineErrors(t *testing.T) {
	v := newVerified(t)
	for name, with := range map[string]map[string]any{
		"a task":       v.with(map[string]any{"baseline": true, "task_id": v.task.ID.String()}),
		"a base check": v.with(map[string]any{"baseline": true, "base_check": map[string]any{"new_tests": []any{}}}),
		"no sha":       {"baseline": true},
		"no matrix":    {"change": v.change.Number, "sha": "deadbeef", "baseline": true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.call(t, "verification.build", with)
			if err == nil {
				t.Fatal("no error")
			}
		})
	}
}

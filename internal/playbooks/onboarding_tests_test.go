package playbooks_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"uuid"

	forecrew "github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestTestsFeedback(t *testing.T) {
	v := newEnv(t)
	tests := []struct {
		name string
		with map[string]any
		want []string
		not  []string
	}{
		{"nothing", map[string]any{}, nil, []string{"reviewer", "does not work"}},
		{"human", map[string]any{"human": " Also macOS. "}, []string{"A reviewer asked for changes to the test plan:\nAlso macOS."}, []string{"does not work"}},
		{"errors", map[string]any{"errors": "win.yaml does not exist"}, []string{"does not work", "win.yaml does not exist"}, []string{"reviewer"}},
		{"both", map[string]any{"human": "x", "errors": "y"}, []string{"reviewer", "does not work"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := v.call(t, "onboarding.tests_feedback", tt.with)
			must(t, err)
			got, _ := out.(string)
			if (len(tt.want) == 0) != (got == "") {
				t.Errorf("feedback = %q", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("feedback lacks %q: %q", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("feedback has %q: %q", n, got)
				}
			}
		})
	}
	_, err := v.call(t, "onboarding.tests_feedback", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func TestTestsCheck(t *testing.T) {
	v := newEnv(t)
	check := func(with map[string]any) (bool, string) {
		out, err := v.call(t, "onboarding.tests_check", with)
		must(t, err)
		m := toMap(t, out)
		return m["valid"] == true, m["errors"].(string)
	}
	if valid, errs := check(map[string]any{"errors": "", "architect": nil, "engineer": " "}); !valid || errs != "" {
		t.Errorf("valid plan = %v %q", valid, errs)
	}
	if valid, errs := check(map[string]any{"smoke": "go: not found"}); valid || errs != "go: not found" {
		t.Errorf("smoke errors = %v %q", valid, errs)
	}
	valid, errs := check(map[string]any{"errors": "no cell", "architect": "needs a mac", "engineer": "needs a licence"})
	if valid || !strings.Contains(errs, "The test architect stopped: needs a mac") || !strings.Contains(errs, "The environment engineer stopped: needs a licence") || !strings.HasSuffix(errs, "no cell") {
		t.Errorf("invalid plan = %v %q", valid, errs)
	}
	_, err := v.call(t, "onboarding.tests_check", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func TestTestsHead(t *testing.T) {
	v := newEnv(t)
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "abc1234"))
	out, err := v.call(t, "onboarding.tests_head", map[string]any{"repo": "acme/app", "head_sha": "def5678"})
	must(t, err)
	if m := toMap(t, out); m["ref"] != "def5678" || fmt.Sprint(m["if_pushed"]) != "[def5678]" {
		t.Errorf("pushed = %v", m)
	}
	out, err = v.call(t, "onboarding.tests_head", map[string]any{"repo": "acme/app", "head_sha": ""})
	must(t, err)
	if m := toMap(t, out); m["ref"] != "abc1234" || fmt.Sprint(m["if_pushed"]) != "[]" {
		t.Errorf("nothing pushed = %v", m)
	}
	_, err = v.call(t, "onboarding.tests_head", map[string]any{"repo": "acme/missing"})
	wantErr(t, err, "read main")
	_, err = v.call(t, "onboarding.tests_head", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
	v.project.ID = domain.NewID()
	_, err = v.call(t, "onboarding.tests_head", map[string]any{"repo": "acme/app"})
	wantErr(t, err, "get project")
}

func TestBaselineRef(t *testing.T) {
	v := newEnv(t)
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "abc1234"))
	for with, want := range map[string]string{"def5678": "def5678", "": "abc1234"} {
		out, err := v.call(t, "onboarding.baseline_ref", map[string]any{"repo": "acme/app", "merged": with})
		must(t, err)
		if got := toMap(t, out)["sha"]; got != want {
			t.Errorf("merged %q: sha = %v, want %s", with, got, want)
		}
	}
	_, err := v.call(t, "onboarding.baseline_ref", map[string]any{"repo": "acme/missing"})
	wantErr(t, err, "read main")
	_, err = v.call(t, "onboarding.baseline_ref", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func smokeEnvs(names ...string) []any {
	var out []any
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "provider": "docker", "change": "added", "reason": "r"})
	}
	return out
}

func TestTestsSmokePlan(t *testing.T) {
	v := newEnv(t)
	v.matrixFiles(t, "abc1234")
	smoke := func(ref string, envs []any) map[string]any {
		out, err := v.call(t, "tests.smoke_plan", map[string]any{"repo": "acme/app", "ref": ref, "environments": envs})
		must(t, err)
		return toMap(t, out)
	}
	// Added and changed environments get a job, unchanged and removed ones not.
	envs := []any{
		map[string]any{"name": "linux-go", "provider": "docker", "change": "changed", "reason": "r"},
		map[string]any{"name": "win", "provider": "machine", "change": "unchanged", "reason": "r"},
		map[string]any{"name": "gone", "provider": "machine", "change": "removed", "reason": "r"},
	}
	got := smoke("abc1234", envs)
	reqs := got["requests"].([]any)
	if len(reqs) != 1 || len(got["if_actions"].([]any)) != 0 {
		t.Fatalf("smoke plan = %v", got)
	}
	r := reqs[0].(map[string]any)
	if r["environment"].(map[string]any)["name"] != "linux-go" || fmt.Sprint(r["needs"]) != "[linux]" || r["command"].(map[string]any)["shell"] != "echo smoke" {
		t.Errorf("request = %v", r)
	}
	got = smoke("abc1234", smokeEnvs("win", "linux-go", "win"))
	if reqs := got["requests"].([]any); len(reqs) != 2 || fmt.Sprint(reqs[0].(map[string]any)["needs"]) != "[linux]" || fmt.Sprint(reqs[1].(map[string]any)["needs"]) != "[windows]" {
		t.Errorf("one job per environment, with the needs of its first cell: %v", reqs)
	}

	// The versions commands are the smoke command, by tool name.
	must(t, v.tracker.SetFile("acme/app", "abc1234", ".forecrew/environments/linux-go.yaml", []byte(linuxGoYAML+"versions: {node: node --version, go: go version}\n")))
	got = smoke("abc1234", smokeEnvs("linux-go"))
	if c := got["requests"].([]any)[0].(map[string]any)["command"].(map[string]any)["shell"]; c != "go version && node --version" {
		t.Errorf("smoke command = %v", c)
	}

	// An environment on GitHub Actions needs the workflow, whether it is new
	// or not, and is smoked only once the workflow is on the default branch.
	must(t, v.tracker.SetFile("acme/app", "abc1234", ".forecrew/environments/win.yaml", []byte("version: 1\nname: win\nprovider: actions\nruns_on: windows-2022\n")))
	got = smoke("abc1234", smokeEnvs("win"))
	if fmt.Sprint(got["if_actions"]) != "[acme/app]" || len(got["requests"].([]any)) != 0 {
		t.Errorf("actions environment, no workflow on main: %v", got)
	}
	must(t, v.tracker.SetFile("acme/app", "main", ".github/workflows/forecrew-matrix.yml", []byte("name: x\n")))
	got = smoke("abc1234", smokeEnvs("win"))
	if fmt.Sprint(got["if_actions"]) != "[acme/app]" || len(got["requests"].([]any)) != 1 {
		t.Errorf("actions environment, workflow on main: %v", got)
	}

	// A matrix that does not load is matrix.plan's to report.
	got = smoke("nowhere", envs)
	if len(got["requests"].([]any)) != 0 || len(got["if_actions"].([]any)) != 0 {
		t.Errorf("invalid matrix: %v", got)
	}
	must(t, v.tracker.SetFile("acme/app", "toobig", ".forecrew/matrix.yaml", []byte(strings.Replace(matrixYAML, "os: [linux, windows]", "os: [a, b, c, d, e, f, g, h, i]\n  x: [a, b, c, d, e, f, g, h, i]\n  y: [a, b, c, d, e, f, g, h, i]", 1))))
	got = smoke("toobig", envs)
	if len(got["requests"].([]any)) != 0 {
		t.Errorf("matrix over the limits: %v", got)
	}

	_, err := v.call(t, "tests.smoke_plan", map[string]any{"repo": "acme/app"})
	wantErr(t, err, "repo and ref are required")
	_, err = v.call(t, "tests.smoke_plan", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

type readFails struct{ github.Tracker }

func (readFails) GetFile(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("boom")
}

func TestTestsSmokePlanReadError(t *testing.T) {
	v := newEnv(t)
	v.wrapped = readFails{v.tracker}
	v.register()
	_, err := v.call(t, "tests.smoke_plan", map[string]any{"repo": "acme/app", "ref": "abc"})
	wantErr(t, err, "boom")

	// The workflow file cannot be read.
	v = newEnv(t)
	v.matrixFiles(t, "abc1234")
	v.wrapped = workflowReadFails{v.tracker}
	v.register()
	_, err = v.call(t, "tests.smoke_plan", map[string]any{"repo": "acme/app", "ref": "abc1234"})
	wantErr(t, err, "read .github/workflows/forecrew-matrix.yml")
	v.project.ID = domain.NewID()
	_, err = v.call(t, "tests.smoke_plan", map[string]any{"repo": "acme/app", "ref": "abc1234"})
	wantErr(t, err, "get project")
}

type workflowReadFails struct{ *testkit.Tracker }

func (w workflowReadFails) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if strings.HasPrefix(path, ".github/") {
		return nil, errors.New("boom")
	}
	return w.Tracker.GetFile(ctx, repo, ref, path)
}

func TestTestsSmokeResult(t *testing.T) {
	v := newEnv(t)
	job := func(env string, exit int) map[string]any {
		j := domain.Job{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
		must(t, v.st.Jobs().Create(t.Context(), &j))
		run := domain.TestRun{AccountID: v.account, ProjectID: v.project.ID, JobID: j.ID, SHA: "abc1234", Environment: env, Status: domain.TestRunFailed, ExitCode: exit, Attempt: 1}
		must(t, v.st.TestRuns().Create(t.Context(), &run, nil))
		return map[string]any{"smoke_job": map[string]any{"job_id": j.ID.String()}}
	}
	result := func(runs ...map[string]any) string {
		out, err := v.call(t, "tests.smoke_result", map[string]any{"runs": runs})
		must(t, err)
		return toMap(t, out)["errors"].(string)
	}
	if got := result(); got != "" {
		t.Errorf("no smoke jobs: %q", got)
	}
	if got := result(job("linux-go", 0)); got != "" {
		t.Errorf("a clean run: %q", got)
	}
	got := result(job("linux-go", 127), job("win", 1), job("linux-go", 0))
	if !strings.Contains(got, "`linux-go` exited with 127") || !strings.Contains(got, "`win` exited with 1") {
		t.Errorf("errors = %q", got)
	}
	_, err := v.call(t, "tests.smoke_result", map[string]any{"runs": []any{map[string]any{}}})
	wantErr(t, err, "no job_id")
	_, err = v.call(t, "tests.smoke_result", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func TestTestsBranch(t *testing.T) {
	v := newEnv(t)
	run := func(id uuid.UUID) string {
		out, err := v.actions["onboarding.tests_branch"](t.Context(), workflow.ActionCall{Key: "k", Run: domain.WorkflowRun{ID: id}})
		must(t, err)
		return out.(map[string]string)["branch"]
	}
	a, b := domain.NewID(), domain.NewID()
	first, again, other := run(a), run(a), run(b)
	if first != again || first == other || !strings.HasPrefix(first, "agent/onboarding-tests-") {
		t.Errorf("branches = %s %s %s", first, again, other)
	}
	_, err := v.call(t, "onboarding.tests_branch", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

func TestTestsProposal(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "onboarding.tests_proposal", map[string]any{"plan": architectOutput(), "environments": engineerOutput()})
	must(t, err)
	m := toMap(t, out)
	body := m["body"].(string)
	if m["title"] != "Add the Forecrew test plan" {
		t.Errorf("title = %v", m["title"])
	}
	for _, want := range []string{
		"Merge this PR to approve", "## Strategy\n\nRun the unit tests", "- `os` (linux, windows): path separators differ",
		"- axes `os` added: the profile lists both platforms", "## Environments\n\nDocker for Linux", "- `linux-go` (docker, added): serves os=linux",
		"- Go 1.27 is the only version (confidence medium, impact low)", "## Open questions\n\n- Which Go versions must be supported?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "forecrew-matrix.yml") {
		t.Errorf("the workflow is named though none was installed:\n%s", body)
	}
	out, err = v.call(t, "onboarding.tests_proposal", map[string]any{"plan": architectOutput(), "environments": engineerOutput(), "workflow": []string{"acme/app"}})
	must(t, err)
	if body := toMap(t, out)["body"].(string); !strings.Contains(body, "Adds `.github/workflows/forecrew-matrix.yml`") {
		t.Errorf("body lacks the workflow:\n%s", body)
	}
	// Text from the agents stays on one line of a list.
	plan := architectOutput()
	plan["open_questions"] = []any{"Which?\n# Injected heading"}
	out, err = v.call(t, "onboarding.tests_proposal", map[string]any{"plan": plan, "environments": map[string]any{}})
	must(t, err)
	body = toMap(t, out)["body"].(string)
	if !strings.Contains(body, "- Which? # Injected heading") || strings.Contains(body, "## Environments") {
		t.Errorf("body = %s", body)
	}
	for name, with := range map[string]map[string]any{
		"bad plan":         {"plan": "x", "environments": map[string]any{}},
		"bad environments": {"plan": map[string]any{}, "environments": "x"},
		"unknown field":    {"x": 1},
	} {
		if _, err := v.call(t, "onboarding.tests_proposal", with); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestFinishOnboarding(t *testing.T) {
	for status, want := range map[domain.ProjectStatus]string{domain.ProjectOnboarding: "onboarded", domain.ProjectOnboarded: "onboarded"} {
		v := newEnv(t, status)
		out, err := v.call(t, "onboarding.finish", nil)
		must(t, err)
		if got := v.getProject(t).Status; string(got) != want || toMap(t, out)["status"] != want {
			t.Errorf("%s: status = %s, output %v", status, got, out)
		}
	}
	v := newEnv(t)
	_, err := v.call(t, "onboarding.finish", nil)
	wantErr(t, err, "is registered, not onboarding")
	v.project.ID = domain.NewID()
	_, err = v.call(t, "onboarding.finish", nil)
	wantErr(t, err, "get project")
}

// startTests without the roles onboards the project itself.
func TestStartTestsWithoutRoles(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	out, err := v.call(t, "onboarding.start_tests", map[string]any{"repo": "acme/app", "project": "app"})
	must(t, err)
	if m := toMap(t, out); m["started"] == true {
		t.Errorf("output = %v", m)
	}
	if got := v.getProject(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
	_, err = v.call(t, "onboarding.start_tests", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
	registered := newEnv(t)
	_, err = registered.call(t, "onboarding.start_tests", map[string]any{"repo": "acme/app", "project": "app"})
	wantErr(t, err, "not onboarding")
}

type startRecorder struct {
	req  workflow.StartRequest
	name string
	err  error
}

func (s *startRecorder) Start(_ context.Context, name string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	s.name, s.req = name, req
	return domain.WorkflowRun{ID: req.ID}, s.err
}

func TestStartTests(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	rec := &startRecorder{}
	v.cfg = &config.Config{Roles: map[string]config.Role{"test-architect": {}, "environment-engineer": {}}}
	v.runs = rec
	v.register()
	key := domain.NewID()
	out, err := v.callKey(t, "onboarding.start_tests", key.String(), map[string]any{"repo": "acme/app", "project": "app"})
	must(t, err)
	if m := toMap(t, out); m["started"] != true || m["run_id"] != key.String() {
		t.Errorf("output = %v", m)
	}
	if rec.name != playbooks.PlaybookOnboardingTests || rec.req.ID != key || rec.req.ProjectID != v.project.ID || rec.req.Inputs["repo"] != "acme/app" || rec.req.Inputs["project"] != "app" {
		t.Errorf("started %s %+v", rec.name, rec.req)
	}
	if got := v.getProject(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status = %s: the tests run ends the onboarding", got)
	}
	_, err = v.callKey(t, "onboarding.start_tests", "not-a-uuid", map[string]any{"repo": "acme/app", "project": "app"})
	wantErr(t, err, "key")
	rec.err = errors.New("no engine")
	_, err = v.call(t, "onboarding.start_tests", map[string]any{"repo": "acme/app", "project": "app"})
	wantErr(t, err, "start the tests of app")

	// One role missing: no tests part.
	delete(v.cfg.Roles, "environment-engineer")
	_, err = v.call(t, "onboarding.start_tests", map[string]any{"repo": "acme/app", "project": "app"})
	must(t, err)
	if got := v.getProject(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
}

func TestProjectRegisterKeep(t *testing.T) {
	for status, wantErr := range map[domain.ProjectStatus]string{
		domain.ProjectOnboarding: "", domain.ProjectOnboarded: "", domain.ProjectRegistered: "is registered, not onboarding or onboarded",
	} {
		v := newEnv(t, status)
		out, err := v.call(t, "project.register", map[string]any{"repo": "acme/app", "keep": true})
		if wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("%s: error = %v", status, err)
			}
			continue
		}
		must(t, err)
		if got := v.getProject(t).Status; got != status || toMap(t, out)["default_branch"] != "main" {
			t.Errorf("%s: status = %s, output %v", status, got, out)
		}
	}
	v := newEnv(t, domain.ProjectOnboarded)
	_, err := v.call(t, "project.register", map[string]any{"repo": "acme/app"})
	wantErr(t, err, "is onboarded, not registered")
}

func TestCompleteOnboardingHold(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	must(t, v.tracker.SetFile("acme/app", "main", ".forecrew/profile.yaml", []byte(profileYAML)))
	out, err := v.call(t, "onboarding.complete", map[string]any{"repo": "acme/app", "ref": "main", "hold": true})
	must(t, err)
	p := v.getProject(t)
	if p.Status != domain.ProjectOnboarding || len(p.Profile) == 0 || toMap(t, out)["status"] != "onboarding" {
		t.Errorf("project = %s %s, output %v", p.Status, p.Profile, out)
	}
}

// settle puts a project whose profile was stored to onboarded, one whose
// profile was not to registered.
func TestSettleOnboardingAfterTheProfile(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	out, err := v.call(t, "onboarding.settle", map[string]any{"profile_done": true})
	must(t, err)
	if got := v.getProject(t).Status; got != domain.ProjectOnboarded || toMap(t, out)["status"] != "onboarded" {
		t.Errorf("status = %s, output %v", got, out)
	}

	// A run whose step complete succeeded has stored it too.
	v = newEnv(t, domain.ProjectOnboarding)
	run := domain.WorkflowRun{AccountID: v.account, ProjectID: v.project.ID, Playbook: "onboarding"}
	must(t, v.st.WorkflowRuns().Create(t.Context(), &run))
	settle := func() {
		_, err := v.actions["onboarding.settle"](t.Context(), workflow.ActionCall{Key: domain.NewID().String(), Run: run})
		must(t, err)
	}
	step := domain.StepRun{AccountID: v.account, RunID: run.ID, StepID: "complete", Attempt: 1, Status: domain.StepFailed}
	must(t, v.st.StepRuns().Create(t.Context(), &step))
	settle() // failed: the profile was not stored
	if got := v.getProject(t).Status; got != domain.ProjectRegistered {
		t.Fatalf("status after a failed complete = %s", got)
	}
	must(t, v.st.Projects().UpdateStatus(t.Context(), v.account, v.project.ID, domain.ProjectRegistered, domain.ProjectOnboarding))
	step.Status = domain.StepSucceeded
	must(t, v.st.StepRuns().Update(t.Context(), &step))
	settle()
	if got := v.getProject(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status after a complete that succeeded = %s", got)
	}
	_, err = v.call(t, "onboarding.settle", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")
}

// baselineEnv is a verified change whose runs are the baseline.
func baselineEnv(t *testing.T) *verified {
	t.Helper()
	v := newVerified(t)
	playbooks.Register(v.actions, playbooks.Deps{
		Store: v.st, Tracker: v.tracker, Signals: v.bus, Sizing: config.DefaultSizing(), Roles: testRoles(), Now: v.clock.Now, Evidence: v.ev,
		Config: &config.Config{Server: config.Server{PublicBaseURL: "https://forecrew.example.com"}}, Packs: forecrew.Files,
	})
	return v
}

func (v *verified) baseline(t *testing.T) string {
	t.Helper()
	got := v.build(t, map[string]any{"baseline": true})
	return got.ReportID
}

func (v *verified) fileIssues(t *testing.T, report string) []map[string]any {
	t.Helper()
	out, err := v.call(t, "baseline.file_issues", map[string]any{"report_id": report})
	must(t, err)
	var issues []map[string]any
	for _, i := range toMap(t, out)["issues"].([]any) {
		issues = append(issues, i.(map[string]any))
	}
	return issues
}

func TestBaselineIssues(t *testing.T) {
	v := baselineEnv(t)
	// unit fails in linux-go and errors in win; e2e passes.
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA", "pkg::TestB")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	report := v.baseline(t)
	issues := v.fileIssues(t, report)
	if len(issues) != 1 || issues[0]["created"] != true || issues[0]["title"] != "Baseline: unit fails in linux-go" {
		t.Fatalf("issues = %v", issues)
	}
	open, err := v.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	if len(open) != 1 {
		t.Fatalf("open issues = %+v", open)
	}
	body := open[0].Body
	for _, want := range []string{"<!-- forecrew-baseline unit/linux-go -->", "`" + headSHA + "`", "Cells: `os=linux`", "- `pkg::TestA`", "- `pkg::TestB`", "Baseline report `" + report + "`", "https://forecrew.example.com/api/v1/artifacts/"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body lacks %q:\n%s", want, body)
		}
	}
	// Again, from the same report: the open issue is found by its marker.
	again := v.fileIssues(t, report)
	if len(again) != 1 || again[0]["created"] == true || again[0]["number"] != issues[0]["number"] {
		t.Errorf("repeat = %v", again)
	}
	// A later baseline that fails the same way adds no second issue.
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	if got := v.fileIssues(t, v.baseline(t)); len(got) != 1 || got[0]["created"] == true {
		t.Errorf("later baseline = %v", got)
	}
	// Once a human closes it, a failure that is still there is filed again.
	closed := "closed"
	_, err = v.tracker.UpdateIssue(t.Context(), "acme/app", open[0].Number, github.IssueUpdate{State: &closed})
	must(t, err)
	if got := v.fileIssues(t, report); len(got) != 1 || got[0]["created"] != true {
		t.Errorf("after the close = %v", got)
	}
}

// A suite that errors has no failing test to name; many failing tests are cut.
func TestBaselineIssuesErroredAndLong(t *testing.T) {
	v := baselineEnv(t)
	var many []string
	for i := range 30 {
		many = append(many, fmt.Sprintf("pkg::Test%02d", i))
	}
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, many...)...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	j := domain.Job{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.JobKindCommand, Spec: domain.JobSpec{Repo: "acme/app", Kind: domain.JobKindCommand}}
	must(t, v.st.Jobs().Create(t.Context(), &j))
	errored := domain.TestRun{AccountID: v.account, ProjectID: v.project.ID, JobID: j.ID, ChangeID: v.change.ID, SHA: headSHA, Suite: "e2e", Environment: "linux-go",
		Cell: map[string]string{"os": "linux"}, Status: domain.TestRunErrored, Attempt: 1}
	must(t, v.st.TestRuns().Create(t.Context(), &errored, nil))
	issues := v.fileIssues(t, v.baseline(t))
	if len(issues) != 2 {
		t.Fatalf("issues = %v", issues)
	}
	open, err := v.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	byTitle := map[string]string{}
	for _, i := range open {
		byTitle[i.Title] = i.Body
	}
	if body := byTitle["Baseline: unit fails in linux-go"]; strings.Count(body, "- `pkg::Test") != 20 || !strings.Contains(body, "- and more, see the report") {
		t.Errorf("long issue:\n%s", body)
	}
	if body := byTitle["Baseline: e2e fails in linux-go"]; !strings.Contains(body, "left no failing test") {
		t.Errorf("errored issue:\n%s", body)
	}
}

// Names from the repo show as inline code, and cannot mention anyone.
func TestBaselineIssuesNamesAreCode(t *testing.T) {
	v := baselineEnv(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::Test`@octocat\n# heading")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	v.fileIssues(t, v.baseline(t))
	open, err := v.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	if len(open) != 1 || !strings.Contains(open[0].Body, "- `pkg::Test'@octocat # heading`") {
		t.Errorf("open = %+v", open)
	}
}

func TestBaselineIssuesFlaky(t *testing.T) {
	v := baselineEnv(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	issues := v.fileIssues(t, v.baseline(t))
	if len(issues) != 1 || issues[0]["title"] != "Baseline: flaky tests" {
		t.Fatalf("issues = %v", issues)
	}
}

func TestBaselineIssuesNothingToFile(t *testing.T) {
	v := baselineEnv(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, "pkg::TestA")...)
	if issues := v.fileIssues(t, v.baseline(t)); len(issues) != 0 {
		t.Errorf("issues = %v", issues)
	}
	open, _ := v.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	if len(open) != 0 {
		t.Errorf("open = %+v", open)
	}
}

func TestBaselineIssuesErrors(t *testing.T) {
	v := baselineEnv(t)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
	report := v.baseline(t)
	_, err := v.call(t, "baseline.file_issues", map[string]any{"report_id": domain.NewID().String()})
	wantErr(t, err, "get verification report")
	_, err = v.call(t, "baseline.file_issues", map[string]any{"x": 1})
	wantErr(t, err, "unknown field")

	other := newVerified(t)
	_, err = other.call(t, "baseline.file_issues", map[string]any{"report_id": report})
	wantErr(t, err, "get verification report") // another account's report is not there

	// A report of another project of the account.
	stored, err := v.st.VerificationReports().Get(t.Context(), v.account, uuid.MustParse(report))
	must(t, err)
	p2 := domain.Project{AccountID: v.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, v.st.Projects().Create(t.Context(), &p2))
	c2 := domain.Change{AccountID: v.account, ProjectID: p2.ID, Repo: "acme/other", Number: 1, Branch: "agent/x", BaseRef: "main", HeadSHA: headSHA, State: domain.ChangeMerged}
	must(t, v.st.Changes().Upsert(t.Context(), &c2))
	foreign := domain.VerificationReport{AccountID: v.account, ProjectID: p2.ID, ChangeID: c2.ID, SHA: headSHA, Status: stored.Status, Report: stored.Report}
	must(t, v.st.VerificationReports().Upsert(t.Context(), &foreign))
	_, err = v.call(t, "baseline.file_issues", map[string]any{"report_id": foreign.ID.String()})
	wantErr(t, err, "belongs to another project")

	for call, errs := range map[string]string{"CreateIssue": "file the baseline issue", "ListOpenIssues": "list the baseline issues", "EnsureLabel": "ensure label"} {
		w := newVerified(t)
		w.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, "pkg::TestA")...)
		rep := w.baseline(t)
		w.wrapped = failing{Tracker: w.tracker, errs: map[string]error{call: errors.New("boom")}}
		playbooks.Register(w.actions, playbooks.Deps{Store: w.st, Tracker: w.wrapped, Signals: w.bus, Roles: testRoles(), Now: w.clock.Now, Evidence: w.ev})
		_, err := w.call(t, "baseline.file_issues", map[string]any{"report_id": rep})
		wantErr(t, err, errs)
	}
}

// Many flaky tests are cut like many failures, and only a marker at the start of
// an issue body counts: a human's issue that quotes it is not the one.
func TestBaselineIssuesFlakyBoundAndMarkerPrefix(t *testing.T) {
	v := baselineEnv(t)
	var ids []string
	for i := range 25 {
		ids = append(ids, fmt.Sprintf("pkg::Test%02d", i))
	}
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestFailed, ids...)...)
	v.addRun(t, "linux-go", "linux", "unit", testResults(domain.TestPassed, ids...)...)
	v.addRun(t, "win", "windows", "unit", testResults(domain.TestPassed, "pkg::Test00")...)
	v.addRun(t, "linux-go", "linux", "e2e", testResults(domain.TestPassed, "e2e::Flow")...)
	_, err := v.tracker.CreateIssue(t.Context(), "acme/app", github.IssueInput{Title: "Quote", Body: "see <!-- forecrew-baseline flaky -->", Labels: []string{"forecrew:baseline"}})
	must(t, err)
	issues := v.fileIssues(t, v.baseline(t))
	if len(issues) != 1 || issues[0]["created"] != true {
		t.Fatalf("issues = %v", issues)
	}
	open, _ := v.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	var body string
	for _, i := range open {
		if i.Title == "Baseline: flaky tests" {
			body = i.Body
		}
	}
	if strings.Count(body, "- `pkg::Test") != 20 || !strings.Contains(body, "- and more, see the report") {
		t.Errorf("flaky issue:\n%s", body)
	}
}

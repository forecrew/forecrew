package playbooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

const testsBranch = "agent/onboarding-tests"

// withTests gives the env the roles of the test part of onboarding, so the
// onboarding playbook starts onboarding-tests, a fake GitHub Actions surface
// when fake is true, and the playbooks the engine that runs them.
func (v *env) withTests(t *testing.T, fake bool) *testkit.FakeActions {
	t.Helper()
	v.wrapped = hexMerges{Tracker: v.tracker}
	v.cfg = &config.Config{Roles: map[string]config.Role{"test-architect": {}, "environment-engineer": {}}}
	var f *testkit.FakeActions
	if fake {
		f = testkit.NewFakeActions(t, v.clock.Now)
		v.ghActions = f.Client
	}
	v.register()
	return f
}

// hexMerges is a tracker whose merge commits have real-looking IDs, as the
// verification wants: the fake's are "merge-<number>".
type hexMerges struct{ *testkit.Tracker }

func mergeSHA(number int) string { return fmt.Sprintf("%040x", number) }

func (h hexMerges) MergePR(ctx context.Context, repo string, number int, in github.MergeInput) (string, error) {
	if _, err := h.Tracker.MergePR(ctx, repo, number, in); err != nil {
		return "", err
	}
	return mergeSHA(number), nil
}

func (h hexMerges) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	for n := 1; n < 100; n++ {
		if ref == mergeSHA(n) {
			ref = fmt.Sprintf("merge-%d", n)
		}
	}
	return h.Tracker.GetFile(ctx, repo, ref, path)
}

// testsPart is the run of onboarding-tests: the test plays the workers, the
// webhook and the human, as for the other playbooks.
type testsPart struct {
	*runner
	heads int
}

// testsOf returns the onboarding-tests run the onboarding run o started.
func testsOf(t *testing.T, o *onboarding) *testsPart {
	t.Helper()
	s := o.waitStep(t, "tests", domain.StepSucceeded) // the run has the ID of the step run
	var wrapped struct {
		Output struct {
			Started bool   `json:"started"`
			RunID   string `json:"run_id"`
		} `json:"output"`
	}
	must(t, json.Unmarshal(s.Output, &wrapped))
	if out := wrapped.Output; !out.Started || out.RunID != s.ID.String() {
		t.Fatalf("onboarding.start_tests output = %s", s.Output)
	}
	child, err := o.st.WorkflowRuns().Get(t.Context(), o.account, s.ID)
	must(t, err)
	r := *o.runner
	r.run = child
	return &testsPart{runner: &r}
}

// startTestsRun starts onboarding-tests on its own, as a refresh does.
func startTestsRun(t *testing.T, v *env) *testsPart {
	t.Helper()
	r := startRun(t, v, "onboarding-tests", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"repo": "acme/app", "project": "app"},
	})
	return &testsPart{runner: r}
}

var testPlanFiles = map[string]string{
	".forecrew/matrix.yaml":                  matrixYAML,
	".forecrew/environments/linux-go.yaml":   linuxGoYAML,
	".forecrew/environments/win.yaml":        winYAML,
	".forecrew/test-strategy.md":             "# Strategy\n",
	".forecrew/environments/unused-win.yaml": "not read\n",
}

func architectOutput() map[string]any {
	return map[string]any{
		"summary":        "Run the unit tests on both systems and the end-to-end flow on Linux.",
		"axes":           []any{map[string]any{"name": "os", "values": []any{"linux", "windows"}, "reason": "path separators differ"}},
		"matrix_changes": []any{map[string]any{"part": "axes", "name": "os", "change": "added", "reason": "the profile lists both platforms"}},
		"assumptions":    []any{map[string]any{"statement": "Go 1.27 is the only version", "confidence": "medium", "impact": "low", "verify_by": "human"}},
		"open_questions": []any{"Which Go versions must be supported?"},
		"escalation":     nil,
	}
}

func engineerOutput() map[string]any {
	return map[string]any{
		"summary": "Docker for Linux, the machine for Windows.",
		"environments": []any{
			map[string]any{"name": "linux-go", "provider": "docker", "change": "added", "reason": "serves os=linux"},
			map[string]any{"name": "win", "provider": "machine", "change": "added", "reason": "serves os=windows"},
		},
		"assumptions": []any{},
		"escalation":  nil,
	}
}

// job waits for the job at path and completes it as its worker would: the
// files it committed are on its branch at the head sha it reports, as is the
// PR once there is one. It returns the job's request.
func (p *testsPart) job(t *testing.T, path string, output map[string]any, files map[string]string) workflow.JobRequest {
	t.Helper()
	s := p.waitStep(t, path, domain.StepWaiting)
	var req workflow.JobRequest
	for _, r := range p.jobs.Requests() {
		if r.ID == s.JobID {
			req = r
		}
	}
	raw, err := json.Marshal(output)
	must(t, err)
	sha := ""
	if files != nil {
		p.heads++
		sha = fmt.Sprintf("tests-%d", p.heads)
		for _, ref := range []string{sha, testsBranch} {
			for name, content := range files {
				must(t, p.tracker.SetFile("acme/app", ref, name, []byte(content)))
			}
		}
		if pr, err := p.tracker.FindPR(t.Context(), "acme/app", testsBranch); err == nil {
			must(t, p.tracker.SetHeadSHA("acme/app", pr.Number, sha))
		}
	}
	p.finishJob(t, s.JobID, raw, sha, 0)
	return req
}

// plan completes the architect and the engineer of round with valid output.
func (p *testsPart) plan(t *testing.T, round, try int) (architect, engineer workflow.JobRequest) {
	t.Helper()
	base := fmt.Sprintf("plan/%d/draft/%d/", round, try)
	architect = p.job(t, base+"architect", architectOutput(), testPlanFiles)
	engineer = p.job(t, base+"engineer", engineerOutput(), testPlanFiles)
	return architect, engineer
}

// smoke completes the smoke jobs of round, one per environment.
func (p *testsPart) smoke(t *testing.T, round int, n int) []workflow.JobRequest {
	t.Helper()
	var reqs []workflow.JobRequest
	for i := range n {
		reqs = append(reqs, p.job(t, fmt.Sprintf("plan/%d/deliver/0/smoke/%d/smoke_job", round, i), map[string]any{}, nil))
	}
	return reqs
}

// checks reports the checks of the PR head.
func (p *testsPart) checks(t *testing.T, round int, conclusion domain.CheckConclusion) {
	t.Helper()
	p.waitStep(t, fmt.Sprintf("plan/%d/deliver/0/approval/0/checks", round), domain.StepWaiting)
	pr, err := p.tracker.FindPR(t.Context(), "acme/app", testsBranch)
	must(t, err)
	p.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Conclusion: conclusion})
}

// reach runs round with valid output up to the pending test_plan_approval gate.
func (p *testsPart) reach(t *testing.T, round int) domain.Gate {
	t.Helper()
	p.plan(t, round, 1)
	p.smoke(t, round, 2)
	p.checks(t, round, domain.ChecksSuccess)
	return p.pendingGate(t, domain.GateTestPlanApproval)
}

// cellRun stores the test run a command job reports: suite and cell as the
// job's request names them, failed with the failing tests, else passed.
func (p *testsPart) cellRun(t *testing.T, path string, failing ...string) workflow.JobRequest {
	t.Helper()
	return p.cellResults(t, path, failing, "pkg::TestOK")
}

// cellResults is cellRun for a run whose passing tests are named.
func (p *testsPart) cellResults(t *testing.T, path string, failing []string, passing ...string) workflow.JobRequest {
	t.Helper()
	s := p.waitStep(t, path, domain.StepWaiting)
	var req workflow.JobRequest
	for _, r := range p.jobs.Requests() {
		if r.ID == s.JobID {
			req = r
		}
	}
	p.finishJob(t, s.JobID, []byte("{}"), "", 0)
	status, counts := domain.TestRunPassed, domain.TestCounts{Passed: 1}
	results := testResults(domain.TestPassed, passing...)
	if len(failing) > 0 {
		status, counts = domain.TestRunFailed, domain.TestCounts{Failed: len(failing)}
		results = testResults(domain.TestFailed, failing...)
	}
	run := domain.TestRun{
		AccountID: p.account, ProjectID: p.project.ID, JobID: s.JobID, SHA: req.BaseRef, Suite: req.Suite, Environment: req.Environment.Name,
		Cell: req.Cell, Status: status, Counts: counts, Attempt: 1, Versions: map[string]string{"go": "go1.27"},
	}
	must(t, p.st.TestRuns().Create(t.Context(), &run, results))
	return req
}

func (p *testsPart) openBaselineIssues(t *testing.T) []string {
	t.Helper()
	issues, err := p.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	var titles []string
	for _, i := range issues {
		titles = append(titles, i.Title)
	}
	return titles
}

// onboardedUpToTests runs the profile part of the onboarding with the test
// roles configured until the test run starts.
func onboardedUpToTests(t *testing.T, v *env) (*onboarding, *testsPart) {
	t.Helper()
	o := startOnboarding(t, v)
	o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	return o, testsOf(t, o)
}

// The profile is merged, the test plan is proposed in a PR and approved, the
// baseline runs with one failing cell, and the failure becomes one issue.
func TestOnboardingTestsEndToEnd(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	o, p := onboardedUpToTests(t, v)
	if got := o.projectNow(t); got.Status != domain.ProjectOnboarding || !strings.Contains(string(got.Profile), `"name":"shop"`) {
		t.Errorf("after the profile: status %s, profile %s", got.Status, got.Profile)
	}

	architect, engineer := p.plan(t, 1, 1)
	for _, req := range []workflow.JobRequest{architect, engineer} {
		if req.Branch != testsBranch || req.Repo != "acme/app" || len(req.Needs) != 1 || req.Needs[0] != "linux" || req.Feedback != "" || req.ProjectID != p.project.ID {
			t.Errorf("job request = %+v", req)
		}
	}
	if architect.Role != "test-architect" || engineer.Role != "environment-engineer" {
		t.Errorf("roles = %s, %s", architect.Role, engineer.Role)
	}
	assertSmokeJobs(t, p.smoke(t, 1, 2))
	p.checks(t, 1, domain.ChecksSuccess)
	pr := assertTestsPR(t, o, p)

	p.decide(t, domain.GateTestPlanApproval, mcp.ToolApprove, "")
	eventually(t, "the merge", func() bool {
		merged, _ := p.tracker.GetPR(t.Context(), "acme/app", pr.Number)
		return merged.Merged
	})
	// The baseline: every suite in every cell, on the merge commit.
	for i, failing := range [][]string{{"pkg::TestParse"}, nil, nil} {
		assertBaselineJob(t, p.cellRun(t, fmt.Sprintf("cells/%d/cell_job", i), failing...), pr.Number)
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status during the baseline = %s", got)
	}
	// The failed required cell runs once more, and fails again: not flaky.
	rerun := p.cellRun(t, "reruns/0/rerun_job", "pkg::TestParse")
	assertBaselineJob(t, rerun, pr.Number)
	if rerun.Suite != "unit" || rerun.Environment.Name != "linux-go" {
		t.Errorf("rerun request = %+v", rerun)
	}
	p.waitRun(t, domain.RunSucceeded)
	o.waitRun(t, domain.RunSucceeded)

	if got := o.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
	assertBaselineIssue(t, p, pr.Number)
}

// assertSmokeJobs checks one smoke job per new environment, on the branch.
func assertSmokeJobs(t *testing.T, smokes []workflow.JobRequest) {
	t.Helper()
	envs := map[string]workflow.JobRequest{}
	for _, s := range smokes {
		if s.Kind != domain.JobKindCommand || s.Repo != "acme/app" || s.BaseRef != "main" || s.Branch != testsBranch || s.Command == nil || s.Command.Shell == "" || s.Environment == nil {
			t.Errorf("smoke request = %+v", s)
		}
		envs[s.Environment.Name] = s
	}
	if len(envs) != 2 || envs["win"].Needs[0] != "windows" {
		t.Errorf("smoke environments = %v", envs)
	}
}

// assertTestsPR checks the pending gate and the PR it is about, and returns the PR.
func assertTestsPR(t *testing.T, o *onboarding, p *testsPart) github.PullRequest {
	t.Helper()
	g := p.pendingGate(t, domain.GateTestPlanApproval)
	pr, err := p.tracker.FindPR(t.Context(), "acme/app", testsBranch)
	must(t, err)
	if g.SubjectRepo != "acme/app" || g.SubjectNumber != pr.Number || !strings.Contains(g.Title, "round 1") || pr.Base != "main" {
		t.Errorf("gate = %+v, PR %+v", g, pr)
	}
	for _, want := range []string{"Run the unit tests on both systems", "`os` (linux, windows)", "`linux-go` (docker, added)", "Which Go versions must be supported?", "Go 1.27 is the only version"} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, pr.Body)
		}
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status before the approval = %s", got)
	}
	return pr
}

// assertBaselineJob checks a command job of the baseline: on the merge
// commit of the PR, nothing laid over it.
func assertBaselineJob(t *testing.T, r workflow.JobRequest, pr int) {
	t.Helper()
	if r.BaseRef != mergeSHA(pr) || r.Kind != domain.JobKindCommand || r.Branch != "" || r.Overlay != nil {
		t.Errorf("baseline request = %+v", r)
	}
}

// assertBaselineIssue checks the one issue the failing cell gave, and the
// report it links, kept under the PR.
func assertBaselineIssue(t *testing.T, p *testsPart, pr int) {
	t.Helper()
	issues, err := p.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	if len(issues) != 1 {
		t.Fatalf("baseline issues = %+v", issues)
	}
	for _, want := range []string{"`pkg::TestParse`", "forecrew-baseline unit/linux-go", mergeSHA(pr), "Baseline report"} {
		if !strings.Contains(issues[0].Body, want) {
			t.Errorf("issue body lacks %q:\n%s", want, issues[0].Body)
		}
	}
	if issues[0].Title != "Baseline: unit fails in linux-go" {
		t.Errorf("issue title = %q", issues[0].Title)
	}
	c, err := p.st.Changes().GetByPR(t.Context(), p.account, "acme/app", pr)
	must(t, err)
	rep, err := p.st.VerificationReports().GetBySHA(t.Context(), p.account, c.ID, mergeSHA(pr))
	must(t, err)
	if rep.ProjectID != p.project.ID || rep.Status != domain.VerificationFailed {
		t.Errorf("report = %+v", rep)
	}
	if _, ok := p.tracker.CheckRun("acme/app", mergeSHA(pr), "forecrew/verification"); ok {
		t.Error("the baseline set a check run")
	}
}

// A test that fails and then passes when its cell runs again is flaky; all
// flaky tests go in one issue and nothing else is filed.
func TestOnboardingTestsFlakyTestsGoInOneIssue(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	_, p := onboardedUpToTests(t, v)
	p.reach(t, 1)
	p.decide(t, domain.GateTestPlanApproval, mcp.ToolApprove, "")
	p.cellRun(t, "cells/0/cell_job", "pkg::TestA", "pkg::TestB")
	p.cellRun(t, "cells/1/cell_job")
	p.cellRun(t, "cells/2/cell_job")
	p.cellResults(t, "reruns/0/rerun_job", nil, "pkg::TestA", "pkg::TestB") // passes
	p.waitRun(t, domain.RunSucceeded)
	issues, err := p.tracker.ListOpenIssues(t.Context(), "acme/app", "forecrew:baseline")
	must(t, err)
	if len(issues) != 1 || issues[0].Title != "Baseline: flaky tests" || !strings.Contains(issues[0].Body, "`pkg::TestA`") || !strings.Contains(issues[0].Body, "`pkg::TestB`") {
		t.Fatalf("issues = %+v", issues)
	}
}

// A baseline with every test passing files nothing.
func TestOnboardingTestsPassingBaselineFilesNothing(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	o, p := onboardedUpToTests(t, v)
	p.reach(t, 1)
	p.decide(t, domain.GateTestPlanApproval, mcp.ToolApprove, "")
	for i := range 3 {
		p.cellRun(t, fmt.Sprintf("cells/%d/cell_job", i))
	}
	p.waitRun(t, domain.RunSucceeded)
	if titles := p.openBaselineIssues(t); len(titles) != 0 {
		t.Errorf("issues = %v", titles)
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
}

// Changes requested start another round with the feedback, on the same PR.
func TestOnboardingTestsChangesRequested(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	_, p := onboardedUpToTests(t, v)
	first := p.reach(t, 1)
	p.decide(t, domain.GateTestPlanApproval, mcp.ToolRequestChanges, "Also test on macOS.")
	architect, engineer := p.plan(t, 2, 1)
	for _, req := range []workflow.JobRequest{architect, engineer} {
		if !strings.Contains(req.Feedback, "A reviewer asked for changes to the test plan:\nAlso test on macOS.") {
			t.Errorf("round 2 feedback = %q", req.Feedback)
		}
	}
	p.smoke(t, 2, 2)
	p.checks(t, 2, domain.ChecksSuccess)
	var second domain.Gate
	eventually(t, "a second gate", func() bool {
		gs, err := p.gates.ListPending(t.Context(), p.account, p.project.ID)
		must(t, err)
		for _, g := range gs {
			if g.Kind == domain.GateTestPlanApproval && g.ID != first.ID {
				second = g
			}
		}
		return second.ID != uuid.Nil()
	})
	if !strings.Contains(second.Title, "round 2") || second.SubjectNumber != first.SubjectNumber {
		t.Errorf("second gate = %+v", second)
	}
}

// A matrix that does not load, or an environment file that is missing, goes
// back to the two roles once with the errors.
func TestOnboardingTestsInvalidMatrixLoopsBackOnce(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	_, p := onboardedUpToTests(t, v)
	broken := map[string]string{".forecrew/matrix.yaml": matrixYAML, ".forecrew/environments/linux-go.yaml": linuxGoYAML} // no win
	p.job(t, "plan/1/draft/1/architect", architectOutput(), broken)
	p.job(t, "plan/1/draft/1/engineer", engineerOutput(), broken)
	architect := p.job(t, "plan/1/draft/2/architect", architectOutput(), testPlanFiles)
	engineer := p.job(t, "plan/1/draft/2/engineer", engineerOutput(), testPlanFiles)
	for _, req := range []workflow.JobRequest{architect, engineer} {
		if !strings.Contains(req.Feedback, "environments/win.yaml does not exist") || !strings.Contains(req.Feedback, "What you left does not work") {
			t.Errorf("second try feedback = %q", req.Feedback)
		}
	}
	p.smoke(t, 1, 2)
	p.checks(t, 1, domain.ChecksSuccess)
	p.pendingGate(t, domain.GateTestPlanApproval)
}

// When the tries run out a needs_you gate opens, and so it does when a role
// stopped for a reason of its own.
func TestOnboardingTestsEscalatesWhenTheTriesRunOut(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	_, p := onboardedUpToTests(t, v)
	stopped := architectOutput()
	stopped["escalation"] = "The profile needs a macOS runner that no GitHub-hosted label provides."
	for try := 1; try <= 2; try++ {
		p.job(t, fmt.Sprintf("plan/1/draft/%d/architect", try), stopped, testPlanFiles)
		req := p.job(t, fmt.Sprintf("plan/1/draft/%d/engineer", try), engineerOutput(), testPlanFiles)
		if try == 2 && !strings.Contains(req.Feedback, "The test architect stopped: The profile needs a macOS runner") {
			t.Errorf("feedback = %q", req.Feedback)
		}
	}
	g := p.pendingGate(t, domain.GateNeedsYou)
	if g.Status != domain.GatePending {
		t.Errorf("gate = %+v", g)
	}
}

// An environment that runs on GitHub Actions gets the workflow on the same
// branch, before the smoke jobs dispatch it.
func TestOnboardingTestsInstallsTheWorkflow(t *testing.T) {
	v := newEnv(t)
	fake := v.withTests(t, true)
	_, p := onboardedUpToTests(t, v)
	files := map[string]string{}
	maps.Copy(files, testPlanFiles)
	files[".forecrew/environments/win.yaml"] = "version: 1\nname: win\nprovider: actions\nruns_on: windows-2022\n"
	p.job(t, "plan/1/draft/1/architect", architectOutput(), files)
	p.job(t, "plan/1/draft/1/engineer", engineerOutput(), files)
	p.waitStep(t, "plan/1/deliver/0/install/0/workflow", domain.StepSucceeded)
	if got, ok := fake.File("acme/app", ".github/workflows/forecrew-matrix.yml"); !ok || len(got) == 0 {
		t.Error("the workflow was not installed")
	}
	reqs := p.smoke(t, 1, 2)
	for _, r := range reqs {
		if r.Environment.Name == "win" && (r.Needs[0] != "windows-2022" || r.Branch != testsBranch) {
			t.Errorf("smoke request of the actions environment = %+v", r)
		}
	}
}

// A repo that has its test plan already leaves the jobs nothing to commit:
// there is no PR, and the baseline runs on the default branch. The refresh
// of an onboarded project runs only this part.
func TestOnboardingTestsRefresh(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarded)
	v.withTests(t, false)
	must(t, v.tracker.SetHeadOfBranch("acme/app", "main", "abc1234"))
	v.matrixFiles(t, "abc1234")
	// The merged pull request the baseline report is kept under.
	must(t, v.tracker.AddBranch("acme/app", "agent/onboarding"))
	pr, err := v.tracker.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "Profile", Head: "agent/onboarding", Base: "main"})
	must(t, err)
	ch := domain.Change{AccountID: v.account, ProjectID: v.project.ID, Repo: "acme/app", Number: pr.Number, Branch: "agent/onboarding", BaseRef: "main", HeadSHA: "p1", State: domain.ChangeMerged, URL: pr.URL}
	must(t, v.st.Changes().Upsert(t.Context(), &ch))

	p := startTestsRun(t, v)
	p.job(t, "plan/1/draft/1/architect", architectOutput(), nil)
	p.job(t, "plan/1/draft/1/engineer", engineerOutput(), nil)
	for i := range 3 {
		req := p.cellRun(t, fmt.Sprintf("cells/%d/cell_job", i))
		if req.BaseRef != "abc1234" {
			t.Errorf("baseline request = %+v", req)
		}
	}
	p.waitRun(t, domain.RunSucceeded)
	if _, err := v.tracker.FindPR(t.Context(), "acme/app", testsBranch); err == nil {
		t.Error("a PR was opened with nothing to commit")
	}
	if got := p.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
	rep, err := p.st.VerificationReports().GetBySHA(t.Context(), p.account, ch.ID, "abc1234")
	must(t, err)
	if rep.Status != domain.VerificationPassed {
		t.Errorf("report = %+v", rep)
	}
}

// A failure in the test part after the profile was stored leaves the project
// onboarded, without a baseline; rejecting the needs_you gate that explains
// it ends the run.
func TestOnboardingTestsFailureKeepsTheProfile(t *testing.T) {
	v := newEnv(t)
	v.withTests(t, false)
	o, p := onboardedUpToTests(t, v)
	s := p.waitStep(t, "plan/1/draft/1/architect", domain.StepWaiting)
	j, err := p.st.Jobs().Get(t.Context(), p.account, s.JobID)
	must(t, err)
	_, err = p.st.Jobs().Cancel(t.Context(), p.account, j.ID)
	must(t, err)
	p.publish(t, &domain.JobCompleted{JobID: j.ID, Status: domain.JobCanceled})
	g := p.pendingGate(t, domain.GateNeedsYou)
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarding {
		t.Errorf("status while the gate is open = %s", got)
	}
	_, err = p.gates.Decide(t.Context(), p.human, mcp.ToolReject, mcp.GateDecisionInput{Gate: g.Ref()})
	must(t, err)
	p.waitRun(t, domain.RunCanceled)
	p.waitStep(t, "finally/settle", domain.StepSucceeded)
	got := o.projectNow(t)
	if got.Status != domain.ProjectOnboarded || len(got.Profile) == 0 {
		t.Errorf("project = %s %s", got.Status, got.Profile)
	}
	if titles := p.openBaselineIssues(t); len(titles) != 0 {
		t.Errorf("issues = %v", titles)
	}
}

// Without the test roles in forecrew.yaml there is no test part: the project
// is onboarded when the profile is stored.
func TestOnboardingWithoutTestRolesEndsWithTheProfile(t *testing.T) {
	o := startOnboarding(t, newEnv(t))
	o.reach(t, 1, fullProfile)
	o.decide(t, domain.GateProfileApproval, mcp.ToolApprove, "")
	o.waitRun(t, domain.RunSucceeded)
	s := o.waitStep(t, "tests", domain.StepSucceeded)
	if !strings.Contains(string(s.Output), `"started":false`) {
		t.Errorf("tests output = %s", s.Output)
	}
	if got := o.projectNow(t).Status; got != domain.ProjectOnboarded {
		t.Errorf("status = %s", got)
	}
}

func (p *testsPart) projectNow(t *testing.T) domain.Project {
	t.Helper()
	pr, err := p.st.Projects().Get(t.Context(), p.account, p.project.ID)
	must(t, err)
	return pr
}

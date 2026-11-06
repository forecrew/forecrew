package playbooks_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// v2Run is a run of the goal-v2 playbook, with the matrix at every head the
// developer will push and the workers that run it. The test plays the
// workers that answer jobs, GitHub and the human, as for the goal playbook.
type v2Run struct {
	*goalRun
}

// startGoalV2 starts goal-v2 for a goal with no platforms; the matrix of
// matrixYAML is at the heads of the first rounds, and workers for it are
// online, except those of the windows cells when linuxOnly.
func startGoalV2(t *testing.T, linuxOnly ...bool) *v2Run {
	t.Helper()
	v := newEnv(t, domain.ProjectOnboarded)
	for round := 1; round <= 4; round++ {
		v.matrixFiles(t, v2sha(round))
	}
	g := v.newGoal(t)
	r := startRun(t, v, "goal-v2", workflow.StartRequest{
		AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
		Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	run := &v2Run{&goalRun{runner: r, goal: g, heads: map[string]int{}}}
	run.workerOnline(t, "linux-box", "docker", "linux")
	if len(linuxOnly) == 0 || !linuxOnly[0] {
		run.workerOnline(t, "windows-box", "windows")
	}
	return run
}

// v2sha is the commit the developer pushes in a round: the verification takes
// commit IDs only.
func v2sha(round int) string { return fmt.Sprintf("%040x", 0xc0ffee00+round) }

// push answers the developer job of a round of the first delivery of the
// task, with the commit it pushed. A rework round moves the PR's head first,
// as the push does.
func (r *v2Run) push(t *testing.T, round int) workflow.JobRequest {
	t.Helper()
	path := build(0, 1, round, "dev/1/develop")
	r.waitStep(t, path, domain.StepWaiting)
	if pr, err := r.tracker.FindPR(t.Context(), "acme/app", "agent/TASK-1"); err == nil {
		must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, v2sha(round)))
	}
	return r.answer(t, path, developerOutput(fmt.Sprintf("Round %d.", round)), 1, v2sha(round))
}

// prAt waits for the PR of the round. A PR the fake opens has a made-up head;
// this makes it the commit the developer pushed, in GitHub and in the change.
func (r *v2Run) prAt(t *testing.T, round int) github.PullRequest {
	t.Helper()
	pr := r.pr(t, 0, 1, round)
	must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, v2sha(round)))
	c := mustChange(t, r, pr.Number)
	c.HeadSHA = v2sha(round)
	must(t, r.st.Changes().Upsert(t.Context(), &c))
	pr.HeadSHA = v2sha(round)
	return pr
}

// workerOnline adds a worker of pool build that has just polled.
func (r *v2Run) workerOnline(t *testing.T, name string, labels ...string) {
	t.Helper()
	w := domain.Worker{AccountID: r.account, Name: name, TokenHash: []byte(name)}
	must(t, r.st.Workers().Create(t.Context(), &w))
	must(t, r.st.Workers().Seen(t.Context(), r.account, w.ID, labels, "1"))
}

// testMap is the output of a test engineer: AC-1 is tested by a unit test,
// AC-2 by an end-to-end one.
func testMap(escalation any) map[string]any {
	return map[string]any{
		"summary": "Tests for both criteria.",
		"tests": []any{
			map[string]any{"criterion": "AC-1", "suite": "unit", "id": "pkg::TestAC1Charge", "file": "pkg/a_test.go"},
			map[string]any{"criterion": "AC-2", "suite": "e2e", "id": "e2e::TestAC2Decline", "file": "e2e/flow_test.go"},
		},
		"files":      []any{"pkg/a_test.go", "e2e/flow_test.go", "pkg/testdata/in.json"},
		"untestable": []any{}, "assumptions": []any{}, "escalation": escalation,
	}
}

var lockedFiles = []string{"e2e/flow_test.go", "pkg/a_test.go", "pkg/testdata/in.json"}

const testsStep = "tasks/0/tests/"

// writeTests answers the test engineer of the round of the test design.
func (r *v2Run) writeTests(t *testing.T, round int, output map[string]any) workflow.JobRequest {
	t.Helper()
	return r.answer(t, fmt.Sprintf("%s%d/write_tests", testsStep, round), output, 0.5, fmt.Sprintf("tests-%d", round))
}

// designed runs the spec, the plan and a test design that is ready at once.
func (r *v2Run) designed(t *testing.T) {
	t.Helper()
	r.oneTask(t)
	r.writeTests(t, 1, testMap(nil))
	r.waitStep(t, testsStep+"1/tmap", domain.StepSucceeded)
}

// outcome says how a command job ended: its test run's status and results.
type outcome func(j domain.Job) (domain.TestRunStatus, []domain.TestResult)

// results of a suite: its tests, all in one status.
func suiteResults(suite string, st domain.TestStatus) []domain.TestResult {
	if suite == "unit" {
		return []domain.TestResult{{Classname: "pkg", Name: "TestAC1Charge", Status: st}}
	}
	return []domain.TestResult{{Classname: "e2e", Name: "TestAC2Decline", Status: st}}
}

func runOf(st domain.TestStatus) domain.TestRunStatus {
	if st == domain.TestPassed {
		return domain.TestRunPassed
	}
	return domain.TestRunFailed
}

// passedRun passes every test of a job; failedRun fails every test of a job.
func passedRun(j domain.Job) (domain.TestRunStatus, []domain.TestResult) {
	return domain.TestRunPassed, suiteResults(j.Spec.Suite, domain.TestPassed)
}

func failedRun(j domain.Job) (domain.TestRunStatus, []domain.TestResult) {
	return domain.TestRunFailed, suiteResults(j.Spec.Suite, domain.TestFailed)
}

// windowsFails passes every cell but the windows ones.
func windowsFails(j domain.Job) (domain.TestRunStatus, []domain.TestResult) {
	if j.Spec.Cell["os"] == "windows" {
		return failedRun(j)
	}
	return passedRun(j)
}

// commands waits for n queued command jobs and completes them as workers
// would, each with the test run outcome says, stored for the commit the job
// tested. It returns the jobs, oldest first.
func (r *v2Run) commands(t *testing.T, n int, how outcome) []domain.Job {
	t.Helper()
	ctx := t.Context()
	queued := func() []domain.Job {
		js, err := r.st.Jobs().List(ctx, r.account, store.JobFilter{Status: domain.JobQueued})
		must(t, err)
		return slices.DeleteFunc(js, func(j domain.Job) bool { return j.Kind != domain.JobKindCommand })
	}
	eventually(t, fmt.Sprintf("%d queued command jobs", n), func() bool { return len(queued()) >= n })
	if got := len(queued()); got != n {
		t.Fatalf("%d command jobs are queued, want %d", got, n)
	}
	r.clock.Advance(time.Second) // later runs are later attempts
	var done []domain.Job
	for range n {
		j, err := r.st.Jobs().LeaseNext(ctx, r.account, r.worker.ID, []string{"docker", "linux", "windows"}, time.Hour)
		must(t, err)
		status, results := how(j)
		run := domain.TestRun{
			AccountID: r.account, ProjectID: r.project.ID, JobID: j.ID, SHA: j.Spec.BaseRef, Suite: j.Spec.Suite, Cell: j.Spec.Cell,
			Status: status, Attempt: 1,
		}
		if j.Spec.Environment != nil {
			run.Environment = j.Spec.Environment.Name
		}
		if status != domain.TestRunPassed {
			run.ExitCode = 1
		}
		for _, res := range results {
			if res.Status == domain.TestPassed {
				run.Counts.Passed++
			} else {
				run.Counts.Failed++
			}
		}
		must(t, r.st.TestRuns().Create(ctx, &run, results))
		_, err = r.st.Jobs().Complete(ctx, r.account, j.ID, r.worker.ID, store.JobResult{Status: domain.JobSucceeded, HeadSHA: j.Spec.BaseRef})
		must(t, err)
		r.publish(t, &domain.JobCompleted{JobID: j.ID, Status: domain.JobSucceeded})
		done = append(done, j)
		r.clock.Advance(time.Second)
	}
	return done
}

// verify plays the verification of a head: the cells of the matrix, the base
// check, which fails every new test as it should, and the reruns.
func (r *v2Run) verify(t *testing.T, cells outcome, reruns int, rerun outcome) {
	t.Helper()
	jobs := r.commands(t, 3, cells)
	if got := commandKeys(jobs); !slices.Equal(got, []string{"unit@linux-go", "e2e@linux-go", "unit@win"}) {
		t.Errorf("matrix jobs = %v", got)
	}
	base := r.commands(t, 3, failedRun)
	for _, j := range base {
		if j.Spec.Overlay == nil || j.Spec.BaseRef != "main" || !slices.Equal(j.Spec.Overlay.Paths, lockedFiles) {
			t.Errorf("base check job = %+v overlay %+v", j.Spec, j.Spec.Overlay)
		}
	}
	if reruns > 0 {
		r.commands(t, reruns, rerun)
	}
}

func commandKeys(jobs []domain.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Spec.Suite+"@"+j.Spec.Environment.Name)
	}
	return out
}

func (r *v2Run) verifyStep(round int, step string) string {
	return build(0, 1, round, "gated/0/verify/1/"+step)
}

// Tests written from the spec, a developer round that has them locked, a
// matrix with a failing cell and its rerun, rework, a green report, the merge
// gate that shows it, and the merge.
func TestGoalV2Delivery(t *testing.T) {
	r := startGoalV2(t)
	r.oneTask(t)

	eng := r.writeTests(t, 1, testMap(nil))
	tasks := r.tasks(t)
	if eng.Role != "test-engineer" || eng.Branch != "agent/TASK-1" || eng.TaskID != tasks[0].ID || !slices.Equal(eng.Needs, []string{"go"}) ||
		eng.SpecID == uuid.Nil() || eng.Feedback != "" || len(eng.LockedPaths) != 0 {
		t.Errorf("test engineer request = %+v", eng)
	}
	if r.startedStep(t, "tasks/0/delivery/") {
		t.Error("the developer started before the tests were ready")
	}

	// Round 1: the developer gets the tests locked.
	dev := r.push(t, 1)
	if dev.Role != "developer" || !slices.Equal(dev.LockedPaths, lockedFiles) || dev.Branch != "agent/TASK-1" {
		t.Errorf("developer request = %+v", dev)
	}
	r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.verify(t, windowsFails, 1, failedRun)
	rev := r.review(t, 0, 1, 1, approveReview())
	for _, want := range []string{"Verification report of acme/app at " + v2sha(1) + ": Failed: ", "<untrusted-data", "unit in win (os=windows) failed"} {
		if !strings.Contains(rev.Feedback, want) {
			t.Errorf("the reviewer's feedback lacks %q:\n%s", want, rev.Feedback)
		}
	}

	// The report fails, so no merge gate: the developer reworks, with the
	// tests still locked.
	dev = r.push(t, 2)
	if !strings.Contains(dev.Feedback, "The verification of ") || !strings.Contains(dev.Feedback, "failed: Failed: ") || !strings.Contains(dev.Feedback, "pkg::TestAC1Charge") ||
		!slices.Equal(dev.LockedPaths, lockedFiles) {
		t.Errorf("rework request = %+v", dev)
	}
	if r.startedStep(t, "tasks/0/delivery/1/approval/0/") {
		t.Error("a merge gate opened for a failed report")
	}
	pr := r.prAt(t, 2)
	r.checks(t, 0, 1, 2, domain.ChecksSuccess)
	r.verify(t, passedRun, 0, nil)
	r.review(t, 0, 1, 2, approveReview())

	g := r.pendingGate(t, domain.GateMergeApproval)
	for _, want := range []string{"Verification: Passed: ", "Report: " + pr.URL, "forecrew/verification", "Checks: success."} {
		if !strings.Contains(g.Summary, want) {
			t.Errorf("the merge gate lacks %q:\n%s", want, g.Summary)
		}
	}
	rep, err := r.st.VerificationReports().Latest(t.Context(), r.account, mustChange(t, r, pr.Number).ID)
	must(t, err)
	if rep.Status != domain.VerificationPassed || rep.SHA != v2sha(2) {
		t.Errorf("report = %s at %s", rep.Status, rep.SHA)
	}
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	r.waitRun(t, domain.RunSucceeded)
	if !r.prNow(t, pr.Number).Merged || r.goalNow(t).Status != domain.GoalDone {
		t.Errorf("merged = %v, goal %s", r.prNow(t, pr.Number).Merged, r.goalNow(t).Status)
	}
}

func mustChange(t *testing.T, r *v2Run, number int) domain.Change {
	t.Helper()
	c, err := r.st.Changes().GetByPR(t.Context(), r.account, "acme/app", number)
	must(t, err)
	return c
}

// failJob finishes the job of the waiting step as failed.
func (r *v2Run) failJob(t *testing.T, path, reason string) {
	t.Helper()
	s := r.waitStep(t, path, domain.StepWaiting)
	j, err := r.st.Jobs().Get(t.Context(), r.account, s.JobID)
	must(t, err)
	leased, err := r.st.Jobs().LeaseNext(t.Context(), r.account, r.worker.ID, j.RequiredLabels(), time.Hour)
	must(t, err)
	_, err = r.st.Jobs().Complete(t.Context(), r.account, leased.ID, r.worker.ID, store.JobResult{Status: domain.JobFailed, Error: reason})
	must(t, err)
	r.publish(t, &domain.JobCompleted{JobID: leased.ID, Status: domain.JobFailed})
}

// waitAttempt waits for the step run (path, attempt) to wait for its job.
func (r *v2Run) waitAttempt(t *testing.T, path string, attempt int) domain.StepRun {
	t.Helper()
	var found domain.StepRun
	eventually(t, fmt.Sprintf("step %s attempt %d", path, attempt), func() bool {
		steps, err := r.st.StepRuns().ListByRun(t.Context(), r.account, r.run.ID)
		must(t, err)
		for _, s := range steps {
			if s.StepID == path && s.Attempt == attempt && s.Status == domain.StepWaiting {
				found = s
				return true
			}
		}
		return false
	})
	return found
}

// A developer that changes a locked test fails its job, naming the files; a
// needs_you gate asks, and approving it reruns the round with the lock.
func TestGoalV2DeveloperTouchesALockedTest(t *testing.T) {
	r := startGoalV2(t)
	r.designed(t)
	path := build(0, 1, 1, "dev/1/develop")
	first := r.request(r.waitStep(t, path, domain.StepWaiting).JobID)
	if !slices.Equal(first.LockedPaths, lockedFiles) {
		t.Fatalf("first request = %+v", first)
	}
	r.failJob(t, path, "changed paths outside the write paths or in the locked ones: pkg/a_test.go")

	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "pkg/a_test.go") || !strings.Contains(g.Summary, "dev/1/develop") {
		t.Errorf("gate summary = %q", g.Summary)
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "Leave the tests alone.")
	retry := r.request(r.waitAttempt(t, path, 2).JobID)
	if !slices.Equal(retry.LockedPaths, lockedFiles) || !strings.Contains(retry.Feedback, "Leave the tests alone.") || retry.ID == first.ID {
		t.Errorf("rerun request = %+v", retry)
	}
}

// A developer that believes a locked test is wrong escalates: a needs_you
// gate asks, and approving it reruns the developer without the lock for that
// round only.
func TestGoalV2ApprovedEscalationLiftsTheLockForOneRound(t *testing.T) {
	r := startGoalV2(t)
	r.designed(t)
	r.waitStep(t, build(0, 1, 1, "dev/1/develop"), domain.StepWaiting)
	stuck := developerOutput("Stopped.")
	stuck["escalation"] = "pkg::TestAC1Charge expects cents, the spec says dollars."
	first := r.answer(t, build(0, 1, 1, "dev/1/develop"), stuck, 1)
	if !slices.Equal(first.LockedPaths, lockedFiles) {
		t.Errorf("first request = %+v", first)
	}
	r.pendingGate(t, domain.GateNeedsYou)
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	rerun := r.answer(t, build(0, 1, 1, "dev/2/develop"), developerOutput("Fixed the test."), 1, v2sha(1))
	if len(rerun.LockedPaths) != 0 {
		t.Errorf("the approved rerun still has the lock: %+v", rerun)
	}
	r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.verify(t, windowsFails, 1, failedRun)
	r.review(t, 0, 1, 1, approveReview())

	// The next round has the lock back.
	dev := r.push(t, 2)
	if !slices.Equal(dev.LockedPaths, lockedFiles) {
		t.Errorf("round 2 request = %+v", dev)
	}
}

// A test engineer that needs the matrix changed first is answered by the test
// architect on the same branch; the engineer then runs again with the reason.
func TestGoalV2MatrixWork(t *testing.T) {
	r := startGoalV2(t)
	r.oneTask(t)
	r.writeTests(t, 1, map[string]any{
		"summary": "Stopped.", "tests": []any{}, "files": []any{}, "untestable": []any{}, "assumptions": []any{},
		"escalation": "AC-2 needs an e2e suite the matrix lacks.",
	})
	arch := r.answer(t, testsStep+"1/matrix_work/0/test_plan", map[string]any{"summary": "Added e2e."}, 0.75, "matrix-1")
	tasks := r.tasks(t)
	if arch.Role != "test-architect" || arch.Branch != "agent/TASK-1" || arch.TaskID != tasks[0].ID || !slices.Equal(arch.Needs, []string{"go"}) ||
		!strings.Contains(arch.Feedback, "AC-2 needs an e2e suite the matrix lacks.") || !strings.Contains(arch.Feedback, "<untrusted-data") {
		t.Errorf("test architect request = %+v", arch)
	}
	if r.startedStep(t, "tasks/0/delivery/") {
		t.Error("the developer started before the tests were ready")
	}
	eng := r.writeTests(t, 2, testMap(nil))
	if !strings.Contains(eng.Feedback, "AC-2 needs an e2e suite the matrix lacks.") || eng.Branch != "agent/TASK-1" {
		t.Errorf("second test engineer request = %+v", eng)
	}
	if r.startedStep(t, testsStep+"2/matrix_work/0/") {
		t.Error("the test architect ran again for a ready design")
	}
	dev := r.push(t, 1)
	if !slices.Equal(dev.LockedPaths, lockedFiles) {
		t.Errorf("developer request = %+v", dev)
	}
}

// Tests that are not ready twice need a human; approving the gate runs the
// engineer once more.
func TestGoalV2TestsNotReady(t *testing.T) {
	r := startGoalV2(t)
	r.oneTask(t)
	partial := testMap(nil)
	partial["tests"] = partial["tests"].([]any)[:1] // AC-2 has no test
	r.writeTests(t, 1, partial)
	again := r.writeTests(t, 2, partial)
	if !strings.Contains(again.Feedback, "Missing: AC-2.") {
		t.Errorf("second request = %+v", again)
	}
	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "not met after 2 iterations") {
		t.Errorf("gate = %+v", g)
	}
	if r.startedStep(t, "tasks/0/delivery/") {
		t.Error("the developer started with tests that are not ready")
	}
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.writeTests(t, 3, testMap(nil))
	r.waitStep(t, testsStep+"3/tmap", domain.StepSucceeded)
	r.push(t, 1)
}

// No online worker has the labels of the windows cells: those jobs and their
// base checks are not started, the report is incomplete, and a needs_you gate
// names the labels. A worker that comes online and an approval run the matrix
// again.
func TestGoalV2IncompleteWithoutAWorker(t *testing.T) {
	r := startGoalV2(t, true)
	r.designed(t)
	r.push(t, 1)
	pr := r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)

	jobs := r.commands(t, 2, passedRun)
	if got := commandKeys(jobs); !slices.Equal(got, []string{"unit@linux-go", "e2e@linux-go"}) {
		t.Errorf("matrix jobs = %v", got)
	}
	r.commands(t, 2, failedRun) // the base check of the linux environments only
	g := r.pendingGate(t, domain.GateNeedsYou)
	for _, want := range []string{
		"Verification of TASK-1 is incomplete", "Incomplete: ", "unit in win (os=windows) has no result yet",
		"no online worker in pool build has the labels windows: os=windows/unit",
		"no online worker in pool build has the labels windows: base check of unit in win", "approve to run the matrix again",
	} {
		if !strings.Contains(g.Title+"\n"+g.Summary, want) {
			t.Errorf("the gate lacks %q:\n%s\n%s", want, g.Title, g.Summary)
		}
	}
	if g.SubjectNumber != pr.Number || g.TaskID != r.tasks(t)[0].ID {
		t.Errorf("gate = %+v", g)
	}
	rep, err := r.st.VerificationReports().Latest(t.Context(), r.account, mustChange(t, r, pr.Number).ID)
	must(t, err)
	if rep.Status != domain.VerificationIncomplete {
		t.Errorf("report = %s", rep.Status)
	}
	if r.startedStep(t, build(0, 1, 1, "gated/0/review")) {
		t.Error("the reviewer started on an incomplete report")
	}

	r.workerOnline(t, "windows-box", "windows")
	r.decide(t, domain.GateNeedsYou, mcp.ToolApprove, "")
	r.verifyAgain(t)
	r.review(t, 0, 1, 1, approveReview())
	if g := r.pendingGate(t, domain.GateMergeApproval); !strings.Contains(g.Summary, "Verification: Passed: ") {
		t.Errorf("merge gate = %+v", g)
	}
}

// verifyAgain plays the second iteration of the verification, now that every
// cell can run.
func (r *v2Run) verifyAgain(t *testing.T) {
	t.Helper()
	jobs := r.commands(t, 3, passedRun)
	if got := commandKeys(jobs); !slices.Equal(got, []string{"unit@linux-go", "e2e@linux-go", "unit@win"}) {
		t.Errorf("second matrix jobs = %v", got)
	}
	r.commands(t, 3, failedRun)
}

// The merge gate cannot be passed for a head with no passed report, even when
// a human approves: github.merge_pr refuses it and a needs_you gate asks.
func TestGoalV2MergeRefusedWithoutAPassedReport(t *testing.T) {
	r := startGoalV2(t)
	r.designed(t)
	r.push(t, 1)
	pr := r.prAt(t, 1)
	r.checks(t, 0, 1, 1, domain.ChecksSuccess)
	r.verify(t, passedRun, 0, nil)
	r.review(t, 0, 1, 1, approveReview())
	r.pendingGate(t, domain.GateMergeApproval)

	// A push after the report: the head has no report any more.
	must(t, r.tracker.SetHeadSHA("acme/app", pr.Number, "pushed-by-a-human"))
	must(t, r.tracker.SetCheckStatus("acme/app", "pushed-by-a-human", github.ChecksSuccess, 1))
	r.decide(t, domain.GateMergeApproval, mcp.ToolApprove, "")
	g := r.pendingGate(t, domain.GateNeedsYou)
	if !strings.Contains(g.Summary, "pushed-by-a-human has no verification report") || r.prNow(t, pr.Number).Merged {
		t.Errorf("gate = %+v, merged = %v", g, r.prNow(t, pr.Number).Merged)
	}
}

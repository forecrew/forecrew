package workflow_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// fanOut runs one job per item, at most two at a time. Each job needs its
// item as a label, so a test can finish them in any order.
const fanOut = `
inputs: [xs]
steps:
  - id: each
    type: foreach
    parallel: 2
    items: '${inputs.xs}'
    steps:
      - {id: build, type: job, with: {role: developer, needs: ['${item}']}}
  - {id: after, type: action, action: record, with: {out: '${steps.each.output}'}}
finally:
  - {id: cleanup, type: action, action: record}
`

func items(xs ...string) map[string]any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return map[string]any{"xs": out}
}

// buildStep is the path of the build step of an item of fanOut.
func buildStep(i string) string { return "each/" + i + "/build" }

func (v *env) jobCount(t *testing.T, status domain.JobStatus) int {
	t.Helper()
	all, err := v.st.Jobs().List(t.Context(), v.account, store.JobFilter{})
	must(t, err)
	n := 0
	for _, j := range all {
		if j.Status == status {
			n++
		}
	}
	return n
}

func TestParallelKeepsItemOrder(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fanOut, items("a", "b"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, b.JobID, domain.JobSucceeded, `{"n":"b"}`, "")
	v.finishJob(t, a.JobID, domain.JobSucceeded, `{"n":"a"}`, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
	out := v.actionCalls()[0].With["out"].([]any)
	var got []string
	for _, round := range out {
		build := round.(map[string]any)["build"].(map[string]any)
		got = append(got, build["output"].(map[string]any)["n"].(string))
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("output order = %v", got)
	}
}

func TestParallelRespectsLimit(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fanOut, items("a", "b", "c", "d", "e"))
	finished, peak := 0, 0
	for finished < 5 {
		var queued []domain.Job
		eventually(t, "jobs in flight", func() bool {
			all, err := v.st.Jobs().List(t.Context(), v.account, store.JobFilter{Status: domain.JobQueued})
			must(t, err)
			queued = all
			return len(queued) == min(2, 5-finished)
		})
		peak = max(peak, len(queued))
		// The newest first: the slot of a later item frees one too.
		v.finishJob(t, queued[len(queued)-1].ID, domain.JobSucceeded, `{}`, "")
		finished++
		eventually(t, "the job to be taken in", func() bool { return v.jobCount(t, domain.JobSucceeded) == finished })
	}
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if n := len(v.jobs.Requests()); n != 5 || peak != 2 {
		t.Fatalf("job requests = %d, max in flight = %d", n, peak)
	}
}

func TestParallelOneRunsItemsInTurn(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, strings.Replace(fanOut, "parallel: 2", "parallel: 1", 1), items("a", "b"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	v.waitRun(t, r.ID, domain.RunWaiting)
	if n := len(v.jobs.Requests()); n != 1 {
		t.Fatalf("job requests = %d, want 1", n)
	}
	v.finishJob(t, a.JobID, domain.JobFailed, "", "")
	// A failed item does not stop the next one.
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, b.JobID, domain.JobSucceeded, `{}`, "")
	v.waitRun(t, r.ID, domain.RunFailed)
}

func TestParallelNoItems(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fanOut, items())
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if out := v.actionCalls()[0].With["out"].([]any); len(out) != 0 {
		t.Fatalf("output = %v", out)
	}
}

// A failing item leaves the others running, also those not started yet, and
// fails the foreach when the last has finished, with no job left behind.
func TestParallelFailureWaitsForTheOthers(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fanOut, items("a", "b", "c"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, a.JobID, domain.JobFailed, "", "")
	c, _ := v.waitJob(t, r.ID, buildStep("2"), 1)
	v.finishJob(t, c.JobID, domain.JobSucceeded, `{}`, "")
	v.waitStep(t, r.ID, buildStep("2"), 1, domain.StepSucceeded)
	v.waitRun(t, r.ID, domain.RunWaiting)
	if got := v.jobCount(t, domain.JobCanceled); got != 0 {
		t.Fatalf("canceled jobs = %d", got)
	}
	v.finishJob(t, b.JobID, domain.JobSucceeded, `{}`, "")
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step each/0/build failed") {
		t.Fatalf("error = %q", done.Error)
	}
	if calls := v.actionCalls(); len(calls) != 1 { // only the finally step
		t.Fatalf("action calls = %+v", calls)
	}
}

// With several failures the run reports the first item's, whichever finished
// first.
func TestParallelReportsTheFirstFailingItem(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fanOut, items("a", "b"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, b.JobID, domain.JobFailed, "", "")
	v.waitStep(t, r.ID, buildStep("1"), 1, domain.StepFailed)
	v.finishJob(t, a.JobID, domain.JobFailed, "", "")
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step each/0/build failed") {
		t.Fatalf("error = %q", done.Error)
	}
}

// An item that escalates waits for its gate while the others go on.
func TestParallelEscalationKeepsItsItem(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [xs]
steps:
  - id: each
    type: foreach
    parallel: 2
    items: '${inputs.xs}'
    steps:
      - {id: build, type: job, on_failure: escalate, with: {role: developer, needs: ['${item}']}}
`, items("a", "b"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, a.JobID, domain.JobFailed, "", "")
	v.waitStep(t, r.ID, buildStep("0")+":escalate", 1, domain.StepWaiting)
	v.finishJob(t, b.JobID, domain.JobSucceeded, `{}`, "")
	v.waitStep(t, r.ID, buildStep("1"), 1, domain.StepSucceeded)
	v.decide(t, r.ID, buildStep("0")+":escalate", 1, domain.GateApproved, "try again")
	retry, req := v.waitJob(t, r.ID, buildStep("0"), 2)
	if !strings.Contains(req.Feedback, "try again") {
		t.Fatalf("request = %+v", req)
	}
	v.finishJob(t, retry.JobID, domain.JobSucceeded, `{}`, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
}

func TestParallelNested(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [xs]
steps:
  - id: outer
    type: foreach
    parallel: 2
    items: '${inputs.xs}'
    steps:
      - id: inner
        type: foreach
        parallel: 1
        items: '${inputs.xs}'
        steps:
          - {id: build, type: job, with: {role: developer, needs: ['i${index}']}}
`, items("0", "1"))
	first, _ := v.waitJob(t, r.ID, "outer/0/inner/0/build", 1)
	second, _ := v.waitJob(t, r.ID, "outer/1/inner/0/build", 1)
	v.finishJob(t, first.JobID, domain.JobFailed, "", "")
	third, _ := v.waitJob(t, r.ID, "outer/0/inner/1/build", 1)
	v.finishJob(t, third.JobID, domain.JobSucceeded, `{}`, "")
	v.finishJob(t, second.JobID, domain.JobSucceeded, `{}`, "")
	fourth, _ := v.waitJob(t, r.ID, "outer/1/inner/1/build", 1)
	v.finishJob(t, fourth.JobID, domain.JobSucceeded, `{}`, "")
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step outer/0/inner/0/build failed") {
		t.Fatalf("error = %q", done.Error)
	}
}

// reviewed runs a job and then a gate per item.
const reviewed = `
inputs: [xs]
steps:
  - id: each
    type: foreach
    parallel: 2
    items: '${inputs.xs}'
    steps:
      - {id: build, type: job, with: {role: developer, needs: ['${item}']}}
      - {id: review, type: gate, with: {kind: merge_approval, title: merge, subject_repo: acme/app}}
finally:
  - {id: cleanup, type: action, action: record}
`

// A rejected gate cancels the run: the jobs of the other items are canceled
// at once and items not started never start.
func TestParallelCancelStopsEveryItem(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, reviewed, items("a", "b", "c"))
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, a.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "each/0/review", 1, domain.GateRejected, "no")
	v.waitRun(t, r.ID, domain.RunCanceled)
	j, err := v.st.Jobs().Get(t.Context(), v.account, b.JobID)
	must(t, err)
	if j.Status != domain.JobCanceled {
		t.Fatalf("job of the other item is %s", j.Status)
	}
	stopped := v.waitStep(t, r.ID, buildStep("1"), 1, domain.StepFailed)
	if !strings.Contains(stopped.Error, "the run ended") {
		t.Fatalf("step error = %q", stopped.Error)
	}
	if n := len(v.jobs.Requests()); n != 2 {
		t.Fatalf("job requests = %d, want 2", n)
	}
	if calls := v.actionCalls(); len(calls) != 1 {
		t.Fatalf("finally calls = %+v", calls)
	}
}

// A job that finished before its run was canceled keeps its real outcome: the
// rejected item ends the replay before it reaches that item's step.
func TestParallelCancelKeepsFinishedJobs(t *testing.T) {
	for _, tt := range []struct {
		job  domain.JobStatus
		step domain.StepStatus
	}{{domain.JobSucceeded, domain.StepSucceeded}, {domain.JobFailed, domain.StepFailed}} {
		t.Run(string(tt.job), func(t *testing.T) {
			v := newEnv(t)
			cfg := v.config(t, map[string]string{"pb": reviewed})
			e, stop := run(t, cfg)
			r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: items("a", "b")})
			must(t, err)
			a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
			b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
			v.finishJob(t, a.JobID, domain.JobSucceeded, `{}`, "")
			g := v.waitStep(t, r.ID, "each/0/review", 1, domain.StepWaiting)
			v.waitRun(t, r.ID, domain.RunWaiting)
			stop()
			v.finishJob(t, b.JobID, tt.job, `{"n":"b"}`, "sha")
			_, err = v.gates.Resolve(t.Context(), v.human, g.GateID, domain.GateRejected, "no")
			must(t, err)

			run(t, cfg)
			v.waitRun(t, r.ID, domain.RunCanceled)
			s := v.waitStep(t, r.ID, buildStep("1"), 1, tt.step)
			if strings.Contains(s.Error, "the run ended") {
				t.Fatalf("step = %+v", s)
			}
			if tt.job == domain.JobSucceeded && output(t, s)["head_sha"] != "sha" {
				t.Fatalf("output = %s", s.Output)
			}
			if got := v.jobCount(t, domain.JobCanceled); got != 0 {
				t.Fatalf("canceled jobs = %d", got)
			}
		})
	}
}

// An item on a wait step has no job to cancel: the run still ends, and the
// job of the third item is canceled.
func TestParallelCancelWithAnItemOnAWait(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [xs]
steps:
  - id: each
    type: foreach
    parallel: 3
    items: '${inputs.xs}'
    steps:
      - {id: build, type: job, with: {role: developer, needs: ['i${item}']}}
      - {id: merged, type: wait, with: {signal: pr.merged, repo: acme/app, number: '${item}'}}
      - {id: review, type: gate, with: {kind: merge_approval, title: merge, subject_repo: acme/app}}
`, map[string]any{"xs": []any{1, 2, 3}})
	first, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	second, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	third, _ := v.waitJob(t, r.ID, buildStep("2"), 1)
	v.finishJob(t, first.JobID, domain.JobSucceeded, `{}`, "")
	v.finishJob(t, second.JobID, domain.JobSucceeded, `{}`, "")
	v.waitStep(t, r.ID, "each/1/merged", 1, domain.StepWaiting)
	v.publish(t, &domain.PRMerged{Repo: "acme/app", Number: 1, SHA: "s"})
	v.decide(t, r.ID, "each/0/review", 1, domain.GateRejected, "no")
	v.waitRun(t, r.ID, domain.RunCanceled)
	j, err := v.st.Jobs().Get(t.Context(), v.account, third.JobID)
	must(t, err)
	if j.Status != domain.JobCanceled {
		t.Fatalf("job is %s", j.Status)
	}
	v.waitStep(t, r.ID, "each/1/merged", 1, domain.StepWaiting)
}

// The engine dies with one item waiting and another between creating its job
// and recording it. After the restart each item continues where it stopped.
func TestParallelCrashResumesWithoutDuplicates(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	cfg := v.config(t, map[string]string{"pb": fanOut})
	e, err := workflow.New(cfg)
	must(t, err)
	r, err := e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: items("a", "b", "c")})
	must(t, err)
	v.crashAt(t, cfg, buildStep("1"))
	if n := len(v.jobs.Requests()); n != 2 {
		t.Fatalf("job requests before restart = %d", n)
	}

	// One job finishes while the engine is down.
	steps, err := v.st.StepRuns().ListByRun(ctx, v.account, r.ID)
	must(t, err)
	for _, s := range steps {
		if s.StepID == buildStep("1") {
			if s.Status != domain.StepRunning {
				t.Fatalf("step after crash = %+v", s)
			}
			v.finishJob(t, s.ID, domain.JobSucceeded, `{}`, "")
		}
	}

	run(t, cfg)
	c, _ := v.waitJob(t, r.ID, buildStep("2"), 1)
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	v.finishJob(t, c.JobID, domain.JobSucceeded, `{}`, "")
	v.finishJob(t, a.JobID, domain.JobSucceeded, `{}`, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
	seen := map[uuid.UUID]bool{}
	for _, req := range v.jobs.Requests() {
		seen[req.ID] = true
	}
	if n := len(v.jobs.Requests()); n != 3 || len(seen) != 3 {
		t.Fatalf("job requests = %d for %d jobs, want 3", n, len(seen))
	}
	if all, _ := v.st.Jobs().List(ctx, v.account, store.JobFilter{}); len(all) != 3 {
		t.Fatalf("jobs = %d, want 3", len(all))
	}
}

// The limit holds after a restart: what is in flight comes from the records.
func TestParallelCrashKeepsLimit(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": fanOut})
	e, err := workflow.New(cfg)
	must(t, err)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: items("a", "b", "c")})
	must(t, err)
	v.crashAt(t, cfg, buildStep("1"))
	run(t, cfg)
	v.waitStep(t, r.ID, buildStep("1"), 1, domain.StepWaiting)
	v.waitRun(t, r.ID, domain.RunWaiting)
	if n := len(v.jobs.Requests()); n != 2 {
		t.Fatalf("job requests after restart = %d, want 2", n)
	}
}

// The engine dies while it cancels the items of a canceled run; the restart
// finishes the job.
func TestParallelCrashWhileCanceling(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": reviewed})
	e, stop := run(t, cfg)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: items("a", "b")})
	must(t, err)
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	b, _ := v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, a.JobID, domain.JobSucceeded, `{}`, "")
	g := v.waitStep(t, r.ID, "each/0/review", 1, domain.StepWaiting)
	v.waitRun(t, r.ID, domain.RunWaiting)
	stop()
	_, err = v.gates.Resolve(t.Context(), v.human, g.GateID, domain.GateRejected, "no")
	must(t, err)

	v.crashAt(t, cfg, buildStep("1")) // after the job was canceled
	run(t, cfg)
	v.waitRun(t, r.ID, domain.RunCanceled)
	j, err := v.st.Jobs().Get(t.Context(), v.account, b.JobID)
	must(t, err)
	if j.Status != domain.JobCanceled {
		t.Fatalf("job is %s", j.Status)
	}
	if calls := v.actionCalls(); len(calls) != 1 {
		t.Fatalf("finally calls = %+v", calls)
	}
}

// failingCancel is a job creator whose Cancel fails until it is turned off.
type failingCancel struct {
	workflow.JobCreator
	err *faultyStore
}

func (c failingCancel) Cancel(ctx context.Context, accountID, jobID uuid.UUID) error {
	if err := c.err.failing("cancel"); err != nil {
		return err
	}
	return c.JobCreator.Cancel(ctx, accountID, jobID)
}

// A job that cannot be canceled yet keeps the run open: it is retried.
func TestParallelCancelRetries(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": reviewed})
	faults := &faultyStore{Store: v.st, fail: map[string]bool{"cancel": true}}
	cfg.Jobs = failingCancel{v.jobs, faults}
	e, _ := run(t, cfg)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: items("a", "b")})
	must(t, err)
	a, _ := v.waitJob(t, r.ID, buildStep("0"), 1)
	v.waitJob(t, r.ID, buildStep("1"), 1)
	v.finishJob(t, a.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "each/0/review", 1, domain.GateRejected, "no")
	time.Sleep(20 * time.Millisecond)
	if got, _ := v.st.WorkflowRuns().Get(t.Context(), v.account, r.ID); got.Status == domain.RunCanceled {
		t.Fatalf("run is canceled while its job is still running")
	}
	faults.set("cancel", false)
	v.waitRun(t, r.ID, domain.RunCanceled)
	if got := v.jobCount(t, domain.JobCanceled); got != 1 {
		t.Fatalf("canceled jobs = %d", got)
	}
}

const fromItem = `
inputs: [xs]
steps:
  - id: each
    type: foreach
    parallel: 2
    items: '${inputs.xs}'
    steps:
      - id: run
        type: job
        with: {kind: command, repo: acme/app, base_ref: main, timeout: 5m, from: '${item}', needs: [lab]}
`

// A command job step takes its cell, suite, environment and command from the
// item; its own keys win and the rest of the item is ignored.
func TestCommandJobFromItem(t *testing.T) {
	v := newEnv(t)
	item := map[string]any{
		"key": "os=linux/unit", "cell": map[string]any{"os": "linux"}, "suite": "unit", "needs": []any{"linux"}, "role": "developer",
		"environment": map[string]any{"name": "host", "provider": "machine"},
		"command":     map[string]any{"argv": []any{"make", "test"}, "reports": []any{"r.xml"}},
	}
	_, r := v.engine(t, fromItem, map[string]any{"xs": []any{item}})
	s, req := v.waitJob(t, r.ID, "each/0/run", 1)
	if req.Kind != domain.JobKindCommand || req.Suite != "unit" || req.Cell["os"] != "linux" || req.Environment.Name != "host" ||
		!slices.Equal(req.Command.Argv, []string{"make", "test"}) || !slices.Equal(req.Command.Reports, []string{"r.xml"}) ||
		!slices.Equal(req.Needs, []string{"lab"}) || req.Role != "" || req.BaseRef != "main" || time.Duration(req.Timeout) != 5*time.Minute {
		t.Fatalf("request = %+v", req)
	}
	v.finishJob(t, s.JobID, domain.JobSucceeded, `{}`, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
}

func TestCommandJobFromNotAnObject(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fromItem, items("text"))
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "from is not an object") {
		t.Fatalf("error = %q", done.Error)
	}
}

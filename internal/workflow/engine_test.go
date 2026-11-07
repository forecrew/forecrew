package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

const linear = `
inputs: [repo]
steps:
  - id: spec
    type: job
    with: {role: product-owner, repo: '${inputs.repo}', needs: [go]}
  - id: note
    type: action
    action: record
    with:
      criteria: ${steps.spec.output.acceptance_criteria}
      first: ${steps.spec.output.acceptance_criteria.0}
      text: '${inputs.repo} has ${steps.spec.output.count} criteria at ${steps.spec.head_sha}'
  - id: checks
    type: wait
    with: {signal: checks.completed, repo: '${inputs.repo}', sha: '${steps.spec.head_sha}'}
`

func TestLinearRun(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, linear, map[string]any{"repo": "Acme/App"})
	if r.Status != domain.RunRunning || r.Playbook != "pb" {
		t.Fatalf("Start = %+v", r)
	}
	spec, req := v.waitJob(t, r.ID, "spec", 1)
	if req.Role != "product-owner" || req.Repo != "Acme/App" || req.AccountID != v.account || req.ProjectID != v.project.ID ||
		!reflect.DeepEqual(req.Needs, []string{"go"}) || req.ID != spec.ID || spec.WaitKey != domain.JobKey(spec.ID) {
		t.Fatalf("job request = %+v, step %+v", req, spec)
	}
	v.waitRun(t, r.ID, domain.RunWaiting)
	v.finishJob(t, spec.JobID, domain.JobSucceeded, `{"acceptance_criteria":["fast","safe"],"count":2}`, "ABC123")

	checks := v.waitStep(t, r.ID, "checks", 1, domain.StepWaiting)
	if checks.WaitKey != "checks:acme/app@abc123" {
		t.Fatalf("wait key = %q", checks.WaitKey)
	}
	calls := v.actionCalls()
	if len(calls) != 1 {
		t.Fatalf("action calls = %v", calls)
	}
	want := map[string]any{"criteria": []any{"fast", "safe"}, "first": "fast", "text": "Acme/App has 2 criteria at ABC123"}
	if !reflect.DeepEqual(calls[0].With, want) || calls[0].Run.ID != r.ID || calls[0].Key == "" {
		t.Fatalf("action call = %+v", calls[0])
	}
	v.publish(t, &domain.ChecksCompleted{Repo: "acme/app", Number: 4, SHA: "abc123", Conclusion: domain.ChecksSuccess})
	done := v.waitRun(t, r.ID, domain.RunSucceeded)
	if done.Error != "" || done.FinishedAt.IsZero() {
		t.Fatalf("run = %+v", done)
	}
	checks = v.waitStep(t, r.ID, "checks", 1, domain.StepSucceeded)
	out := output(t, checks)
	if out["signal"] != "checks.completed" || out["output"].(map[string]any)["conclusion"] != "success" || checks.SignalID == uuid.Nil() {
		t.Fatalf("checks output = %v", out)
	}
}

const reviewLoop = `
inputs: [repo]
steps:
  - id: build
    type: loop
    max_iterations: 3
    until: steps.review.output.decision == approved
    steps:
      - id: develop
        type: job
        with: {role: developer, repo: '${inputs.repo}', branch: 'agent/x-${iteration}', feedback: '${steps.review.output.feedback}'}
      - id: review
        type: gate
        with: {kind: merge_approval, title: 'Merge round ${iteration}', subject_repo: '${inputs.repo}', subject_number: 9, summary: '${steps.develop.output.summary}'}
  - id: after
    type: action
    action: record
    with: {decision: '${steps.review.output.decision}', rounds: '${steps.build.output.iterations}'}
`

func TestGateApprove(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, reviewLoop, map[string]any{"repo": "acme/app"})
	dev, req := v.waitJob(t, r.ID, "build/1/develop", 1)
	if req.Branch != "agent/x-1" || req.Feedback != "" {
		t.Fatalf("request = %+v", req)
	}
	v.finishJob(t, dev.JobID, domain.JobSucceeded, `{"summary":"did it"}`, "")
	review := v.waitStep(t, r.ID, "build/1/review", 1, domain.StepWaiting)
	g, err := v.gates.Get(t.Context(), v.account, review.GateID)
	must(t, err)
	if g.Kind != domain.GateMergeApproval || g.Title != "Merge round 1" || g.Summary != "did it" || g.SubjectNumber != 9 || g.SubjectRepo != "acme/app" {
		t.Fatalf("gate = %+v", g)
	}
	v.decide(t, r.ID, "build/1/review", 1, domain.GateApproved, "ship it")
	v.waitRun(t, r.ID, domain.RunSucceeded)
	calls := v.actionCalls()
	if len(calls) != 1 || calls[0].With["decision"] != "approved" || calls[0].With["rounds"] != 1 {
		t.Fatalf("after = %+v", calls)
	}
}

func TestGateRequestChanges(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, reviewLoop, map[string]any{"repo": "acme/app"})
	dev, _ := v.waitJob(t, r.ID, "build/1/develop", 1)
	v.finishJob(t, dev.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "build/1/review", 1, domain.GateChangesRequested, "add tests")

	dev, req := v.waitJob(t, r.ID, "build/2/develop", 1)
	if req.Feedback != "add tests" || req.Branch != "agent/x-2" {
		t.Fatalf("round 2 request = %+v", req)
	}
	v.finishJob(t, dev.JobID, domain.JobSucceeded, `{}`, "")
	g := v.decide(t, r.ID, "build/2/review", 1, domain.GateApproved, "")
	if g.Number != 2 {
		t.Fatalf("a new review round must open a new gate, got %s", g.Ref())
	}
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if calls := v.actionCalls(); len(calls) != 1 || calls[0].With["rounds"] != 2 {
		t.Fatalf("after = %+v", calls)
	}
}

func TestGateReject(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, reviewLoop, map[string]any{"repo": "acme/app"})
	dev, _ := v.waitJob(t, r.ID, "build/1/develop", 1)
	v.finishJob(t, dev.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "build/1/review", 1, domain.GateRejected, "no")
	done := v.waitRun(t, r.ID, domain.RunCanceled)
	if !strings.Contains(done.Error, "build/1/review: gate rejected") {
		t.Fatalf("error = %q", done.Error)
	}
	if len(v.actionCalls()) != 0 {
		t.Fatal("steps after a rejected gate ran")
	}
}

func TestGateRequestChangesOutsideLoop(t *testing.T) {
	const pb = `
steps:
  - {id: review, type: gate, on_failure: %s, with: {kind: spec_approval, title: spec, subject_repo: acme/app}}
  - {id: after, type: action, action: record}
`
	t.Run("fail", func(t *testing.T) {
		v := newEnv(t)
		_, r := v.engine(t, fmt.Sprintf(pb, "fail"), nil)
		v.decide(t, r.ID, "review", 1, domain.GateChangesRequested, "too vague")
		done := v.waitRun(t, r.ID, domain.RunFailed)
		if !strings.Contains(done.Error, "step review failed: changes requested outside a loop: too vague") || len(v.actionCalls()) != 0 {
			t.Fatalf("run = %+v, calls %d", done, len(v.actionCalls()))
		}
	})
	t.Run("escalate", func(t *testing.T) {
		v := newEnv(t)
		_, r := v.engine(t, fmt.Sprintf(pb, "escalate"), nil)
		first := v.decide(t, r.ID, "review", 1, domain.GateChangesRequested, "too vague")
		esc := v.waitStep(t, r.ID, "review:escalate", 1, domain.StepWaiting)
		g, _ := v.gates.Get(t.Context(), v.account, esc.GateID)
		if g.Kind != domain.GateNeedsYou || !strings.Contains(g.Summary, "too vague") {
			t.Fatalf("escalation gate = %+v", g)
		}
		v.decide(t, r.ID, "review:escalate", 1, domain.GateApproved, "")
		second := v.decide(t, r.ID, "review", 2, domain.GateApproved, "")
		if second.ID == first.ID {
			t.Fatal("the retry reused the resolved gate")
		}
		v.waitRun(t, r.ID, domain.RunSucceeded)
		if len(v.actionCalls()) != 1 {
			t.Fatalf("calls = %d", len(v.actionCalls()))
		}
	})
}

const exhaust = `
steps:
  - id: polish
    type: loop
    max_iterations: 2
    until: steps.try.output.done
    on_failure: %s
    steps:
      - {id: try, type: action, action: record, with: {round: '${iteration}'}}
`

func TestLoopUntil(t *testing.T) {
	v := newEnv(t)
	v.actions["record"] = v.record(func(c workflow.ActionCall, _ int) (any, error) {
		return map[string]any{"done": c.With["round"] == 2}, nil
	})
	_, r := v.engine(t, fmt.Sprintf(exhaust, "fail"), nil)
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if n := len(v.actionCalls()); n != 2 {
		t.Fatalf("rounds = %d, want 2", n)
	}
}

func TestLoopExhaustedFails(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fmt.Sprintf(exhaust, "fail"), nil)
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, `loop polish: "steps.try.output.done" not met after 2 iterations`) || len(v.actionCalls()) != 2 {
		t.Fatalf("run = %+v, calls %d", done, len(v.actionCalls()))
	}
}

func TestLoopEscalation(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, fmt.Sprintf(exhaust, "escalate"), nil)
	esc := v.waitStep(t, r.ID, "polish:escalate", 1, domain.StepWaiting)
	g, err := v.gates.Get(t.Context(), v.account, esc.GateID)
	must(t, err)
	if g.Kind != domain.GateNeedsYou || g.SubjectRepo != "acme/app" || !strings.Contains(g.Summary, "not met after 2 iterations") {
		t.Fatalf("needs_you gate = %+v", g)
	}
	v.waitRun(t, r.ID, domain.RunWaiting)
	// approve runs one more round, which still fails the condition
	v.decide(t, r.ID, "polish:escalate", 1, domain.GateApproved, "")
	v.waitStep(t, r.ID, "polish/3/try", 1, domain.StepSucceeded)
	esc2 := v.waitStep(t, r.ID, "polish:escalate", 2, domain.StepWaiting)
	if esc2.GateID == esc.GateID {
		t.Fatal("second escalation reused the gate")
	}
	// reject stops the run
	v.decide(t, r.ID, "polish:escalate", 2, domain.GateRejected, "")
	done := v.waitRun(t, r.ID, domain.RunCanceled)
	if !strings.Contains(done.Error, "escalation rejected") || len(v.actionCalls()) != 3 {
		t.Fatalf("run = %+v, calls %d", done, len(v.actionCalls()))
	}
}

func TestStepEscalation(t *testing.T) {
	v := newEnv(t)
	v.actions["record"] = v.record(func(_ workflow.ActionCall, n int) (any, error) {
		if n == 1 {
			return nil, errors.New("flaked")
		}
		return "ok", nil
	})
	_, r := v.engine(t, `
steps:
  - {id: build, type: job, on_failure: escalate, with: {role: developer, repo: acme/app, feedback: be careful}}
  - {id: flaky, type: action, action: record, on_failure: escalate}
`, nil)
	dev, _ := v.waitJob(t, r.ID, "build", 1)
	v.finishJob(t, dev.JobID, domain.JobFailed, "", "")
	esc := v.waitStep(t, r.ID, "build:escalate", 1, domain.StepWaiting)
	g, _ := v.gates.Get(t.Context(), v.account, esc.GateID)
	if !strings.Contains(g.Summary, "step build failed: JOB-1 failed: it broke") {
		t.Fatalf("summary = %q", g.Summary)
	}
	v.decide(t, r.ID, "build:escalate", 1, domain.GateApproved, "use the other API")
	dev2, req := v.waitJob(t, r.ID, "build", 2)
	if dev2.JobID == dev.JobID || req.Feedback != "be careful\n\nuse the other API" {
		t.Fatalf("retry = %+v, %+v", dev2, req)
	}
	v.finishJob(t, dev2.JobID, domain.JobSucceeded, `{}`, "")
	v.waitStep(t, r.ID, "flaky", 1, domain.StepFailed)
	v.decide(t, r.ID, "flaky:escalate", 1, domain.GateApproved, "")
	done := v.waitRun(t, r.ID, domain.RunSucceeded)
	if flaky := v.waitStep(t, r.ID, "flaky", 2, domain.StepSucceeded); output(t, flaky)["output"] != "ok" || done.Error != "" {
		t.Fatalf("flaky = %s", flaky.Output)
	}
}

// A loop with on_failure skip: a failure inside it opens a needs_you gate,
// approving it skips the loop and the run goes on; rejecting stops the run.
func TestLoopSkip(t *testing.T) {
	const pb = `
steps:
  - id: optional
    type: loop
    max_iterations: 1
    until: steps.risky.output
    on_failure: skip
    steps:
      - {id: risky, type: action, action: record}
  - {id: after, type: action, action: record, with: {skipped: '${steps.optional.output.skipped}'}}
`
	fail := func(workflow.ActionCall, int) (any, error) { return nil, errors.New("broke") }
	t.Run("approved", func(t *testing.T) {
		v := newEnv(t)
		v.actions["record"] = v.record(func(c workflow.ActionCall, _ int) (any, error) {
			if _, ok := c.With["skipped"]; ok {
				return "after", nil
			}
			return fail(c, 0)
		})
		_, r := v.engine(t, pb, nil)
		esc := v.waitStep(t, r.ID, "optional:escalate", 1, domain.StepWaiting)
		g, err := v.gates.Get(t.Context(), v.account, esc.GateID)
		must(t, err)
		if !strings.Contains(g.Summary, "step optional/1/risky failed: broke") || !strings.Contains(g.Summary, "skip it") {
			t.Fatalf("summary = %q", g.Summary)
		}
		v.decide(t, r.ID, "optional:escalate", 1, domain.GateApproved, "")
		v.waitRun(t, r.ID, domain.RunSucceeded)
		calls := v.actionCalls()
		if last := calls[len(calls)-1]; last.With["skipped"] != true {
			t.Errorf("after got %v", last.With)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		v := newEnv(t)
		v.actions["record"] = v.record(fail)
		_, r := v.engine(t, pb, nil)
		v.waitStep(t, r.ID, "optional:escalate", 1, domain.StepWaiting)
		v.decide(t, r.ID, "optional:escalate", 1, domain.GateRejected, "")
		v.waitRun(t, r.ID, domain.RunCanceled)
	})
	// A human who rejects a gate inside the loop stops the run: that is not a
	// failure to skip.
	t.Run("a gate rejected inside", func(t *testing.T) {
		v := newEnv(t)
		_, r := v.engine(t, `
steps:
  - id: optional
    type: loop
    max_iterations: 1
    until: steps.ask.output.decision
    on_failure: skip
    steps:
      - {id: ask, type: gate, with: {kind: needs_you, title: "Sure", subject_repo: acme/app}}
`, nil)
		v.waitStep(t, r.ID, "optional/1/ask", 1, domain.StepWaiting)
		v.decide(t, r.ID, "optional/1/ask", 1, domain.GateRejected, "no")
		done := v.waitRun(t, r.ID, domain.RunCanceled)
		steps, err := v.st.StepRuns().ListByRun(t.Context(), v.account, r.ID)
		must(t, err)
		if !strings.Contains(done.Error, "gate rejected") || slices.ContainsFunc(steps, func(s domain.StepRun) bool { return s.StepID == "optional:escalate" }) {
			t.Errorf("run = %+v", done)
		}
	})
	t.Run("not failing", func(t *testing.T) {
		v := newEnv(t)
		v.actions["record"] = v.record(func(c workflow.ActionCall, _ int) (any, error) {
			if _, ok := c.With["skipped"]; ok {
				return "after", nil
			}
			return true, nil
		})
		_, r := v.engine(t, pb, nil)
		v.waitRun(t, r.ID, domain.RunSucceeded)
		calls := v.actionCalls()
		if last := calls[len(calls)-1]; last.With["skipped"] != false {
			t.Errorf("after got %v", last.With)
		}
	})
}

func TestForeach(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [tasks]
steps:
  - id: each
    type: foreach
    items: ${inputs.tasks}
    steps:
      - {id: do, type: action, action: record, with: {title: '${item.title}', index: '${index}'}}
  - {id: sum, type: action, action: record, with: {all: '${steps.each.output}'}}
`, map[string]any{"tasks": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}, map[string]any{"title": "c"}}})
	v.waitRun(t, r.ID, domain.RunSucceeded)
	calls := v.actionCalls()
	if len(calls) != 4 {
		t.Fatalf("calls = %d", len(calls))
	}
	for i, title := range []string{"a", "b", "c"} {
		if calls[i].With["title"] != title || calls[i].With["index"] != i {
			t.Errorf("item %d = %v", i, calls[i].With)
		}
	}
	all, _ := calls[3].With["all"].([]any)
	if len(all) != 3 || all[2].(map[string]any)["do"].(map[string]any)["output"].(map[string]any)["title"] != "c" {
		t.Fatalf("sum = %v", calls[3].With)
	}
	steps, _ := v.st.StepRuns().ListByRun(t.Context(), v.account, r.ID)
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.StepID)
	}
	if want := []string{"each/0/do", "each/1/do", "each/2/do", "sum"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("step runs = %v, want %v", ids, want)
	}
}

func TestForeachNotAList(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [tasks]
steps:
  - {id: each, type: foreach, items: '${inputs.tasks}', steps: [{id: do, type: action, action: record}]}
`, map[string]any{"tasks": "abc"})
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "is not a list") {
		t.Fatalf("error = %q", done.Error)
	}
}

func TestEarlySignal(t *testing.T) {
	v := newEnv(t)
	v.publish(t, &domain.PRMerged{Repo: "acme/app", Number: 5, SHA: "f00"})
	_, r := v.engine(t, `
steps:
  - {id: merged, type: wait, with: {signal: pr.merged, repo: ACME/app, number: 5}}
  - {id: then, type: action, action: record, with: {sha: '${steps.merged.output.sha}'}}
`, nil)
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if calls := v.actionCalls(); len(calls) != 1 || calls[0].With["sha"] != "f00" {
		t.Fatalf("calls = %+v", calls)
	}
}

// TestWaitCouncilDecided: a decided council gives the wait the signal's
// values; a council that failed or was canceled fails the step.
func TestWaitCouncilDecided(t *testing.T) {
	const pb = `
inputs: [council]
steps:
  - {id: council, type: wait, with: {signal: council.decided, council_id: '${inputs.council}'}}
  - {id: then, type: action, action: record, with: {decision: '${steps.council.output.decision_id}'}}
`
	id, decision := domain.NewID(), domain.NewID()
	t.Run("decided", func(t *testing.T) {
		v := newEnv(t)
		_, r := v.engine(t, pb, map[string]any{"council": id.String()})
		v.waitStep(t, r.ID, "council", 1, domain.StepWaiting)
		v.publish(t, &domain.CouncilDecidedSignal{CouncilID: id, Status: domain.CouncilDecided, DecisionID: decision})
		v.waitRun(t, r.ID, domain.RunSucceeded)
		if calls := v.actionCalls(); len(calls) != 1 || calls[0].With["decision"] != decision.String() {
			t.Fatalf("calls = %+v", calls)
		}
	})
	for _, status := range []domain.CouncilStatus{domain.CouncilFailed, domain.CouncilCanceled} {
		t.Run(string(status), func(t *testing.T) {
			v := newEnv(t)
			v.publish(t, &domain.CouncilDecidedSignal{CouncilID: id, Status: status})
			_, r := v.engine(t, pb, map[string]any{"council": id.String()})
			done := v.waitRun(t, r.ID, domain.RunFailed)
			if !strings.Contains(done.Error, "council "+string(status)) {
				t.Fatalf("error = %q", done.Error)
			}
		})
	}
	v := newEnv(t)
	_, r := v.engine(t, `
steps:
  - {id: council, type: wait, with: {signal: council.decided, council_id: '${steps.council.output.none}'}}
`, nil)
	if done := v.waitRun(t, r.ID, domain.RunFailed); !strings.Contains(done.Error, "empty council_id") {
		t.Fatalf("error = %q", done.Error)
	}
}

// TestStartWithID: a run started with an ID is started once.
func TestStartWithID(t *testing.T) {
	v := newEnv(t)
	e, _ := run(t, v.config(t, map[string]string{"pb": "steps:\n- {id: a, type: action, action: record}"}))
	id := domain.NewID()
	req := workflow.StartRequest{ID: id, AccountID: v.account, ProjectID: v.project.ID}
	first, err := e.Start(t.Context(), "pb", req)
	must(t, err)
	again, err := e.Start(t.Context(), "pb", req)
	must(t, err)
	if first.ID != id || again.ID != id {
		t.Fatalf("runs %s, %s, want %s", first.ID, again.ID, id)
	}
	v.waitRun(t, id, domain.RunSucceeded)
	if n := len(v.actionCalls()); n != 1 {
		t.Fatalf("action calls = %d, want 1", n)
	}
}

func TestWaitTimeout(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
steps:
  - {id: merged, type: wait, timeout: 1h, with: {signal: pr.closed, repo: acme/app, number: 5}}
`, nil)
	v.waitStep(t, r.ID, "merged", 1, domain.StepWaiting)
	v.waitRun(t, r.ID, domain.RunWaiting)
	v.clock.Advance(time.Hour)
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "timed out after 1h0m0s waiting for pr:acme/app#5") {
		t.Fatalf("error = %q", done.Error)
	}
}

func TestStepFailures(t *testing.T) {
	tests := []struct {
		name   string
		pb     string
		inputs map[string]any
		want   string
	}{
		{"job with", "inputs: [n]\nsteps:\n- {id: a, type: job, with: {role: dev, needs: '${inputs.n}'}}", map[string]any{"n": "go"}, "step a failed: with: json: cannot unmarshal"},
		{"gate with", "inputs: [n]\nsteps:\n- {id: g, type: gate, with: {kind: spec_approval, title: t, subject_repo: r, subject_number: '${inputs.n}'}}", map[string]any{"n": "x"}, "step g failed: with:"},
		{"gate kind", "inputs: [k]\nsteps:\n- {id: g, type: gate, with: {kind: '${inputs.k}', title: t, subject_repo: r}}", map[string]any{"k": "bogus"}, "step g failed: open bogus gate"},
		{"wait with", "inputs: [n]\nsteps:\n- {id: w, type: wait, with: {signal: pr.merged, repo: r, number: '${inputs.n}'}}", map[string]any{"n": "x"}, "step w failed: with:"},
		{"wait empty", "inputs: [s]\nsteps:\n- {id: w, type: wait, with: {signal: checks.completed, repo: r, sha: '${inputs.s}'}}", map[string]any{"s": ""}, "wait for checks.completed: empty sha"},
		{"wait job", "inputs: [s]\nsteps:\n- {id: w, type: wait, with: {signal: job.completed, job_id: '${inputs.s}'}}", map[string]any{"s": uuid.Nil().String()}, "empty job_id"},
		{"wait gate", "inputs: [s]\nsteps:\n- {id: w, type: wait, with: {signal: gate.resolved, gate_id: '${inputs.s}'}}", map[string]any{"s": uuid.Nil().String()}, "empty gate_id"},
		{"wait gate id", "inputs: [s]\nsteps:\n- {id: w, type: wait, with: {signal: gate.resolved, gate_id: '${inputs.s}'}}", map[string]any{"s": "x"}, "with: invalid uuid"},
		{"action", "steps:\n- {id: a, type: action, action: fail}", nil, "step a failed: nope"},
		{"action output", "steps:\n- {id: a, type: action, action: chan}", nil, "step a failed: output: json: unsupported type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			v.actions["fail"] = func(context.Context, workflow.ActionCall) (any, error) { return nil, errors.New("nope") }
			v.actions["chan"] = func(context.Context, workflow.ActionCall) (any, error) { return make(chan int), nil }
			v.jobs.Err = nil
			_, r := v.engine(t, tt.pb, tt.inputs)
			done := v.waitRun(t, r.ID, domain.RunFailed)
			if !strings.Contains(done.Error, tt.want) {
				t.Fatalf("error = %q, want %q", done.Error, tt.want)
			}
		})
	}
}

func TestJobCreatorError(t *testing.T) {
	v := newEnv(t)
	v.jobs.Err = errors.New("no such role")
	_, r := v.engine(t, "steps:\n- {id: a, type: job, with: {role: dev}}", nil)
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step a failed: create job: no such role") {
		t.Fatalf("error = %q", done.Error)
	}
}

func TestStartAndNew(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	e, _ := run(t, v.config(t, map[string]string{"pb": "inputs: [repo]\nsteps:\n- {id: a, type: action, action: record}"}))
	_, err := e.Start(ctx, "nope", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	if !errors.Is(err, workflow.ErrUnknownPlaybook) {
		t.Fatalf("unknown playbook: %v", err)
	}
	_, err = e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	if !errors.Is(err, workflow.ErrMissingInput) || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("missing input: %v", err)
	}
	_, err = e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"repo": make(chan int)}})
	if err == nil {
		t.Fatal("unmarshalable inputs accepted")
	}
	_, err = e.Start(ctx, "pb", workflow.StartRequest{AccountID: domain.NewID(), ProjectID: v.project.ID, Inputs: map[string]any{"repo": "x"}})
	if err == nil {
		t.Fatal("run of another account's project started")
	}

	cfg := v.config(t, map[string]string{"pb": "steps:\n- {id: l, type: loop, max_iterations: 1, until: iteration, steps: [{id: a, type: action, action: missing}]}"})
	if _, err := workflow.New(cfg); !errors.Is(err, workflow.ErrInvalidPlaybook) || !strings.Contains(err.Error(), `step "a": action "missing" is not registered`) {
		t.Fatalf("New = %v", err)
	}
}

func TestUnknownPlaybookRun(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	gone := domain.WorkflowRun{AccountID: v.account, ProjectID: v.project.ID, Playbook: "gone", Status: domain.RunRunning}
	must(t, v.st.WorkflowRuns().Create(ctx, &gone))
	bad := domain.WorkflowRun{AccountID: v.account, ProjectID: v.project.ID, Playbook: "pb", Status: domain.RunWaiting, Inputs: []byte(`[1]`)}
	must(t, v.st.WorkflowRuns().Create(ctx, &bad))
	done := domain.WorkflowRun{AccountID: v.account, ProjectID: v.project.ID, Playbook: "pb", Status: domain.RunSucceeded}
	must(t, v.st.WorkflowRuns().Create(ctx, &done))
	run(t, v.config(t, map[string]string{"pb": "steps:\n- {id: a, type: action, action: record}"}))
	if r := v.waitRun(t, gone.ID, domain.RunFailed); r.Error != `failed: playbook "gone" is not loaded` && !strings.Contains(r.Error, `playbook "gone" is not loaded`) {
		t.Fatalf("error = %q", r.Error)
	}
	if r := v.waitRun(t, bad.ID, domain.RunFailed); !strings.HasPrefix(r.Error, "inputs:") {
		t.Fatalf("error = %q", r.Error)
	}
	time.Sleep(20 * time.Millisecond)
	if len(v.actionCalls()) != 0 {
		t.Fatal("a finished run was advanced")
	}
}

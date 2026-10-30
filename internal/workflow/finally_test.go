package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

const withFinally = `
inputs: [repo]
steps:
  - {id: build, type: job, with: {role: developer, repo: '${inputs.repo}'}}
  - {id: review, type: gate, with: {kind: merge_approval, title: merge, subject_repo: acme/app}}
finally:
  - {id: cleanup, type: action, action: record, with: {repo: '${inputs.repo}'}}
`

func TestFinallyRunsWhenTheRunFails(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, withFinally, map[string]any{"repo": "acme/app"})
	build, _ := v.waitJob(t, r.ID, "build", 1)
	v.finishJob(t, build.JobID, domain.JobFailed, "", "")
	got := v.waitRun(t, r.ID, domain.RunFailed)
	calls := v.actionCalls()
	if len(calls) != 1 || calls[0].With["repo"] != "acme/app" || calls[0].Run.ID != r.ID {
		t.Fatalf("finally calls = %+v", calls)
	}
	if !strings.Contains(got.Error, "build failed") {
		t.Errorf("run error = %q", got.Error)
	}
	s := v.waitStep(t, r.ID, "finally/cleanup", 1, domain.StepSucceeded)
	if calls[0].Key != s.ID.String() {
		t.Errorf("key = %s, step run %s", calls[0].Key, s.ID)
	}
}

// A canceled job fails its step like a failed one, whether or not its
// job.completed signal arrives, and the finally steps run.
func TestFinallyRunsWhenAJobIsCanceled(t *testing.T) {
	for _, published := range []bool{true, false} {
		t.Run(map[bool]string{true: "signal", false: "lost signal"}[published], func(t *testing.T) {
			v := newEnv(t)
			_, r := v.engine(t, withFinally, map[string]any{"repo": "acme/app"})
			build, _ := v.waitJob(t, r.ID, "build", 1)
			_, err := v.st.Jobs().Cancel(t.Context(), v.account, build.JobID)
			must(t, err)
			if published {
				v.publish(t, &domain.JobCompleted{JobID: build.JobID, Status: domain.JobCanceled})
			}
			got := v.waitRun(t, r.ID, domain.RunFailed)
			if got.Error != "step build failed: JOB-1 canceled" {
				t.Errorf("run error = %q", got.Error)
			}
			if n := len(v.actionCalls()); n != 1 {
				t.Errorf("finally calls = %d, want 1", n)
			}
		})
	}
}

func TestFinallyRunsWhenTheRunIsCanceled(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, withFinally, map[string]any{"repo": "acme/app"})
	build, _ := v.waitJob(t, r.ID, "build", 1)
	v.finishJob(t, build.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "review", 1, domain.GateRejected, "")
	v.waitRun(t, r.ID, domain.RunCanceled)
	if n := len(v.actionCalls()); n != 1 {
		t.Fatalf("finally calls = %d, want 1", n)
	}
}

func TestFinallySkippedWhenTheRunSucceeds(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, withFinally, map[string]any{"repo": "acme/app"})
	build, _ := v.waitJob(t, r.ID, "build", 1)
	v.finishJob(t, build.JobID, domain.JobSucceeded, `{}`, "")
	v.decide(t, r.ID, "review", 1, domain.GateApproved, "")
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if n := len(v.actionCalls()); n != 0 {
		t.Fatalf("finally calls = %d, want 0", n)
	}
}

func TestFinallyContinuesAfterAFailingStep(t *testing.T) {
	v := newEnv(t)
	failed := make(chan struct{}, 1)
	v.actions["boom"] = func(context.Context, workflow.ActionCall) (any, error) {
		failed <- struct{}{}
		return nil, errors.New("boom")
	}
	_, r := v.engine(t, `
steps:
  - {id: build, type: job, with: {role: developer, repo: acme/app}}
finally:
  - {id: first, type: action, action: boom}
  - {id: second, type: action, action: record}
`, nil)
	build, _ := v.waitJob(t, r.ID, "build", 1)
	v.finishJob(t, build.JobID, domain.JobFailed, "", "")
	v.waitRun(t, r.ID, domain.RunFailed)
	<-failed
	v.waitStep(t, r.ID, "finally/first", 1, domain.StepFailed)
	v.waitStep(t, r.ID, "finally/second", 1, domain.StepSucceeded)
}

func TestFinallyIsNotRepeatedAfterARestart(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": withFinally})
	e, err := workflow.New(cfg)
	must(t, err)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	must(t, err)

	// The engine dies after the finally action ran, before recording it.
	_, stop := run(t, cfg)
	build, _ := v.waitJob(t, r.ID, "build", 1)
	stop()
	v.finishJob(t, build.JobID, domain.JobFailed, "", "")
	v.crashAt(t, cfg, "finally/cleanup")
	if got, _ := v.st.WorkflowRuns().Get(t.Context(), v.account, r.ID); got.Status == domain.RunFailed {
		t.Fatalf("run ended despite the crash: %s", got.Status)
	}

	run(t, cfg)
	v.waitRun(t, r.ID, domain.RunFailed)
	calls := v.actionCalls()
	if len(calls) != 2 || calls[0].Key != calls[1].Key {
		t.Fatalf("finally calls = %+v, want two with the same key", calls)
	}

	// Another restart leaves the finished run alone.
	run(t, cfg)
	if n := len(v.actionCalls()); n != 2 {
		t.Fatalf("finally calls after another restart = %d", n)
	}
}

func TestFinallyActionsMustBeRegistered(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": "steps:\n- {id: a, type: action, action: record}\nfinally:\n- {id: b, type: action, action: nope}"})
	_, err := workflow.New(cfg)
	if !errors.Is(err, workflow.ErrInvalidPlaybook) || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("New = %v", err)
	}
}

func TestParsePlaybookFinally(t *testing.T) {
	p, err := workflow.ParsePlaybook("x", []byte(withFinally))
	if err != nil || len(p.Finally) != 1 || p.Finally[0].ID != "cleanup" {
		t.Fatalf("ParsePlaybook = %+v, %v", p, err)
	}
}

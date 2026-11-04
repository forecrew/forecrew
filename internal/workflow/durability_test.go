package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var errCrash = errors.New("crash")

// crashStore fails every step run update once a step run with id is about
// to be marked waiting or done: the engine is killed after the step's side
// effect and before the step is recorded as done.
type crashStore struct {
	store.Store
	id      string
	crashed chan struct{}
	once    sync.Once
}

func newCrashStore(st store.Store, id string) *crashStore {
	return &crashStore{Store: st, id: id, crashed: make(chan struct{})}
}

func (c *crashStore) StepRuns() store.StepRunRepo { return crashSteps{c.Store.StepRuns(), c} }

// WorkflowRuns fails reads after the crash, so the dead engine does not
// advance the run again before it is stopped.
func (c *crashStore) WorkflowRuns() store.WorkflowRunRepo {
	return crashRuns{c.Store.WorkflowRuns(), c}
}

type crashRuns struct {
	store.WorkflowRunRepo
	c *crashStore
}

func (r crashRuns) Get(ctx context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error) {
	select {
	case <-r.c.crashed:
		return domain.WorkflowRun{}, errCrash
	default:
	}
	return r.WorkflowRunRepo.Get(ctx, accountID, id)
}

type crashSteps struct {
	store.StepRunRepo
	c *crashStore
}

func (s crashSteps) Update(ctx context.Context, sr *domain.StepRun) error {
	select {
	case <-s.c.crashed:
		return errCrash
	default:
	}
	if sr.StepID == s.c.id {
		s.c.once.Do(func() { close(s.c.crashed) })
		return errCrash
	}
	return s.StepRunRepo.Update(ctx, sr)
}

// crashAt runs an engine on a store that crashes at step id and waits for
// the crash; it returns once the engine has stopped.
func (v *env) crashAt(t *testing.T, cfg workflow.Config, id string) {
	t.Helper()
	cs := newCrashStore(v.st, id)
	cfg.Store = cs
	_, stop := run(t, cfg)
	select {
	case <-cs.crashed:
	case <-time.After(5 * time.Second):
		t.Fatalf("no crash at %s", id)
	}
	stop()
}

func TestRestartDoesNotRepeatSideEffects(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	cfg := v.config(t, map[string]string{"pb": `
steps:
  - {id: build, type: job, with: {role: developer, repo: acme/app}}
  - {id: review, type: gate, with: {kind: merge_approval, title: merge, subject_repo: acme/app}}
  - {id: after, type: action, action: record}
`})
	e, err := workflow.New(cfg)
	must(t, err)
	r, err := e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	must(t, err)

	// The job is created, then the engine dies before recording it.
	v.crashAt(t, cfg, "build")
	if n := len(v.jobs.Requests()); n != 1 {
		t.Fatalf("job requests before restart = %d", n)
	}
	steps, _ := v.st.StepRuns().ListByRun(ctx, v.account, r.ID)
	if len(steps) != 1 || steps[0].Status != domain.StepRunning {
		t.Fatalf("steps after crash = %+v", steps)
	}
	// The job finishes while the engine is down (its ID is the step run's).
	// On restart the job step finds its job and completes; the gate is
	// opened and the engine dies again.
	v.finishJob(t, steps[0].ID, domain.JobSucceeded, `{}`, "")
	v.crashAt(t, cfg, "review")
	if n := len(v.jobs.Requests()); n != 1 {
		t.Fatalf("job requests after restart = %d, want 1", n)
	}
	// The action runs and the engine dies before recording it: an action is
	// called again with the same key and must be idempotent.
	v.decideStored(t, r.ID, "review")
	v.crashAt(t, cfg, "after")

	run(t, cfg)
	v.waitRun(t, r.ID, domain.RunSucceeded)
	if n := len(v.jobs.Requests()); n != 1 {
		t.Fatalf("job requests = %d, want 1", n)
	}
	if all, _ := v.st.Jobs().List(ctx, v.account, store.JobFilter{}); len(all) != 1 {
		t.Fatalf("jobs = %d, want 1", len(all))
	}
	if all, _ := v.st.Gates().List(ctx, v.account, store.GateFilter{}); len(all) != 1 {
		t.Fatalf("gates = %d, want 1", len(all))
	}
	calls := v.actionCalls()
	if len(calls) != 2 || calls[0].Key != calls[1].Key {
		t.Fatalf("action calls = %+v, want two with the same key", calls)
	}
	steps, _ = v.st.StepRuns().ListByRun(ctx, v.account, r.ID)
	if len(steps) != 3 {
		t.Fatalf("step runs = %d, want 3", len(steps))
	}
}

// decideStored approves the gate of a gate step that may still be recorded
// as running (its gate ID is the step run ID).
func (v *env) decideStored(t *testing.T, runID uuid.UUID, path string) {
	t.Helper()
	steps, err := v.st.StepRuns().ListByRun(t.Context(), v.account, runID)
	must(t, err)
	for _, s := range steps {
		if s.StepID == path {
			_, err := v.gates.Resolve(t.Context(), v.human, s.ID, domain.GateApproved, "")
			must(t, err)
			return
		}
	}
	t.Fatalf("no step %s", path)
}

func TestConcurrentAdvance(t *testing.T) {
	v := newEnv(t)
	ctx := t.Context()
	// A slow action leaves time for duplicate advances of the same run.
	v.actions["record"] = v.record(func(workflow.ActionCall, int) (any, error) {
		time.Sleep(3 * time.Millisecond)
		return nil, nil
	})
	cfg := v.config(t, map[string]string{"pb": `
inputs: [n]
steps:
  - {id: a, type: action, action: record, with: {n: '${inputs.n}'}}
  - {id: merged, type: wait, with: {signal: pr.merged, repo: acme/app, number: '${inputs.n}'}}
  - {id: b, type: action, action: record, with: {n: '${inputs.n}'}}
`})
	cfg.Workers, cfg.Sweep = 8, time.Millisecond
	e, _ := run(t, cfg)
	const runs = 5
	var ids []domain.WorkflowRun
	for i := range runs {
		r, err := e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"n": i + 1}})
		must(t, err)
		ids = append(ids, r)
	}
	var wg sync.WaitGroup
	for i := range runs {
		for range 4 {
			wg.Go(func() {
				s, err := domain.NewSignal(v.account, v.project.ID, &domain.PRMerged{Repo: "acme/app", Number: i + 1, SHA: fmt.Sprint(i)}, v.clock.Now())
				if err == nil {
					_, err = v.bus.Publish(ctx, s)
				}
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	for _, r := range ids {
		v.waitRun(t, r.ID, domain.RunSucceeded)
	}
	calls := v.actionCalls()
	keys := map[string]int{}
	for _, c := range calls {
		keys[c.Key]++
	}
	if len(calls) != 2*runs || len(keys) != 2*runs {
		t.Fatalf("action calls = %d with %d keys, want %d each", len(calls), len(keys), 2*runs)
	}
}

func TestLostGateSignal(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
steps:
  - {id: review, type: gate, with: {kind: spec_approval, title: spec, subject_repo: acme/app}}
`, nil)
	s := v.waitStep(t, r.ID, "review", 1, domain.StepWaiting)
	silent := gates.New(v.st, dropPublisher{}, v.clock.Now)
	_, err := silent.Resolve(t.Context(), v.human, s.GateID, domain.GateApproved, "fine")
	if !errors.Is(err, errDropped) {
		t.Fatalf("Resolve = %v", err)
	}
	v.waitRun(t, r.ID, domain.RunSucceeded)
	done := v.waitStep(t, r.ID, "review", 1, domain.StepSucceeded)
	if out := output(t, done)["output"].(map[string]any); out["feedback"] != "fine" {
		t.Fatalf("output = %v", out)
	}
}

// A wait for gate.resolved finds the gate resolved when the signal was lost.
func TestLostGateSignalOfAWaitStep(t *testing.T) {
	v := newEnv(t)
	silent := gates.New(v.st, dropPublisher{}, v.clock.Now)
	g, err := silent.Open(t.Context(), gates.OpenRequest{AccountID: v.account, ProjectID: v.project.ID, Kind: domain.GateAssumptionReview, Title: "Review A-1: x", SubjectRepo: "acme/app"})
	must(t, err)
	_, r := v.engine(t, `
inputs: [gate]
steps:
  - {id: answer, type: wait, with: {signal: gate.resolved, gate_id: '${inputs.gate}'}}
`, map[string]any{"gate": g.ID.String()})
	s := v.waitStep(t, r.ID, "answer", 1, domain.StepWaiting)
	if s.GateID != g.ID {
		t.Fatalf("step = %+v", s)
	}
	_, err = silent.Resolve(t.Context(), v.human, g.ID, domain.GateChangesRequested, "check it")
	if !errors.Is(err, errDropped) {
		t.Fatalf("Resolve = %v", err)
	}
	v.waitRun(t, r.ID, domain.RunSucceeded)
	done := v.waitStep(t, r.ID, "answer", 1, domain.StepSucceeded)
	if out := output(t, done)["output"].(map[string]any); out["decision"] != "changes_requested" || out["feedback"] != "check it" {
		t.Fatalf("output = %v", out)
	}
}

var errDropped = errors.New("dropped")

type dropPublisher struct{}

func (dropPublisher) Publish(context.Context, domain.Signal) (bool, error) { return false, errDropped }

func TestLostJobSignal(t *testing.T) {
	for _, tt := range []struct {
		status domain.JobStatus
		run    domain.RunStatus
	}{{domain.JobSucceeded, domain.RunSucceeded}, {domain.JobFailed, domain.RunFailed}} {
		t.Run(string(tt.status), func(t *testing.T) {
			v := newEnv(t)
			_, r := v.engine(t, "steps:\n- {id: build, type: job, with: {role: developer}}", nil)
			s := v.waitStep(t, r.ID, "build", 1, domain.StepWaiting)
			// The job finishes but job.completed is never published.
			j, err := v.st.Jobs().LeaseNext(t.Context(), v.account, v.worker.ID, []string{"runtime:"}, time.Hour)
			must(t, err)
			_, err = v.st.Jobs().Complete(t.Context(), v.account, j.ID, v.worker.ID, store.JobResult{Status: tt.status, Output: []byte(`{"ok":true}`), HeadSHA: "abc", Error: "boom"})
			must(t, err)
			v.waitRun(t, r.ID, tt.run)
			done, _ := v.st.StepRuns().Get(t.Context(), v.account, s.ID)
			if tt.status == domain.JobSucceeded && (output(t, done)["head_sha"] != "abc" || done.SignalID != uuid.Nil()) {
				t.Fatalf("step = %+v", done)
			}
			if tt.status == domain.JobFailed && !strings.Contains(done.Error, "JOB-1 failed: boom") {
				t.Fatalf("error = %q", done.Error)
			}
		})
	}
}

func TestWaitJobCompletedFails(t *testing.T) {
	v := newEnv(t)
	_, r := v.engine(t, `
inputs: [job]
steps:
  - {id: done, type: wait, with: {signal: job.completed, job_id: '${inputs.job}'}}
`, map[string]any{"job": mustJob(t, v).String()})
	v.waitStep(t, r.ID, "done", 1, domain.StepWaiting)
	jobs, _ := v.st.Jobs().List(t.Context(), v.account, store.JobFilter{})
	v.finishJob(t, jobs[0].ID, domain.JobFailed, "", "")
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step done failed: JOB-1 failed: it broke") {
		t.Fatalf("error = %q", done.Error)
	}
}

// mustJob creates a queued job outside any run.
func mustJob(t *testing.T, v *env) uuid.UUID {
	t.Helper()
	id, err := v.jobs.Create(t.Context(), workflow.JobRequest{AccountID: v.account, ProjectID: v.project.ID, Role: "developer"})
	must(t, err)
	return id
}

// idlessCreator creates jobs with new IDs, ignoring JobRequest.ID.
type idlessCreator struct{ inner workflow.JobCreator }

func (c idlessCreator) Create(ctx context.Context, req workflow.JobRequest) (uuid.UUID, error) {
	req.ID = uuid.Nil()
	return c.inner.Create(ctx, req)
}

func (c idlessCreator) Cancel(ctx context.Context, accountID, jobID uuid.UUID) error {
	return c.inner.Cancel(ctx, accountID, jobID)
}

func TestCreatorMustUseJobID(t *testing.T) {
	v := newEnv(t)
	cfg := v.config(t, map[string]string{"pb": "steps:\n- {id: build, type: job, with: {role: developer}}"})
	cfg.Jobs = idlessCreator{v.jobs}
	e, _ := run(t, cfg)
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	must(t, err)
	done := v.waitRun(t, r.ID, domain.RunFailed)
	if !strings.Contains(done.Error, "step build failed: create job: job creator returned job") || !strings.Contains(done.Error, "must use JobRequest.ID") {
		t.Fatalf("error = %q", done.Error)
	}
}

func TestWaitingRunResumesRunning(t *testing.T) {
	v := newEnv(t)
	release := make(chan struct{})
	v.actions["record"] = v.record(func(workflow.ActionCall, int) (any, error) {
		<-release
		return nil, nil
	})
	_, r := v.engine(t, `
steps:
  - {id: merged, type: wait, with: {signal: pr.merged, repo: acme/app, number: 1}}
  - {id: after, type: action, action: record}
  - {id: closed, type: wait, with: {signal: pr.closed, repo: acme/app, number: 2}}
`, nil)
	v.waitRun(t, r.ID, domain.RunWaiting)
	v.publish(t, &domain.PRMerged{Repo: "acme/app", Number: 1, SHA: "s"})
	v.waitRun(t, r.ID, domain.RunRunning) // while the action runs
	close(release)
	v.waitStep(t, r.ID, "closed", 1, domain.StepWaiting)
	v.waitRun(t, r.ID, domain.RunWaiting)
	v.publish(t, &domain.PRClosed{Repo: "acme/app", Number: 2})
	v.waitRun(t, r.ID, domain.RunSucceeded)
}

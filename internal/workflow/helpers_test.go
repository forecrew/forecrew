package workflow_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	must(t, err)
	return b
}

// env is a store with one account and project, a signal bus, a gate
// service, a recording job creator and an action registry.
type env struct {
	clock   *testkit.Clock
	st      store.Store
	bus     *testkit.SignalBus
	gates   *testkit.Gates
	jobs    *testkit.JobCreator
	actions workflow.Actions
	account uuid.UUID
	project domain.Project
	worker  domain.Worker
	human   domain.Actor

	mu    sync.Mutex
	calls map[string][]workflow.ActionCall
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	return newEnvOn(t, st, clock, acc)
}

// newEnvOn builds an env on st, adding a project and a worker to acc.
func newEnvOn(t *testing.T, st store.Store, clock *testkit.Clock, acc domain.Account) *env {
	t.Helper()
	ctx := t.Context()
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &p))
	w := domain.Worker{AccountID: acc.ID, Name: "box", TokenHash: []byte(domain.NewID().String())}
	must(t, st.Workers().Create(ctx, &w))
	bus := testkit.NewSignalBus(st)
	v := &env{
		clock: clock, st: st, bus: bus, gates: testkit.NewGates(st, bus, clock.Now), jobs: &testkit.JobCreator{Store: st},
		account: acc.ID, project: p, worker: w, human: domain.Actor{AccountID: acc.ID, Kind: domain.ActorUser, Name: "ana"},
		calls: map[string][]workflow.ActionCall{},
	}
	v.actions = workflow.Actions{"record": v.record(nil)}
	return v
}

// record returns an action that records its calls under the action name
// "record" and returns result(call, n) for the n-th call (from 1), or the
// call's with when result is nil.
func (v *env) record(result func(call workflow.ActionCall, n int) (any, error)) workflow.Action {
	return func(_ context.Context, call workflow.ActionCall) (any, error) {
		v.mu.Lock()
		v.calls["record"] = append(v.calls["record"], call)
		n := len(v.calls["record"])
		v.mu.Unlock()
		if result == nil {
			return call.With, nil
		}
		return result(call, n)
	}
}

func (v *env) actionCalls() []workflow.ActionCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.calls["record"])
}

func (v *env) config(t *testing.T, playbooks map[string]string) workflow.Config {
	t.Helper()
	books := map[string]*workflow.Playbook{}
	for name, src := range playbooks {
		p, err := workflow.ParsePlaybook(name, []byte(src))
		must(t, err)
		books[name] = p
	}
	return workflow.Config{
		Store: v.st, Bus: v.bus, Jobs: v.jobs, Gates: v.gates, Actions: v.actions, Playbooks: books,
		Now: v.clock.Now, Workers: 4, Sweep: 5 * time.Millisecond,
	}
}

// run starts an engine with cfg until the test ends; stop ends it early.
func run(t *testing.T, cfg workflow.Config) (e *workflow.Engine, stop func()) {
	t.Helper()
	e, err := workflow.New(cfg)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run = %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return e, stop
}

// engine runs an engine with the playbook pb and starts a run of it.
func (v *env) engine(t *testing.T, pb string, inputs map[string]any) (*workflow.Engine, domain.WorkflowRun) {
	t.Helper()
	e, _ := run(t, v.config(t, map[string]string{"pb": pb}))
	r, err := e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: inputs})
	must(t, err)
	return e, r
}

// eventually polls cond until it holds or 5 s have passed.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitRun waits for the run to reach status.
func (v *env) waitRun(t *testing.T, runID uuid.UUID, status domain.RunStatus) domain.WorkflowRun {
	t.Helper()
	var r domain.WorkflowRun
	eventually(t, "run "+string(status), func() bool {
		var err error
		r, err = v.st.WorkflowRuns().Get(t.Context(), v.account, runID)
		must(t, err)
		return r.Status == status
	})
	return r
}

// waitStep waits for the step run (path, attempt) to reach status.
func (v *env) waitStep(t *testing.T, runID uuid.UUID, path string, attempt int, status domain.StepStatus) domain.StepRun {
	t.Helper()
	var found domain.StepRun
	eventually(t, "step "+path+" "+string(status), func() bool {
		steps, err := v.st.StepRuns().ListByRun(t.Context(), v.account, runID)
		must(t, err)
		for _, s := range steps {
			if s.StepID == path && s.Attempt == attempt && s.Status == status {
				found = s
				return true
			}
		}
		return false
	})
	return found
}

// finishJob runs the queued job jobID on the test worker with the result and
// publishes job.completed.
func (v *env) finishJob(t *testing.T, jobID uuid.UUID, status domain.JobStatus, output, sha string) {
	t.Helper()
	ctx := t.Context()
	j, err := v.st.Jobs().Get(ctx, v.account, jobID)
	must(t, err)
	leased, err := v.st.Jobs().LeaseNext(ctx, v.account, v.worker.ID, j.RequiredLabels(), time.Hour)
	must(t, err)
	if leased.ID != jobID {
		t.Fatalf("leased %s, want %s", leased.Ref(), j.Ref())
	}
	res := store.JobResult{Status: status, HeadSHA: sha, Error: "it broke"}
	if output != "" {
		res.Output = json.RawMessage(output)
	}
	_, err = v.st.Jobs().Complete(ctx, v.account, jobID, v.worker.ID, res)
	must(t, err)
	v.publish(t, &domain.JobCompleted{JobID: jobID, Status: status})
}

func (v *env) publish(t *testing.T, p domain.SignalPayload) {
	t.Helper()
	s, err := domain.NewSignal(v.account, v.project.ID, p, v.clock.Now())
	must(t, err)
	_, err = v.bus.Publish(t.Context(), s)
	must(t, err)
}

// waitJob waits for the job step (path, attempt) and returns its request.
func (v *env) waitJob(t *testing.T, runID uuid.UUID, path string, attempt int) (domain.StepRun, workflow.JobRequest) {
	t.Helper()
	s := v.waitStep(t, runID, path, attempt, domain.StepWaiting)
	for _, req := range v.jobs.Requests() {
		if req.ID == s.JobID {
			return s, req
		}
	}
	t.Fatalf("no job request for step %s", path)
	return s, workflow.JobRequest{}
}

// decide resolves the gate of the waiting gate step (path, attempt).
func (v *env) decide(t *testing.T, runID uuid.UUID, path string, attempt int, d domain.GateStatus, feedback string) domain.Gate {
	t.Helper()
	s := v.waitStep(t, runID, path, attempt, domain.StepWaiting)
	g, err := v.gates.Resolve(t.Context(), v.human, s.GateID, d, feedback)
	must(t, err)
	return g
}

// output returns the decoded output of a finished step run.
func output(t *testing.T, s domain.StepRun) map[string]any {
	t.Helper()
	var v map[string]any
	must(t, json.Unmarshal(s.Output, &v))
	return v
}

var _ gates.Publisher = (*testkit.SignalBus)(nil)

package playbooks_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

// runner is an env with a running engine and one run of a playbook of
// packs/playbooks: the test plays the workers, the GitHub webhook and the
// human.
type runner struct {
	*env
	jobs   *testkit.JobCreator
	worker domain.Worker
	human  domain.Actor
	run    domain.WorkflowRun
	// register, when set, is told of every job the test finishes, as the
	// job service does (see withRegisters).
	register *registers.Service
}

// startRun starts a run of playbook with req. The engine stops when the test
// ends.
func startRun(t *testing.T, v *env, playbook string, req workflow.StartRequest) *runner {
	t.Helper()
	books, err := workflow.LoadPlaybooks(forecrew.Files, "packs/playbooks")
	must(t, err)
	r := &runner{env: v, jobs: &testkit.JobCreator{Store: v.st}, human: domain.Actor{AccountID: v.account, Kind: domain.ActorUser, Name: "ana"}}
	r.worker = domain.Worker{AccountID: v.account, Name: "box", TokenHash: []byte(domain.NewID().String())}
	must(t, v.st.Workers().Create(t.Context(), &r.worker))
	e, err := workflow.New(workflow.Config{
		Store: v.st, Bus: v.bus, Jobs: r.jobs, Gates: v.gates, Actions: v.actions, Playbooks: books,
		Now: v.clock.Now, Workers: 2, Sweep: 5 * time.Millisecond,
	})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = e.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	r.run, err = e.Start(t.Context(), playbook, req)
	must(t, err)
	return r
}

// finishJob completes the queued job as its worker would, with output, the
// commit it pushed and its cost, and publishes job.completed.
func (r *runner) finishJob(t *testing.T, id uuid.UUID, output []byte, sha string, cost float64) {
	t.Helper()
	ctx := t.Context()
	j, err := r.st.Jobs().Get(ctx, r.account, id)
	must(t, err)
	leased, err := r.st.Jobs().LeaseNext(ctx, r.account, r.worker.ID, j.RequiredLabels(), time.Hour)
	must(t, err)
	done, err := r.st.Jobs().Complete(ctx, r.account, leased.ID, r.worker.ID, store.JobResult{Status: domain.JobSucceeded, Output: output, HeadSHA: sha, CostUSD: cost})
	must(t, err)
	if r.register != nil {
		must(t, r.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error { return r.register.JobCompleted(ctx, tx, done) }))
	}
	r.publish(t, &domain.JobCompleted{JobID: leased.ID, Status: domain.JobSucceeded})
}

func (r *runner) github() domain.Actor {
	a := githubBot
	a.AccountID = r.account
	return a
}

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

// waitStep waits for the step run (path, attempt 1 unless given) to reach status.
func (r *runner) waitStep(t *testing.T, path string, status domain.StepStatus) domain.StepRun {
	t.Helper()
	var found domain.StepRun
	eventually(t, "step "+path+" "+string(status), func() bool {
		steps, err := r.st.StepRuns().ListByRun(t.Context(), r.account, r.run.ID)
		must(t, err)
		for _, s := range steps {
			if s.StepID == path && s.Attempt == 1 && s.Status == status {
				found = s
				return true
			}
		}
		return false
	})
	return found
}

func (r *runner) waitRun(t *testing.T, status domain.RunStatus) domain.WorkflowRun {
	t.Helper()
	var run domain.WorkflowRun
	eventually(t, "run "+string(status), func() bool {
		var err error
		run, err = r.st.WorkflowRuns().Get(t.Context(), r.account, r.run.ID)
		must(t, err)
		return run.Status == status
	})
	return run
}

func (r *runner) publish(t *testing.T, p domain.SignalPayload) {
	t.Helper()
	sig, err := domain.NewSignal(r.account, r.project.ID, p, r.clock.Now())
	must(t, err)
	_, err = r.bus.Publish(t.Context(), sig)
	must(t, err)
}

// pendingGate waits for the one pending gate of kind and returns it.
func (r *runner) pendingGate(t *testing.T, kind domain.GateKind) domain.Gate {
	t.Helper()
	var g domain.Gate
	eventually(t, string(kind)+" gate", func() bool {
		gs, err := r.gates.ListPending(t.Context(), r.account, r.project.ID)
		must(t, err)
		for _, x := range gs {
			if x.Kind == kind {
				g = x
				return true
			}
		}
		return false
	})
	return g
}

// decide answers the pending gate of kind with a gate tool.
func (r *runner) decide(t *testing.T, kind domain.GateKind, tool, feedback string) domain.Gate {
	t.Helper()
	g := r.pendingGate(t, kind)
	out, err := r.gates.Decide(t.Context(), r.human, tool, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: feedback})
	must(t, err)
	return out
}

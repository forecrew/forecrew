//go:build integration

package workflow_test

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

// newPostgresEnv builds an env on a freshly migrated database.
func newPostgresEnv(t *testing.T) *env {
	t.Helper()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(t.Context())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("default account: %v, %v", accounts, err)
	}
	return newEnvOn(t, st, testkit.NewClock(time.Now()), accounts[0])
}

// TestPostgresRun runs a playbook on Postgres: an early signal, a job woken
// through ListWaiting (the sweep is too slow to matter) and a restart that
// resumes the run through ListActive.
func TestPostgresRun(t *testing.T) {
	v := newPostgresEnv(t)
	ctx := t.Context()
	cfg := v.config(t, map[string]string{"pb": `
inputs: [repo]
steps:
  - {id: merged, type: wait, with: {signal: pr.merged, repo: '${inputs.repo}', number: 3}}
  - {id: build, type: job, with: {role: developer, repo: '${inputs.repo}', base_ref: '${steps.merged.output.sha}'}}
  - {id: review, type: gate, with: {kind: merge_approval, title: merge, subject_repo: '${inputs.repo}', subject_number: 3}}
  - {id: after, type: action, action: record, with: {sha: '${steps.build.head_sha}', decision: '${steps.review.output.decision}'}}
`})
	cfg.Sweep = time.Hour

	// The signal is stored before the run starts.
	v.publish(t, &domain.PRMerged{Repo: "acme/app", Number: 3, SHA: "base1"})
	e, stop := run(t, cfg)
	r, err := e.Start(ctx, "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID, Inputs: map[string]any{"repo": "acme/app"}})
	must(t, err)
	build, req := v.waitJob(t, r.ID, "build", 1)
	if req.BaseRef != "base1" {
		t.Fatalf("job request = %+v", req)
	}
	v.waitRun(t, r.ID, domain.RunWaiting)
	if waiting, err := v.st.StepRuns().ListWaiting(ctx, v.account, domain.JobKey(build.JobID)); err != nil || len(waiting) != 1 {
		t.Fatalf("ListWaiting = %v, %v", waiting, err)
	}
	// The job's signal wakes the run through ListWaiting.
	v.finishJob(t, build.JobID, domain.JobSucceeded, `{"summary":"done"}`, "head1")
	review := v.waitStep(t, r.ID, "review", 1, domain.StepWaiting)

	// Restart: the gate is resolved while no engine runs, and the new engine
	// finds the run through ListActive.
	stop()
	active, err := v.st.WorkflowRuns().ListActive(ctx, v.account)
	if err != nil || len(active) != 1 || active[0].ID != r.ID {
		t.Fatalf("ListActive = %v, %v", active, err)
	}
	_, err = v.gates.Resolve(ctx, v.human, review.GateID, domain.GateApproved, "")
	must(t, err)
	run(t, cfg)
	v.waitRun(t, r.ID, domain.RunSucceeded)

	calls := v.actionCalls()
	if len(calls) != 1 || calls[0].With["sha"] != "head1" || calls[0].With["decision"] != "approved" {
		t.Fatalf("action calls = %+v", calls)
	}
	if jobs, _ := v.st.Jobs().List(ctx, v.account, store.JobFilter{}); len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	steps, _ := v.st.StepRuns().ListByRun(ctx, v.account, r.ID)
	if len(steps) != 4 {
		t.Fatalf("step runs = %d, want 4", len(steps))
	}
	for _, s := range steps {
		if s.Status != domain.StepSucceeded || s.FinishedAt.IsZero() {
			t.Errorf("step %s = %s", s.StepID, s.Status)
		}
	}
}

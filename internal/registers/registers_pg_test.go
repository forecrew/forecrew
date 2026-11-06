//go:build integration

package registers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// An assumption_record call that is in flight while its job completes either
// commits before the completion or is refused: the register never gets an
// item from a job that is visibly finished.
func TestRecordAssumptionRacesCompletionOnPostgres(t *testing.T) {
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	st := postgres.New(pool)
	ctx := t.Context()
	accounts, err := st.Accounts().List(ctx)
	must(t, err)
	acc := accounts[0].ID
	p := domain.Project{AccountID: acc, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &p))
	g := domain.Goal{AccountID: acc, ProjectID: p.ID, Title: "Ship it", Status: domain.GoalInProgress, IssueNumber: 5, CreatedBy: "user:me"}
	must(t, st.Goals().Create(ctx, &g))
	regs := registers.New(st, testkit.NewSignalBus(st), time.Now)

	newJob := func() domain.Job {
		j := domain.Job{AccountID: acc, ProjectID: p.ID, Role: "developer", Runtime: "claude-code", GoalID: g.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
		must(t, st.Jobs().Create(ctx, &j))
		return testkit.Lease(t, st, j)
	}
	count := func() int {
		as, err := st.Assumptions().List(ctx, acc, store.AssumptionFilter{})
		must(t, err)
		return len(as)
	}
	finish := func(tx store.Store, j domain.Job) error {
		_, err := tx.Jobs().Complete(ctx, acc, j.ID, j.WorkerID, store.JobResult{Status: domain.JobSucceeded})
		return err
	}

	// Before the completion: recorded.
	j := newJob()
	if _, _, err := regs.RecordAssumption(ctx, j, stated("Recorded while running", domain.LevelLow)); err != nil {
		t.Fatal(err)
	}

	// After the completion: refused, nothing stored.
	must(t, st.WithTx(ctx, func(_ context.Context, tx store.Store) error { return finish(tx, j) }))
	if _, _, err := regs.RecordAssumption(ctx, j, stated("Recorded too late", domain.LevelHigh)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("after completion: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d assumptions after a refused record, want 1", n)
	}

	// During the completion: the record waits for the job's row, which the
	// completing transaction holds, and is refused when that commits.
	j = newJob()
	done := make(chan error, 1)
	must(t, st.WithTx(ctx, func(_ context.Context, tx store.Store) error {
		if err := finish(tx, j); err != nil {
			return err
		}
		go func() {
			_, _, err := regs.RecordAssumption(ctx, j, stated("Recorded during completion", domain.LevelHigh))
			done <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&waiting))
			if waiting > 0 {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("the record did not wait for the completion: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("the record never waited for the job's lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}))
	if err := <-done; !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("during completion: %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d assumptions after the raced record, want 1", n)
	}
}

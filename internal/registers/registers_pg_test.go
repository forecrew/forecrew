//go:build integration

package registers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
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

// pgWorld is a Postgres database with a project and a goal, a register and a
// gate service.
type pgWorld struct {
	pool  *pgxpool.Pool
	st    store.Store
	regs  *registers.Service
	gates *gates.Keeper
	acc   uuid.UUID
	p     domain.Project
	g     domain.Goal
}

func newPGWorld(t *testing.T) *pgWorld {
	t.Helper()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(t.Context())
	must(t, err)
	acc := accounts[0].ID
	p := domain.Project{AccountID: acc, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	g := domain.Goal{AccountID: acc, ProjectID: p.ID, Title: "Ship it", Status: domain.GoalInProgress, IssueNumber: 5, CreatedBy: "user:me"}
	must(t, st.Goals().Create(t.Context(), &g))
	bus := testkit.NewSignalBus(st)
	return &pgWorld{pool, st, registers.New(st, bus, time.Now), gates.New(st, bus, time.Now), acc, p, g}
}

func (w *pgWorld) job(t *testing.T) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: w.acc, ProjectID: w.p.ID, Role: "developer", Runtime: "claude-code", GoalID: w.g.ID, Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, w.st.Jobs().Create(t.Context(), &j))
	return testkit.Lease(t, w.st, j)
}

// waitForLock returns once a session of the database waits for a lock.
func (w *pgWorld) waitForLock(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`).Scan(&n))
		if n > 0 {
			return
		}
	}
	t.Fatal("nobody waited for a lock")
}

func deadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// A high-impact assumption_record opens a review gate; ask_human opens a
// question gate. Both take the job row before the gate counter, so racing
// calls of one job never deadlock.
func TestRecordRacesGateOpenWithoutDeadlock(t *testing.T) {
	w := newPGWorld(t)
	ctx := t.Context()
	var deadlocks atomic.Int64
	check := func(err error) {
		if deadlock(err) {
			deadlocks.Add(1)
			t.Errorf("deadlock: %v", err)
		} else if err != nil {
			t.Errorf("unexpected: %v", err)
		}
	}
	for round := range 15 {
		j := w.job(t)
		var wg sync.WaitGroup
		for k := range 6 {
			wg.Go(func() {
				if k%2 == 0 {
					_, _, err := w.regs.RecordAssumption(ctx, j, stated(fmt.Sprintf("round %d high %d", round, k), domain.LevelHigh))
					check(err)
					return
				}
				_, err := w.gates.Open(ctx, gates.OpenRequest{
					AccountID: j.AccountID, ProjectID: j.ProjectID, Kind: domain.GateNeedsYou, Origin: domain.GateFromAskHuman, Title: "q",
					SubjectRepo: "acme/app", GoalID: j.GoalID, JobID: j.ID,
					Before: func(ctx context.Context, tx store.Store) error {
						return tx.Jobs().LockLeased(ctx, j.AccountID, j.ID, j.WorkerID, j.Attempts)
					},
				})
				check(err)
			})
		}
		wg.Wait()
	}
}

// Completing a job whose output has a high-impact assumption holds the job
// row and then opens a review gate; a request_access of the same job takes
// the job row first and the gate counter after. They must not deadlock: the
// request waits and is refused once the job has completed.
func TestCompleteRacesRequestAccessWithoutDeadlock(t *testing.T) {
	w := newPGWorld(t)
	ctx := t.Context()
	policy, err := knowledge.ParsePolicy([]byte("version: 1\nsections:\n  closed: {owner: architect, access: {propose: [architect], read: [architect]}}\n"))
	must(t, err)
	svc := &proposals.Service{Store: w.st, Gates: w.gates, Policy: func(context.Context, domain.Project) (knowledge.Policy, error) { return policy, nil }}
	for range 5 {
		j := w.job(t)
		out, err := json.Marshal(map[string]any{"assumptions": []domain.StatedAssumption{stated("From the output", domain.LevelHigh)}})
		must(t, err)
		access := make(chan error, 1)
		completeErr := w.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			done, err := tx.Jobs().Complete(ctx, j.AccountID, j.ID, j.WorkerID, store.JobResult{Status: domain.JobSucceeded, Output: out})
			if err != nil {
				return err
			}
			go func() {
				_, err := svc.RequestAccess(context.WithoutCancel(ctx), j, proposals.AccessRequest{Section: "closed", Level: "propose", Reason: "r"})
				access <- err
			}()
			w.waitForLock(t)
			return w.regs.JobCompleted(ctx, tx, done)
		})
		if completeErr != nil {
			t.Fatalf("complete: %v", completeErr)
		}
		if err := <-access; !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("request_access during completion: %v, want a lost lease", err)
		}
	}
}

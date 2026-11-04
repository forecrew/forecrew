//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

func TestJobsCreateAndQuery(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	g := f.newGoal(t)
	j := f.newJob(t, "Linux", " docker", "linux")
	if j.Number != 1 || j.Status != domain.JobQueued || j.MaxAttempts != domain.DefaultMaxAttempts || j.Ref() != "JOB-1" {
		t.Fatalf("created = %+v", j)
	}
	got, err := repo.Get(ctx, f.account, j.ID)
	if err != nil || got.Role != "developer" || got.Spec.Prompt != "go" || got.Spec.Env != nil || got.Spec.RepoToken != "" {
		t.Fatalf("get = %+v, %v (secrets must not be stored)", got, err)
	}
	if want := []string{"docker", "linux"}; fmt.Sprint(got.Needs) != fmt.Sprint(want) {
		t.Fatalf("needs = %v, want %v", got.Needs, want)
	}
	byNum, err := repo.GetByNumber(ctx, f.account, 1)
	if err != nil || byNum.ID != j.ID {
		t.Fatalf("by number = %+v, %v", byNum, err)
	}
	_, err = repo.GetByNumber(ctx, f.account, 2)
	wantErr(t, err, store.ErrNotFound)

	withGoal := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "reviewer", Runtime: "codex", GoalID: g.ID, MaxAttempts: 1}
	must(t, repo.Create(ctx, &withGoal))
	list, err := repo.List(ctx, f.account, store.JobFilter{ProjectID: f.project.ID})
	if err != nil || len(list) != 2 || list[0].ID != withGoal.ID {
		t.Fatalf("list newest first = %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, store.JobFilter{GoalID: g.ID, Status: domain.JobQueued, Limit: 1})
	if len(list) != 1 || list[0].ID != withGoal.ID || list[0].GoalID != g.ID {
		t.Fatalf("filtered = %+v", list)
	}
	list, _ = repo.List(ctx, f.account, store.JobFilter{WorkerID: domain.NewID(), TaskID: domain.NewID()})
	if len(list) != 0 {
		t.Fatalf("by worker = %+v", list)
	}
	orphan := domain.Job{AccountID: f.account, ProjectID: domain.NewID(), Role: "x", Runtime: "codex"}
	wantErr(t, repo.Create(ctx, &orphan), store.ErrNotFound)
}

func TestLeaseNextMatchesLabels(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	docker := f.newJob(t, "docker")
	plain := f.newJob(t)
	worker := f.newWorker(t, "w1").ID

	// No docker label: the older docker job is skipped.
	got, err := repo.LeaseNext(ctx, f.account, worker, []string{"Runtime:Claude-Code"}, time.Minute)
	if err != nil || got.ID != plain.ID || got.Status != domain.JobRunning || got.WorkerID != worker {
		t.Fatalf("lease = %+v, %v", got, err)
	}
	if d := time.Until(got.LeaseExpiresAt); d < 50*time.Second || d > time.Minute+time.Second || got.StartedAt.IsZero() {
		t.Fatalf("lease times = %+v", got)
	}
	_, err = repo.LeaseNext(ctx, f.account, worker, []string{"runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	_, err = repo.LeaseNext(ctx, f.account, worker, []string{"docker"}, time.Minute) // no runtime label
	wantErr(t, err, store.ErrNoJob)
	_, err = repo.LeaseNext(ctx, f.account, domain.NewID(), []string{"docker", "runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob) // unknown worker

	got, err = repo.LeaseNext(ctx, f.account, worker, []string{"DOCKER", "runtime:claude-code", "linux"}, time.Minute)
	if err != nil || got.ID != docker.ID {
		t.Fatalf("lease with docker = %+v, %v", got, err)
	}
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	_, err = repo.LeaseNext(ctx, other.ID, worker, []string{"docker", "runtime:claude-code"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
}

func TestLeaseNextConcurrent(t *testing.T) {
	f := newFixture(t)
	for range 40 {
		f.newJob(t)
	}
	leased := leaseAll(t, f, 4)
	if len(leased) != 40 {
		t.Fatalf("leased %d jobs", len(leased))
	}
}

// leaseAll drains the queue with several goroutines and returns the leased
// job IDs, failing when one is leased twice.
func leaseAll(t *testing.T, f *fixture, workers int) map[uuid.UUID]int {
	t.Helper()
	var (
		mu     sync.Mutex
		leased = map[uuid.UUID]int{}
		wg     sync.WaitGroup
	)
	q := queue.New(f.st.Jobs())
	for i := range workers {
		worker := f.newWorker(t, fmt.Sprintf("w%d", i)).ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := q.Lease(t.Context(), f.account, worker, []string{"runtime:claude-code"})
				if err != nil {
					if !errors.Is(err, store.ErrNoJob) {
						t.Error(err)
					}
					return
				}
				mu.Lock()
				leased[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for id, n := range leased {
		if n != 1 {
			t.Errorf("job %s leased %d times", id, n)
		}
	}
	return leased
}

func TestQueueDrainsConcurrently(t *testing.T) {
	f := newFixture(t)
	q := queue.New(f.st.Jobs())
	for range 200 {
		j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "dev", Runtime: "claude-code"}
		must(t, q.Enqueue(t.Context(), &j))
	}
	leased := leaseAll(t, f, 8)
	if len(leased) != 200 {
		t.Fatalf("leased %d of 200", len(leased))
	}
	var queued int
	must(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE status = 'queued'`).Scan(&queued))
	if queued != 0 {
		t.Fatalf("%d jobs still queued", queued)
	}
}

func TestJobLeaseLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	worker, other := f.newWorker(t, "w1").ID, f.newWorker(t, "w2").ID
	labels := []string{"runtime:claude-code"}
	j := f.newJob(t)
	leased, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
	must(t, err)

	ext, err := repo.ExtendLease(ctx, f.account, j.ID, worker, time.Hour)
	if err != nil || !ext.LeaseExpiresAt.After(leased.LeaseExpiresAt.Add(30*time.Minute)) {
		t.Fatalf("extend = %+v, %v", ext, err)
	}
	cur, err := repo.ExtendLease(ctx, f.account, j.ID, other, time.Hour)
	wantErr(t, err, store.ErrLeaseLost)
	if cur.ID != j.ID {
		t.Fatalf("lease lost returns the job: %+v", cur)
	}
	_, err = repo.ExtendLease(ctx, f.account, domain.NewID(), worker, time.Hour)
	wantErr(t, err, store.ErrNotFound)

	_, err = repo.Complete(ctx, f.account, j.ID, other, store.JobResult{Status: domain.JobSucceeded})
	wantErr(t, err, store.ErrLeaseLost)
	_, err = repo.Complete(ctx, f.account, j.ID, worker, store.JobResult{Status: domain.JobCanceled})
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = repo.Complete(ctx, f.account, domain.NewID(), worker, store.JobResult{Status: domain.JobSucceeded})
	wantErr(t, err, store.ErrNotFound)

	res := store.JobResult{Status: domain.JobSucceeded, Output: []byte(`{"ok": true}`), HeadSHA: "abc", CostUSD: 1.25,
		Usage: domain.Usage{InputTokens: 10, OutputTokens: 5}}
	done, err := repo.Complete(ctx, f.account, j.ID, worker, res)
	if err != nil || done.Status != domain.JobSucceeded || done.WorkerID != uuid.Nil() || done.FinishedAt.IsZero() {
		t.Fatalf("complete = %+v, %v", done, err)
	}
	got, _ := repo.Get(ctx, f.account, j.ID)
	if string(got.Output) != `{"ok": true}` || got.HeadSHA != "abc" || got.CostUSD != 1.25 || got.Usage.InputTokens != 10 || !got.LeaseExpiresAt.IsZero() {
		t.Fatalf("stored = %+v", got)
	}
	_, err = repo.Complete(ctx, f.account, j.ID, worker, res)
	wantErr(t, err, store.ErrLeaseLost)
	_, err = repo.Cancel(ctx, f.account, j.ID)
	wantErr(t, err, domain.ErrInvalidTransition)
}

func TestJobReleaseAndCancel(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	worker := f.newWorker(t, "w1").ID
	labels := []string{"runtime:claude-code"}
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "dev", Runtime: "claude-code", MaxAttempts: 2}
	must(t, repo.Create(ctx, &j))

	for attempt, want := range []domain.JobStatus{domain.JobQueued, domain.JobFailed} {
		_, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
		must(t, err)
		_, err = repo.Release(ctx, f.account, j.ID, domain.NewID(), "nope")
		wantErr(t, err, store.ErrLeaseLost)
		rel, err := repo.Release(ctx, f.account, j.ID, worker, "disk full")
		if err != nil || rel.Status != want || rel.Attempts != attempt+1 || rel.WorkerID != uuid.Nil() {
			t.Fatalf("release %d = %+v, %v", attempt, rel, err)
		}
		_, err = f.pool.Exec(ctx, `UPDATE jobs SET run_after = NULL`) // skip the backoff
		must(t, err)
	}
	got, _ := repo.Get(ctx, f.account, j.ID)
	if got.Error != "attempts exhausted: disk full" || got.FinishedAt.IsZero() {
		t.Fatalf("failed job = %+v", got)
	}

	// Cancel works on queued and on running jobs.
	queued := f.newJob(t)
	c, err := repo.Cancel(ctx, f.account, queued.ID)
	if err != nil || c.Status != domain.JobCanceled {
		t.Fatalf("cancel queued = %+v, %v", c, err)
	}
	running := f.newJob(t)
	_, err = repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
	must(t, err)
	c, err = repo.Cancel(ctx, f.account, running.ID)
	if err != nil || c.Status != domain.JobCanceled || c.WorkerID != uuid.Nil() {
		t.Fatalf("cancel running = %+v, %v", c, err)
	}
	ext, err := repo.ExtendLease(ctx, f.account, running.ID, worker, time.Minute)
	wantErr(t, err, store.ErrLeaseLost)
	if ext.Status != domain.JobCanceled {
		t.Fatalf("worker must see the cancel: %+v", ext)
	}
	_, err = repo.Cancel(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestRequeueExpired(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	worker := f.newWorker(t, "w1").ID
	labels := []string{"runtime:claude-code"}
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "dev", Runtime: "claude-code", MaxAttempts: 2}
	must(t, repo.Create(ctx, &j))
	fresh := f.newJob(t)

	// Nothing is expired yet.
	_, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Hour)
	must(t, err)
	if got, err := repo.RequeueExpired(ctx, f.account); err != nil || len(got) != 0 {
		t.Fatalf("requeue = %+v, %v", got, err)
	}

	// First expiry: back to queued with attempts+1 and no backoff.
	_, err = f.pool.Exec(ctx, `UPDATE jobs SET run_after = now() + interval '1 hour', lease_expires_at = now() - interval '1 second' WHERE id = $1`, j.ID)
	must(t, err)
	got, err := repo.RequeueExpired(ctx, f.account)
	if err != nil || len(got) != 1 || got[0].ID != j.ID || got[0].Status != domain.JobQueued || got[0].Attempts != 1 || got[0].WorkerID != uuid.Nil() || !got[0].RunAfter.IsZero() {
		t.Fatalf("first requeue = %+v, %v", got, err)
	}

	// Second expiry reaches the attempt limit: failed.
	again, err := repo.LeaseNext(ctx, f.account, worker, labels, -time.Second)
	if err != nil || again.ID != j.ID {
		t.Fatalf("relase = %+v, %v", again, err)
	}
	got, err = repo.RequeueExpired(ctx, f.account)
	if err != nil || len(got) != 1 || got[0].Status != domain.JobFailed || got[0].Attempts != 2 || got[0].Error != "attempts exhausted: lease expired" {
		t.Fatalf("second requeue = %+v, %v", got, err)
	}
	if cur, _ := repo.Get(ctx, f.account, fresh.ID); cur.Status != domain.JobQueued {
		t.Fatalf("untouched job = %+v", cur)
	}
}

func TestJobEvents(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.JobEvents()
	j := f.newJob(t)

	events := []domain.JobEvent{
		{Type: domain.EventLog, Text: "one"},
		{Type: domain.EventToolCall, Data: []byte(`{"name": "bash"}`), Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	out, err := repo.Append(ctx, f.account, j.ID, events)
	if err != nil || len(out) != 2 || out[0].Seq != 1 || out[1].Seq != 2 || out[0].Time.IsZero() || out[1].Time.Year() != 2026 {
		t.Fatalf("append = %+v, %v", out, err)
	}
	more, err := repo.Append(ctx, f.account, j.ID, []domain.JobEvent{{Type: domain.EventError, Text: "three"}})
	if err != nil || more[0].Seq != 3 {
		t.Fatalf("second append = %+v, %v", more, err)
	}
	list, err := repo.List(ctx, f.account, j.ID, 1, 0)
	if err != nil || len(list) != 2 || list[0].Seq != 2 || string(list[0].Data) != `{"name": "bash"}` || list[1].Text != "three" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, j.ID, 0, 2)
	if len(list) != 2 {
		t.Fatalf("limit = %+v", list)
	}
	_, err = repo.Append(ctx, f.account, domain.NewID(), events)
	wantErr(t, err, store.ErrNotFound)
	_, err = repo.Append(ctx, f.account, j.ID, []domain.JobEvent{{Type: "bogus"}})
	if err == nil {
		t.Fatal("unknown event type accepted")
	}

	// Concurrent appends keep seq unique and gapless.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := repo.Append(ctx, f.account, j.ID, []domain.JobEvent{{Type: domain.EventLog}, {Type: domain.EventLog}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	all, _ := repo.List(ctx, f.account, j.ID, 0, 0)
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
	if len(all) != 3+16 {
		t.Fatalf("%d events", len(all))
	}
}

func TestLeaseNextRuntimeCase(t *testing.T) {
	f := newFixture(t)
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "dev", Runtime: "Claude-Code"}
	must(t, f.st.Jobs().Create(t.Context(), &j))
	w := f.newWorker(t, "w1")
	got, err := f.st.Jobs().LeaseNext(t.Context(), f.account, w.ID, []string{"Runtime:claude-code"}, time.Minute)
	if err != nil || got.ID != j.ID {
		t.Fatalf("lease = %+v, %v", got, err)
	}
}

func TestReleaseBacksOff(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	worker := f.newWorker(t, "w1").ID
	labels := []string{"runtime:claude-code"}
	j := f.newJob(t)
	_, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
	must(t, err)

	rel, err := repo.Release(ctx, f.account, j.ID, worker, "flaky")
	if d := time.Until(rel.RunAfter); err != nil || d < domain.RetryBackoff(1)-5*time.Second || d > domain.RetryBackoff(1) {
		t.Fatalf("run after in %v, %v", d, err)
	}
	_, err = repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
	wantErr(t, err, store.ErrNoJob)

	// Once run_after has passed, the job is leased again.
	_, err = f.pool.Exec(ctx, `UPDATE jobs SET run_after = now() - interval '1 second' WHERE id = $1`, j.ID)
	must(t, err)
	got, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Minute)
	if err != nil || got.ID != j.ID || got.Attempts != 1 {
		t.Fatalf("lease after backoff = %+v, %v", got, err)
	}
}

func TestRequeueExpiredWaitsForLockedRow(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	worker := f.newWorker(t, "w1").ID
	labels := []string{"runtime:claude-code"}
	var ids []uuid.UUID
	for range 3 {
		f.newJob(t)
		j, err := repo.LeaseNext(ctx, f.account, worker, labels, time.Hour)
		must(t, err)
		ids = append(ids, j.ID)
	}
	// The first job's daemon died while a write on its row was still open
	// (an events insert); the other jobs of the worker keep heartbeating.
	_, err := f.pool.Exec(ctx, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, ids[0])
	must(t, err)
	_, err = repo.ExtendLease(ctx, f.account, ids[1], worker, time.Hour)
	must(t, err)
	tx, err := f.pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE`, ids[0])
	must(t, err)
	committed := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		committed <- tx.Commit(ctx)
	}()
	// The transaction is used by one goroutine at a time: the test only
	// touches it again (the deferred Rollback) after the commit returned.
	defer func() {
		if err := <-committed; err != nil {
			t.Errorf("commit: %v", err)
		}
	}()
	got, err := repo.RequeueExpired(ctx, f.account)
	if err != nil || len(got) != 1 || got[0].ID != ids[0] || got[0].Status != domain.JobQueued {
		t.Fatalf("requeue = %+v, %v; a locked expired lease must not be skipped", got, err)
	}
}

// TestJobMCPTokenLookup: the hash is set only on the lease it names and is
// found only while that lease runs; the gate origin round-trips.
func TestJobMCPTokenLookup(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jobs := f.st.Jobs()
	w := f.newWorker(t, "w", "runtime:claude-code")
	other := f.newWorker(t, "other", "runtime:claude-code")
	hash := make([]byte, 32)
	hash[0] = 7

	queued := f.newJob(t)
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, queued.ID, w.ID, 0, hash), store.ErrLeaseLost)
	j, err := jobs.LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, j.ID, other.ID, j.Attempts, hash), store.ErrLeaseLost)
	wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, j.ID, w.ID, j.Attempts+1, hash), store.ErrLeaseLost)
	must(t, jobs.SetJobMCPTokenHash(ctx, f.account, j.ID, w.ID, j.Attempts, hash))
	got, err := jobs.GetByJobMCPTokenHash(ctx, hash)
	if err != nil || got.ID != j.ID || got.WorkerID != w.ID {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	_, err = jobs.GetByJobMCPTokenHash(ctx, make([]byte, 32))
	wantErr(t, err, store.ErrNotFound)

	// Extending the lease keeps the token; releasing it kills the token.
	_, err = jobs.ExtendLease(ctx, f.account, j.ID, w.ID, time.Minute)
	must(t, err)
	_, err = jobs.GetByJobMCPTokenHash(ctx, hash)
	must(t, err)
	_, err = jobs.Release(ctx, f.account, j.ID, w.ID, "retry")
	must(t, err)
	_, err = jobs.GetByJobMCPTokenHash(ctx, hash)
	wantErr(t, err, store.ErrNotFound)

	// A running job whose lease expired is leased again, by the same worker
	// or another one: the token of the old lease stays dead, and a hash for
	// the old attempt is refused.
	for _, next := range []domain.Worker{w, other} {
		f.newJob(t)
		cur, err := jobs.LeaseNext(ctx, f.account, next.ID, []string{"runtime:claude-code"}, time.Minute)
		must(t, err)
		old := make([]byte, 32)
		old[0], old[1] = 9, byte(next.Name[0])
		must(t, jobs.SetJobMCPTokenHash(ctx, f.account, cur.ID, next.ID, cur.Attempts, old))
		must(t, exec(f.pool, `UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, cur.ID))
		expired, err := jobs.GetByJobMCPTokenHash(ctx, old) // the caller checks the expiry
		if err != nil || expired.LeaseExpiresAt.After(time.Now()) {
			t.Fatalf("expired lease = %+v, %v", expired, err)
		}
		requeued, err := jobs.RequeueExpired(ctx, f.account)
		if err != nil || len(requeued) != 1 {
			t.Fatalf("requeued = %+v, %v", requeued, err)
		}
		must(t, exec(f.pool, `UPDATE jobs SET run_after = NULL WHERE id = $1`, cur.ID))
		again, err := jobs.LeaseNext(ctx, f.account, next.ID, []string{"runtime:claude-code"}, time.Minute)
		must(t, err)
		if again.ID != cur.ID || again.Attempts != cur.Attempts+1 {
			t.Fatalf("leased again = %+v after %+v", again, cur)
		}
		_, err = jobs.GetByJobMCPTokenHash(ctx, old)
		wantErr(t, err, store.ErrNotFound)
		wantErr(t, jobs.SetJobMCPTokenHash(ctx, f.account, cur.ID, next.ID, cur.Attempts, hash), store.ErrLeaseLost)
		fresh := make([]byte, 32)
		fresh[0], fresh[1] = 8, byte(next.Name[0])
		must(t, jobs.SetJobMCPTokenHash(ctx, f.account, cur.ID, next.ID, again.Attempts, fresh))
		_, err = jobs.Release(ctx, f.account, cur.ID, next.ID, "next case")
		must(t, err)
	}

	gate := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Origin: domain.GateFromAskHuman,
		Title: "JOB-1 asks: which?", SubjectRepo: "acme/forecrew", JobID: j.ID}
	must(t, f.st.Gates().Create(ctx, &gate))
	if g, err := f.st.Gates().Get(ctx, f.account, gate.ID); err != nil || !g.AsksHuman() {
		t.Fatalf("ask_human gate = %+v, %v", g, err)
	}
	playbook := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "stuck", SubjectRepo: "acme/forecrew"}
	must(t, f.st.Gates().Create(ctx, &playbook))
	if g, err := f.st.Gates().Get(ctx, f.account, playbook.ID); err != nil || g.Origin != domain.GateFromPlaybook {
		t.Fatalf("playbook gate = %+v, %v", g, err)
	}
	bad := domain.Gate{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Origin: domain.GateFromAskHuman, Title: "x", SubjectRepo: "acme/forecrew"}
	if err := f.st.Gates().Create(ctx, &bad); err == nil {
		t.Fatal("ask_human gate without a job accepted")
	}
}

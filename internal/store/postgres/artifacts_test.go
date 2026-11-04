//go:build integration

package postgres

import (
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func (f *fixture) artifact(job uuid.UUID, name string) domain.Artifact {
	key := "jobs/" + f.account.String() + "/" + job.String() + "/" + name
	if job == uuid.Nil() {
		key = "reports/" + name + "-" + domain.NewID().String()
	}
	return domain.Artifact{AccountID: f.account, ProjectID: f.project.ID, JobID: job, Name: name, ContentType: "text/plain",
		Size: 5, SHA256: strings.Repeat("ab", 32), StorageKey: key}
}

func TestArtifacts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := f.newJob(t)

	b, a := f.artifact(job.ID, "b.txt"), f.artifact(job.ID, "a.txt")
	must(t, repo.Put(ctx, &b))
	must(t, repo.Put(ctx, &a))
	if b.ID == uuid.Nil() || b.CreatedAt.IsZero() {
		t.Fatalf("Put did not fill %+v", b)
	}
	got, err := repo.Get(ctx, f.account, a.ID)
	if err != nil || got.Name != "a.txt" || got.JobID != job.ID || got.StorageKey != a.StorageKey || got.Size != 5 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := repo.ListByJob(ctx, f.account, job.ID)
	if err != nil || len(list) != 2 || list[0].Name != "a.txt" || list[1].Name != "b.txt" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// A re-upload replaces the row and keeps its ID.
	again := f.artifact(job.ID, "a.txt")
	again.Size, again.ContentType = 9, "application/json"
	must(t, repo.Put(ctx, &again))
	if again.ID != a.ID {
		t.Fatalf("re-upload changed the ID: %s, was %s", again.ID, a.ID)
	}
	got, _ = repo.Get(ctx, f.account, a.ID)
	if got.Size != 9 || got.ContentType != "application/json" {
		t.Fatalf("after re-upload = %+v", got)
	}
	if list, _ := repo.ListByJob(ctx, f.account, job.ID); len(list) != 2 {
		t.Fatalf("re-upload added a row: %+v", list)
	}

	// Another account sees nothing and cannot attach to this project.
	other := f.account
	other[0] ^= 0xff
	_, err = repo.Get(ctx, other, a.ID)
	wantErr(t, err, store.ErrNotFound)
	foreign := f.artifact(job.ID, "x.txt")
	foreign.AccountID = other
	wantErr(t, repo.Put(ctx, &foreign), store.ErrNotFound)

	// Artifacts without a job: always new rows, unique storage key.
	r1, r2 := f.artifact(uuid.Nil(), "report.md"), f.artifact(uuid.Nil(), "report.md")
	must(t, repo.Put(ctx, &r1))
	must(t, repo.Put(ctx, &r2))
	dup := f.artifact(uuid.Nil(), "other.md")
	dup.StorageKey = r1.StorageKey
	wantErr(t, repo.Put(ctx, &dup), store.ErrConflict)
}

func TestArtifactsOlderThanAndDelete(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := f.newJob(t)
	old, fresh := f.artifact(job.ID, "old.txt"), f.artifact(job.ID, "fresh.txt")
	must(t, repo.Put(ctx, &old))
	must(t, repo.Put(ctx, &fresh))
	if _, err := f.pool.Exec(ctx, `UPDATE artifacts SET created_at = now() - interval '10 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	got, err := repo.ListOlderThan(ctx, f.account, cutoff, 10)
	if err != nil || len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("older = %+v, %v", got, err)
	}
	if got, _ := repo.ListOlderThan(ctx, f.account, time.Now().Add(time.Hour), 1); len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("limit 1 = %+v", got)
	}

	// A verification report keeps its data when its artifact is deleted.
	ch := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 1, Branch: "b", BaseRef: "main", HeadSHA: "s", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &ch))
	if _, err := f.pool.Exec(ctx, `INSERT INTO verification_reports (account_id, project_id, change_id, sha, status, report, artifact_id)
		VALUES ($1, $2, $3, 's', 'passed', '{}', $4)`, f.account, f.project.ID, ch.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	must(t, repo.Delete(ctx, f.account, old.ID))
	var linked *uuid.UUID
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT artifact_id, status FROM verification_reports WHERE change_id = $1`, ch.ID).Scan(&linked, &status); err != nil {
		t.Fatal(err)
	}
	if linked != nil || status != "passed" {
		t.Fatalf("report after delete: artifact %v, status %s", linked, status)
	}
	_, err = repo.Get(ctx, f.account, old.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, repo.Delete(ctx, f.account, old.ID), store.ErrNotFound)
}

func TestArtifactsPutLeased(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := f.newJob(t)
	w, other := f.newWorker(t, "box"), f.newWorker(t, "other")

	// Queued: nobody holds a lease.
	a := f.artifact(job.ID, "a.txt")
	_, err := repo.PutLeased(ctx, &a, w.ID)
	wantErr(t, err, store.ErrLeaseLost)

	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)
	_, err = repo.PutLeased(ctx, &a, other.ID)
	wantErr(t, err, store.ErrLeaseLost)
	if rows, _ := repo.ListByJob(ctx, f.account, job.ID); len(rows) != 0 {
		t.Fatalf("refused writes left rows: %+v", rows)
	}

	replaced, err := repo.PutLeased(ctx, &a, w.ID)
	if err != nil || replaced != "" {
		t.Fatalf("first write: %q, %v", replaced, err)
	}
	again := f.artifact(job.ID, "a.txt")
	again.StorageKey += "~2"
	replaced, err = repo.PutLeased(ctx, &again, w.ID)
	if err != nil || replaced != a.StorageKey || again.ID != a.ID {
		t.Fatalf("replacement: %q, %v, id %s want %s", replaced, err, again.ID, a.ID)
	}

	// Once the job is over, the write is refused and changes nothing.
	_, err = f.st.Jobs().Complete(ctx, f.account, job.ID, w.ID, store.JobResult{Status: domain.JobSucceeded})
	must(t, err)
	late := f.artifact(job.ID, "late.txt")
	_, err = repo.PutLeased(ctx, &late, w.ID)
	wantErr(t, err, store.ErrLeaseLost)
	if rows, _ := repo.ListByJob(ctx, f.account, job.ID); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	missing := f.artifact(domain.NewID(), "x.txt")
	_, err = repo.PutLeased(ctx, &missing, w.ID)
	wantErr(t, err, store.ErrNotFound)
}

// A write that arrives while the job is locked by a transaction ending the
// lease waits for it and then sees the lease gone: check and write are one
// step, nothing slips in between.
func TestArtifactsPutLeasedWaitsForTheJobLock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	job := f.newJob(t)
	w := f.newWorker(t, "box")
	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"runtime:claude-code"}, time.Minute)
	must(t, err)

	tx, err := f.pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'canceled', worker_id = NULL, lease_expires_at = NULL, finished_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		a := f.artifact(job.ID, "late.txt")
		_, err := f.st.Artifacts().PutLeased(ctx, &a, w.ID)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("PutLeased did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, tx.Commit(ctx))
	wantErr(t, <-done, store.ErrLeaseLost)
	if rows, _ := f.st.Artifacts().ListByJob(ctx, f.account, job.ID); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestArtifactsExistingKeys(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Artifacts()
	job := f.newJob(t)
	a := f.artifact(job.ID, "a.txt")
	must(t, repo.Put(ctx, &a))
	got, err := repo.ExistingKeys(ctx, []string{a.StorageKey, "jobs/none"})
	if err != nil || len(got) != 1 || got[0] != a.StorageKey {
		t.Fatalf("ExistingKeys = %v, %v", got, err)
	}
	if got, err := repo.ExistingKeys(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("ExistingKeys(nil) = %v, %v", got, err)
	}
}

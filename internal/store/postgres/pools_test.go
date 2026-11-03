//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestTestSystemMigration applies 00003 on the MVP schema with data in it:
// existing jobs become agent jobs in pool build, and the migration rolls
// back cleanly.
func TestTestSystemMigration(t *testing.T) {
	pool := testkit.NewDatabase(t)
	ctx := t.Context()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS())
	must(t, err)
	_, err = p.UpTo(ctx, 2)
	must(t, err)
	var job, worker string
	must(t, pool.QueryRow(ctx, `WITH a AS (SELECT id FROM accounts LIMIT 1),
		pr AS (INSERT INTO projects (account_id, name, repo, status) SELECT id, 'p', 'o/p', 'registered' FROM a RETURNING id, account_id)
		INSERT INTO jobs (account_id, project_id, number, status, role, runtime, spec)
		SELECT account_id, id, 1, 'queued', 'developer', 'claude-code', '{}' FROM pr RETURNING id`).Scan(&job))
	must(t, pool.QueryRow(ctx, `INSERT INTO workers (account_id, name, token_hash)
		SELECT id, 'w', '\x01' FROM accounts LIMIT 1 RETURNING id`).Scan(&worker))

	_, err = p.UpTo(ctx, 3)
	must(t, err)
	var kind, jobPool, workerPool string
	must(t, pool.QueryRow(ctx, `SELECT kind, pool FROM jobs WHERE id = $1`, job).Scan(&kind, &jobPool))
	must(t, pool.QueryRow(ctx, `SELECT pool FROM workers WHERE id = $1`, worker).Scan(&workerPool))
	if kind != "agent" || jobPool != "build" || workerPool != "build" {
		t.Fatalf("after migration: kind %q, job pool %q, worker pool %q", kind, jobPool, workerPool)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET kind = 'script' WHERE id = $1`, job); err == nil {
		t.Fatal("unknown kind accepted")
	}
	_, err = p.DownTo(ctx, 2)
	must(t, err)
	_, err = p.Up(ctx)
	must(t, err)
}

// TestLeaseNextStaysInPool: a worker leases only jobs of its stored pool,
// whatever labels it announces.
func TestLeaseNextStaysInPool(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Jobs()
	build := f.newJob(t)
	if build.Kind != domain.JobKindAgent || build.Pool != domain.PoolBuild {
		t.Fatalf("defaults: kind %q, pool %q", build.Kind, build.Pool)
	}
	ctl := domain.Worker{AccountID: f.account, Name: "ctl", Pool: domain.PoolControl, TokenHash: []byte("hash-ctl")}
	must(t, f.st.Workers().Create(ctx, &ctl))
	builder := f.newWorker(t, "builder")
	if builder.Pool != domain.PoolBuild {
		t.Fatalf("worker pool = %q", builder.Pool)
	}
	all := []string{"runtime:claude-code", "runtime:codex", "docker", "linux", "pool:build", "build"}

	_, err := repo.LeaseNext(ctx, f.account, ctl.ID, all, time.Minute)
	wantErr(t, err, store.ErrNoJob)

	review := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Pool: domain.PoolControl, Role: "reviewer", Runtime: "codex",
		Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "review"}}
	must(t, repo.Create(ctx, &review))
	_, err = repo.LeaseNext(ctx, f.account, builder.ID, all, time.Minute)
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	if _, err := repo.LeaseNext(ctx, f.account, builder.ID, all, time.Minute); err == nil {
		t.Fatal("build worker leased the control job")
	}
	got, err := repo.LeaseNext(ctx, f.account, ctl.ID, all, time.Minute)
	if err != nil || got.ID != review.ID || got.Pool != domain.PoolControl {
		t.Fatalf("control lease = %+v, %v", got, err)
	}
	stored, err := f.st.Workers().Get(ctx, f.account, ctl.ID)
	if err != nil || stored.Pool != domain.PoolControl {
		t.Fatalf("stored worker = %+v, %v", stored, err)
	}
}

// TestLeaseNextPoolMatrix: a control worker never leases a build job and a
// build worker never leases a control job, whether the labels match or not,
// pool labels included.
func TestLeaseNextPoolMatrix(t *testing.T) {
	matching := []string{"runtime:claude-code", "linux"}
	tests := []struct {
		name, worker, job string
		labels            []string
		leased            bool
	}{
		{"control worker, build job, labels match", domain.PoolControl, domain.PoolBuild, matching, false},
		{"control worker, build job, labels differ", domain.PoolControl, domain.PoolBuild, []string{"gpu"}, false},
		{"control worker, build job, pool label", domain.PoolControl, domain.PoolBuild, append([]string{"pool:build"}, matching...), false},
		{"build worker, control job, labels match", domain.PoolBuild, domain.PoolControl, matching, false},
		{"build worker, control job, labels differ", domain.PoolBuild, domain.PoolControl, []string{"gpu"}, false},
		{"build worker, control job, pool label", domain.PoolBuild, domain.PoolControl, append([]string{"pool:control"}, matching...), false},
		{"control worker, control job", domain.PoolControl, domain.PoolControl, matching, true},
		{"control worker, control job, labels differ", domain.PoolControl, domain.PoolControl, []string{"gpu"}, false},
		{"build worker, build job", domain.PoolBuild, domain.PoolBuild, matching, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			w := domain.Worker{AccountID: f.account, Name: "w", Pool: tt.worker, TokenHash: []byte("hash-w")}
			must(t, f.st.Workers().Create(ctx, &w))
			j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Pool: tt.job, Role: "reviewer", Runtime: "claude-code",
				Needs: []string{"linux"}, Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "review"}}
			must(t, f.st.Jobs().Create(ctx, &j))
			got, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, tt.labels, time.Minute)
			if !tt.leased {
				wantErr(t, err, store.ErrNoJob)
				return
			}
			if err != nil || got.ID != j.ID {
				t.Fatalf("lease = %+v, %v", got, err)
			}
		})
	}
}

// TestSetPool: a moved worker leases from its new pool at the next lease;
// the job it runs stays leased to it.
func TestSetPool(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	labels := []string{"runtime:claude-code"}
	w := f.newWorker(t, "w")
	running := f.newJob(t)
	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, time.Minute)
	must(t, err)
	review := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Pool: domain.PoolControl, Role: "reviewer", Runtime: "claude-code",
		Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "review"}}
	must(t, f.st.Jobs().Create(ctx, &review))
	f.newJob(t) // a build job it must no longer lease

	must(t, f.st.Workers().SetPool(ctx, f.account, w.ID, domain.PoolControl))
	got, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, time.Minute)
	if err != nil || got.ID != review.ID {
		t.Fatalf("lease after move = %+v, %v", got, err)
	}
	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	if _, err := f.st.Jobs().ExtendLease(ctx, f.account, running.ID, w.ID, time.Minute); err != nil {
		t.Fatalf("running job after move: %v", err)
	}
	must(t, f.st.Workers().SetPool(ctx, f.account, w.ID, ""))
	if stored, _ := f.st.Workers().Get(ctx, f.account, w.ID); stored.Pool != domain.PoolBuild {
		t.Fatalf("pool = %q, want build", stored.Pool)
	}
	wantErr(t, f.st.Workers().SetPool(ctx, f.account, domain.NewID(), domain.PoolControl), store.ErrNotFound)
}

// TestLeaseNextCommandJob: a command job needs no runtime label.
func TestLeaseNextCommandJob(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	cmd := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Needs: []string{"docker"},
		Spec: domain.JobSpec{Kind: domain.JobKindCommand, Repo: "acme/forecrew", Command: &domain.CommandSpec{Shell: "make test"}}}
	must(t, f.st.Jobs().Create(ctx, &cmd))
	w := f.newWorker(t, "w")
	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"linux"}, time.Minute)
	wantErr(t, err, store.ErrNoJob)
	got, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, []string{"docker"}, time.Minute)
	if err != nil || got.ID != cmd.ID || got.Kind != domain.JobKindCommand || got.Spec.Command.Shell != "make test" {
		t.Fatalf("lease = %+v, %v", got, err)
	}
}

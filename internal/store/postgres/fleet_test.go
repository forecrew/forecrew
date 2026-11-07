//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// hash32 returns a fake SHA-256 for bytea columns.
func hash32(b byte) []byte {
	h := make([]byte, 32)
	h[0] = b
	return h
}

// TestFleetMigration applies 00007 on the schema before it with data in it:
// static workers stay static with no machine row, jobs ask for nothing, and
// the migration rolls back cleanly.
func TestFleetMigration(t *testing.T) {
	pool := newPoolAt(t, 6)
	ctx := t.Context()
	var account, project, worker, job string
	must(t, pool.QueryRow(ctx, `SELECT id FROM accounts`).Scan(&account))
	must(t, pool.QueryRow(ctx, `INSERT INTO projects (account_id, name, repo, status) VALUES ($1, 'p', 'o/p', 'registered') RETURNING id`, account).Scan(&project))
	must(t, pool.QueryRow(ctx, `INSERT INTO workers (account_id, name, token_hash, pool, labels) VALUES ($1, 'box', '\x01', 'control', '{go}') RETURNING id`, account).Scan(&worker))
	must(t, pool.QueryRow(ctx, `INSERT INTO jobs (account_id, project_id, number, status, role, runtime, spec, pool)
		VALUES ($1, $2, 1, 'queued', 'architect', 'codex', '{}', 'control') RETURNING id`, account, project).Scan(&job))

	p := provider(t, pool)
	_, err := p.UpTo(ctx, 7)
	must(t, err)

	st := New(pool)
	// Read with SQL: the store reads columns of later migrations.
	var w domain.Worker
	must(t, pool.QueryRow(ctx, `SELECT id, account_id, pool, ephemeral FROM workers WHERE token_hash = '\x01'`).Scan(&w.ID, &w.AccountID, &w.Pool, &w.Ephemeral))
	if w.Ephemeral || w.Pool != "control" || w.ID.String() != worker {
		t.Fatalf("worker = %+v", w)
	}
	var machines int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM machines`).Scan(&machines))
	if machines != 0 {
		t.Fatalf("%d machines", machines)
	}
	leased, err := st.Jobs().LeaseNext(ctx, w.AccountID, w.ID, []string{"runtime:codex"}, domain.LeaseDuration)
	must(t, err)
	if leased.ID.String() != job || leased.Resources != (domain.Resources{}) {
		t.Fatalf("job = %+v", leased)
	}

	_, err = p.DownTo(ctx, 6)
	must(t, err)
	mustFail(t, pool, `SELECT ephemeral FROM workers`)
	_, err = p.Up(ctx)
	must(t, err)
}

func TestWorkerEphemeralAndJobResources(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	w := domain.Worker{AccountID: f.account, Name: "machine-1", TokenHash: hash32(1), Ephemeral: true}
	must(t, f.st.Workers().Create(ctx, &w))
	got, err := f.st.Workers().Get(ctx, f.account, w.ID)
	must(t, err)
	if !got.Ephemeral {
		t.Fatal("not ephemeral")
	}
	if s := f.newWorker(t, "static"); s.Ephemeral {
		t.Fatal("a static worker is ephemeral")
	}
	want := domain.Resources{MinCPU: 4, MinMemoryMB: 8192, MinDiskGB: 50, GPU: 1}
	j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "codex", Resources: want,
		Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "go"}}
	must(t, f.st.Jobs().Create(ctx, &j))
	stored, err := f.st.Jobs().Get(ctx, f.account, j.ID)
	must(t, err)
	if stored.Resources != want {
		t.Fatalf("resources = %+v", stored.Resources)
	}
	var raw []byte
	must(t, f.pool.QueryRow(ctx, `SELECT resources FROM jobs WHERE id = $1`, j.ID).Scan(&raw))
	var round domain.Resources
	must(t, json.Unmarshal(raw, &round))
	if round != want {
		t.Fatalf("column = %s", raw)
	}
}

// fleetRows inserts a provider and a shape of account (nil: instance-wide).
func fleetRows(t *testing.T, pool *pgxpool.Pool, name string, account any) (providerID, shapeID string) {
	t.Helper()
	ctx := t.Context()
	must(t, pool.QueryRow(ctx, `INSERT INTO capacity_providers (account_id, name, kind) VALUES ($1, $2, 'fake') RETURNING id`, account, name).Scan(&providerID))
	must(t, pool.QueryRow(ctx, `INSERT INTO shapes (account_id, provider_id, name, os, arch, cpu, memory_mb, disk_gb, image)
		VALUES ($1, $2, $3, 'linux', 'amd64', 2, 4096, 20, 'forecrew-linux') RETURNING id`, account, providerID, name).Scan(&shapeID))
	return providerID, shapeID
}

// secondOrg returns another organization with an ephemeral worker and a
// queued job.
func secondOrg(t *testing.T, f *fixture) (account uuid.UUID, worker domain.Worker, job domain.Job) {
	t.Helper()
	ctx := t.Context()
	a := domain.Account{Name: "Other", Slug: "other"}
	must(t, f.st.Accounts().Create(ctx, &a))
	p := domain.Project{AccountID: a.ID, Name: "o", Repo: "other/o", Status: domain.ProjectRegistered}
	must(t, f.st.Projects().Create(ctx, &p))
	worker = domain.Worker{AccountID: a.ID, Name: "machine-b", TokenHash: hash32(9), Ephemeral: true}
	must(t, f.st.Workers().Create(ctx, &worker))
	job = domain.Job{AccountID: a.ID, ProjectID: p.ID, Role: "developer", Runtime: "codex", Spec: domain.JobSpec{Repo: "other/o", Prompt: "go"}}
	must(t, f.st.Jobs().Create(ctx, &job))
	return a.ID, worker, job
}

// TestFleetScoping: the schema itself keeps organizations apart, whatever
// the code above it does.
func TestFleetScoping(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pool := f.pool
	orgB, workerB, jobB := secondOrg(t, f)
	provA, shapeA := fleetRows(t, pool, "a", f.account)
	provB, shapeB := fleetRows(t, pool, "b", orgB)
	provI, shapeI := fleetRows(t, pool, "cloud", nil)

	// A shape has its provider's organization.
	insertShape := `INSERT INTO shapes (account_id, provider_id, name, os, arch, cpu, memory_mb, disk_gb, image) VALUES ($1, $2, 'x', 'linux', 'amd64', 1, 1, 1, 'i')`
	mustFail(t, pool, insertShape, f.account, provB)
	mustFail(t, pool, insertShape, f.account, provI)
	mustFail(t, pool, insertShape, nil, provA)
	mustFail(t, pool, `UPDATE shapes SET provider_id = $1 WHERE id = $2`, provB, shapeA)
	// Neither changes organization.
	mustFail(t, pool, `UPDATE capacity_providers SET account_id = $1 WHERE id = $2`, orgB, provA)
	mustFail(t, pool, `UPDATE capacity_providers SET account_id = $1 WHERE id = $2`, f.account, provI)

	insertMachine := `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at) VALUES ($1, $2, $3, now()) RETURNING id`
	// A machine runs a shape of its provider, of its organization or
	// instance-wide.
	mustFail(t, pool, insertMachine, f.account, provB, shapeA)
	mustFail(t, pool, insertMachine, f.account, provB, shapeB)
	mustFail(t, pool, `INSERT INTO machines (provider_id, shape_id) VALUES ($1, $2)`, provA, shapeA)

	// A warm machine is unassigned; once assigned it stays so.
	var warm string
	must(t, pool.QueryRow(ctx, `INSERT INTO machines (provider_id, shape_id) VALUES ($1, $2) RETURNING id`, provI, shapeI).Scan(&warm))
	mustFail(t, pool, `UPDATE machines SET worker_id = $1 WHERE id = $2`, workerB.ID, warm)
	mustFail(t, pool, `UPDATE machines SET job_id = $1 WHERE id = $2`, jobB.ID, warm)
	mustFail(t, pool, `UPDATE machines SET account_id = $1 WHERE id = $2`, f.account, warm) // no assigned_at
	must(t, exec(pool, `UPDATE machines SET account_id = $1, assigned_at = now() WHERE id = $2`, f.account, warm))
	mustFail(t, pool, `UPDATE machines SET account_id = $1 WHERE id = $2`, orgB, warm)
	mustFail(t, pool, `UPDATE machines SET account_id = NULL, assigned_at = NULL WHERE id = $1`, warm)

	// Its worker and job are of its organization, and the worker ephemeral.
	mustFail(t, pool, `UPDATE machines SET worker_id = $1 WHERE id = $2`, workerB.ID, warm)
	mustFail(t, pool, `UPDATE machines SET job_id = $1 WHERE id = $2`, jobB.ID, warm)
	static := f.newWorker(t, "static")
	mustFail(t, pool, `UPDATE machines SET worker_id = $1 WHERE id = $2`, static.ID, warm)
	mine := domain.Worker{AccountID: f.account, Name: "machine-a", TokenHash: hash32(8), Ephemeral: true}
	must(t, f.st.Workers().Create(ctx, &mine))
	must(t, exec(pool, `UPDATE machines SET worker_id = $1, job_id = $2 WHERE id = $3`, mine.ID, f.newJob(t).ID, warm))
	// A join code has its provider's organization.
	join := `INSERT INTO provider_join_codes (account_id, code_hash, claim_hash, provider_id, expires_at) VALUES ($1, $2, $3, $4, now())`
	mustFail(t, pool, join, f.account, hash32(11), hash32(12), provB)
	mustFail(t, pool, join, f.account, hash32(11), hash32(12), provI)
	mustFail(t, pool, join, nil, hash32(11), hash32(12), provA)
	must(t, exec(pool, join, f.account, hash32(11), hash32(12), provA))
	must(t, exec(pool, join, nil, hash32(13), hash32(14), provI))
	mustFail(t, pool, `UPDATE provider_join_codes SET provider_id = $1 WHERE code_hash = $2`, provB, hash32(11))

	// A usage record has its machine's organization and shape.
	var used string
	must(t, pool.QueryRow(ctx, insertMachine, f.account, provI, shapeI).Scan(&used))
	usage := `INSERT INTO usage_records (account_id, machine_id, shape_id, ownership, minutes, cost, started_at, ended_at) VALUES ($1, $2, $3, 'hosted', 1, 0, now(), now())`
	mustFail(t, pool, usage, orgB, used, shapeI)
	mustFail(t, pool, usage, f.account, used, shapeA)
	must(t, exec(pool, usage, f.account, used, shapeI))
	mustFail(t, pool, `UPDATE usage_records SET account_id = $1`, orgB)
	mustFail(t, pool, `UPDATE machines SET shape_id = $1 WHERE id = $2`, shapeA, used)

	// One worker, one machine.
	var other string
	must(t, pool.QueryRow(ctx, insertMachine, f.account, provA, shapeA).Scan(&other))
	mustFail(t, pool, `UPDATE machines SET worker_id = $1 WHERE id = $2`, mine.ID, other)
	// One external ID per provider.
	must(t, exec(pool, `UPDATE machines SET external_id = 'vm-1' WHERE id = $1`, warm))
	must(t, exec(pool, `UPDATE machines SET external_id = 'vm-1' WHERE id = $1`, other)) // another provider
	var twin string
	must(t, pool.QueryRow(ctx, insertMachine, f.account, provA, shapeA).Scan(&twin))
	mustFail(t, pool, `UPDATE machines SET external_id = 'vm-1' WHERE id = $1`, twin)

	// jobs_taken starts at 0 and cannot go below it.
	var taken int
	must(t, pool.QueryRow(ctx, `SELECT jobs_taken FROM machines WHERE id = $1`, twin).Scan(&taken))
	if taken != 0 {
		t.Fatalf("jobs_taken = %d", taken)
	}
	mustFail(t, pool, `UPDATE machines SET jobs_taken = -1 WHERE id = $1`, twin)

	// destroyed_at goes with the state.
	mustFail(t, pool, `UPDATE machines SET state = 'destroyed' WHERE id = $1`, twin)
	must(t, exec(pool, `UPDATE machines SET state = 'destroyed', destroyed_at = now() WHERE id = $1`, twin))
	mustFail(t, pool, `UPDATE machines SET state = 'gone' WHERE id = $1`, other)
}

// An ephemeral worker of one organization, a machine's, never leases a job
// of another, whatever labels it announces.
func TestMachineLeasesOnlyItsOrganization(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	orgB, workerB, jobB := secondOrg(t, f)
	mine := f.newJob(t)
	labels := []string{"runtime:claude-code", "runtime:codex"}
	prov, shape := fleetRows(t, f.pool, "b", orgB)
	must(t, exec(f.pool, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at, state, worker_id) VALUES ($1, $2, $3, now(), 'ready', $4)`,
		orgB, prov, shape, workerB.ID))

	_, err := f.st.Jobs().LeaseNext(ctx, f.account, workerB.ID, labels, domain.LeaseDuration)
	wantErr(t, err, store.ErrNoJob)
	got, err := f.st.Jobs().LeaseNext(ctx, orgB, workerB.ID, labels, domain.LeaseDuration)
	must(t, err)
	if got.ID != jobB.ID {
		t.Fatalf("leased %s", got.ID)
	}
	if j, _ := f.st.Jobs().Get(ctx, f.account, mine.ID); j.Status != domain.JobQueued {
		t.Fatalf("job of the other organization = %s", j.Status)
	}
}

// The store contract, on the same suite as the fake.
func TestFleetStore(t *testing.T) {
	f := newFixture(t)
	testkit.FleetStoreSuite(t, f.st, f.account, f.project.ID)
}

// A machine's worker leases only what its shape fits, while the machine can
// still take jobs, and one with no machine leases nothing.
func TestLeaseNextEphemeral(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	prov, shape := fleetRows(t, f.pool, "a", f.account) // 2 CPU, 4096 MB, 20 GB
	w := domain.Worker{AccountID: f.account, Name: "machine-a", TokenHash: hash32(1), Ephemeral: true}
	must(t, f.st.Workers().Create(ctx, &w))
	labels := []string{"runtime:codex"}
	job := func(r domain.Resources) domain.Job {
		j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "codex", Resources: r, Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "go"}}
		must(t, f.st.Jobs().Create(ctx, &j))
		return j
	}
	big := job(domain.Resources{MinMemoryMB: 8192})
	_ = job(domain.Resources{GPU: 1})
	fits := job(domain.Resources{MinCPU: 2, MinDiskGB: 20})

	_, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, domain.LeaseDuration)
	wantErr(t, err, store.ErrNoJob) // no machine

	var machine string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at, worker_id) VALUES ($1, $2, $3, now(), $4) RETURNING id`,
		f.account, prov, shape, w.ID).Scan(&machine))
	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, domain.LeaseDuration)
	wantErr(t, err, store.ErrNoJob) // requested

	must(t, exec(f.pool, `UPDATE machines SET state = 'ready' WHERE id = $1`, machine))
	got, err := f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, domain.LeaseDuration)
	must(t, err)
	if got.ID != fits.ID {
		t.Fatalf("leased %s, not the job that fits; the big one is %s", got.Ref(), big.Ref())
	}
	must(t, exec(f.pool, `UPDATE shapes SET memory_mb = 8192 WHERE id = $1`, shape))
	must(t, exec(f.pool, `UPDATE machines SET jobs_taken = 1 WHERE id = $1`, machine))
	_, err = f.st.Jobs().LeaseNext(ctx, f.account, w.ID, labels, domain.LeaseDuration)
	wantErr(t, err, store.ErrNoJob) // max_jobs reached

	// A static worker reports no size, so resources do not restrict it.
	static := f.newWorker(t, "static")
	got, err = f.st.Jobs().LeaseNext(ctx, f.account, static.ID, labels, domain.LeaseDuration)
	must(t, err)
	if got.ID != big.ID {
		t.Fatalf("static worker leased %s", got.Ref())
	}
}

// readyMachine returns a ready machine of a new shape with its worker, and
// the jobs for it.
func readyMachine(t *testing.T, f *fixture, name string, maxJobs, jobs int) (string, domain.Worker) {
	t.Helper()
	ctx := t.Context()
	prov, shape := fleetRows(t, f.pool, name, f.account)
	must(t, exec(f.pool, `UPDATE shapes SET max_jobs = $1 WHERE id = $2`, maxJobs, shape))
	w := domain.Worker{AccountID: f.account, Name: "machine-" + name, TokenHash: mcp.HashToken(name), Ephemeral: true}
	must(t, f.st.Workers().Create(ctx, &w))
	var machine string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at, state, worker_id) VALUES ($1, $2, $3, now(), 'ready', $4) RETURNING id`,
		f.account, prov, shape, w.ID).Scan(&machine))
	for range jobs {
		j := domain.Job{AccountID: f.account, ProjectID: f.project.ID, Role: "developer", Runtime: "codex",
			Spec: domain.JobSpec{Repo: "acme/forecrew", Prompt: "go", Timeout: domain.Duration(time.Hour)}}
		must(t, f.st.Jobs().Create(ctx, &j))
	}
	return machine, w
}

// lease is a poll of an ephemeral worker: the real lease and its record on the
// machine in one transaction. It reports whether the job was leased.
func lease(ctx context.Context, st store.Store, w domain.Worker) (bool, error) {
	err := st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		j, err := tx.Jobs().LeaseNext(ctx, w.AccountID, w.ID, []string{"runtime:codex"}, domain.LeaseDuration)
		if err != nil {
			return err
		}
		return fleet.RecordLease(ctx, tx, w, j)
	})
	if errors.Is(err, store.ErrNoJob) || errors.Is(err, store.ErrConflict) {
		return false, nil
	}
	return err == nil, err
}

// Of concurrent polls through the real LeaseNext and RecordLease only
// max_jobs win, and the losers leave their jobs queued.
func TestConcurrentLeases(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	machine, w := readyMachine(t, f, "a", 2, 8)
	var won atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := lease(ctx, f.st, w)
			if err != nil {
				t.Error(err)
			}
			if ok {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	var taken int
	var state string
	must(t, f.pool.QueryRow(ctx, `SELECT jobs_taken, state FROM machines WHERE id = $1`, machine).Scan(&taken, &state))
	running, err := f.st.Jobs().List(ctx, f.account, store.JobFilter{Status: domain.JobRunning})
	must(t, err)
	if won.Load() != 2 || taken != 2 || state != "busy" || len(running) != 2 {
		t.Fatalf("%d won, jobs_taken %d, %s, %d running", won.Load(), taken, state, len(running))
	}
}

// A lease racing Retire: whichever wins, a machine never drains under a
// running job.
func TestRetireRacesLease(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for i := range 30 {
		id, w := readyMachine(t, f, fmt.Sprintf("r%d", i), 1, 1)
		machine := uuid.MustParse(id)
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := lease(ctx, f.st, w); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if err := f.st.Machines().Retire(ctx, machine, domain.MachineReady, 0); err != nil && !errors.Is(err, store.ErrConflict) {
				t.Error(err)
			}
		})
		wg.Wait()
		m, err := f.st.Machines().Get(ctx, machine)
		must(t, err)
		running, err := f.st.Jobs().List(ctx, f.account, store.JobFilter{Status: domain.JobRunning, WorkerID: w.ID})
		must(t, err)
		if (m.State == domain.MachineDraining) == (len(running) == 1) {
			t.Fatalf("round %d: machine %s with %d running jobs", i, m.State, len(running))
		}
		for _, j := range running {
			_, err := f.st.Jobs().Complete(ctx, f.account, j.ID, w.ID, store.JobResult{Status: domain.JobSucceeded})
			must(t, err)
		}
		queued, err := f.st.Jobs().List(ctx, f.account, store.JobFilter{Status: domain.JobQueued})
		must(t, err)
		for _, j := range queued {
			_, err := f.st.Jobs().Cancel(ctx, f.account, j.ID)
			must(t, err)
		}
	}
}

func TestBootstrapTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pool := f.pool
	orgB, _, _ := secondOrg(t, f)
	provI, shapeI := fleetRows(t, pool, "cloud", nil)
	var warm string
	must(t, pool.QueryRow(ctx, `INSERT INTO machines (provider_id, shape_id) VALUES ($1, $2) RETURNING id`, provI, shapeI).Scan(&warm))

	// An unassigned machine's token has neither organization nor expiry.
	must(t, exec(pool, `INSERT INTO bootstrap_tokens (machine_id, token_hash) VALUES ($1, $2)`, warm, hash32(1)))
	mustFail(t, pool, `UPDATE bootstrap_tokens SET account_id = $1 WHERE machine_id = $2`, f.account, warm) // no expiry
	mustFail(t, pool, `UPDATE bootstrap_tokens SET used_at = now() WHERE machine_id = $1`, warm)
	mustFail(t, pool, `UPDATE bootstrap_tokens SET account_id = $1, expires_at = now() WHERE machine_id = $2`, f.account, warm) // machine unassigned

	// An assigned machine's token has exactly its organization: not
	// another, and not none.
	var assigned string
	must(t, pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at) VALUES ($1, $2, $3, now()) RETURNING id`, f.account, provI, shapeI).Scan(&assigned))
	mustFail(t, pool, `INSERT INTO bootstrap_tokens (machine_id, token_hash) VALUES ($1, $2)`, assigned, hash32(3))
	mustFail(t, pool, `INSERT INTO bootstrap_tokens (machine_id, account_id, token_hash, expires_at) VALUES ($1, $2, $3, now())`, assigned, orgB, hash32(3))
	must(t, exec(pool, `INSERT INTO bootstrap_tokens (machine_id, account_id, token_hash, expires_at) VALUES ($1, $2, $3, now())`, assigned, f.account, hash32(3)))

	must(t, exec(pool, `UPDATE machines SET account_id = $1, assigned_at = now() WHERE id = $2`, f.account, warm))
	mustFail(t, pool, `UPDATE bootstrap_tokens SET account_id = $1, expires_at = now() WHERE machine_id = $2`, orgB, warm)
	mustFail(t, pool, `UPDATE bootstrap_tokens SET token_hash = $1 WHERE machine_id = $2`, hash32(4), warm) // still unbound, machine is not
	must(t, exec(pool, `UPDATE bootstrap_tokens SET account_id = $1, expires_at = now() + interval '5 minutes' WHERE machine_id = $2`, f.account, warm))
	must(t, exec(pool, `UPDATE bootstrap_tokens SET used_at = now() WHERE machine_id = $1`, warm))
	// One per machine, a hash is a hash.
	mustFail(t, pool, `INSERT INTO bootstrap_tokens (machine_id, token_hash) VALUES ($1, $2)`, warm, hash32(2))
	mustFail(t, pool, `UPDATE bootstrap_tokens SET token_hash = '\x01' WHERE machine_id = $1`, warm)
}

// takeJob is the update of store.MachineRepo.TakeJob's contract.
const takeJob = `UPDATE machines SET jobs_taken = jobs_taken + 1
	WHERE id = $1 AND jobs_taken < (SELECT max_jobs FROM shapes WHERE id = machines.shape_id)`

// TestTakeJobLimit: of concurrent leases only max_jobs win, and the schema
// refuses going past it whatever the query, without trapping a machine whose
// shape's max_jobs was lowered later.
func TestTakeJobLimit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pool := f.pool
	prov, shape := fleetRows(t, pool, "a", f.account)
	must(t, exec(pool, `UPDATE shapes SET max_jobs = 2 WHERE id = $1`, shape))
	var machine string
	must(t, pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at) VALUES ($1, $2, $3, now()) RETURNING id`,
		f.account, prov, shape).Scan(&machine))

	var won atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			tag, err := pool.Exec(ctx, takeJob, machine)
			if err != nil {
				t.Error(err)
				return
			}
			won.Add(tag.RowsAffected())
		})
	}
	wg.Wait()
	var taken int
	must(t, pool.QueryRow(ctx, `SELECT jobs_taken FROM machines WHERE id = $1`, machine).Scan(&taken))
	if won.Load() != 2 || taken != 2 {
		t.Fatalf("%d leases won, jobs_taken = %d, want 2", won.Load(), taken)
	}

	mustFail(t, pool, `UPDATE machines SET jobs_taken = 3 WHERE id = $1`, machine)
	mustFail(t, pool, `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at, jobs_taken) VALUES ($1, $2, $3, now(), 3)`, f.account, prov, shape)

	// Lowering max_jobs stops new jobs and leaves the machine able to go.
	must(t, exec(pool, `UPDATE shapes SET max_jobs = 1 WHERE id = $1`, shape))
	mustFail(t, pool, `UPDATE machines SET jobs_taken = 3 WHERE id = $1`, machine)
	must(t, exec(pool, `UPDATE machines SET state = 'draining' WHERE id = $1`, machine))
	must(t, exec(pool, `UPDATE machines SET state = 'destroyed', destroyed_at = now() WHERE id = $1`, machine))
}

func TestFleetConstraints(t *testing.T) {
	f := newFixture(t)
	pool := f.pool
	prov, shape := fleetRows(t, pool, "a", f.account)

	// Names are unique within a scope, instance-wide included.
	mustFail(t, pool, `INSERT INTO capacity_providers (account_id, name, kind) VALUES ($1, 'a', 'fake')`, f.account)
	fleetRows(t, pool, "cloud", nil)
	mustFail(t, pool, `INSERT INTO capacity_providers (name, kind) VALUES ('cloud', 'fake')`)
	// Providers.
	mustFail(t, pool, `INSERT INTO capacity_providers (name, kind) VALUES ('Bad Name', 'fake')`)
	mustFail(t, pool, `INSERT INTO capacity_providers (name, kind) VALUES ('p', 'vsphere')`)
	mustFail(t, pool, `UPDATE capacity_providers SET status = 'active' WHERE id = $1`, prov) // agent without a token
	mustFail(t, pool, `UPDATE capacity_providers SET mode = 'direct', token_hash = $1 WHERE id = $2`, hash32(1), prov)
	mustFail(t, pool, `UPDATE capacity_providers SET token_hash = '\x01' WHERE id = $1`, prov)
	must(t, exec(pool, `UPDATE capacity_providers SET token_hash = $1, status = 'active' WHERE id = $2`, hash32(1), prov))
	must(t, exec(pool, `INSERT INTO capacity_providers (name, kind, mode, status) VALUES ('direct', 'fake', 'direct', 'active')`))
	// Shapes.
	for _, bad := range []string{`cpu = 0`, `memory_mb = 0`, `disk_gb = 0`, `gpu = -1`, `os = 'bsd'`, `arch = 'mips'`, `pool = 'actions'`,
		`price_per_minute = -1`, `monthly_budget = -1`, `max = 0`, `warm_min = 2`, `idle_seconds = -1`, `boot_timeout_seconds = 0`, `max_jobs = 0`} {
		mustFail(t, pool, `UPDATE shapes SET `+bad+` WHERE id = $1`, shape)
	}
	// Policy.
	mustFail(t, pool, `INSERT INTO fleet_policy (account_id, routing) VALUES ($1, 'anywhere')`, f.account)
	mustFail(t, pool, `INSERT INTO fleet_policy (account_id, plan) VALUES ($1, 'gold')`, f.account)
	must(t, exec(pool, `INSERT INTO fleet_policy (account_id) VALUES ($1)`, f.account))
	mustFail(t, pool, `INSERT INTO fleet_policy (account_id) VALUES ($1)`, f.account)
	var routing, plan string
	must(t, pool.QueryRow(t.Context(), `SELECT routing, plan FROM fleet_policy`).Scan(&routing, &plan))
	if want := domain.DefaultFleetPolicy(f.account); routing != string(want.Routing) || plan != string(want.Plan) {
		t.Fatalf("column defaults %s %s differ from domain.DefaultFleetPolicy", routing, plan)
	}
	// Images: one per name, version, arch and format in a scope.
	digest := "sha256:" + strings.Repeat("a", 64)
	image := `INSERT INTO images (name, version, os, arch, format, uri, digest, signature, built_at) VALUES ('l', '1', 'linux', 'amd64', 'rootfs', 'u', $1, 's', now())`
	must(t, exec(pool, image, digest))
	mustFail(t, pool, image, digest)
	mustFail(t, pool, `UPDATE images SET digest = 'md5:1'`)
	mustFail(t, pool, `UPDATE images SET format = 'iso'`)
	// Join codes: a code is confirmed only after a claim, used after both.
	join := `INSERT INTO provider_join_codes (account_id, code_hash, expires_at, provider_id, claim_hash, used_at) VALUES ($1, $2, now(), $3, $4, $5)`
	mustFail(t, pool, join, f.account, hash32(1), prov, nil, nil)
	mustFail(t, pool, join, f.account, hash32(1), nil, hash32(2), "2026-01-01")
	must(t, exec(pool, join, f.account, hash32(1), prov, hash32(2), "2026-01-01"))
	// Usage: one record per machine.
	var machine string
	must(t, pool.QueryRow(t.Context(), `INSERT INTO machines (account_id, provider_id, shape_id, assigned_at) VALUES ($1, $2, $3, now()) RETURNING id`, f.account, prov, shape).Scan(&machine))
	usage := `INSERT INTO usage_records (account_id, machine_id, shape_id, ownership, minutes, cost, started_at, ended_at) VALUES ($1, $2, $3, $4, $5, 0, now(), now() + interval '1 minute')`
	mustFail(t, pool, usage, f.account, machine, shape, "rented", 1)
	mustFail(t, pool, usage, f.account, machine, shape, "own", -1)
	must(t, exec(pool, usage, f.account, machine, shape, "own", 1))
	mustFail(t, pool, usage, f.account, machine, shape, "own", 1)
}

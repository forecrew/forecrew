package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

type jobRepo struct{ db dbtx }

var jobCols = `id, account_id, project_id, number, status, kind, pool, role, runtime, needs, resources, ` +
	cuuid("goal_id") + `, ` + cuuid("spec_id") + `, ` + cuuid("task_id") + `, spec, attempts, max_attempts, ` +
	cuuid("worker_id") + `, ` + ctime("run_after") + `, ` + ctime("lease_expires_at") + `, ` + ctime("started_at") + `, ` + ctime("finished_at") + `, output, ` +
	ctext("head_sha") + `, ` + ctext("error") + `, context_manifest, usage, cost_usd, created_at, updated_at`

func scanJob(row pgx.Row) (j domain.Job, err error) {
	var spec, usage, output, resources, manifest []byte
	err = row.Scan(&j.ID, &j.AccountID, &j.ProjectID, &j.Number, &j.Status, &j.Kind, &j.Pool, &j.Role, &j.Runtime, &j.Needs, &resources,
		&j.GoalID, &j.SpecID, &j.TaskID, &spec, &j.Attempts, &j.MaxAttempts,
		&j.WorkerID, &j.RunAfter, &j.LeaseExpiresAt, &j.StartedAt, &j.FinishedAt, &output,
		&j.HeadSHA, &j.Error, &manifest, &usage, &j.CostUSD, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return j, err
	}
	utc(&j.RunAfter, &j.LeaseExpiresAt, &j.StartedAt, &j.FinishedAt, &j.CreatedAt, &j.UpdatedAt)
	j.Output = output
	if err := json.Unmarshal(spec, &j.Spec); err != nil {
		return j, fmt.Errorf("decode job spec: %w", err)
	}
	if err := json.Unmarshal(usage, &j.Usage); err != nil {
		return j, fmt.Errorf("decode job usage: %w", err)
	}
	if err := json.Unmarshal(resources, &j.Resources); err != nil {
		return j, fmt.Errorf("decode job resources: %w", err)
	}
	if manifest != nil {
		if err := json.Unmarshal(manifest, &j.ContextManifest); err != nil {
			return j, fmt.Errorf("decode job context manifest: %w", err)
		}
	}
	return j, nil
}

func (r jobRepo) Create(ctx context.Context, j *domain.Job) error {
	j.ID = newID(j.ID)
	j.Status = domain.JobQueued
	j.Kind = j.Kind.OrDefault()
	j.Pool = domain.OrDefaultPool(j.Pool)
	j.Needs = domain.NormalizeLabels(j.Needs)
	if j.MaxAttempts == 0 {
		j.MaxAttempts = domain.DefaultMaxAttempts
	}
	spec, err := json.Marshal(j.Spec.Redacted())
	if err != nil {
		return fmt.Errorf("encode job spec: %w", err)
	}
	usage, err := json.Marshal(j.Usage)
	if err != nil {
		return fmt.Errorf("encode job usage: %w", err)
	}
	resources, err := json.Marshal(j.Resources)
	if err != nil {
		return fmt.Errorf("encode job resources: %w", err)
	}
	var manifest json.RawMessage
	if j.ContextManifest != nil {
		if manifest, err = json.Marshal(j.ContextManifest); err != nil {
			return fmt.Errorf("encode job context manifest: %w", err)
		}
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		err := owned(ctx, tx, j.AccountID, ref{"projects", j.ProjectID}, ref{"goals", j.GoalID}, ref{"specs", j.SpecID}, ref{"tasks", j.TaskID})
		if err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, j.AccountID, string(domain.RefJob))
		if err != nil {
			return mapErr(err)
		}
		j.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO jobs (id, account_id, project_id, number, status, role, runtime, needs, goal_id, spec_id, task_id,
				spec, attempts, max_attempts, usage, cost_usd, kind, pool, resources, context_manifest)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20) RETURNING created_at, updated_at`,
			j.ID, j.AccountID, j.ProjectID, j.Number, string(j.Status), j.Role, j.Runtime, j.Needs,
			opt(j.GoalID), opt(j.SpecID), opt(j.TaskID), spec, j.Attempts, j.MaxAttempts, usage, j.CostUSD, string(j.Kind), j.Pool, resources, optJSON(manifest)).Scan(&j.CreatedAt, &j.UpdatedAt))
	})
}

func (r jobRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Job, error) {
	return one(ctx, r.db, scanJob, `SELECT `+jobCols+` FROM jobs WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r jobRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Job, error) {
	return one(ctx, r.db, scanJob, `SELECT `+jobCols+` FROM jobs WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r jobRepo) List(ctx context.Context, accountID uuid.UUID, f store.JobFilter) ([]domain.Job, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	if f.InProjects != nil {
		w.and("project_id = ANY(?)", f.InProjects)
	}
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "task_id = ?", f.TaskID)
	andNot(w, "worker_id = ?", f.WorkerID)
	andNot(w, "role = ?", f.Role)
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanJob, `SELECT `+jobCols+` FROM jobs`+w.sql("created_at DESC, id DESC", f.Limit), w.args...)
}

// LeaseNext implements store.JobRepo (docs/contracts/data-model.md, "Leasing query").
func (r jobRepo) LeaseNext(ctx context.Context, accountID, workerID uuid.UUID, labels []string, leaseFor time.Duration) (domain.Job, error) {
	j, err := scanJob(r.db.QueryRow(ctx, `UPDATE jobs SET status = 'running', worker_id = $2, started_at = now(),
			lease_expires_at = now() + $4::interval, updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE account_id = $1 AND status = 'queued' AND (run_after IS NULL OR run_after <= now())
			  AND pool = (SELECT w.pool FROM workers w WHERE w.id = $2 AND w.account_id = $1)
			  AND (needs || CASE WHEN kind = 'agent' THEN ARRAY[lower('runtime:' || runtime)] ELSE '{}'::text[] END) <@ $3::text[]
			  AND (NOT (SELECT w.ephemeral FROM workers w WHERE w.id = $2 AND w.account_id = $1) OR EXISTS (
			    SELECT 1 FROM machines m JOIN shapes s ON s.id = m.shape_id
			    WHERE m.worker_id = $2 AND m.account_id = $1 AND m.state IN ('ready', 'busy') AND m.jobs_taken < s.max_jobs
			      AND s.cpu >= COALESCE((jobs.resources->>'min_cpu')::int, 0)
			      AND s.memory_mb >= COALESCE((jobs.resources->>'min_memory_mb')::int, 0)
			      AND s.disk_gb >= COALESCE((jobs.resources->>'min_disk_gb')::int, 0)
			      AND s.gpu >= COALESCE((jobs.resources->>'gpu')::int, 0)))
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+jobCols, accountID, workerID, domain.NormalizeLabels(labels), leaseFor))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, store.ErrNoJob
	}
	return j, mapErr(err)
}

func (r jobRepo) ExtendLease(ctx context.Context, accountID, jobID, workerID uuid.UUID, leaseFor time.Duration) (domain.Job, error) {
	j, err := one(ctx, r.db, scanJob, `UPDATE jobs SET lease_expires_at = now() + $4::interval, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND worker_id = $3 AND status = 'running'
		RETURNING `+jobCols, accountID, jobID, workerID, leaseFor)
	if !errors.Is(err, store.ErrNotFound) {
		return j, err
	}
	j, err = r.Get(ctx, accountID, jobID)
	if err != nil {
		return j, err
	}
	return j, fmt.Errorf("job %s: %w", j.Ref(), store.ErrLeaseLost)
}

// mutate loads a job FOR UPDATE, lets fn change it through the domain
// methods and writes the result, all in one transaction. fn gets the
// database clock.
func (r jobRepo) mutate(ctx context.Context, accountID, jobID uuid.UUID, fn func(j *domain.Job, now time.Time) error) (domain.Job, error) {
	var out domain.Job
	err := inTx(ctx, r.db, func(tx dbtx) error {
		j, err := one(ctx, tx, scanJob, `SELECT `+jobCols+` FROM jobs WHERE account_id = $1 AND id = $2 FOR UPDATE`, accountID, jobID)
		if err != nil {
			return err
		}
		out = j
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		if err := fn(&out, now); err != nil {
			return err
		}
		return writeJob(ctx, tx, &out)
	})
	return out, err
}

func dbNow(ctx context.Context, db dbtx) (time.Time, error) {
	var now time.Time
	err := db.QueryRow(ctx, `SELECT now()`).Scan(&now)
	return now.UTC(), err
}

// writeJob stores the fields that change after creation.
func writeJob(ctx context.Context, db dbtx, j *domain.Job) error {
	usage, err := json.Marshal(j.Usage)
	if err != nil {
		return fmt.Errorf("encode job usage: %w", err)
	}
	return mapErr(db.QueryRow(ctx, `UPDATE jobs SET status = $3, attempts = $4, worker_id = $5, lease_expires_at = $6, started_at = $7,
			finished_at = $8, output = $9, head_sha = $10, error = $11, usage = $12, cost_usd = $13, updated_at = $14, run_after = $15
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		j.AccountID, j.ID, string(j.Status), j.Attempts, opt(j.WorkerID), optTime(j.LeaseExpiresAt), optTime(j.StartedAt),
		optTime(j.FinishedAt), optJSON(j.Output), opt(j.HeadSHA), opt(j.Error), usage, j.CostUSD, j.UpdatedAt.UTC(), optTime(j.RunAfter)).Scan(&j.UpdatedAt))
}

// leaseHeldBy checks that j is running under workerID.
func leaseHeldBy(j *domain.Job, workerID uuid.UUID) error {
	if j.Status != domain.JobRunning || j.WorkerID != workerID {
		return fmt.Errorf("job %s is %s: %w", j.Ref(), j.Status, store.ErrLeaseLost)
	}
	return nil
}

func (r jobRepo) Complete(ctx context.Context, accountID, jobID, workerID uuid.UUID, res store.JobResult) (domain.Job, error) {
	return r.mutate(ctx, accountID, jobID, func(j *domain.Job, now time.Time) error {
		if err := leaseHeldBy(j, workerID); err != nil {
			return err
		}
		if res.Status != domain.JobSucceeded && res.Status != domain.JobFailed {
			return fmt.Errorf("result status %q: %w", res.Status, domain.ErrInvalidTransition)
		}
		if err := j.Finish(res.Status, now); err != nil {
			return err
		}
		j.Output, j.HeadSHA, j.Error, j.Usage, j.CostUSD = res.Output, res.HeadSHA, res.Error, res.Usage, res.CostUSD
		return nil
	})
}

func (r jobRepo) Release(ctx context.Context, accountID, jobID, workerID uuid.UUID, reason string) (domain.Job, error) {
	return r.mutate(ctx, accountID, jobID, func(j *domain.Job, now time.Time) error {
		if err := leaseHeldBy(j, workerID); err != nil {
			return err
		}
		return j.Release(reason, now)
	})
}

func (r jobRepo) Cancel(ctx context.Context, accountID, jobID uuid.UUID) (domain.Job, error) {
	return r.mutate(ctx, accountID, jobID, func(j *domain.Job, now time.Time) error {
		return j.Finish(domain.JobCanceled, now)
	})
}

func (r jobRepo) RequeueExpired(ctx context.Context, accountID uuid.UUID) ([]domain.Job, error) {
	out := []domain.Job{}
	err := inTx(ctx, r.db, func(tx dbtx) error {
		expired, err := many(ctx, tx, scanJob, `SELECT `+jobCols+` FROM jobs
			WHERE account_id = $1 AND status = 'running' AND lease_expires_at <= now()
			ORDER BY lease_expires_at, id FOR UPDATE`, accountID)
		if err != nil {
			return err
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		for _, j := range expired {
			if err := j.Requeue("lease expired", now); err != nil {
				return err
			}
			if err := writeJob(ctx, tx, &j); err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lockLeased locks the job row, the lock complete, cancel and the lease
// reaper take, so a write in the same transaction is ordered against them.
// With a non-nil workerID it also requires the job to be running under that
// worker, and fails with ErrLeaseLost otherwise.
func lockLeased(ctx context.Context, tx dbtx, accountID, jobID, workerID uuid.UUID) error {
	var status string
	var holder uuid.UUID
	err := tx.QueryRow(ctx, `SELECT status, `+cuuid("worker_id")+` FROM jobs WHERE account_id = $1 AND id = $2 FOR UPDATE`,
		accountID, jobID).Scan(&status, &holder)
	if err != nil {
		return mapErr(err)
	}
	if workerID != uuid.Nil() && (status != string(domain.JobRunning) || holder != workerID) {
		return fmt.Errorf("job %s is %s: %w", jobID, status, store.ErrLeaseLost)
	}
	return nil
}

func (r jobRepo) SetJobMCPTokenHash(ctx context.Context, accountID, jobID, workerID uuid.UUID, attempts int, hash []byte) error {
	err := mustUpdate(ctx, r.db, `UPDATE jobs SET job_mcp_token_hash = $5
		WHERE account_id = $1 AND id = $2 AND worker_id = $3 AND attempts = $4 AND status = 'running'`,
		accountID, jobID, workerID, attempts, hash)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("job %s: %w", jobID, store.ErrLeaseLost)
	}
	return err
}

func (r jobRepo) GetByJobMCPTokenHash(ctx context.Context, hash []byte) (domain.Job, error) {
	// The CHECK and the trigger keep the hash on running jobs of their
	// current lease only; the status test is belt and braces.
	return one(ctx, r.db, scanJob, `SELECT `+jobCols+` FROM jobs WHERE job_mcp_token_hash = $1 AND status = 'running'`, hash)
}

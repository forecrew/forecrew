package postgres

import (
	"context"
	"strconv"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ---- test runs

type testRunRepo struct{ db dbtx }

var testRunCols = `id, account_id, project_id, job_id, ` + cuuid("change_id") + `, sha, suite, environment, cell, status, counts,
	duration_ms, exit_code, versions, report_artifacts, attempt, created_at`

func scanTestRun(row pgx.Row) (r domain.TestRun, err error) {
	err = row.Scan(&r.ID, &r.AccountID, &r.ProjectID, &r.JobID, &r.ChangeID, &r.SHA, &r.Suite, &r.Environment, &r.Cell, &r.Status, &r.Counts,
		&r.DurationMS, &r.ExitCode, &r.Versions, &r.ReportArtifacts, &r.Attempt, &r.CreatedAt)
	utc(&r.CreatedAt)
	if r.Cell == nil {
		r.Cell = map[string]string{}
	}
	if r.Versions == nil {
		r.Versions = map[string]string{}
	}
	if r.ReportArtifacts == nil {
		r.ReportArtifacts = []string{}
	}
	return r, err
}

func (r testRunRepo) Create(ctx context.Context, run *domain.TestRun, results []domain.TestResult) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, run.AccountID, ref{"projects", run.ProjectID}, ref{"jobs", run.JobID}, ref{"changes", run.ChangeID}); err != nil {
			return err
		}
		id := newID(run.ID)
		cell, versions, artifacts := run.Cell, run.Versions, run.ReportArtifacts
		if cell == nil {
			cell = map[string]string{}
		}
		if versions == nil {
			versions = map[string]string{}
		}
		if artifacts == nil {
			artifacts = []string{}
		}
		err := tx.QueryRow(ctx, `INSERT INTO test_runs (id, account_id, project_id, job_id, change_id, sha, suite, environment, cell, status, counts,
				duration_ms, exit_code, versions, report_artifacts, attempt)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			RETURNING id, created_at`,
			id, run.AccountID, run.ProjectID, run.JobID, opt(run.ChangeID), run.SHA, run.Suite, run.Environment, cell, string(run.Status), run.Counts,
			run.DurationMS, run.ExitCode, versions, artifacts, run.Attempt).Scan(&run.ID, &run.CreatedAt)
		if err != nil {
			return mapErr(err)
		}
		utc(&run.CreatedAt)
		return insertResults(ctx, tx, run, results)
	})
}

// insertResults stores the results of run with one statement and writes the
// new IDs and the run's ID and account back.
func insertResults(ctx context.Context, tx dbtx, run *domain.TestRun, results []domain.TestResult) error {
	if len(results) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(results))
	names, classnames, statuses, messages := make([]string, len(results)), make([]string, len(results)), make([]string, len(results)), make([]string, len(results))
	durations := make([]int64, len(results))
	for i, res := range results {
		ids[i] = newID(res.ID)
		names[i], classnames[i], statuses[i], messages[i], durations[i] = res.Name, res.Classname, string(res.Status), res.Message, res.DurationMS
	}
	_, err := tx.Exec(ctx, `INSERT INTO test_results (id, account_id, run_id, name, classname, status, duration_ms, message)
		SELECT id, $2, $3, name, classname, status, duration_ms, message
		FROM unnest($1::uuid[], $4::text[], $5::text[], $6::text[], $7::bigint[], $8::text[]) AS t(id, name, classname, status, duration_ms, message)`,
		ids, run.AccountID, run.ID, names, classnames, statuses, durations, messages)
	if err != nil {
		return mapErr(err)
	}
	for i := range results {
		results[i].ID, results[i].AccountID, results[i].RunID = ids[i], run.AccountID, run.ID
	}
	return nil
}

func (r testRunRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.TestRun, error) {
	return one(ctx, r.db, scanTestRun, `SELECT `+testRunCols+` FROM test_runs WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r testRunRepo) List(ctx context.Context, accountID uuid.UUID, f store.TestRunFilter) ([]domain.TestRun, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "job_id = ?", f.JobID)
	andNot(w, "change_id = ?", f.ChangeID)
	andNot(w, "sha = ?", f.SHA)
	return many(ctx, r.db, scanTestRun, `SELECT `+testRunCols+` FROM test_runs`+w.sql("created_at, id", f.Limit), w.args...)
}

func scanTestResult(row pgx.Row) (t domain.TestResult, err error) {
	err = row.Scan(&t.ID, &t.AccountID, &t.RunID, &t.Name, &t.Classname, &t.Status, &t.DurationMS, &t.Message)
	return t, err
}

func (r testRunRepo) Results(ctx context.Context, accountID, runID uuid.UUID) ([]domain.TestResult, error) {
	return many(ctx, r.db, scanTestResult, `SELECT id, account_id, run_id, name, classname, status, duration_ms, message FROM test_results
		WHERE account_id = $1 AND run_id = $2 ORDER BY classname, name, id`, accountID, runID)
}

func (r testRunRepo) Failures(ctx context.Context, accountID, runID uuid.UUID, limit int) ([]domain.TestResult, error) {
	q := `SELECT id, account_id, run_id, name, classname, status, duration_ms, message FROM test_results
		WHERE account_id = $1 AND run_id = $2 AND status IN ('failed', 'errored') ORDER BY classname, name, id`
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	return many(ctx, r.db, scanTestResult, q, accountID, runID)
}

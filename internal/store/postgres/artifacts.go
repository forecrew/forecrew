package postgres

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
)

// ---- artifacts

type artifactRepo struct{ db dbtx }

var artifactCols = `id, account_id, project_id, ` + cuuid("job_id") + `, name, content_type, size, sha256, storage_key, created_at`

func scanArtifact(row pgx.Row) (a domain.Artifact, err error) {
	err = row.Scan(&a.ID, &a.AccountID, &a.ProjectID, &a.JobID, &a.Name, &a.ContentType, &a.Size, &a.SHA256, &a.StorageKey, &a.CreatedAt)
	utc(&a.CreatedAt)
	return a, err
}

// Put upserts on (job_id, name): a re-upload keeps the ID and replaces the
// rest. An artifact without a job is always a new row.
func (r artifactRepo) Put(ctx context.Context, a *domain.Artifact) error {
	if err := owned(ctx, r.db, a.AccountID, ref{"projects", a.ProjectID}, ref{"jobs", a.JobID}); err != nil {
		return err
	}
	id := newID(a.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO artifacts (id, account_id, project_id, job_id, name, content_type, size, sha256, storage_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (job_id, name) DO UPDATE SET content_type = EXCLUDED.content_type, size = EXCLUDED.size,
			sha256 = EXCLUDED.sha256, storage_key = EXCLUDED.storage_key, created_at = now()
		RETURNING id, created_at`,
		id, a.AccountID, a.ProjectID, opt(a.JobID), a.Name, a.ContentType, a.Size, a.SHA256, a.StorageKey).Scan(&a.ID, &a.CreatedAt))
}

func (r artifactRepo) PutLeased(ctx context.Context, a *domain.Artifact, workerID uuid.UUID) (string, error) {
	var replaced string
	err := inTx(ctx, r.db, func(tx dbtx) error {
		if err := lockLeased(ctx, tx, a.AccountID, a.JobID, workerID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT storage_key FROM artifacts WHERE account_id = $1 AND job_id = $2 AND name = $3`,
			a.AccountID, a.JobID, a.Name).Scan(&replaced)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return mapErr(err)
		}
		return artifactRepo{tx}.Put(ctx, a)
	})
	return replaced, err
}

func (r artifactRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Artifact, error) {
	return one(ctx, r.db, scanArtifact, `SELECT `+artifactCols+` FROM artifacts WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r artifactRepo) ListByJob(ctx context.Context, accountID, jobID uuid.UUID) ([]domain.Artifact, error) {
	return many(ctx, r.db, scanArtifact, `SELECT `+artifactCols+` FROM artifacts WHERE account_id = $1 AND job_id = $2 ORDER BY name`, accountID, jobID)
}

func (r artifactRepo) ListOlderThan(ctx context.Context, accountID uuid.UUID, t time.Time, limit int) ([]domain.Artifact, error) {
	w := newWhere(accountID).and("created_at < ?", t.UTC())
	return many(ctx, r.db, scanArtifact, `SELECT `+artifactCols+` FROM artifacts`+w.sql("created_at, id", limit), w.args...)
}

// Delete removes the row. A verification report that points at it keeps
// its data and loses the link (artifact_id becomes NULL).
func (r artifactRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		if _, err := tx.Exec(ctx, `UPDATE verification_reports SET artifact_id = NULL, updated_at = now() WHERE account_id = $1 AND artifact_id = $2`,
			accountID, id); err != nil {
			return mapErr(err)
		}
		return mustUpdate(ctx, tx, `DELETE FROM artifacts WHERE account_id = $1 AND id = $2`, accountID, id)
	})
}

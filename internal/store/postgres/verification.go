package postgres

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
)

// ---- verification reports

type verificationReportRepo struct{ db dbtx }

var verificationCols = `id, account_id, project_id, change_id, sha, status, report, ` + cuuid("artifact_id") + `, created_at, updated_at`

func scanVerification(row pgx.Row) (r domain.VerificationReport, err error) {
	err = row.Scan(&r.ID, &r.AccountID, &r.ProjectID, &r.ChangeID, &r.SHA, &r.Status, &r.Report, &r.ArtifactID, &r.CreatedAt, &r.UpdatedAt)
	utc(&r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (r verificationReportRepo) Upsert(ctx context.Context, v *domain.VerificationReport) error {
	if err := owned(ctx, r.db, v.AccountID, ref{"projects", v.ProjectID}, ref{"changes", v.ChangeID}, ref{"artifacts", v.ArtifactID}); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO verification_reports (id, account_id, project_id, change_id, sha, status, report, artifact_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (change_id, sha) DO UPDATE SET status = EXCLUDED.status, report = EXCLUDED.report,
			artifact_id = EXCLUDED.artifact_id, updated_at = now()
		RETURNING id, created_at, updated_at`,
		newID(v.ID), v.AccountID, v.ProjectID, v.ChangeID, v.SHA, string(v.Status), []byte(v.Report), opt(v.ArtifactID)).
		Scan(&v.ID, &v.CreatedAt, &v.UpdatedAt))
}

func (r verificationReportRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.VerificationReport, error) {
	return one(ctx, r.db, scanVerification, `SELECT `+verificationCols+` FROM verification_reports WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r verificationReportRepo) GetBySHA(ctx context.Context, accountID, changeID uuid.UUID, sha string) (domain.VerificationReport, error) {
	return one(ctx, r.db, scanVerification, `SELECT `+verificationCols+` FROM verification_reports
		WHERE account_id = $1 AND change_id = $2 AND sha = $3`, accountID, changeID, sha)
}

func (r verificationReportRepo) Latest(ctx context.Context, accountID, changeID uuid.UUID) (domain.VerificationReport, error) {
	return one(ctx, r.db, scanVerification, `SELECT `+verificationCols+` FROM verification_reports
		WHERE account_id = $1 AND change_id = $2 ORDER BY updated_at DESC, id DESC LIMIT 1`, accountID, changeID)
}

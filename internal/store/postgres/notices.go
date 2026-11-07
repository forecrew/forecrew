package postgres

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// FleetNotices implements store.Store.
func (s *Store) FleetNotices() store.FleetNoticeRepo { return noticeRepo{s.db} }

type noticeRepo struct{ db dbtx }

var noticeCols = `id, ` + cuuid("account_id") + `, machine_id, provider_id, ` + cuuid("job_id") + `, kind, detail, created_at, ` + ctime("resolved_at") + `, resolved_by`

func scanNotice(row pgx.Row) (n domain.FleetNotice, err error) {
	err = row.Scan(&n.ID, &n.AccountID, &n.MachineID, &n.ProviderID, &n.JobID, &n.Kind, &n.Detail, &n.CreatedAt, &n.ResolvedAt, &n.ResolvedBy)
	utc(&n.CreatedAt, &n.ResolvedAt)
	return n, err
}

func (r noticeRepo) Create(ctx context.Context, n *domain.FleetNotice) error {
	if err := n.Validate(); err != nil {
		return err
	}
	n.ID = newID(n.ID)
	err := r.db.QueryRow(ctx, `INSERT INTO fleet_notices (id, account_id, machine_id, provider_id, job_id, kind, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		n.ID, opt(n.AccountID), n.MachineID, n.ProviderID, opt(n.JobID), n.Kind, n.Detail).Scan(&n.CreatedAt)
	utc(&n.CreatedAt)
	return mapErr(err)
}

func (r noticeRepo) List(ctx context.Context, all bool, limit int) ([]domain.FleetNotice, error) {
	w := &where{conds: []string{"true"}}
	if !all {
		w.conds = append(w.conds, "resolved_at IS NULL")
	}
	return many(ctx, r.db, scanNotice, `SELECT `+noticeCols+` FROM fleet_notices`+w.sql("created_at DESC, id DESC", limit), w.args...)
}

func (r noticeRepo) Resolve(ctx context.Context, id uuid.UUID, by string) (domain.FleetNotice, error) {
	n, err := one(ctx, r.db, scanNotice, `UPDATE fleet_notices SET resolved_at = now(), resolved_by = $2
		WHERE id = $1 AND resolved_at IS NULL RETURNING `+noticeCols, id, by)
	if !errors.Is(err, store.ErrNotFound) {
		return n, err
	}
	if _, err := one(ctx, r.db, scanNotice, `SELECT `+noticeCols+` FROM fleet_notices WHERE id = $1`, id); err != nil {
		return n, err
	}
	return n, fmt.Errorf("notice %s is resolved already: %w", id, store.ErrConflict)
}

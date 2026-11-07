package postgres

import (
	"context"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Usage implements store.Store.
func (s *Store) Usage() store.UsageRepo { return usageRepo{s.db} }

type usageRepo struct{ db dbtx }

const usageCols = `id, account_id, machine_id, shape_id, ownership, minutes::float8, cost::float8, started_at, ended_at, created_at`

func scanUsage(row pgx.Row) (u domain.UsageRecord, err error) {
	err = row.Scan(&u.ID, &u.AccountID, &u.MachineID, &u.ShapeID, &u.Ownership, &u.Minutes, &u.Cost, &u.StartedAt, &u.EndedAt, &u.CreatedAt)
	utc(&u.StartedAt, &u.EndedAt, &u.CreatedAt)
	return u, err
}

func (r usageRepo) Create(ctx context.Context, u *domain.UsageRecord) error {
	if err := u.Validate(); err != nil {
		return err
	}
	u.ID = newID(u.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO usage_records (id, account_id, machine_id, shape_id, ownership, minutes, cost, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING minutes::float8, cost::float8, created_at`,
		u.ID, u.AccountID, u.MachineID, u.ShapeID, string(u.Ownership), u.Minutes, u.Cost, u.StartedAt.UTC(), u.EndedAt.UTC()).
		Scan(&u.Minutes, &u.Cost, &u.CreatedAt))
}

// usageWhere selects records of an organization, of every one for the zero ID.
func usageWhere(accountID uuid.UUID, f store.UsageFilter) *where {
	w := &where{conds: []string{"true"}}
	andNot(w, "account_id = ?", accountID)
	andNot(w, "shape_id = ?", f.ShapeID)
	andNot(w, "ownership = ?", string(f.Ownership))
	if !f.From.IsZero() {
		w.and("ended_at >= ?", f.From.UTC())
	}
	if !f.To.IsZero() {
		w.and("ended_at < ?", f.To.UTC())
	}
	return w
}

func (r usageRepo) List(ctx context.Context, accountID uuid.UUID, f store.UsageFilter) ([]domain.UsageRecord, error) {
	w := usageWhere(accountID, f)
	return many(ctx, r.db, scanUsage, `SELECT `+usageCols+` FROM usage_records`+w.sql("ended_at, id", 0), w.args...)
}

func (r usageRepo) Sum(ctx context.Context, accountID uuid.UUID, f store.UsageFilter) (store.UsageTotal, error) {
	w := usageWhere(accountID, f)
	var t store.UsageTotal
	err := r.db.QueryRow(ctx, `SELECT COALESCE(sum(minutes), 0)::float8, COALESCE(sum(cost), 0)::float8 FROM usage_records WHERE `+strings.Join(w.conds, " AND "), w.args...).
		Scan(&t.Minutes, &t.Cost)
	return t, mapErr(err)
}

func (r usageRepo) SumByProvider(ctx context.Context, providerID uuid.UUID, from, to time.Time) (store.UsageTotal, error) {
	w := usageWhere(uuid.Nil(), store.UsageFilter{From: from, To: to})
	w.and("machine_id IN (SELECT id FROM machines WHERE provider_id = ?)", providerID)
	var t store.UsageTotal
	err := r.db.QueryRow(ctx, `SELECT COALESCE(sum(minutes), 0)::float8, COALESCE(sum(cost), 0)::float8 FROM usage_records WHERE `+strings.Join(w.conds, " AND "), w.args...).
		Scan(&t.Minutes, &t.Cost)
	return t, mapErr(err)
}

func (r usageRepo) Unmetered(ctx context.Context, since time.Time, limit int) ([]domain.Machine, error) {
	w := &where{conds: []string{"state = 'destroyed'", "account_id IS NOT NULL", "NOT EXISTS (SELECT 1 FROM usage_records u WHERE u.machine_id = machines.id)"}}
	if !since.IsZero() {
		w.and("destroyed_at >= ?", since.UTC())
	}
	return many(ctx, r.db, scanMachine, `SELECT `+machineCols+` FROM machines`+w.sql("destroyed_at, id", limit), w.args...)
}

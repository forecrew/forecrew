package testkit

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Usage implements store.Store.
func (s *Store) Usage() store.UsageRepo { return usageRepo{s} }

type usageRepo struct{ s *Store }

// round keeps what the Postgres columns keep: numeric(12,3) minutes and
// numeric(14,6) cost.
func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

func (r usageRepo) Create(_ context.Context, u *domain.UsageRecord) error {
	if err := u.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.usage.exists(func(x domain.UsageRecord) bool { return x.MachineID == u.MachineID }) {
			return fmt.Errorf("usage of machine %s: %w", u.MachineID, store.ErrConflict)
		}
		// The composite key: the machine, its organization and its shape.
		if !d.machines.exists(func(m domain.Machine) bool {
			return m.ID == u.MachineID && m.AccountID == u.AccountID && m.ShapeID == u.ShapeID
		}) {
			return fmt.Errorf("usage of machine %s: no such machine of that organization and shape: %w", u.MachineID, domain.ErrInvalidRecord)
		}
		u.ID = newID(u.ID)
		u.Minutes, u.Cost = round(u.Minutes, 3), round(u.Cost, 6)
		u.StartedAt, u.EndedAt, u.CreatedAt = u.StartedAt.UTC(), u.EndedAt.UTC(), now
		d.usage.insert(*u)
		return nil
	})
}

func usageMatch(accountID uuid.UUID, f store.UsageFilter) func(domain.UsageRecord) bool {
	return func(u domain.UsageRecord) bool {
		return zeroOr(accountID, u.AccountID) && zeroOr(f.ShapeID, u.ShapeID) && zeroOr(f.Ownership, u.Ownership) &&
			(f.From.IsZero() || !u.EndedAt.Before(f.From)) && (f.To.IsZero() || u.EndedAt.Before(f.To))
	}
}

func (r usageRepo) List(_ context.Context, accountID uuid.UUID, f store.UsageFilter) ([]domain.UsageRecord, error) {
	rows, _ := listRows(r.s, usageT, 0, usageMatch(accountID, f))
	slices.SortStableFunc(rows, func(a, b domain.UsageRecord) int { return a.EndedAt.Compare(b.EndedAt) })
	return rows, nil
}

func sumUsage(rows []domain.UsageRecord) store.UsageTotal {
	var t store.UsageTotal
	for _, u := range rows {
		t.Minutes += u.Minutes
		t.Cost += u.Cost
	}
	return t
}

func (r usageRepo) Sum(ctx context.Context, accountID uuid.UUID, f store.UsageFilter) (store.UsageTotal, error) {
	rows, err := r.List(ctx, accountID, f)
	return sumUsage(rows), err
}

func (r usageRepo) SumByProvider(_ context.Context, providerID uuid.UUID, from, to time.Time) (store.UsageTotal, error) {
	rows, err := read(r.s, func(d *storeData) ([]domain.UsageRecord, error) {
		match := usageMatch(uuid.Nil(), store.UsageFilter{From: from, To: to})
		return d.usage.filter(0, func(u domain.UsageRecord) bool {
			return match(u) && d.machines.exists(func(m domain.Machine) bool { return m.ID == u.MachineID && m.ProviderID == providerID })
		}), nil
	})
	return sumUsage(rows), err
}

func (r usageRepo) Unmetered(_ context.Context, since time.Time, limit int) ([]domain.Machine, error) {
	rows, err := read(r.s, func(d *storeData) ([]domain.Machine, error) {
		return d.machines.filter(0, func(m domain.Machine) bool {
			return m.State == domain.MachineDestroyed && m.AccountID != uuid.Nil() && !m.DestroyedAt.Before(since) &&
				!d.usage.exists(func(u domain.UsageRecord) bool { return u.MachineID == m.ID })
		}), nil
	})
	slices.SortStableFunc(rows, func(a, b domain.Machine) int { return a.DestroyedAt.Compare(b.DestroyedAt) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, err
}

func usageT(d *storeData) *table[domain.UsageRecord] { return &d.usage }

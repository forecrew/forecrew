package testkit

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// FleetNotices implements store.Store.
func (s *Store) FleetNotices() store.FleetNoticeRepo { return noticeRepo{s} }

type noticeRepo struct{ s *Store }

func (r noticeRepo) Create(_ context.Context, n *domain.FleetNotice) error {
	if err := n.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.notices.exists(func(x domain.FleetNotice) bool { return x.MachineID == n.MachineID && x.Kind == n.Kind }) {
			return fmt.Errorf("notice of machine %s: %w", n.MachineID, store.ErrConflict)
		}
		n.ID = newID(n.ID)
		n.CreatedAt = now
		d.notices.insert(*n)
		return nil
	})
}

func (r noticeRepo) List(_ context.Context, all bool, limit int) ([]domain.FleetNotice, error) {
	return read(r.s, func(d *storeData) ([]domain.FleetNotice, error) {
		return d.notices.newest(limit, func(n domain.FleetNotice) bool { return all || n.ResolvedAt.IsZero() }), nil
	})
}

func (r noticeRepo) Resolve(_ context.Context, id uuid.UUID, by string) (domain.FleetNotice, error) {
	var out domain.FleetNotice
	err := byID(r.s, func(d *storeData) *table[domain.FleetNotice] { return &d.notices },
		func(n domain.FleetNotice) bool { return n.ID == id },
		func(n *domain.FleetNotice, now time.Time) error {
			if !n.ResolvedAt.IsZero() {
				return fmt.Errorf("notice %s is resolved already: %w", id, store.ErrConflict)
			}
			n.ResolvedAt, n.ResolvedBy = now, by
			out = *n
			return nil
		})
	return out, err
}

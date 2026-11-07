package fleet

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// backfillEvery spaces the search for destroyed machines metering missed.
const backfillEvery = time.Hour

// ActionMetered records a usage record written after its machine ended.
const ActionMetered = "machine.metered"

// MeterStart is when machine m starts to cost its organization: the later of
// its assignment and its start at the provider. A warm machine costs nobody
// until it is assigned, and a machine that never started costs nothing, so a
// job kept waiting by a location with no room is not billed for it.
func MeterStart(m domain.Machine) (time.Time, bool) {
	if m.AssignedAt.IsZero() || m.BootingAt.IsZero() {
		return time.Time{}, false
	}
	if m.BootingAt.After(m.AssignedAt) {
		return m.BootingAt, true
	}
	return m.AssignedAt, true
}

// Meter returns the usage record of machine m of shape s, which ended at end:
// minutes to the thousandth and cost to the millionth, as the columns keep
// them, so the record is what a sum of records gives back.
func Meter(m domain.Machine, s domain.Shape, end time.Time) domain.UsageRecord {
	start, ok := MeterStart(m)
	if !ok || end.Before(start) {
		start = end
	}
	minutes := roundTo(end.Sub(start).Minutes(), 3)
	return domain.UsageRecord{
		AccountID: m.AccountID, MachineID: m.ID, ShapeID: m.ShapeID, Ownership: s.Ownership(),
		Minutes: minutes, Cost: roundTo(minutes*s.PricePerMinute, 6), StartedAt: start.UTC(), EndedAt: end.UTC(),
	}
}

func roundTo(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// meter writes the usage record of machine id, destroyed in tx, in the same
// transaction: the machine ends and is billed together or not at all, and a
// second try finds the record and writes none (store.ErrConflict). An
// unassigned machine costs nobody and has none. It returns the record written,
// nil when there is none.
func meter(ctx context.Context, tx store.Store, id uuid.UUID) (*domain.UsageRecord, error) {
	m, err := tx.Machines().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("machine %s: %w", id, err)
	}
	if m.AccountID == uuid.Nil() || m.DestroyedAt.IsZero() {
		return nil, nil
	}
	shape, err := tx.Shapes().Get(ctx, m.ShapeID)
	if err != nil {
		return nil, fmt.Errorf("shape of machine %s: %w", id, err)
	}
	u := Meter(m, shape, m.DestroyedAt)
	switch err := tx.Usage().Create(ctx, &u); {
	case errors.Is(err, store.ErrConflict):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("meter machine %s: %w", id, err)
	}
	return &u, nil
}

// backfill meters destroyed machines that have no record, at most once per
// backfillEvery: those that ended before metering existed, or by a path that
// did not meter. Each one is in its organization's audit log, so a late record
// is never silent.
func (c *Controller) backfill(ctx context.Context) error {
	now := c.cfg.Now()
	if !c.lastBackfill.IsZero() && now.Sub(c.lastBackfill) < backfillEvery {
		return nil
	}
	missed, err := c.cfg.Store.Usage().Unmetered(ctx, time.Time{}, 500)
	if err != nil {
		return fmt.Errorf("find unmetered machines: %w", err)
	}
	var errs []error
	for _, m := range missed {
		err := c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			u, err := meter(ctx, tx, m.ID)
			if err != nil || u == nil {
				return err
			}
			c.cfg.Logger.WarnContext(ctx, "metered a machine after it ended", "machine_id", m.ID, "minutes", u.Minutes)
			return appendAudit(ctx, tx, m.AccountID, m.ID, ActionMetered, map[string]any{"minutes": u.Minutes, "cost": u.Cost})
		})
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	c.lastBackfill = now
	return nil
}

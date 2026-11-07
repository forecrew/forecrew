package fleet

import (
	"cmp"
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

// MeterEnd is when machine m stopped costing its organization: when it failed
// or, if it never did, was destroyed. A machine still up has not ended.
func MeterEnd(m domain.Machine) (time.Time, bool) {
	switch m.State {
	case domain.MachineFailed:
		return m.UpdatedAt, true // the failure is the row's last change
	case domain.MachineDestroyed:
		return m.DestroyedAt, true
	}
	return time.Time{}, false
}

// meter writes the usage record of machine id, which ended in tx (failed or
// destroyed), in the same transaction: the machine ends and is billed together
// or not at all, and a second try, or the destroy after a failure, finds the
// record and writes none (store.ErrConflict). A machine that fails is billed
// to its failure, even when nothing ever says it is gone. An unassigned
// machine costs nobody and has none. It returns the record written, nil when
// there is none.
func meter(ctx context.Context, tx store.Store, id uuid.UUID) (*domain.UsageRecord, error) {
	m, err := tx.Machines().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("machine %s: %w", id, err)
	}
	end, ended := MeterEnd(m)
	if m.AccountID == uuid.Nil() || !ended {
		return nil, nil
	}
	shape, err := tx.Shapes().Get(ctx, m.ShapeID)
	if err != nil {
		return nil, fmt.Errorf("shape of machine %s: %w", id, err)
	}
	u := Meter(m, shape, end)
	switch err := tx.Usage().Create(ctx, &u); {
	case errors.Is(err, store.ErrConflict):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("meter machine %s: %w", id, err)
	}
	return &u, nil
}

// backfill meters ended machines that have no record, at most once per
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

// UsageTotals adds up minutes and cost.
type UsageTotals struct {
	Machines int     `json:"machines"`
	Minutes  float64 `json:"minutes"`
	Cost     float64 `json:"cost"`
}

func (t *UsageTotals) add(minutes, cost float64) {
	t.Machines++
	t.Minutes = roundTo(t.Minutes+minutes, 3)
	t.Cost = roundTo(t.Cost+cost, 6)
}

// ShapeUsage is what the machines of one shape cost.
type ShapeUsage struct {
	Shape     string           `json:"shape"`
	Ownership domain.Ownership `json:"ownership"`
	UsageTotals
}

// UsageReport is what an organization's machines cost over [From, To): the
// usage records of machines that ended then, by shape and ownership, and an
// estimate of the machines still running, which are billed when they end. It
// is the input of billing (S.3).
type UsageReport struct {
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Currency string       `json:"currency"`
	Total    UsageTotals  `json:"total"`
	Hosted   UsageTotals  `json:"hosted"`
	Own      UsageTotals  `json:"own"`
	Shapes   []ShapeUsage `json:"shapes"`
	// Running is what the machines up now cost so far, not billed yet.
	Running UsageTotals `json:"running"`
	// Records are the usage records themselves, when asked for.
	Records []domain.UsageRecord `json:"records,omitempty"`
}

// Usage reports what an organization's machines cost over [from, to).
func (c *Controller) Usage(ctx context.Context, accountID uuid.UUID, from, to time.Time, records bool) (UsageReport, error) {
	st := c.cfg.Store
	out := UsageReport{From: from.UTC(), To: to.UTC(), Currency: c.cfg.Currency, Shapes: []ShapeUsage{}}
	list, err := st.Usage().List(ctx, accountID, store.UsageFilter{From: from, To: to})
	if err != nil {
		return out, fmt.Errorf("usage records: %w", err)
	}
	shapes, err := st.Shapes().ListAvailable(ctx, accountID)
	if err != nil {
		return out, fmt.Errorf("shapes: %w", err)
	}
	byID := map[uuid.UUID]domain.Shape{}
	for _, s := range shapes {
		byID[s.ID] = s
	}
	at := map[uuid.UUID]int{}
	for _, u := range list {
		i, ok := at[u.ShapeID]
		if !ok {
			i = len(out.Shapes)
			at[u.ShapeID] = i
			out.Shapes = append(out.Shapes, ShapeUsage{Shape: cmp.Or(byID[u.ShapeID].Name, u.ShapeID.String()), Ownership: u.Ownership})
		}
		out.Shapes[i].add(u.Minutes, u.Cost)
		out.Total.add(u.Minutes, u.Cost)
		if u.Ownership == domain.OwnershipHosted {
			out.Hosted.add(u.Minutes, u.Cost)
		} else {
			out.Own.add(u.Minutes, u.Cost)
		}
	}
	if records {
		out.Records = list
	}
	now := c.cfg.Now()
	if now.Before(from) || !now.Before(to) {
		return out, nil
	}
	up, err := st.Machines().List(ctx, store.MachineFilter{AccountID: accountID, Active: true})
	if err != nil {
		return out, fmt.Errorf("machines: %w", err)
	}
	for _, m := range up {
		if s, ok := byID[m.ShapeID]; ok {
			if _, started := MeterStart(m); !started {
				continue
			}
			u := Meter(m, s, now)
			out.Running.add(u.Minutes, u.Cost)
		}
	}
	return out, nil
}

package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func councilsT(d *storeData) *table[domain.Council] { return &d.councils }

func contributionsT(d *storeData) *table[domain.CouncilContribution] { return &d.council }

// Councils implements store.Store.
func (s *Store) Councils() store.CouncilRepo { return councilRepo{s} }

type councilRepo struct{ s *Store }

// copied returns c without sharing its slices with the stored row.
func copied(c domain.Council) domain.Council {
	c.Constraints = slices.Clone(c.Constraints)
	return c
}

func (r councilRepo) Create(_ context.Context, c *domain.Council) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, c.AccountID, c.ProjectID), foreign(&d.goals, c.AccountID, c.GoalID),
			foreign(&d.gates, c.AccountID, c.GateID), foreign(&d.runs, c.AccountID, c.RunID)); err != nil {
			return err
		}
		id := newID(c.ID)
		if d.councils.exists(func(x domain.Council) bool { return x.ID == id }) {
			return fmt.Errorf("council %s: %w", id, store.ErrConflict)
		}
		c.ID, c.Number, c.CreatedAt, c.UpdatedAt = id, d.nextNumber(c.AccountID, domain.RefCouncil), now, now
		if c.Status == "" {
			c.Status = domain.CouncilRunning
		}
		if c.Constraints == nil {
			c.Constraints = []string{}
		}
		d.councils.insert(copied(*c))
		return nil
	})
}

func (r councilRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Council, error) {
	c, err := getRow(r.s, councilsT, accountID, id)
	return copied(c), err
}

// GetForUpdate implements store.CouncilRepo; transactions are serialized.
func (r councilRepo) GetForUpdate(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error) {
	return r.Get(ctx, accountID, id)
}

func (r councilRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Council, error) {
	c, err := findRow(r.s, councilsT, func(c domain.Council) bool { return c.AccountID == accountID && c.Number == number })
	return copied(c), err
}

func (r councilRepo) List(_ context.Context, accountID uuid.UUID, f store.CouncilFilter) ([]domain.Council, error) {
	return read(r.s, func(d *storeData) ([]domain.Council, error) {
		out := d.councils.newest(f.Limit, func(c domain.Council) bool {
			return c.AccountID == accountID && zeroOr(f.ProjectID, c.ProjectID) && zeroOr(f.GoalID, c.GoalID) && zeroOr(f.GateID, c.GateID) && zeroOr(f.Status, c.Status)
		})
		for i := range out {
			out[i] = copied(out[i])
		}
		return out, nil
	})
}

func (r councilRepo) Update(_ context.Context, c *domain.Council) error {
	return update(r.s, councilsT, c.AccountID, c.ID, func(cur *domain.Council, now time.Time) error {
		cur.Constraints, cur.Brief, cur.GateID, cur.RunID, cur.UpdatedAt = slices.Clone(c.Constraints), c.Brief, c.GateID, c.RunID, now
		if cur.Constraints == nil {
			cur.Constraints = []string{}
		}
		c.UpdatedAt = now
		return nil
	})
}

func (r councilRepo) UpdateStatus(_ context.Context, accountID, id uuid.UUID, from, to domain.CouncilStatus) error {
	return update(r.s, councilsT, accountID, id, func(cur *domain.Council, now time.Time) error {
		if err := casStatus(&cur.Status, from, to); err != nil {
			return err
		}
		cur.UpdatedAt = now
		return nil
	})
}

func (r councilRepo) AddContribution(_ context.Context, c *domain.CouncilContribution) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.councils, c.AccountID, c.CouncilID), foreign(&d.jobs, c.AccountID, c.JobID)); err != nil {
			return err
		}
		if d.council.exists(func(x domain.CouncilContribution) bool {
			return (c.JobID != uuid.Nil() && x.JobID == c.JobID) ||
				(x.CouncilID == c.CouncilID && x.Role == c.Role && x.Runtime == c.Runtime && x.Stage == c.Stage && x.Round == c.Round)
		}) {
			return fmt.Errorf("contribution of %s to %s round %d: %w", c.Role, c.Stage, c.Round, store.ErrConflict)
		}
		c.ID, c.CreatedAt = newID(c.ID), now
		row := *c
		row.Content = json.RawMessage(slices.Clone(c.Content))
		d.council.insert(row)
		return nil
	})
}

func (r councilRepo) SetEvidenceCheck(_ context.Context, accountID, contributionID uuid.UUID, checks []domain.SourceCheck) error {
	return update(r.s, contributionsT, accountID, contributionID, func(c *domain.CouncilContribution, _ time.Time) error {
		c.EvidenceCheck = slices.Clone(checks)
		if c.EvidenceCheck == nil {
			c.EvidenceCheck = []domain.SourceCheck{}
		}
		return nil
	})
}

func (r councilRepo) Contributions(_ context.Context, accountID, councilID uuid.UUID, stage domain.CouncilStage) ([]domain.CouncilContribution, error) {
	return listRows(r.s, contributionsT, 0, func(c domain.CouncilContribution) bool {
		return c.AccountID == accountID && c.CouncilID == councilID && zeroOr(stage, c.Stage)
	})
}

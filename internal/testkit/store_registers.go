package testkit

import (
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func assumptionsT(d *storeData) *table[domain.Assumption] { return &d.assumptions }
func decisionsT(d *storeData) *table[domain.Decision]     { return &d.decisions }

// Assumptions implements store.Store.
func (s *Store) Assumptions() store.AssumptionRepo { return assumptionRepo{s} }

type assumptionRepo struct{ s *Store }

func (r assumptionRepo) Create(_ context.Context, a *domain.Assumption) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		err := firstErr(foreign(&d.projects, a.AccountID, a.ProjectID), foreign(&d.goals, a.AccountID, a.GoalID),
			foreign(&d.jobs, a.AccountID, a.JobID), foreign(&d.specs, a.AccountID, a.SpecID))
		if err != nil {
			return err
		}
		a.ID = newID(a.ID)
		a.Number = d.nextNumber(a.AccountID, domain.RefAssumption)
		if a.Status == "" {
			a.Status = domain.AssumptionUnverified
		}
		if a.Dependents == nil {
			a.Dependents = []domain.Dependent{}
		}
		a.CreatedAt, a.UpdatedAt = now, now
		d.assumptions.insert(*a)
		return nil
	})
}

func (r assumptionRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Assumption, error) {
	return getRow(r.s, assumptionsT, accountID, id)
}

func (r assumptionRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Assumption, error) {
	return findRow(r.s, assumptionsT, func(a domain.Assumption) bool { return a.AccountID == accountID && a.Number == number })
}

func (r assumptionRepo) List(_ context.Context, accountID uuid.UUID, f store.AssumptionFilter) ([]domain.Assumption, error) {
	return listRows(r.s, assumptionsT, f.Limit, func(a domain.Assumption) bool {
		return a.AccountID == accountID && zeroOr(f.ProjectID, a.ProjectID) && zeroOr(f.GoalID, a.GoalID) && zeroOr(f.JobID, a.JobID) &&
			zeroOr(f.Status, a.Status) && zeroOr(f.Impact, a.Impact) && zeroOr(f.VerifyBy, a.VerifyBy)
	})
}

func (r assumptionRepo) AddDependents(_ context.Context, accountID, id uuid.UUID, deps []domain.Dependent) error {
	return update(r.s, assumptionsT, accountID, id, func(a *domain.Assumption, now time.Time) error {
		merged := store.MergeDependents(a.Dependents, deps)
		if len(merged) != len(a.Dependents) {
			a.Dependents, a.UpdatedAt = merged, now
		}
		return nil
	})
}

func (r assumptionRepo) RaiseImpact(_ context.Context, accountID, id uuid.UUID, impact domain.Level) error {
	return update(r.s, assumptionsT, accountID, id, func(a *domain.Assumption, now time.Time) error {
		if slices.Index(domain.Levels(), impact) > slices.Index(domain.Levels(), a.Impact) {
			a.Impact, a.UpdatedAt = impact, now
		}
		return nil
	})
}

// Lock does nothing: transactions of the fake store run one at a time.
func (r assumptionRepo) Lock(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil }

func (r assumptionRepo) Resolve(_ context.Context, a *domain.Assumption, from domain.AssumptionStatus) error {
	if _, err := from.Transition(a.Status); err != nil {
		return err
	}
	return update(r.s, assumptionsT, a.AccountID, a.ID, func(cur *domain.Assumption, _ time.Time) error {
		if cur.Status != from {
			return fmt.Errorf("assumption %s is %s, not %s: %w", cur.Ref(), cur.Status, from, store.ErrConflict)
		}
		cur.Status, cur.Evidence, cur.ResolvedBy, cur.ResolvedAt, cur.UpdatedAt = a.Status, a.Evidence, a.ResolvedBy, a.ResolvedAt, a.UpdatedAt
		return nil
	})
}

// Decisions implements store.Store.
func (s *Store) Decisions() store.DecisionRepo { return decisionRepo{s} }

type decisionRepo struct{ s *Store }

func (r decisionRepo) Create(_ context.Context, dec *domain.Decision) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		err := firstErr(foreign(&d.projects, dec.AccountID, dec.ProjectID), foreign(&d.goals, dec.AccountID, dec.GoalID),
			foreign(&d.gates, dec.AccountID, dec.GateID))
		if err != nil {
			return err
		}
		for _, id := range dec.Assumptions {
			if err := foreign(&d.assumptions, dec.AccountID, id); err != nil {
				return err
			}
		}
		dec.ID = newID(dec.ID)
		dec.Number = d.nextNumber(dec.AccountID, domain.RefDecision)
		if dec.Options == nil {
			dec.Options = []domain.DecisionOption{}
		}
		if dec.Assumptions == nil {
			dec.Assumptions = []uuid.UUID{}
		}
		dec.CreatedAt = now
		d.decisions.insert(*dec)
		return nil
	})
}

func (r decisionRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Decision, error) {
	return getRow(r.s, decisionsT, accountID, id)
}

func (r decisionRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Decision, error) {
	return findRow(r.s, decisionsT, func(d domain.Decision) bool { return d.AccountID == accountID && d.Number == number })
}

func (r decisionRepo) List(_ context.Context, accountID uuid.UUID, f store.DecisionFilter) ([]domain.Decision, error) {
	return listRows(r.s, decisionsT, f.Limit, func(d domain.Decision) bool {
		return d.AccountID == accountID && zeroOr(f.ProjectID, d.ProjectID) && zeroOr(f.GoalID, d.GoalID) && zeroOr(f.CouncilID, d.CouncilID)
	})
}

package testkit

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func proposalsT(d *storeData) *table[domain.Proposal] { return &d.proposals }

// Proposals implements store.Store.
func (s *Store) Proposals() store.ProposalRepo { return proposalRepo{s} }

type proposalRepo struct{ s *Store }

func (r proposalRepo) Create(_ context.Context, p *domain.Proposal) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, p.AccountID, p.ProjectID), foreign(&d.gates, p.AccountID, p.GateID), foreign(&d.goals, p.AccountID, p.GoalID)); err != nil {
			return err
		}
		if !p.Status.Final() && d.proposals.exists(func(x domain.Proposal) bool { return openWith(x, p.AccountID, p.ProjectID, p.DedupeKey) }) {
			return fmt.Errorf("proposal %s: %w", p.DedupeKey, store.ErrConflict)
		}
		p.ID = newID(p.ID)
		p.Number = d.nextNumber(p.AccountID, domain.RefProposal)
		p.CreatedAt, p.UpdatedAt = now, now
		if p.Evidence == nil {
			p.Evidence = []domain.ProposalEvidence{}
		}
		d.proposals.insert(*p)
		return nil
	})
}

// openWith reports whether x is the proposal of a project with the key that
// is not final.
func openWith(x domain.Proposal, accountID, projectID uuid.UUID, key string) bool {
	return x.AccountID == accountID && x.ProjectID == projectID && x.DedupeKey == key && !x.Status.Final()
}

func (r proposalRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Proposal, error) {
	return getRow(r.s, proposalsT, accountID, id)
}

func (r proposalRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Proposal, error) {
	return findRow(r.s, proposalsT, func(p domain.Proposal) bool { return p.AccountID == accountID && p.Number == number })
}

func (r proposalRepo) GetOpen(_ context.Context, accountID, projectID uuid.UUID, dedupeKey string) (domain.Proposal, error) {
	return findRow(r.s, proposalsT, func(p domain.Proposal) bool { return openWith(p, accountID, projectID, dedupeKey) })
}

func (r proposalRepo) List(_ context.Context, accountID uuid.UUID, f store.ProposalFilter) ([]domain.Proposal, error) {
	return listRows(r.s, proposalsT, f.Limit, func(p domain.Proposal) bool {
		return p.AccountID == accountID && zeroOr(f.ProjectID, p.ProjectID) && zeroOr(f.Status, p.Status) && zeroOr(f.Category, p.Category) &&
			zeroOr(f.DedupeKey, p.DedupeKey) && zeroOr(f.GateID, p.GateID) && zeroOr(f.GoalID, p.GoalID)
	})
}

func (r proposalRepo) Update(_ context.Context, p *domain.Proposal, from domain.ProposalStatus) error {
	if p.Status != from {
		if _, err := from.Transition(p.Status); err != nil {
			return err
		}
	}
	return update(r.s, proposalsT, p.AccountID, p.ID, func(cur *domain.Proposal, now time.Time) error {
		if cur.Status != from {
			return fmt.Errorf("proposal %s is %q, not %q: %w", cur.Ref(), cur.Status, from, store.ErrConflict)
		}
		number, created := cur.Number, cur.CreatedAt
		*cur = *p
		cur.Number, cur.CreatedAt, cur.UpdatedAt = number, created, now
		p.UpdatedAt = now
		return nil
	})
}

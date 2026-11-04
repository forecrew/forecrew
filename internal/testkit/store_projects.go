package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func accountsT(d *storeData) *table[domain.Account] { return &d.accounts }
func projectsT(d *storeData) *table[domain.Project] { return &d.projects }

// Accounts implements store.Store.
func (s *Store) Accounts() store.AccountRepo { return accountRepo{s} }

type accountRepo struct{ s *Store }

func (r accountRepo) Create(_ context.Context, a *domain.Account) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		a.ID = newID(a.ID)
		if a.Slug == "" {
			a.Slug = domain.Slugify(a.Name)
		}
		if d.accounts.exists(func(x domain.Account) bool { return x.ID == a.ID || x.Slug == a.Slug }) {
			return fmt.Errorf("account %s: %w", a.ID, store.ErrConflict)
		}
		a.CreatedAt = now
		d.accounts.insert(*a)
		return nil
	})
}

func (r accountRepo) Get(_ context.Context, id uuid.UUID) (domain.Account, error) {
	return getRow(r.s, accountsT, id, id)
}

func (r accountRepo) List(context.Context) ([]domain.Account, error) {
	return listRows(r.s, accountsT, 0, func(domain.Account) bool { return true })
}

// Projects implements store.Store.
func (s *Store) Projects() store.ProjectRepo { return projectRepo{s} }

type projectRepo struct{ s *Store }

func (r projectRepo) Create(_ context.Context, p *domain.Project) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		dup := d.projects.exists(func(x domain.Project) bool {
			return x.AccountID == p.AccountID && (strings.EqualFold(x.Repo, p.Repo) || x.Name == p.Name)
		})
		if dup {
			return fmt.Errorf("project %s: %w", p.Repo, store.ErrConflict)
		}
		p.ID = newID(p.ID)
		p.CreatedAt, p.UpdatedAt = now, now
		d.projects.insert(*p)
		return nil
	})
}

func (r projectRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Project, error) {
	return getRow(r.s, projectsT, accountID, id)
}

func (r projectRepo) GetByName(_ context.Context, accountID uuid.UUID, nameOrRepo string) (domain.Project, error) {
	return findRow(r.s, projectsT, func(p domain.Project) bool {
		return p.AccountID == accountID && (strings.EqualFold(p.Name, nameOrRepo) || strings.EqualFold(p.Repo, nameOrRepo))
	})
}

func (r projectRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Project, error) {
	return listRows(r.s, projectsT, 0, func(p domain.Project) bool { return p.AccountID == accountID })
}

func (r projectRepo) ListByRepo(_ context.Context, repo string) ([]domain.Project, error) {
	return listRows(r.s, projectsT, 0, func(p domain.Project) bool { return strings.EqualFold(p.Repo, repo) })
}

func (r projectRepo) UpdateStatus(_ context.Context, accountID, id uuid.UUID, from, to domain.ProjectStatus) error {
	return update(r.s, projectsT, accountID, id, func(p *domain.Project, now time.Time) error {
		p.UpdatedAt = now
		return casStatus(&p.Status, from, to)
	})
}

func (r projectRepo) SetProfile(_ context.Context, accountID, id uuid.UUID, profile json.RawMessage) error {
	return update(r.s, projectsT, accountID, id, func(p *domain.Project, now time.Time) error {
		p.Profile = profile
		p.UpdatedAt = now
		return nil
	})
}

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
		taken := func(slug string) bool {
			return d.accounts.exists(func(x domain.Account) bool { return x.Slug == slug })
		}
		slug := a.Slug
		if slug == "" {
			base := domain.Slugify(a.Name)
			slug = base
			for n := 2; taken(slug); n++ {
				slug = domain.SlugCandidate(base, n)
			}
		}
		if taken(slug) || d.accounts.exists(func(x domain.Account) bool { return x.ID == a.ID }) {
			return fmt.Errorf("account %s: %w", a.ID, store.ErrConflict)
		}
		a.Slug = slug
		a.CreatedAt = now
		d.accounts.insert(*a)
		return nil
	})
}

func (r accountRepo) Get(_ context.Context, id uuid.UUID) (domain.Account, error) {
	return getRow(r.s, accountsT, id, id)
}

func (r accountRepo) GetBySlug(_ context.Context, slug string) (domain.Account, error) {
	return findRow(r.s, accountsT, func(a domain.Account) bool { return a.Slug == slug })
}

func (r accountRepo) List(context.Context) ([]domain.Account, error) {
	return listRows(r.s, accountsT, 0, func(domain.Account) bool { return true })
}

func (r accountRepo) SetMaxTokenTTL(_ context.Context, id uuid.UUID, ttl time.Duration) error {
	return update(r.s, accountsT, id, id, func(a *domain.Account, _ time.Time) error {
		a.MaxTokenTTL = domain.Duration(ttl)
		return nil
	})
}

func (r accountRepo) Lock(_ context.Context, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		_, err := d.accounts.get(id, id)
		return err
	})
}

func (r accountRepo) Rename(_ context.Context, id uuid.UUID, name, slug string) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if d.accounts.exists(func(a domain.Account) bool { return a.ID != id && a.Slug == slug }) {
			return fmt.Errorf("slug %s: %w", slug, store.ErrConflict)
		}
		a, err := d.accounts.get(id, id)
		if err != nil {
			return err
		}
		a.Name, a.Slug = name, slug
		return nil
	})
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

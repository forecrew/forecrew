package testkit

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func artifactsT(d *storeData) *table[domain.Artifact] { return &d.artifacts }

// Artifacts implements store.Store.
func (s *Store) Artifacts() store.ArtifactRepo { return artifactRepo{s} }

type artifactRepo struct{ s *Store }

func (r artifactRepo) Put(_ context.Context, a *domain.Artifact) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		_, err := putArtifact(d, now, a)
		return err
	})
}

func (r artifactRepo) PutLeased(_ context.Context, a *domain.Artifact, workerID uuid.UUID) (string, error) {
	var replaced string
	err := r.s.do(func(d *storeData, now time.Time) error {
		j, err := d.jobs.get(a.AccountID, a.JobID)
		if err != nil {
			return err
		}
		if err := checkLease(j, workerID); err != nil {
			return err
		}
		replaced, err = putArtifact(d, now, a)
		return err
	})
	return replaced, err
}

// putArtifact inserts or replaces a; it returns the storage key it replaced.
func putArtifact(d *storeData, now time.Time, a *domain.Artifact) (string, error) {
	if err := firstErr(foreign(&d.projects, a.AccountID, a.ProjectID), foreign(&d.jobs, a.AccountID, a.JobID)); err != nil {
		return "", err
	}
	cur, err := d.artifacts.find(func(x domain.Artifact) bool {
		return a.JobID != uuid.Nil() && x.AccountID == a.AccountID && x.JobID == a.JobID && x.Name == a.Name
	})
	if err != nil {
		if d.artifacts.exists(func(x domain.Artifact) bool { return x.StorageKey == a.StorageKey }) {
			return "", fmt.Errorf("storage key %s: %w", a.StorageKey, store.ErrConflict)
		}
		a.ID, a.CreatedAt = newID(a.ID), now
		d.artifacts.insert(*a)
		return "", nil
	}
	replaced := cur.StorageKey
	a.ID, a.CreatedAt = cur.ID, now
	*cur = *a
	return replaced, nil
}

func (r artifactRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Artifact, error) {
	return getRow(r.s, artifactsT, accountID, id)
}

func (r artifactRepo) ListByJob(_ context.Context, accountID, jobID uuid.UUID) ([]domain.Artifact, error) {
	out, err := listRows(r.s, artifactsT, 0, func(a domain.Artifact) bool { return a.AccountID == accountID && a.JobID == jobID })
	slices.SortFunc(out, func(a, b domain.Artifact) int { return cmp.Compare(a.Name, b.Name) })
	return out, err
}

func (r artifactRepo) ListOlderThan(_ context.Context, accountID uuid.UUID, t time.Time, limit int) ([]domain.Artifact, error) {
	out, err := listRows(r.s, artifactsT, 0, func(a domain.Artifact) bool { return a.AccountID == accountID && a.CreatedAt.Before(t) })
	slices.SortStableFunc(out, func(a, b domain.Artifact) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func (r artifactRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if _, err := d.artifacts.get(accountID, id); err != nil {
			return err
		}
		d.artifacts.rows = slices.DeleteFunc(d.artifacts.rows, func(a domain.Artifact) bool { return a.AccountID == accountID && a.ID == id })
		return nil
	})
}

func (r artifactRepo) ExistingKeys(_ context.Context, keys []string) ([]string, error) {
	rows, err := listRows(r.s, artifactsT, 0, func(a domain.Artifact) bool { return slices.Contains(keys, a.StorageKey) })
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.StorageKey)
	}
	return out, err
}

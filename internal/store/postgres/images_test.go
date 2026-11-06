//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestImages(t *testing.T) {
	f := newFixture(t)
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))
	testkit.ImageStoreSuite(t, f.st, f.account, other.ID)
}

// TestImageDeleteKeepsPinnedImages: a live machine whose shape names the
// image, or the bare name while it is the newest, keeps it.
func TestImageDeleteKeepsPinnedImages(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Images()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	old, newest := testkit.TestImage(f.account, "pinned", "1", base), testkit.TestImage(f.account, "pinned", "2", base.Add(time.Hour))
	must(t, repo.Create(ctx, &old))
	must(t, repo.Create(ctx, &newest))

	var provider, shape, machine uuid.UUID
	must(t, f.pool.QueryRow(ctx, `INSERT INTO capacity_providers (account_id, name, kind, mode, status) VALUES ($1, 'home', 'fake', 'direct', 'active') RETURNING id`, f.account).Scan(&provider))
	addShape := func(name, image string) uuid.UUID {
		var id uuid.UUID
		must(t, f.pool.QueryRow(ctx, `INSERT INTO shapes (account_id, provider_id, name, os, arch, cpu, memory_mb, disk_gb, image)
			VALUES ($1, $2, $3, 'linux', 'amd64', 2, 2048, 10, $4) RETURNING id`, f.account, provider, name, image).Scan(&id))
		return id
	}
	shape = addShape("by-name", "pinned")
	addMachine := func(shape uuid.UUID) uuid.UUID {
		var id uuid.UUID
		must(t, f.pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, state, assigned_at) VALUES ($1, $2, $3, 'booting', now()) RETURNING id`, f.account, provider, shape).Scan(&id))
		return id
	}
	machine = addMachine(shape)

	// The bare name follows the newest build: the old one may go, the newest may not.
	if err := repo.Delete(ctx, newest.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("newest of a live shape: %v", err)
	}
	pinnedShape := addShape("by-version", "pinned@1")
	pinnedMachine := addMachine(pinnedShape)
	if err := repo.Delete(ctx, old.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pinned version: %v", err)
	}
	for _, id := range []uuid.UUID{pinnedMachine, machine} {
		_, err := f.pool.Exec(ctx, `UPDATE machines SET state = 'failed' WHERE id = $1`, id)
		must(t, err)
	}
	must(t, repo.Delete(ctx, old.ID))
	must(t, repo.Delete(ctx, newest.ID))
}

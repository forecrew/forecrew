//go:build integration

package postgres

import (
	"errors"
	"fmt"
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
	must(t, f.pool.QueryRow(ctx, `INSERT INTO capacity_providers (account_id, name, kind, mode, status, capabilities) VALUES ($1, 'home', 'fake', 'direct', 'active', '{"image_formats":["rootfs"]}') RETURNING id`, f.account).Scan(&provider))
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

// imageFleet creates provider, shape and live machine rows for the image
// tests; account zero makes the provider and shape instance-wide.
type imageFleet struct {
	t *testing.T
	f *fixture
	n int
}

func (r *imageFleet) machine(shapeOwner, machineOwner uuid.UUID, formats, image string) uuid.UUID {
	r.t.Helper()
	ctx := r.t.Context()
	r.n++
	var provider, shape, machine uuid.UUID
	must(r.t, r.f.pool.QueryRow(ctx, `INSERT INTO capacity_providers (account_id, name, kind, mode, status, capabilities) VALUES ($1, $2, 'fake', 'direct', 'active', $3::jsonb) RETURNING id`,
		optID(shapeOwner), fmt.Sprintf("p%d", r.n), `{"image_formats":`+formats+`}`).Scan(&provider))
	must(r.t, r.f.pool.QueryRow(ctx, `INSERT INTO shapes (account_id, provider_id, name, os, arch, cpu, memory_mb, disk_gb, image)
		VALUES ($1, $2, $3, 'linux', 'amd64', 2, 2048, 10, $4) RETURNING id`, optID(shapeOwner), provider, fmt.Sprintf("s%d", r.n), image).Scan(&shape))
	must(r.t, r.f.pool.QueryRow(ctx, `INSERT INTO machines (account_id, provider_id, shape_id, state, assigned_at) VALUES ($1, $2, $3, 'booting', now()) RETURNING id`,
		machineOwner, provider, shape).Scan(&machine))
	return machine
}

func optID(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}

// TestImageDeleteChecksTheMachinesOrganization: an organization's image
// pinned on an instance-wide shape is kept for that organization's machines,
// not for another organization's.
func TestImageDeleteChecksTheMachinesOrganization(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	repo := f.st.Images()
	built := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	mine, theirs := testkit.TestImage(f.account, "shared-name", "1", built), testkit.TestImage(other.ID, "shared-name", "1", built)
	must(t, repo.Create(ctx, &mine))
	must(t, repo.Create(ctx, &theirs))

	rows := &imageFleet{t: t, f: f}
	machine := rows.machine(uuid.Nil(), f.account, `["rootfs"]`, "shared-name@1") // an instance-wide shape, a machine of f.account
	if err := repo.Delete(ctx, mine.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("own image on an instance-wide shape: %v", err)
	}
	// The other organization has no machine: its image of the same name and version goes.
	must(t, repo.Delete(ctx, theirs.ID))
	_, err := f.pool.Exec(ctx, `UPDATE machines SET state = 'failed' WHERE id = $1`, machine)
	must(t, err)
	must(t, repo.Delete(ctx, mine.ID))
}

// TestImageDeleteFollowsResolve: a bare name keeps what Resolve picks for the
// provider's formats, so the newest image in a format the provider does not
// start is not kept, and the newest it does start is.
func TestImageDeleteFollowsResolve(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Images()
	built := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	oldRootfs := testkit.TestImage(f.account, "bare", "1", built)
	newRootfs := testkit.TestImage(f.account, "bare", "2", built.Add(time.Hour))
	newQcow := testkit.TestImage(f.account, "bare", "3", built.Add(2*time.Hour))
	newQcow.Format, newQcow.Contents.Kernel = domain.ImageQCOW2, nil
	for _, i := range []*domain.Image{&oldRootfs, &newRootfs, &newQcow} {
		must(t, repo.Create(ctx, i))
	}
	rows := &imageFleet{t: t, f: f}
	machine := rows.machine(f.account, f.account, `["rootfs"]`, "bare")

	// The provider starts rootfs only: bare@2 is what it boots, though bare@3 is newer.
	if err := repo.Delete(ctx, newRootfs.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("what Resolve picks: %v", err)
	}
	must(t, repo.Delete(ctx, newQcow.ID))
	must(t, repo.Delete(ctx, oldRootfs.ID))
	_, err := f.pool.Exec(ctx, `UPDATE machines SET state = 'destroyed', destroyed_at = now() WHERE id = $1`, machine)
	must(t, err)
	must(t, repo.Delete(ctx, newRootfs.ID))
}

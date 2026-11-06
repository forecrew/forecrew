package testkit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func imagesT(d *storeData) *table[domain.Image] { return &d.images }

// Images implements store.Store.
func (s *Store) Images() store.ImageRepo { return imageRepo{s} }

type imageRepo struct{ s *Store }

func (r imageRepo) Create(_ context.Context, i *domain.Image) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if i.BuiltAt.IsZero() {
		return fmt.Errorf("image %s: built_at is required: %w", i.Name, domain.ErrInvalidRecord)
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.images.exists(func(x domain.Image) bool {
			return x.AccountID == i.AccountID && x.Name == i.Name && x.Version == i.Version && x.Arch == i.Arch && x.Format == i.Format
		}) {
			return fmt.Errorf("image %s@%s: %w", i.Name, i.Version, store.ErrConflict)
		}
		i.ID = newID(i.ID)
		i.BuiltAt, i.CreatedAt = i.BuiltAt.UTC(), now
		d.images.insert(*i)
		return nil
	})
}

func (r imageRepo) Get(_ context.Context, id uuid.UUID) (domain.Image, error) {
	return findRow(r.s, imagesT, func(i domain.Image) bool { return i.ID == id })
}

func (r imageRepo) List(_ context.Context, accountID uuid.UUID, f store.ImageFilter) ([]domain.Image, error) {
	rows, _ := listRows(r.s, imagesT, 0, func(i domain.Image) bool {
		return (i.AccountID == accountID || i.AccountID == uuid.Nil()) &&
			zeroOr(f.Name, i.Name) && zeroOr(f.OS, i.OS) && zeroOr(f.Arch, i.Arch) && zeroOr(f.Format, i.Format)
	})
	slices.SortStableFunc(rows, func(a, b domain.Image) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return b.BuiltAt.Compare(a.BuiltAt)
	})
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	return rows, nil
}

func (r imageRepo) Resolve(_ context.Context, accountID uuid.UUID, ref string, os domain.MachineOS, arch domain.MachineArch, formats []domain.ImageFormat) (domain.Image, error) {
	name, version, ok := domain.ParseImageRef(ref)
	if !ok {
		return domain.Image{}, fmt.Errorf("image %q must be a name or name@version: %w", ref, domain.ErrInvalidRecord)
	}
	rows, _ := listRows(r.s, imagesT, 0, func(i domain.Image) bool {
		return i.Name == name && (version == "" || i.Version == version) && i.OS == os && i.Arch == arch &&
			slices.Contains(formats, i.Format) && (i.AccountID == accountID || i.AccountID == uuid.Nil())
	})
	if len(rows) == 0 {
		return domain.Image{}, fmt.Errorf("image %s for %s/%s: %w", ref, os, arch, store.ErrNotFound)
	}
	// Same order as the Postgres store: newest, own before instance-wide,
	// then the provider's first format.
	return slices.MinFunc(rows, func(a, b domain.Image) int {
		if c := b.BuiltAt.Compare(a.BuiltAt); c != 0 {
			return c
		}
		if (a.AccountID == uuid.Nil()) != (b.AccountID == uuid.Nil()) {
			if a.AccountID == uuid.Nil() {
				return 1
			}
			return -1
		}
		return slices.Index(formats, a.Format) - slices.Index(formats, b.Format)
	}), nil
}

// Delete does not know about machines: the fake has none until the fleet
// repositories join it, so the pin check is the Postgres store's alone.
func (r imageRepo) Delete(_ context.Context, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.images.rows, func(x domain.Image) bool { return x.ID == id })
		if i < 0 {
			return fmt.Errorf("image %s: %w", id, store.ErrNotFound)
		}
		d.images.rows = slices.Delete(d.images.rows, i, i+1)
		return nil
	})
}

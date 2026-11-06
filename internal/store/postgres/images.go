package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Images implements store.Store.
func (s *Store) Images() store.ImageRepo { return imageRepo{s.db} }

type imageRepo struct{ db dbtx }

var imageCols = `id, ` + cuuid("account_id") + `, name, version, os, arch, format, uri, digest, signature, contents, built_at, created_at`

func scanImage(row pgx.Row) (i domain.Image, err error) {
	var contents []byte
	err = row.Scan(&i.ID, &i.AccountID, &i.Name, &i.Version, &i.OS, &i.Arch, &i.Format, &i.URI, &i.Digest, &i.Signature,
		&contents, &i.BuiltAt, &i.CreatedAt)
	if err != nil {
		return i, err
	}
	utc(&i.BuiltAt, &i.CreatedAt)
	if err := json.Unmarshal(contents, &i.Contents); err != nil {
		return i, fmt.Errorf("decode image contents: %w", err)
	}
	return i, nil
}

func (r imageRepo) Create(ctx context.Context, i *domain.Image) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if i.BuiltAt.IsZero() {
		return fmt.Errorf("image %s: built_at is required: %w", i.Name, domain.ErrInvalidRecord)
	}
	contents, err := json.Marshal(i.Contents)
	if err != nil {
		return fmt.Errorf("encode image contents: %w", err)
	}
	i.ID = newID(i.ID)
	err = r.db.QueryRow(ctx, `INSERT INTO images (id, account_id, name, version, os, arch, format, uri, digest, signature, contents, built_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING built_at, created_at`,
		i.ID, opt(i.AccountID), i.Name, i.Version, string(i.OS), string(i.Arch), string(i.Format), i.URI, i.Digest, i.Signature,
		contents, i.BuiltAt.UTC()).Scan(&i.BuiltAt, &i.CreatedAt)
	utc(&i.BuiltAt, &i.CreatedAt)
	return mapErr(err)
}

func (r imageRepo) Get(ctx context.Context, id uuid.UUID) (domain.Image, error) {
	return one(ctx, r.db, scanImage, `SELECT `+imageCols+` FROM images WHERE id = $1`, id)
}

func (r imageRepo) List(ctx context.Context, accountID uuid.UUID, f store.ImageFilter) ([]domain.Image, error) {
	w := (&where{}).and("(account_id = ? OR account_id IS NULL)", opt(accountID))
	andNot(w, "name = ?", f.Name)
	andNot(w, "os = ?", string(f.OS))
	andNot(w, "arch = ?", string(f.Arch))
	andNot(w, "format = ?", string(f.Format))
	return many(ctx, r.db, scanImage, `SELECT `+imageCols+` FROM images`+w.sql("name, built_at DESC, id", f.Limit), w.args...)
}

func (r imageRepo) Resolve(ctx context.Context, accountID uuid.UUID, ref string, os domain.MachineOS, arch domain.MachineArch, formats []domain.ImageFormat) (domain.Image, error) {
	name, version, ok := domain.ParseImageRef(ref)
	if !ok {
		return domain.Image{}, fmt.Errorf("image %q must be a name or name@version: %w", ref, domain.ErrInvalidRecord)
	}
	fs := make([]string, len(formats))
	for i, f := range formats {
		fs[i] = string(f)
	}
	// The newest build wins; at the same time the organization's own image
	// before the instance's, then the format the provider lists first.
	i, err := one(ctx, r.db, scanImage, `SELECT `+imageCols+` FROM images
		WHERE name = $1 AND ($2 = '' OR version = $2) AND os = $3 AND arch = $4 AND format = ANY($5)
		  AND (account_id = $6 OR account_id IS NULL)
		ORDER BY built_at DESC, (account_id IS NULL), array_position($5::text[], format), id LIMIT 1`,
		name, version, string(os), string(arch), fs, opt(accountID))
	if err != nil {
		return i, fmt.Errorf("image %s for %s/%s: %w", ref, os, arch, err)
	}
	return i, nil
}

// Delete removes an image. An image a live machine would boot is kept: for
// every live machine, the image that Resolve gives its shape's reference, for
// the machine's organization and the formats its provider starts, is that
// machine's. So an organization's image pinned on an instance-wide shape is
// kept for that organization's machines only, and a bare name keeps what is
// newest as Resolve picks it, format included.
func (r imageRepo) Delete(ctx context.Context, id uuid.UUID) error {
	img, err := r.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("image %s: %w", id, err)
	}
	pinned, err := r.pinned(ctx, img)
	if err != nil {
		return err
	}
	if pinned {
		return fmt.Errorf("image %s is in use by a live machine: %w", id, store.ErrConflict)
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM images WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("image %s: %w", id, store.ErrNotFound)
	}
	return nil
}

// pinned reports whether a live machine would boot img.
func (r imageRepo) pinned(ctx context.Context, img domain.Image) (bool, error) {
	type use struct {
		account uuid.UUID
		ref     string
		formats []byte
	}
	uses, err := many(ctx, r.db, func(row pgx.Row) (u use, err error) {
		err = row.Scan(&u.account, &u.ref, &u.formats)
		return u, err
	}, `SELECT DISTINCT `+cuuid("m.account_id")+`, s.image, COALESCE(p.capabilities->'image_formats', '[]'::jsonb)
		FROM machines m JOIN shapes s ON s.id = m.shape_id JOIN capacity_providers p ON p.id = m.provider_id
		WHERE m.state NOT IN ('destroyed','failed') AND s.image IN ($1, $2) AND s.os = $3 AND s.arch = $4`, img.Name, img.Name+"@"+img.Version, string(img.OS), string(img.Arch))
	if err != nil {
		return false, err
	}
	for _, u := range uses {
		var formats []domain.ImageFormat
		if err := json.Unmarshal(u.formats, &formats); err != nil {
			return false, fmt.Errorf("decode image formats: %w", err)
		}
		got, err := r.Resolve(ctx, u.account, u.ref, img.OS, img.Arch, formats)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrInvalidRecord) {
			continue
		}
		if err != nil {
			return false, err
		}
		if got.ID == img.ID {
			return true, nil
		}
	}
	return false, nil
}

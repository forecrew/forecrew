package postgres

import (
	"context"
	"encoding/json"
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

// Delete removes an image. A live machine whose shape pins the image
// (name@version, or the bare name while the image is the newest) keeps it.
func (r imageRepo) Delete(ctx context.Context, id uuid.UUID) error {
	var pinned bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM machines m JOIN shapes s ON s.id = m.shape_id
		WHERE m.state NOT IN ('destroyed','failed') AND s.os = i.os AND s.arch = i.arch
		  AND (i.account_id IS NULL OR s.account_id = i.account_id)
		  AND (s.image = i.name || '@' || i.version
		       OR (s.image = i.name AND NOT EXISTS (SELECT 1 FROM images n
		             WHERE n.name = i.name AND n.os = i.os AND n.arch = i.arch AND n.built_at > i.built_at
		               AND (n.account_id IS NULL OR n.account_id = s.account_id)))))
		FROM images i WHERE i.id = $1`, id).Scan(&pinned)
	if err != nil {
		return fmt.Errorf("image %s: %w", id, mapErr(err))
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

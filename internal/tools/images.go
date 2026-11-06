package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// List sizes of list_images.
const (
	DefaultImageLimit = 100
	MaxImageLimit     = 500
)

// Scopes of an image, as list_images says them.
const (
	ScopeOrganization = "organization"
	ScopeInstance     = "instance"
)

// ListImages implements mcp.ToolService.
func (s *Service) ListImages(ctx context.Context, by domain.Actor, in mcp.ListImagesInput) ([]mcp.ImageInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	f := store.ImageFilter{Name: in.Name, OS: domain.MachineOS(in.OS), Arch: domain.MachineArch(in.Arch), Format: domain.ImageFormat(in.Format), Limit: in.Limit}
	for _, c := range []struct {
		what string
		ok   bool
	}{
		{"os " + in.OS, in.OS == "" || slices.Contains(domain.MachineOSes(), f.OS)},
		{"arch " + in.Arch, in.Arch == "" || slices.Contains(domain.MachineArches(), f.Arch)},
		{"format " + in.Format, in.Format == "" || slices.Contains(domain.ImageFormats(), f.Format)},
		{fmt.Sprintf("limit %d", in.Limit), in.Limit >= 0},
	} {
		if !c.ok {
			return nil, fmt.Errorf("unknown %s: %w", c.what, mcp.ErrInvalidInput)
		}
	}
	f.Limit = min(cmp.Or(in.Limit, DefaultImageLimit), MaxImageLimit)
	imgs, err := s.cfg.Store.Images().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ImageInfo, len(imgs))
	for i, img := range imgs {
		out[i] = mcp.ImageInfo{Image: img, Scope: ScopeInstance}
		if img.AccountID != uuid.Nil() {
			out[i].Scope = ScopeOrganization
		}
	}
	return out, nil
}

// imageManager checks that by may manage images of the organization, or of
// the instance, and that by is a person: a service account, a token without
// an owner or a machine never registers an image others trust (fleet.md,
// Images).
func (s *Service) imageManager(ctx context.Context, by domain.Actor, instance bool) error {
	if by.Kind != domain.ActorUser {
		return fmt.Errorf("only a person registers or removes images: %w", authz.ErrDenied)
	}
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return err
	}
	if !instance {
		return nil
	}
	id, err := uuid.Parse(by.Name)
	if err != nil {
		return fmt.Errorf("instance images need an instance admin: %w", authz.ErrDenied)
	}
	u, err := s.cfg.Store.Directory().User(ctx, id)
	if err != nil || !u.InstanceAdmin {
		return fmt.Errorf("instance images need an instance admin: %w", authz.ErrDenied)
	}
	return nil
}

// AddImage registers a signed image for the organization or, for an
// instance admin, the instance. The server checks the form only: the fleet
// agent checks the signature against the keys it trusts.
func (s *Service) AddImage(ctx context.Context, by domain.Actor, in images.AddInput) (mcp.ImageInfo, error) {
	if err := s.imageManager(ctx, by, in.Instance); err != nil {
		return mcp.ImageInfo{}, err
	}
	account, scope := by.AccountID, ScopeOrganization
	if in.Instance {
		account, scope = uuid.Nil(), ScopeInstance
	}
	img := in.Image(account)
	if img.BuiltAt.IsZero() {
		return mcp.ImageInfo{}, fmt.Errorf("built_at is required: %w", mcp.ErrInvalidInput)
	}
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Images().Create(ctx, &img); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "image.registered", "image", img.ID,
			map[string]any{"name": img.Name, "version": img.Version, "format": img.Format, "scope": scope, "digest": img.Digest})
	})
	if errors.Is(err, domain.ErrInvalidRecord) {
		err = fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.ImageInfo{}, conflict(err, fmt.Sprintf("image %s@%s (%s, %s) exists", img.Name, img.Version, img.Arch, img.Format))
	}
	return mcp.ImageInfo{Image: img, Scope: scope}, nil
}

// DeleteImage removes an image by its UUID. A live machine that uses it
// keeps it: the answer is a conflict.
func (s *Service) DeleteImage(ctx context.Context, by domain.Actor, ref string) (mcp.ImageInfo, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return mcp.ImageInfo{}, fmt.Errorf("image %q must be a UUID: %w", ref, mcp.ErrInvalidInput)
	}
	if by.Kind != domain.ActorUser {
		return mcp.ImageInfo{}, fmt.Errorf("only a person registers or removes images: %w", authz.ErrDenied)
	}
	img, err := s.cfg.Store.Images().Get(ctx, id)
	if err == nil && img.AccountID != uuid.Nil() && img.AccountID != by.AccountID {
		err = store.ErrNotFound // another organization's image does not exist here
	}
	if err != nil {
		return mcp.ImageInfo{}, notFound(err, "image "+ref)
	}
	scope := ScopeOrganization
	if img.AccountID == uuid.Nil() {
		scope = ScopeInstance
	}
	if err := s.imageManager(ctx, by, scope == ScopeInstance); err != nil {
		return mcp.ImageInfo{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Images().Delete(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "image.deleted", "image", id,
			map[string]any{"name": img.Name, "version": img.Version, "scope": scope})
	})
	if err != nil {
		return mcp.ImageInfo{}, conflict(notFound(err, "image "+ref), "image "+ref+" is used by a live machine")
	}
	return mcp.ImageInfo{Image: img, Scope: scope}, nil
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// List sizes of list_image_builds.
const (
	DefaultImageBuildLimit = 20
	MaxImageBuildLimit     = 100
)

// nextAfterBuild is what a person does with a finished build. The build itself
// registers and signs nothing (fleet.md, Windows images on a host).
const nextAfterBuild = "The manifest on the host is unsigned and nothing is registered. Check it, sign it with your image key on a computer that is not that host (forecrew image sign), then register it (forecrew image add, or Register the image on the location's page) and put the public key in the host's image-keys.pub. See docs/images.md."

// buildProvider finds the location a build is about: the caller's own, or an
// instance-wide one for an instance admin. Another organization's is not found.
func (s *Service) buildProvider(ctx context.Context, by domain.Actor, ref string) (domain.CapacityProvider, error) {
	all, err := s.providers(ctx, by.AccountID)
	if err != nil {
		return domain.CapacityProvider{}, err
	}
	admin, err := s.instanceAdmin(ctx, by)
	if err != nil {
		return domain.CapacityProvider{}, err
	}
	all = slices.DeleteFunc(all, func(p domain.CapacityProvider) bool { return p.AccountID != by.AccountID && !admin })
	p, err := store.FindActive(ref, all, func(p domain.CapacityProvider) (uuid.UUID, string, bool) {
		return p.ID, p.Name, p.Status == domain.ProviderRevoked
	})
	if err != nil {
		return domain.CapacityProvider{}, fmt.Errorf("provider %s: %w", ref, mcp.ErrNotFound)
	}
	return p, nil
}

// StartImageBuild implements mcp.ToolService. It needs what managing the
// location needs, on the location's own organization, and a person: the
// statement about the licence is theirs, so a service account, an agent or an
// app acting for a person cannot make it. The request holds references only,
// and nothing here registers or signs the image that comes of it.
func (s *Service) StartImageBuild(ctx context.Context, by domain.Actor, in mcp.StartImageBuildInput) (mcp.ImageBuildInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerManage); err != nil {
		return mcp.ImageBuildInfo{}, err
	}
	if by.Kind != domain.ActorUser || authz.Delegated(by) {
		return mcp.ImageBuildInfo{}, fmt.Errorf("only a person at their own session or token starts a Windows image build: the licence statement is theirs: %w", authz.ErrDenied)
	}
	if !in.LicenseConfirmed {
		return mcp.ImageBuildInfo{}, fmt.Errorf("license_confirmed must be true: confirm that you hold a Windows Server licence that covers building and running this image; Forecrew does not provide Windows: %w", mcp.ErrInvalidInput)
	}
	p, err := s.buildProvider(ctx, by, in.Provider)
	if err != nil {
		return mcp.ImageBuildInfo{}, err
	}
	if err := fleet.CheckBuildable(p); err != nil {
		return mcp.ImageBuildInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err := fleet.ExpireImageBuilds(ctx, s.cfg.Store, s.cfg.Now(), p.ID); err != nil {
		return mcp.ImageBuildInfo{}, err
	}
	b := domain.ImageBuild{AccountID: p.AccountID, ProviderID: p.ID, Version: in.Version, ISO: in.ISO, ISOChecksum: in.ISOChecksum,
		ChocolateySHA256: in.ChocolateySHA256, WindowsImageName: in.WindowsImageName, LicenseConfirmedBy: by.String()}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ImageBuilds().Create(ctx, &b); err != nil {
			return err
		}
		if p.AccountID == uuid.Nil() {
			return nil // the organization's log has no place for an instance-wide location
		}
		return s.audit(ctx, tx, by, uuid.Nil(), fleet.ActionImageBuildRequested, "image_build", b.ID,
			map[string]any{"provider": p.Name, "version": b.Version, "iso_checksum": b.ISOChecksum, "license_confirmed": true})
	})
	switch {
	case errors.Is(err, domain.ErrInvalidRecord):
		return mcp.ImageBuildInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	case err != nil:
		return mcp.ImageBuildInfo{}, conflict(err, "this location is building an image already: wait for it to end")
	}
	return buildInfo(b, p.Name, in.WrapUntrusted), nil
}

// ListImageBuilds implements mcp.ToolService.
func (s *Service) ListImageBuilds(ctx context.Context, by domain.Actor, in mcp.ListImageBuildsInput) ([]mcp.ImageBuildInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermWorkerRead); err != nil {
		return nil, err
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("limit %d: %w", in.Limit, mcp.ErrInvalidInput)
	}
	p, err := s.buildProvider(ctx, by, in.Provider)
	if err != nil {
		return nil, err
	}
	if err := fleet.ExpireImageBuilds(ctx, s.cfg.Store, s.cfg.Now(), p.ID); err != nil {
		return nil, err
	}
	limit := DefaultImageBuildLimit
	if in.Limit > 0 {
		limit = min(in.Limit, MaxImageBuildLimit)
	}
	builds, err := s.cfg.Store.ImageBuilds().List(ctx, p.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ImageBuildInfo, len(builds))
	for i, b := range builds {
		out[i] = buildInfo(b, p.Name, in.WrapUntrusted)
	}
	return out, nil
}

// buildInfo shows b. What the agent reported in words is moved, with wrap, to
// a block marked as data: it reaches an MCP client that may be an agent.
func buildInfo(b domain.ImageBuild, provider string, wrap bool) mcp.ImageBuildInfo {
	info := mcp.ImageBuildInfo{ImageBuild: b, Provider: provider}
	if b.State == domain.ImageBuildDone {
		info.Next = nextAfterBuild
	}
	if wrap && b.Step+b.Error+b.Manifest != "" {
		raw, _ := json.Marshal(map[string]string{"step": b.Step, "error": b.Error, "manifest": b.Manifest}) // plain strings
		info.Step, info.Error, info.Manifest = "", "", ""
		info.Untrusted = contextpack.WrapUntrusted("image build of "+provider+", as its agent reported it", string(raw))
	}
	return info
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// connectedApps returns the caller's active authorizations in the organization.
func (s *Service) connectedApps(ctx context.Context, by domain.Actor) ([]mcp.ConnectedApp, error) {
	id, ok := by.SubjectID()
	if !ok || by.Kind != domain.ActorUser {
		return nil, fmt.Errorf("only a person has connected apps: %w", mcp.ErrInvalidInput)
	}
	families, err := s.cfg.Store.OAuth().ListFamilies(ctx, by.AccountID, id)
	if err != nil {
		return nil, fmt.Errorf("list connected apps: %w", err)
	}
	current := uuid.Nil()
	if by.Credential.Kind == domain.CredentialOAuthGrant {
		g, err := s.cfg.Store.OAuth().GetGrant(ctx, by.Credential.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		current = g.FamilyID
	}
	out := make([]mcp.ConnectedApp, 0, len(families))
	for _, g := range families {
		c, err := s.cfg.Store.OAuth().GetClientByID(ctx, g.ClientID)
		if err != nil {
			return nil, fmt.Errorf("read client of a connected app: %w", err)
		}
		out = append(out, mcp.ConnectedApp{ID: g.FamilyID, Name: c.Name, Client: c.ClientID, Scopes: g.Scopes, RefreshedAt: g.CreatedAt, Current: g.FamilyID == current})
	}
	return out, nil
}

// ListConnectedApps implements mcp.ToolService.
func (s *Service) ListConnectedApps(ctx context.Context, by domain.Actor) ([]mcp.ConnectedApp, error) {
	if err := s.authz.AuthorizeScope(ctx, by, domain.ScopeRead); err != nil {
		return nil, err
	}
	return s.connectedApps(ctx, by)
}

// RevokeConnectedApp implements mcp.ToolService. Like a token, the app in use
// can always end itself; another of the caller's needs a credential that may
// write, and somebody else's does not exist for the caller.
func (s *Service) RevokeConnectedApp(ctx context.Context, by domain.Actor, in mcp.ConnectedAppInput) (mcp.ConnectedApp, error) {
	var none mcp.ConnectedApp
	id, err := uuid.Parse(in.App)
	if err != nil {
		return none, fmt.Errorf("app must be the ID of a connected app: %w", mcp.ErrInvalidInput)
	}
	apps, err := s.connectedApps(ctx, by)
	if err != nil {
		return none, err
	}
	i := slices.IndexFunc(apps, func(a mcp.ConnectedApp) bool { return a.ID == id })
	if i < 0 {
		return none, fmt.Errorf("no connected app %s: %w", id, mcp.ErrNotFound)
	}
	app := apps[i]
	if !app.Current {
		if err := s.authz.AuthorizeScope(ctx, by, domain.ScopeWrite); err != nil {
			return none, err
		}
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.OAuth().RevokeFamily(ctx, app.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "oauth.revoked", "oauth_family", app.ID, map[string]any{"client": app.Client})
	})
	if err != nil {
		return none, err
	}
	return app, nil
}

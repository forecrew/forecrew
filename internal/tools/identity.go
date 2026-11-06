package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// How long invitation links and tokens last. Only the operator goes past the
// limits, with auth.token_default_ttl or forecrew token create on the server.
const (
	DefaultInvitationTTL = 7 * 24 * time.Hour
	MaxInvitationTTL     = 30 * 24 * time.Hour
	MaxTokenTTL          = 365 * 24 * time.Hour
)

// parseTTL reads an optional duration of a tool input, at most limit.
func parseTTL(in string, fallback, limit time.Duration) (time.Duration, error) {
	if in == "" {
		return fallback, nil
	}
	ttl, err := time.ParseDuration(in)
	if err != nil || ttl <= 0 {
		return 0, fmt.Errorf("ttl %q must be a positive duration such as 720h: %w", in, mcp.ErrInvalidInput)
	}
	if ttl > limit {
		return 0, fmt.Errorf("ttl %s is above the limit of %s: %w", ttl, limit, mcp.ErrInvalidInput)
	}
	return ttl, nil
}

// topScope is the highest scope class among the permissions of bound.
func topScope(bound []store.BoundRole) domain.Scope {
	top := domain.Scope("")
	for _, b := range bound {
		for _, p := range b.Role.Permissions {
			if top == "" || p.Scope().Includes(top) {
				top = p.Scope()
			}
		}
	}
	return top
}

// CreateToken implements mcp.ToolService.
func (s *Service) CreateToken(ctx context.Context, by domain.Actor, in mcp.CreateTokenInput) (mcp.CreateTokenOutput, error) {
	var out mcp.CreateTokenOutput
	if !domain.ValidName(in.Name) {
		return out, fmt.Errorf("name %q must be a lowercase name: %w", in.Name, mcp.ErrInvalidInput)
	}
	if len(in.Scopes) == 0 {
		return out, fmt.Errorf("scopes are required: %w", mcp.ErrInvalidInput)
	}
	ttl, err := parseTTL(in.TTL, s.cfg.TokenTTL, MaxTokenTTL)
	if err != nil {
		return out, err
	}
	// A leaked read token must not be able to outlive its revocation.
	if err := s.authz.AuthorizeScope(ctx, by, domain.ScopeWrite); err != nil {
		return out, err
	}
	kind, owner, err := s.tokenOwner(ctx, by, in.ServiceAccount)
	if err != nil {
		return out, err
	}
	if err := s.checkScopes(ctx, by, kind, owner, in.Scopes); err != nil {
		return out, err
	}
	secret, err := auth.NewToken(auth.PrefixToken)
	if err != nil {
		return out, err
	}
	t := domain.MCPToken{AccountID: by.AccountID, Name: in.Name, TokenHash: mcp.HashToken(secret), OwnerKind: kind, OwnerID: owner,
		Scopes: in.Scopes, ExpiresAt: s.cfg.Now().Add(ttl)}
	if err := t.Validate(); err != nil {
		return out, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.MCPTokens().Create(ctx, &t); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "token.created", "token", t.ID, map[string]any{"name": t.Name, "owner_kind": kind, "owner_id": owner.String(), "scopes": t.Scopes})
	})
	if errors.Is(err, store.ErrConflict) {
		return out, fmt.Errorf("a token named %s exists already: %w", t.Name, mcp.ErrConflict)
	}
	if err != nil {
		return out, err
	}
	return mcp.CreateTokenOutput{Token: tokenInfo(by, t), Secret: secret}, nil
}

// tokenOwner returns who a new token belongs to: the calling user, or the
// service account, which needs token.manage and every permission of its roles
// (permissions.md, No escalation).
func (s *Service) tokenOwner(ctx context.Context, by domain.Actor, serviceAccount string) (domain.SubjectKind, uuid.UUID, error) {
	if serviceAccount == "" {
		id, ok := by.SubjectID()
		if !ok || by.Kind != domain.ActorUser {
			return "", uuid.Nil(), fmt.Errorf("personal tokens are for signed-in users; name a service_account: %w", mcp.ErrInvalidInput)
		}
		return domain.SubjectUser, id, nil
	}
	if err := s.allowOrg(ctx, by, domain.PermTokenManage); err != nil {
		return "", uuid.Nil(), err
	}
	sa, err := s.serviceAccount(ctx, by.AccountID, serviceAccount)
	if err != nil {
		return "", uuid.Nil(), err
	}
	if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectServiceAccount, sa.ID); err != nil {
		return "", uuid.Nil(), err
	}
	return domain.SubjectServiceAccount, sa.ID, nil
}

// checkScopes refuses a scope above what the owner's roles allow, or above
// the token the call is made with.
func (s *Service) checkScopes(ctx context.Context, by domain.Actor, kind domain.SubjectKind, owner uuid.UUID, scopes []domain.Scope) error {
	bound, err := s.cfg.Store.RoleBindings().Effective(ctx, by.AccountID, kind, owner)
	if err != nil {
		return err
	}
	top := topScope(bound)
	var within []domain.Scope
	if c, ok := mcp.CallerFromContext(ctx); ok {
		within = c.Scopes
	}
	for _, sc := range scopes {
		if !slices.Contains(domain.Scopes(), sc) {
			return fmt.Errorf("unknown scope %q: %w", sc, mcp.ErrInvalidInput)
		}
		if !top.Includes(sc) {
			return fmt.Errorf("scope %s is above what the owner's roles allow: %w", sc, authz.ErrDenied)
		}
		if within != nil && !slices.ContainsFunc(within, func(w domain.Scope) bool { return w.Includes(sc) }) {
			return fmt.Errorf("scope %s is above the token you are using: %w", sc, authz.ErrDenied)
		}
	}
	return nil
}

// grants resolves the roles of an invitation, and refuses those by may not
// give: project.manage on a project, and every permission of the role at its
// scope (permissions.md, No escalation).
func (s *Service) grants(ctx context.Context, by domain.Actor, in []mcp.RoleGrant) ([]domain.Grant, error) {
	out := make([]domain.Grant, 0, len(in))
	for _, g := range in {
		project, err := s.optionalProject(ctx, by.AccountID, g.Project)
		if err != nil {
			return nil, err
		}
		role, err := s.roleFor(ctx, by.AccountID, g.Role, project)
		if err != nil {
			return nil, err
		}
		if project != uuid.Nil() {
			if err := s.allow(ctx, by, domain.PermProjectManage, project); err != nil {
				return nil, err
			}
		}
		if err := s.authz.Covers(ctx, by, role, authz.Resource{AccountID: by.AccountID, ProjectID: project}); err != nil {
			return nil, err
		}
		out = append(out, domain.Grant{RoleID: role.ID, ProjectID: project})
	}
	return out, nil
}

// CreateInvitation implements mcp.ToolService.
func (s *Service) CreateInvitation(ctx context.Context, by domain.Actor, in mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error) {
	var out mcp.CreateInvitationOutput
	if err := s.allowOrg(ctx, by, domain.PermMemberManage); err != nil {
		return out, err
	}
	ttl, err := parseTTL(in.TTL, DefaultInvitationTTL, MaxInvitationTTL)
	if err != nil {
		return out, err
	}
	grants, err := s.grants(ctx, by, in.Roles)
	if err != nil {
		return out, err
	}
	secret, err := auth.NewToken(auth.PrefixInvitation)
	if err != nil {
		return out, err
	}
	inv := domain.Invitation{AccountID: by.AccountID, Email: strings.TrimSpace(in.Email), GitHubLogin: strings.TrimSpace(in.GitHubLogin), Grants: grants,
		TokenHash: mcp.HashToken(secret), ExpiresAt: s.cfg.Now().Add(ttl), InvitedBy: by.String()}
	if err := inv.Validate(); err != nil {
		return out, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if inv.GitHubLogin != "" {
		if inv.GitHubUserID, err = s.gitHubUserID(ctx, inv.GitHubLogin); err != nil {
			return out, err
		}
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Invitations().Create(ctx, &inv); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "invitation.created", "invitation", inv.ID, map[string]any{"email": inv.Email, "github_login": inv.GitHubLogin, "github_user_id": inv.GitHubUserID, "roles": grants})
	})
	if err != nil {
		return out, err
	}
	return mcp.CreateInvitationOutput{Invitation: inv, URL: auth.InvitationURL(s.cfg.PublicURL, secret)}, nil
}

// gitHubUserID resolves the login of an invitation now: acceptance goes by
// the ID, because a login can be renamed and then taken by someone else.
func (s *Service) gitHubUserID(ctx context.Context, login string) (int64, error) {
	if s.cfg.GitHubUserID == nil {
		return 0, fmt.Errorf("GitHub logins cannot be looked up here: %w", mcp.ErrUnavailable)
	}
	id, err := s.cfg.GitHubUserID(ctx, login)
	if errors.Is(err, auth.ErrNoGitHubUser) {
		return 0, fmt.Errorf("GitHub has no user %s: %w", login, mcp.ErrInvalidInput)
	}
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "look up a GitHub login", "error", err)
		return 0, fmt.Errorf("GitHub cannot be reached, try again later: %w", mcp.ErrUnavailable)
	}
	return id, nil
}

// ListInvitations implements mcp.ToolService.
func (s *Service) ListInvitations(ctx context.Context, by domain.Actor) ([]domain.Invitation, error) {
	if err := s.allowOrg(ctx, by, domain.PermMemberManage); err != nil {
		return nil, err
	}
	return s.cfg.Store.Invitations().List(ctx, by.AccountID)
}

// RevokeInvitation implements mcp.ToolService.
func (s *Service) RevokeInvitation(ctx context.Context, by domain.Actor, in mcp.InvitationRefInput) (domain.Invitation, error) {
	var inv domain.Invitation
	if err := s.allowOrg(ctx, by, domain.PermMemberManage); err != nil {
		return inv, err
	}
	id, err := uuid.Parse(in.Invitation)
	if err != nil {
		return inv, fmt.Errorf("%q is not an invitation ID: %w", in.Invitation, mcp.ErrInvalidInput)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Invitations().Revoke(ctx, by.AccountID, id); err != nil {
			return err
		}
		if inv, err = tx.Invitations().Get(ctx, by.AccountID, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "invitation.revoked", "invitation", id, nil)
	})
	if errors.Is(err, store.ErrNotFound) {
		return inv, fmt.Errorf("invitation %s: %w", in.Invitation, mcp.ErrNotFound)
	}
	return inv, err
}

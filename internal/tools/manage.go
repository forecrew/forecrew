package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// CancelJob implements mcp.ToolService.
func (s *Service) CancelJob(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermJobCancel, j.ProjectID); err != nil {
		return mcp.JobInfo{}, err
	}
	if _, err := s.cfg.Jobs.Cancel(ctx, by, by.AccountID, j.ID); errors.Is(err, domain.ErrInvalidTransition) {
		return mcp.JobInfo{}, fmt.Errorf("%s is %s, only queued or running jobs can be canceled: %w", j.Ref(), j.Status, err)
	} else if err != nil {
		return mcp.JobInfo{}, err
	}
	return s.jobInfo(ctx, by, j)
}

// ownToken reports whether t is by's: the owner's, or the very token of an
// ownerless actor.
func ownToken(by domain.Actor, t domain.MCPToken) bool {
	if by.Kind == domain.ActorMCP {
		return t.OwnerKind == "" && t.Name == by.Name
	}
	id, ok := by.SubjectID()
	return ok && t.OwnerID == id && string(t.OwnerKind) == string(by.Kind)
}

// ListTokens implements mcp.ToolService: the caller's own tokens, and
// everybody's with token.manage.
func (s *Service) ListTokens(ctx context.Context, by domain.Actor) ([]mcp.TokenInfo, error) {
	if err := s.authz.AuthorizeScope(ctx, by, domain.ScopeRead); err != nil {
		return nil, err
	}
	everyone, err := s.authz.Can(ctx, by, domain.PermTokenManage, authz.Organization(by.AccountID))
	if err != nil {
		return nil, err
	}
	tokens, err := s.cfg.Store.MCPTokens().List(ctx, by.AccountID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	out := make([]mcp.TokenInfo, 0, len(tokens))
	for _, t := range tokens {
		if everyone || ownToken(by, t) {
			out = append(out, tokenInfo(by, t))
		}
	}
	return out, nil
}

// current reports whether t is the token by is calling with.
func current(by domain.Actor, t domain.MCPToken) bool {
	if by.Kind == domain.ActorMCP {
		return ownToken(by, t)
	}
	return by.Credential.Kind == domain.CredentialToken && by.Credential.ID == t.ID
}

// tokenInfo is the view of t for by.
func tokenInfo(by domain.Actor, t domain.MCPToken) mcp.TokenInfo {
	return mcp.NewTokenInfo(t, current(by, t) && t.RevokedAt.IsZero())
}

// RevokeToken implements mcp.ToolService.
func (s *Service) RevokeToken(ctx context.Context, by domain.Actor, in mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
	if in.Token == "" {
		return mcp.RevokeTokenOutput{}, fmt.Errorf("token is required: %w", mcp.ErrInvalidInput)
	}
	t, err := s.findToken(ctx, by, in.Token)
	if err != nil {
		return mcp.RevokeTokenOutput{}, err
	}
	if err := s.checkRevoke(ctx, by, t); err != nil {
		return mcp.RevokeTokenOutput{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.MCPTokens().Revoke(ctx, by.AccountID, t.ID); err != nil {
			return err
		}
		tokens, err := tx.MCPTokens().List(ctx, by.AccountID) // for its revocation time
		if err != nil {
			return err
		}
		t = tokens[slices.IndexFunc(tokens, func(x domain.MCPToken) bool { return x.ID == t.ID })]
		return s.audit(ctx, tx, by, uuid.Nil(), "token.revoked", "token", t.ID, map[string]any{"name": t.Name})
	})
	if err != nil {
		return mcp.RevokeTokenOutput{}, err
	}
	return mcp.NewRevokeTokenOutput(tokenInfo(by, t), current(by, t)), nil
}

// checkRevoke checks that by may revoke t. The token in use can always end
// itself; another of the caller's own needs a credential that may write, so a
// leaked read token cannot take its owner's others down; anybody else's needs
// token.manage.
func (s *Service) checkRevoke(ctx context.Context, by domain.Actor, t domain.MCPToken) error {
	switch {
	case current(by, t):
		return nil
	case ownToken(by, t):
		return s.authz.AuthorizeScope(ctx, by, domain.ScopeWrite)
	}
	return s.allowOrg(ctx, by, domain.PermTokenManage)
}

// findToken finds an active token by name or ID. Names repeat across owners,
// so a name means the caller's own token first.
func (s *Service) findToken(ctx context.Context, by domain.Actor, ref string) (domain.MCPToken, error) {
	tokens, err := s.cfg.Store.MCPTokens().List(ctx, by.AccountID)
	if err != nil {
		return domain.MCPToken{}, err
	}
	key := func(t domain.MCPToken) (uuid.UUID, string, bool) { return t.ID, t.Name, !t.RevokedAt.IsZero() }
	mine := slices.DeleteFunc(slices.Clone(tokens), func(t domain.MCPToken) bool { return !ownToken(by, t) })
	if t, err := store.FindActive(ref, mine, key); err == nil {
		return t, nil
	}
	t, err := store.FindActive(ref, tokens, key)
	if err != nil {
		return domain.MCPToken{}, fmt.Errorf("no active token %q: %w", ref, mcp.ErrNotFound)
	}
	return t, nil
}

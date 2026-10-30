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

// CancelJob implements mcp.ToolService.
func (s *Service) CancelJob(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	if _, err := s.cfg.Jobs.Cancel(ctx, by, by.AccountID, j.ID); errors.Is(err, domain.ErrInvalidTransition) {
		return mcp.JobInfo{}, fmt.Errorf("%s is %s, only queued or running jobs can be canceled: %w", j.Ref(), j.Status, err)
	} else if err != nil {
		return mcp.JobInfo{}, err
	}
	return s.JobStatus(ctx, by, mcp.JobRefInput{Job: j.ID.String()})
}

// ListTokens implements mcp.ToolService.
func (s *Service) ListTokens(ctx context.Context, by domain.Actor) ([]mcp.TokenInfo, error) {
	tokens, err := s.cfg.Store.MCPTokens().List(ctx, by.AccountID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	out := make([]mcp.TokenInfo, len(tokens))
	for i, t := range tokens {
		out[i] = tokenInfo(by, t)
	}
	return out, nil
}

// tokenInfo is the view of t for by, which has the token's name when it is
// the caller's own.
func tokenInfo(by domain.Actor, t domain.MCPToken) mcp.TokenInfo {
	return mcp.NewTokenInfo(t, by.Kind == domain.ActorMCP && by.Name == t.Name && t.RevokedAt.IsZero())
}

// RevokeToken implements mcp.ToolService.
func (s *Service) RevokeToken(ctx context.Context, by domain.Actor, in mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
	if in.Token == "" {
		return mcp.RevokeTokenOutput{}, fmt.Errorf("token is required: %w", mcp.ErrInvalidInput)
	}
	var t domain.MCPToken
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		tokens, err := tx.MCPTokens().List(ctx, by.AccountID)
		if err != nil {
			return err
		}
		if t, err = store.FindActive(in.Token, tokens, func(t domain.MCPToken) (uuid.UUID, string, bool) {
			return t.ID, t.Name, !t.RevokedAt.IsZero()
		}); err != nil {
			return fmt.Errorf("no active token %q: %w", in.Token, mcp.ErrNotFound)
		}
		if err := tx.MCPTokens().Revoke(ctx, by.AccountID, t.ID); err != nil {
			return err
		}
		if tokens, err = tx.MCPTokens().List(ctx, by.AccountID); err != nil { // for its revocation time
			return err
		}
		t = tokens[slices.IndexFunc(tokens, func(x domain.MCPToken) bool { return x.ID == t.ID })]
		return s.audit(ctx, tx, by, uuid.Nil(), "token.revoked", "token", t.ID, map[string]any{"name": t.Name})
	})
	if err != nil {
		return mcp.RevokeTokenOutput{}, err
	}
	return mcp.NewRevokeTokenOutput(tokenInfo(by, t), by.Kind == domain.ActorMCP && by.Name == t.Name), nil
}

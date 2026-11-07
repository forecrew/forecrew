//go:build enterprise

package ee

import (
	"context"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// ListSCIMTokens implements mcp.EnterpriseTools.
func (s *Service) ListSCIMTokens(ctx context.Context, by domain.Actor) ([]domain.SCIMToken, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return nil, err
	}
	return s.st.SCIMTokens().List(ctx, by.AccountID)
}

// coversSCIM is No escalation for a SCIM token: it adds members to every team
// with source scim and suspends every member with source scim.
func (s *Service) coversSCIM(ctx context.Context, by domain.Actor) error {
	teams, err := s.st.Teams().List(ctx, by.AccountID)
	if err != nil {
		return err
	}
	for _, t := range teams {
		if t.Source != domain.SourceSCIM {
			continue
		}
		if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectTeam, t.ID); err != nil {
			return err
		}
	}
	members, err := s.st.Memberships().List(ctx, by.AccountID, store.MembershipFilter{})
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Source != domain.SourceSCIM {
			continue
		}
		if err := s.authz.CoversHolder(ctx, by, domain.SubjectUser, m.UserID); err != nil {
			return err
		}
	}
	return nil
}

// CreateSCIMToken implements mcp.EnterpriseTools.
func (s *Service) CreateSCIMToken(ctx context.Context, by domain.Actor, in mcp.CreateSCIMTokenInput) (mcp.CreateSCIMTokenOutput, error) {
	var out mcp.CreateSCIMTokenOutput
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return out, err
	}
	if !domain.ValidName(in.Name) {
		return out, wrap("name "+in.Name+" must be a lowercase name", mcp.ErrInvalidInput)
	}
	if err := s.coversSCIM(ctx, by); err != nil {
		return out, err
	}
	a, err := s.st.Accounts().Get(ctx, by.AccountID)
	if err != nil {
		return out, err
	}
	secret, err := auth.NewToken(auth.PrefixSCIM)
	if err != nil {
		return out, err
	}
	t := domain.SCIMToken{AccountID: by.AccountID, Name: in.Name, TokenHash: mcp.HashToken(secret), CreatedBy: by.String()}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.SCIMTokens().Create(ctx, &t); err != nil {
			return err
		}
		return audit(ctx, tx, by, "scim_token.created", "scim_token", t.ID, map[string]any{"name": t.Name})
	})
	if err != nil {
		return out, storeErr(err, "a SCIM token named "+t.Name+" exists")
	}
	return mcp.CreateSCIMTokenOutput{Token: t, Secret: secret, URL: s.public + "/scim/v2/" + a.Slug}, nil
}

// RevokeSCIMToken implements mcp.EnterpriseTools.
func (s *Service) RevokeSCIMToken(ctx context.Context, by domain.Actor, in mcp.SCIMTokenRefInput) (domain.SCIMToken, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return domain.SCIMToken{}, err
	}
	all, err := s.st.SCIMTokens().List(ctx, by.AccountID)
	if err != nil {
		return domain.SCIMToken{}, err
	}
	t, err := store.FindActive(in.Token, all, func(t domain.SCIMToken) (id uuid.UUID, name string, revoked bool) {
		return t.ID, t.Name, !t.RevokedAt.IsZero()
	})
	if err != nil {
		return t, storeErr(err, "SCIM token "+in.Token)
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.SCIMTokens().Revoke(ctx, by.AccountID, t.ID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "scim_token.revoked", "scim_token", t.ID, map[string]any{"name": t.Name})
	})
	t.RevokedAt = s.now()
	return t, storeErr(err, "SCIM token "+in.Token)
}

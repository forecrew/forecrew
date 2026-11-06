package auth

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

// Resolve returns the caller of a session cookie and moves the session's idle
// expiry forward. A session that cannot be used any more wraps ErrNoSession,
// and one whose user was disabled or lost the membership is ended with the
// others concerned, whichever path made that change.
func (s *Service) Resolve(ctx context.Context, token string) (mcp.Caller, error) {
	st := s.cfg.Store
	now := s.cfg.Now()
	sess, err := st.Sessions().GetByTokenHash(ctx, mcp.HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !sess.Active(now)) {
		return mcp.Caller{}, ErrNoSession
	}
	if err != nil {
		return mcp.Caller{}, err
	}
	user, err := st.Users().Get(ctx, sess.UserID)
	if err != nil {
		return mcp.Caller{}, err
	}
	if !user.DisabledAt.IsZero() {
		return mcp.Caller{}, s.end(ctx, user.ID, uuid.Nil())
	}
	c := mcp.Caller{Actor: domain.UserActor(sess.AccountID, user.ID, domain.Credential{Kind: domain.CredentialSession, ID: sess.ID})}
	if sess.AccountID != uuid.Nil() {
		m, err := st.Memberships().Get(ctx, sess.AccountID, user.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return mcp.Caller{}, err
		}
		if err != nil || m.Status != domain.MembershipActive {
			if !user.InstanceAdmin {
				return mcp.Caller{}, s.end(ctx, user.ID, sess.AccountID)
			}
			c.BreakGlass = true
		}
	}
	if err := st.Sessions().Touch(ctx, sess.ID, earliest(now.Add(s.cfg.Auth.SessionIdle), sess.ExpiresAt)); err != nil {
		s.cfg.Logger.WarnContext(ctx, "touch session", "user_id", user.ID, "error", err)
	}
	return c, nil
}

// end revokes the user's sessions, those in accountID when it is not zero,
// and returns ErrNoSession or what went wrong.
func (s *Service) end(ctx context.Context, userID, accountID uuid.UUID) error {
	if err := s.cfg.Store.Sessions().RevokeByUser(ctx, userID, accountID); err != nil {
		return err
	}
	return ErrNoSession
}

// SignOut ends the session of a cookie; an unknown one is already gone.
func (s *Service) SignOut(ctx context.Context, token string) error {
	sess, err := s.cfg.Store.Sessions().GetByTokenHash(ctx, mcp.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.cfg.Store.Sessions().Revoke(ctx, sess.UserID, sess.ID)
}

// SignOutEverywhere ends every session of the user.
func (s *Service) SignOutEverywhere(ctx context.Context, userID uuid.UUID) error {
	return s.cfg.Store.Sessions().RevokeByUser(ctx, userID, uuid.Nil())
}

// Switch moves a session to the organization called slug, which the user must
// be an active member of. An instance admin may enter any organization to
// recover access, and that use is audited there. A session from an
// organization's connection stays in that organization.
func (s *Service) Switch(ctx context.Context, c mcp.Caller, slug string) (domain.Account, error) {
	id, ok := c.Actor.SubjectID()
	if !ok || c.Actor.Credential.Kind != domain.CredentialSession {
		return domain.Account{}, errors.New("only a session has a current organization")
	}
	var acc domain.Account
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if acc, err = tx.Accounts().GetBySlug(ctx, slug); errors.Is(err, store.ErrNotFound) {
			return ErrNotMember
		} else if err != nil {
			return err
		}
		if err := checkOrigin(ctx, tx, id, c.Actor.Credential.ID, acc.ID); err != nil {
			return err
		}
		if err := enter(ctx, tx, c, id, acc.ID); err != nil {
			return err
		}
		return tx.Sessions().SetAccount(ctx, c.Actor.Credential.ID, acc.ID)
	})
	return acc, err
}

// enter lets the user into an organization: an active member, or an instance
// admin, whose use is audited there.
func enter(ctx context.Context, tx store.Store, c mcp.Caller, userID, accountID uuid.UUID) error {
	m, err := tx.Memberships().Get(ctx, accountID, userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && m.Status == domain.MembershipActive {
		return nil
	}
	user, err := tx.Users().Get(ctx, userID)
	if err != nil {
		return err
	}
	if !user.InstanceAdmin {
		return ErrNotMember
	}
	by := domain.UserActor(accountID, userID, c.Actor.Credential)
	if err := audit(ctx, tx, by, "instance_admin.entered", "organization", accountID, nil); err != nil {
		return fmt.Errorf("audit instance admin: %w", err)
	}
	return nil
}

// checkOrigin refuses to move a session out of the organization whose
// connection signed it in.
func checkOrigin(ctx context.Context, st store.Store, userID, sessionID, accountID uuid.UUID) error {
	limit, err := sessionLimit(ctx, st, userID, sessionID)
	if err != nil {
		return err
	}
	if limit != uuid.Nil() && limit != accountID {
		return ErrNotMember
	}
	return nil
}

// sessionLimit returns the organization a session is limited to: that of the
// connection that signed it in, zero for an instance connection.
func sessionLimit(ctx context.Context, st store.Store, userID, sessionID uuid.UUID) (uuid.UUID, error) {
	sessions, err := st.Sessions().ListByUser(ctx, userID)
	if err != nil {
		return uuid.Nil(), err
	}
	i := slices.IndexFunc(sessions, func(x domain.Session) bool { return x.ID == sessionID })
	if i < 0 {
		return uuid.Nil(), ErrNoSession
	}
	if sessions[i].ConnectionID == uuid.Nil() {
		return uuid.Nil(), nil
	}
	conn, err := st.Connections().Get(ctx, sessions[i].ConnectionID)
	return conn.AccountID, err
}

// Organization is an organization a user is an active member of.
type Organization struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

// Profile returns the user of c and the organizations they can switch to: a
// session from an organization's connection only has that one.
func (s *Service) Profile(ctx context.Context, c mcp.Caller) (domain.User, []Organization, error) {
	userID, ok := c.Actor.SubjectID()
	if !ok {
		return domain.User{}, nil, store.ErrNotFound
	}
	u, err := s.cfg.Store.Users().Get(ctx, userID)
	if err != nil {
		return u, nil, err
	}
	limit := uuid.Nil()
	if c.Actor.Credential.Kind == domain.CredentialSession {
		if limit, err = sessionLimit(ctx, s.cfg.Store, userID, c.Actor.Credential.ID); err != nil {
			return u, nil, err
		}
	}
	memberships, err := s.cfg.Store.Memberships().ListByUser(ctx, userID)
	if err != nil {
		return u, nil, err
	}
	orgs := []Organization{}
	for _, m := range memberships {
		if m.Status != domain.MembershipActive || (limit != uuid.Nil() && limit != m.AccountID) {
			continue
		}
		a, err := s.cfg.Store.Accounts().Get(ctx, m.AccountID)
		if err != nil {
			return u, nil, err
		}
		orgs = append(orgs, Organization{ID: a.ID, Name: a.Name, Slug: a.Slug})
	}
	return u, orgs, nil
}

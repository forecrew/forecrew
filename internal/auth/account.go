package auth

import (
	"context"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrNotFound means the session or identity is not the caller's.
var ErrNotFound = errors.New("not found")

// SessionInfo is a session of the calling user as they see it.
type SessionInfo struct {
	ID           uuid.UUID `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	UserAgent    string    `json:"user_agent,omitempty"`
	IP           string    `json:"ip,omitempty"`
	Organization string    `json:"organization,omitempty"` // slug of its current organization
	Connection   string    `json:"connection,omitempty"`   // name of the connection that signed it in
	// Current is the session of this request.
	Current bool `json:"current"`
}

// Sessions lists the active sessions of the calling user, newest first.
func (s *Service) Sessions(ctx context.Context, c mcp.Caller) ([]SessionInfo, error) {
	userID, ok := c.Actor.SubjectID()
	if !ok {
		return nil, ErrNotFound
	}
	sessions, err := s.cfg.Store.Sessions().ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	out := []SessionInfo{}
	for _, x := range sessions {
		if !x.Active(now) {
			continue
		}
		info := SessionInfo{ID: x.ID, CreatedAt: x.CreatedAt, LastSeenAt: x.LastSeenAt, ExpiresAt: x.ExpiresAt, UserAgent: x.UserAgent, IP: x.IP,
			Current: c.Actor.Credential.Kind == domain.CredentialSession && c.Actor.Credential.ID == x.ID}
		if x.AccountID != uuid.Nil() {
			if a, err := s.cfg.Store.Accounts().Get(ctx, x.AccountID); err == nil {
				info.Organization = a.Slug
			}
		}
		if x.ConnectionID != uuid.Nil() {
			if conn, err := s.cfg.Store.Connections().Get(ctx, x.ConnectionID); err == nil {
				info.Connection = conn.Name
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b SessionInfo) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// EndSession ends one of the calling user's sessions; ErrNotFound when it is
// not theirs or is over.
func (s *Service) EndSession(ctx context.Context, c mcp.Caller, id uuid.UUID) error {
	userID, ok := c.Actor.SubjectID()
	if !ok {
		return ErrNotFound
	}
	sessions, err := s.cfg.Store.Sessions().ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	if !slices.ContainsFunc(sessions, func(x domain.Session) bool { return x.ID == id && x.Active(now) }) {
		return ErrNotFound
	}
	return s.cfg.Store.Sessions().Revoke(ctx, userID, id)
}

// IdentityInfo is a sign-in linked to the calling user.
type IdentityInfo struct {
	ID         uuid.UUID `json:"id"`
	Connection string    `json:"connection"` // name
	Kind       string    `json:"kind"`
	Login      string    `json:"login,omitempty"`
	Email      string    `json:"email,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
}

// Identities lists the sign-ins linked to the calling user.
func (s *Service) Identities(ctx context.Context, c mcp.Caller) ([]IdentityInfo, error) {
	userID, ok := c.Actor.SubjectID()
	if !ok {
		return nil, ErrNotFound
	}
	identities, err := s.cfg.Store.Identities().ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]IdentityInfo, 0, len(identities))
	for _, i := range identities {
		conn, err := s.cfg.Store.Connections().Get(ctx, i.ConnectionID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, IdentityInfo{ID: i.ID, Connection: conn.Name, Kind: string(conn.Kind), Login: i.Login, Email: i.Email, LastUsedAt: i.LastUsedAt, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

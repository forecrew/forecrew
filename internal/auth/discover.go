package auth

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// DiscoverInput is what a sign-in page knows: the organization of the
// /login?org=<slug> link, the email the person typed, the invitation token of
// their link. All are optional.
type DiscoverInput struct {
	Org    string `json:"org,omitempty"`
	Email  string `json:"email,omitempty"`
	Invite string `json:"invite,omitempty"`
}

// ConnectionInfo is a sign-in button: never the client ID, issuer or secret
// reference.
type ConnectionInfo struct {
	ID       uuid.UUID             `json:"id"`
	Kind     domain.ConnectionKind `json:"kind"`
	Name     string                `json:"name"`
	LoginURL string                `json:"login_url"`
}

// Discovery is what a sign-in page shows. With Redirect the page sends the
// person straight there: their email's domain enforces that connection.
type Discovery struct {
	Connections []ConnectionInfo `json:"connections"`
	Redirect    string           `json:"redirect,omitempty"`
	TokenLogin  bool             `json:"token_login"`
}

// Discover returns the instance connections plus those of the organization
// the input points at.
func (s *Service) Discover(ctx context.Context, in DiscoverInput) (Discovery, error) {
	st := s.cfg.Store
	out := Discovery{Connections: []ConnectionInfo{}, TokenLogin: s.cfg.Auth.TokenLogin}
	account, err := s.pointedAt(ctx, in)
	if err != nil {
		return out, err
	}
	var enforced domain.VerifiedDomain
	if in.Email != "" {
		d, err := st.VerifiedDomains().GetVerified(ctx, emailDomain(in.Email))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		if err == nil {
			account = d.AccountID
			if d.EnforceSSO {
				enforced = d
			}
		}
	}
	conns, err := st.Connections().List(ctx, uuid.Nil())
	if err != nil {
		return out, err
	}
	if account != uuid.Nil() {
		own, err := st.Connections().List(ctx, account)
		if err != nil {
			return out, err
		}
		conns = append(conns, own...)
	}
	for _, c := range slices.DeleteFunc(conns, func(c domain.Connection) bool { return !c.Enabled }) {
		info := ConnectionInfo{ID: c.ID, Kind: c.Kind, Name: c.Name, LoginURL: loginURL(c.ID, in)}
		out.Connections = append(out.Connections, info)
		if c.ID == enforced.ConnectionID {
			out.Redirect = info.LoginURL
		}
	}
	return out, nil
}

// pointedAt returns the organization of the invitation or slug; zero when
// neither names one, so an unknown slug shows only the instance connections.
func (s *Service) pointedAt(ctx context.Context, in DiscoverInput) (uuid.UUID, error) {
	if in.Invite != "" {
		inv, err := s.cfg.Store.Invitations().GetByTokenHash(ctx, mcp.HashToken(in.Invite))
		if err == nil && inv.Pending(s.cfg.Now()) {
			return inv.AccountID, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return uuid.Nil(), err
		}
	}
	if in.Org != "" {
		a, err := s.cfg.Store.Accounts().GetBySlug(ctx, in.Org)
		if err == nil {
			return a.ID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return uuid.Nil(), err
		}
	}
	return uuid.Nil(), nil
}

func loginURL(id uuid.UUID, in DiscoverInput) string {
	q := url.Values{}
	if in.Org != "" {
		q.Set("org", in.Org)
	}
	if in.Invite != "" {
		q.Set("invite", in.Invite)
	}
	u := "/auth/" + id.String() + "/login"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// InvitationURL is the link of an invitation.
func InvitationURL(baseURL, token string) string {
	return strings.TrimRight(baseURL, "/") + "/login?invite=" + url.QueryEscape(token)
}

//go:build enterprise

// Package ee holds the enterprise features of Forecrew: verified domains and
// SSO enforcement, just-in-time provisioning, group mapping, SCIM and custom
// roles (docs/enterprise.md). It builds only with the tag enterprise and
// reaches open core through the interfaces of internal/enterprise.
package ee

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/enterprise"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

func init() {
	enterprise.Register(func(d enterprise.Deps) enterprise.Features {
		s := New(d, net.DefaultResolver)
		return enterprise.Features{Tools: s, SignIn: s, SCIM: s.SCIM(), Run: s.Run}
	})
}

// Resolver looks up DNS TXT records; net.Resolver is one.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Service implements the enterprise features.
type Service struct {
	st     store.Store
	authz  *authz.Engine
	gates  gates.Service
	dns    Resolver
	log    *slog.Logger
	now    func() time.Time
	public string
}

var (
	_ mcp.EnterpriseTools = (*Service)(nil)
	_ auth.Policy         = (*Service)(nil)
)

// New returns the service. Gates may be nil: then only the audit log and the
// server log tell of a lost domain.
func New(d enterprise.Deps, dns Resolver) *Service {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{st: d.Store, authz: d.Authz, gates: d.Gates, dns: dns, log: d.Logger, now: d.Now, public: strings.TrimRight(d.PublicURL, "/")}
}

// allow checks that by holds p on the organization.
func (s *Service) allow(ctx context.Context, by domain.Actor, p domain.Permission) error {
	return s.authz.Authorize(ctx, by, p, authz.Organization(by.AccountID))
}

// audit records what by did. Secrets never go in data.
func audit(ctx context.Context, st store.Store, by domain.Actor, action, subject string, id uuid.UUID, data map[string]any) error {
	raw, err := marshal(data)
	if err != nil {
		return err
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{AccountID: by.AccountID, Actor: by.String(), Credential: by.Credential.String(),
		Action: action, SubjectType: subject, SubjectID: id, Data: raw})
}

// storeErr turns store errors into what callers see.
func storeErr(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return wrap(what, mcp.ErrNotFound)
	case errors.Is(err, store.ErrLastOwner):
		return wrap(err.Error(), mcp.ErrConflict)
	case errors.Is(err, store.ErrConflict):
		return wrap(what, mcp.ErrConflict)
	}
	return err
}

// connection finds a connection the organization can use by UUID or name:
// its own, then the instance's.
func (s *Service) connection(ctx context.Context, accountID uuid.UUID, ref string) (domain.Connection, error) {
	for _, owner := range []uuid.UUID{accountID, uuid.Nil()} {
		conns, err := s.st.Connections().List(ctx, owner)
		if err != nil {
			return domain.Connection{}, err
		}
		for _, c := range conns {
			if c.Name == ref || c.ID.String() == ref {
				return c, nil
			}
		}
	}
	return domain.Connection{}, wrap("connection "+ref, mcp.ErrNotFound)
}

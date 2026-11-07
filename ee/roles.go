//go:build enterprise

package ee

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// covers checks that by holds every permission of r on the organization: a
// role can be bound anywhere, so holding it on one project is not enough.
func (s *Service) covers(ctx context.Context, by domain.Actor, r domain.Role) error {
	return s.authz.Covers(ctx, by, r, authz.Organization(by.AccountID))
}

// CreateRole implements mcp.EnterpriseTools.
func (s *Service) CreateRole(ctx context.Context, by domain.Actor, in mcp.CreateRoleInput) (domain.Role, error) {
	if err := s.allow(ctx, by, domain.PermRoleManage); err != nil {
		return domain.Role{}, err
	}
	r := domain.Role{AccountID: by.AccountID, Name: in.Name, Description: in.Description, Scope: in.Scope, Permissions: slices.Sorted(slices.Values(in.Permissions)), Custom: true}
	if err := r.Validate(); err != nil {
		return r, wrap(err.Error(), mcp.ErrInvalidInput)
	}
	if err := s.covers(ctx, by, r); err != nil {
		return r, err
	}
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Roles().Create(ctx, &r); err != nil {
			return err
		}
		return audit(ctx, tx, by, "role.created", "role", r.ID, map[string]any{"name": r.Name, "scope": r.Scope, "permissions": r.Permissions})
	})
	return r, storeErr(err, "a role named "+r.Name+" exists")
}

// customRole finds a custom role of the organization by UUID or name.
func (s *Service) customRole(ctx context.Context, accountID uuid.UUID, ref string) (domain.Role, error) {
	roles, err := s.st.Roles().List(ctx, accountID)
	if err != nil {
		return domain.Role{}, err
	}
	found := slices.DeleteFunc(roles, func(r domain.Role) bool { return !r.Custom || (r.ID.String() != ref && r.Name != ref) })
	switch len(found) {
	case 0:
		return domain.Role{}, wrap("custom role "+ref, mcp.ErrNotFound)
	case 1:
		return found[0], nil
	}
	return domain.Role{}, wrap("an organization and a project role are called "+ref+": name it by UUID", mcp.ErrInvalidInput)
}

// UpdateRole implements mcp.EnterpriseTools.
func (s *Service) UpdateRole(ctx context.Context, by domain.Actor, in mcp.UpdateRoleInput) (domain.Role, error) {
	if err := s.allow(ctx, by, domain.PermRoleManage); err != nil {
		return domain.Role{}, err
	}
	r, err := s.customRole(ctx, by.AccountID, in.Role)
	if err != nil {
		return r, err
	}
	if err := s.covers(ctx, by, r); err != nil {
		return r, err
	}
	r.Name = cmp.Or(in.Name, r.Name)
	if in.Description != nil {
		r.Description = *in.Description
	}
	if in.Permissions != nil {
		r.Permissions = slices.Sorted(slices.Values(*in.Permissions))
	}
	if err := r.Validate(); err != nil {
		return r, wrap(err.Error(), mcp.ErrInvalidInput)
	}
	if err := s.covers(ctx, by, r); err != nil {
		return r, err
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Roles().Update(ctx, &r); err != nil {
			return err
		}
		return audit(ctx, tx, by, "role.updated", "role", r.ID, map[string]any{"name": r.Name, "permissions": r.Permissions})
	})
	return r, storeErr(err, "a role named "+r.Name+" exists")
}

// inUse names what holds on to a role besides its bindings, which the store
// refuses by itself: a pending invitation or a connection's JIT defaults.
func (s *Service) inUse(ctx context.Context, tx store.Store, accountID, roleID uuid.UUID, now time.Time) ([]string, error) {
	var out []string
	invitations, err := tx.Invitations().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, inv := range invitations {
		if inv.Pending(now) && slices.ContainsFunc(inv.Grants, func(g domain.Grant) bool { return g.RoleID == roleID }) {
			out = append(out, "invitation "+inv.ID.String())
		}
	}
	conns, err := tx.Connections().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if slices.Contains(c.JIT.DefaultRoles, roleID) {
			out = append(out, "connection "+c.Name)
		}
	}
	return out, nil
}

// DeleteRole implements mcp.EnterpriseTools.
func (s *Service) DeleteRole(ctx context.Context, by domain.Actor, in mcp.RoleRefInput) (domain.Role, error) {
	if err := s.allow(ctx, by, domain.PermRoleManage); err != nil {
		return domain.Role{}, err
	}
	r, err := s.customRole(ctx, by.AccountID, in.Role)
	if err != nil {
		return r, err
	}
	if err := s.covers(ctx, by, r); err != nil {
		return r, err
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		users, err := s.inUse(ctx, tx, by.AccountID, r.ID, s.now())
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return wrap("role "+r.Name+" is used by "+strings.Join(users, ", "), mcp.ErrConflict)
		}
		if err := tx.Roles().Delete(ctx, by.AccountID, r.ID); err != nil {
			return err
		}
		return audit(ctx, tx, by, "role.deleted", "role", r.ID, map[string]any{"name": r.Name})
	})
	return r, storeErr(err, "role "+r.Name+" is bound")
}

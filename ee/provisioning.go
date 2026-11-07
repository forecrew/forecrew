//go:build enterprise

package ee

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Provisions implements auth.Policy.
func (s *Service) Provisions(c domain.Connection) bool { return c.JIT.Enabled }

// Admit implements auth.Policy: the membership of a first sign-in through a
// connection with JIT, then the teams of the groups the provider names.
func (s *Service) Admit(ctx context.Context, tx store.Store, conn domain.Connection, c auth.Claims, u domain.User) error {
	if conn.JIT.Enabled {
		if err := provision(ctx, tx, conn, u); err != nil {
			return err
		}
	}
	if conn.GroupClaim == "" || c.Groups == nil {
		return nil
	}
	return syncGroups(ctx, tx, conn, c.Groups, u)
}

// provision creates the membership with the default roles. Whoever is a
// member already, suspended ones included, is left as they are.
func provision(ctx context.Context, tx store.Store, conn domain.Connection, u domain.User) error {
	_, err := tx.Memberships().Get(ctx, conn.AccountID, u.ID)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return err
	}
	grants := make([]domain.Grant, len(conn.JIT.DefaultRoles))
	for i, id := range conn.JIT.DefaultRoles {
		grants[i] = domain.Grant{RoleID: id}
	}
	return auth.Join(ctx, tx, conn.AccountID, u.ID, domain.SourceJIT, grants, domain.SystemActor(conn.AccountID, "jit"))
}

// syncGroups makes the user's idp membership of each team mapped through conn
// match groups, in every organization where the user is an active member.
// Manual and scim members are left alone, and so are teams mapped through
// other connections only.
func syncGroups(ctx context.Context, tx store.Store, conn domain.Connection, groups []string, u domain.User) error {
	mappings, err := tx.Connections().GroupMappings(ctx, conn.ID)
	if err != nil {
		return err
	}
	type target struct{ account, team uuid.UUID }
	want := map[target]bool{}
	for _, m := range mappings {
		k := target{m.AccountID, m.TeamID}
		want[k] = want[k] || slices.Contains(groups, m.ExternalGroup)
	}
	for k, in := range want {
		if err := syncTeam(ctx, tx, k.account, k.team, u.ID, in); err != nil {
			return err
		}
	}
	return nil
}

// syncTeam adds an active member to a team or takes them out, if group
// mapping put them there.
func syncTeam(ctx context.Context, tx store.Store, accountID, teamID, userID uuid.UUID, in bool) error {
	m, err := tx.Memberships().Get(ctx, accountID, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.Status != domain.MembershipActive) {
		return nil
	}
	if err != nil {
		return err
	}
	members, err := tx.Teams().Members(ctx, accountID, teamID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(members, func(x domain.TeamMember) bool { return x.UserID == userID })
	action := ""
	switch {
	case in && i < 0:
		action = "team.member_added"
		err = tx.Teams().AddMember(ctx, &domain.TeamMember{TeamID: teamID, UserID: userID, AccountID: accountID, Source: domain.SourceIDP})
	case !in && i >= 0 && members[i].Source == domain.SourceIDP:
		action = "team.member_removed"
		err = tx.Teams().RemoveMember(ctx, accountID, teamID, userID)
	}
	if err != nil || action == "" {
		return err
	}
	return audit(ctx, tx, domain.SystemActor(accountID, "idp"), action, "team", teamID, map[string]any{"user": userID})
}

// defaultRole finds an organization role for a JIT default.
func (s *Service) defaultRole(ctx context.Context, accountID uuid.UUID, name string) (domain.Role, error) {
	r, err := s.st.Roles().GetByName(ctx, accountID, domain.RoleScopeOrganization, name)
	if errors.Is(err, store.ErrNotFound) {
		return r, wrap("no organization role "+name, mcp.ErrInvalidInput)
	}
	return r, err
}

// coversConnection is No escalation for what a connection trusts: the roles
// of its JIT defaults and of the teams mapped to it.
func (s *Service) coversConnection(ctx context.Context, by domain.Actor, c domain.Connection) error {
	for _, id := range c.JIT.DefaultRoles {
		r, err := s.st.Roles().Get(ctx, by.AccountID, id)
		if err != nil {
			return err
		}
		if err := s.covers(ctx, by, r); err != nil {
			return err
		}
	}
	mappings, err := s.st.Connections().GroupMappings(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.AccountID != by.AccountID {
			continue
		}
		if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectTeam, m.TeamID); err != nil {
			return err
		}
	}
	return nil
}

// SetConnectionProvisioning implements mcp.EnterpriseTools.
func (s *Service) SetConnectionProvisioning(ctx context.Context, by domain.Actor, in mcp.SetProvisioningInput) (mcp.ProvisioningInfo, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.ProvisioningInfo{}, err
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return mcp.ProvisioningInfo{}, err
	}
	if c.AccountID != by.AccountID {
		return mcp.ProvisioningInfo{}, wrap("connection "+c.Name+" comes from the server file", mcp.ErrInvalidInput)
	}
	if err := s.coversConnection(ctx, by, c); err != nil {
		return mcp.ProvisioningInfo{}, err
	}
	if in.JITEnabled != nil {
		c.JIT.Enabled = *in.JITEnabled
	}
	if in.GroupClaim != nil {
		c.GroupClaim = *in.GroupClaim
	}
	if in.DefaultRoles != nil {
		c.JIT.DefaultRoles = []uuid.UUID{}
		for _, name := range *in.DefaultRoles {
			r, err := s.defaultRole(ctx, by.AccountID, name)
			if err != nil {
				return mcp.ProvisioningInfo{}, err
			}
			if err := s.covers(ctx, by, r); err != nil {
				return mcp.ProvisioningInfo{}, err
			}
			c.JIT.DefaultRoles = append(c.JIT.DefaultRoles, r.ID)
		}
	}
	if err := c.Validate(); err != nil {
		return mcp.ProvisioningInfo{}, wrap(err.Error(), mcp.ErrInvalidInput)
	}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().Update(ctx, &c); err != nil {
			return err
		}
		return audit(ctx, tx, by, "connection.provisioning_changed", "connection", c.ID, map[string]any{"jit": c.JIT, "group_claim": c.GroupClaim})
	})
	return mcp.ProvisioningInfo{ConnectionID: c.ID, Name: c.Name, JIT: c.JIT, GroupClaim: c.GroupClaim}, storeErr(err, "connection "+c.Name)
}

// ListGroupMappings implements mcp.EnterpriseTools.
func (s *Service) ListGroupMappings(ctx context.Context, by domain.Actor) ([]domain.GroupMapping, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return nil, err
	}
	return s.mappings(ctx, s.st, by.AccountID)
}

// mappings returns the organization's mappings on every connection it can
// use, oldest first.
func (s *Service) mappings(ctx context.Context, st store.Store, accountID uuid.UUID) ([]domain.GroupMapping, error) {
	out := []domain.GroupMapping{}
	for _, owner := range []uuid.UUID{accountID, uuid.Nil()} {
		conns, err := st.Connections().List(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, c := range conns {
			all, err := st.Connections().GroupMappings(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			for _, m := range all {
				if m.AccountID == accountID {
					out = append(out, m)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b domain.GroupMapping) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID.String(), b.ID.String()))
	})
	return out, nil
}

// team finds a team by UUID or slug.
func (s *Service) team(ctx context.Context, accountID uuid.UUID, ref string) (domain.Team, error) {
	var t domain.Team
	var err error
	if id, perr := uuid.Parse(ref); perr == nil {
		t, err = s.st.Teams().Get(ctx, accountID, id)
	} else {
		t, err = s.st.Teams().GetBySlug(ctx, accountID, ref)
	}
	return t, storeErr(err, "team "+ref)
}

// AddGroupMapping implements mcp.EnterpriseTools. Everyone in the group joins
// the team at sign-in, so it needs every permission of the team's roles.
func (s *Service) AddGroupMapping(ctx context.Context, by domain.Actor, in mcp.AddGroupMappingInput) (domain.GroupMapping, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return domain.GroupMapping{}, err
	}
	if in.Group == "" {
		return domain.GroupMapping{}, wrap("group is required", mcp.ErrInvalidInput)
	}
	c, err := s.connection(ctx, by.AccountID, in.Connection)
	if err != nil {
		return domain.GroupMapping{}, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return domain.GroupMapping{}, err
	}
	if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectTeam, t.ID); err != nil {
		return domain.GroupMapping{}, err
	}
	m := domain.GroupMapping{AccountID: by.AccountID, ConnectionID: c.ID, ExternalGroup: in.Group, TeamID: t.ID}
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().AddGroupMapping(ctx, &m); err != nil {
			return err
		}
		return audit(ctx, tx, by, "group_mapping.added", "team", t.ID, map[string]any{"connection": c.Name, "group": in.Group})
	})
	return m, storeErr(err, "group "+in.Group+" is mapped to that team")
}

// RemoveGroupMapping implements mcp.EnterpriseTools.
func (s *Service) RemoveGroupMapping(ctx context.Context, by domain.Actor, in mcp.GroupMappingRefInput) (domain.GroupMapping, error) {
	if err := s.allow(ctx, by, domain.PermConnectionManage); err != nil {
		return domain.GroupMapping{}, err
	}
	all, err := s.mappings(ctx, s.st, by.AccountID)
	if err != nil {
		return domain.GroupMapping{}, err
	}
	i := slices.IndexFunc(all, func(m domain.GroupMapping) bool { return m.ID.String() == in.Mapping })
	if i < 0 {
		return domain.GroupMapping{}, wrap("mapping "+in.Mapping, mcp.ErrNotFound)
	}
	m := all[i]
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().RemoveGroupMapping(ctx, by.AccountID, m.ID); err != nil {
			return err
		}
		if err := audit(ctx, tx, by, "group_mapping.removed", "team", m.TeamID, map[string]any{"group": m.ExternalGroup}); err != nil {
			return err
		}
		left, err := s.mappings(ctx, tx, by.AccountID)
		if err != nil || slices.ContainsFunc(left, func(x domain.GroupMapping) bool { return x.TeamID == m.TeamID }) {
			return err
		}
		return dropIDPMembers(ctx, tx, by.AccountID, m.TeamID)
	})
	return m, storeErr(err, "mapping "+in.Mapping)
}

// dropIDPMembers removes the members group mapping added: without a mapping
// no sign-in would ever take them out.
func dropIDPMembers(ctx context.Context, tx store.Store, accountID, teamID uuid.UUID) error {
	members, err := tx.Teams().Members(ctx, accountID, teamID)
	if err != nil {
		return err
	}
	for _, x := range members {
		if x.Source != domain.SourceIDP {
			continue
		}
		if err := tx.Teams().RemoveMember(ctx, accountID, teamID, x.UserID); err != nil {
			return err
		}
	}
	return nil
}

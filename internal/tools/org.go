package tools

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// ListMembers implements mcp.ToolService.
func (s *Service) ListMembers(ctx context.Context, by domain.Actor) ([]mcp.MemberInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgRead); err != nil {
		return nil, err
	}
	return s.members(ctx, by, uuid.Nil())
}

// members describes the members of the organization, or only the one named
// by only. Emails need member.manage; a project the caller cannot read is not
// named, as in list_role_bindings.
func (s *Service) members(ctx context.Context, by domain.Actor, only uuid.UUID) ([]mcp.MemberInfo, error) {
	st := s.cfg.Store
	seeEmail, err := s.authz.Can(ctx, by, domain.PermMemberManage, authz.Organization(by.AccountID))
	if err != nil {
		return nil, err
	}
	memberships, err := st.Memberships().List(ctx, by.AccountID, store.MembershipFilter{})
	if err != nil {
		return nil, err
	}
	if only != uuid.Nil() {
		memberships = slices.DeleteFunc(memberships, func(m domain.Membership) bool { return m.UserID != only })
	}
	ids := make([]uuid.UUID, len(memberships))
	for i, m := range memberships {
		ids[i] = m.UserID
	}
	users, err := st.Users().ListByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	bindings, err := st.RoleBindings().List(ctx, by.AccountID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser})
	if err != nil {
		return nil, err
	}
	roles, err := st.Roles().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	projects, err := st.Projects().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	visible, err := s.authz.Projects(ctx, by, domain.PermProjectRead)
	if err != nil {
		return nil, err
	}
	teams, err := s.teamsOfUsers(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.MemberInfo, 0, len(memberships))
	for _, m := range memberships {
		i := slices.IndexFunc(users, func(u domain.User) bool { return u.ID == m.UserID })
		if i < 0 {
			continue
		}
		u := users[i]
		info := mcp.MemberInfo{UserID: u.ID, Name: u.Name, Status: m.Status, Source: m.Source, Disabled: !u.DisabledAt.IsZero(),
			Roles: []mcp.MemberRole{}, Teams: teams[u.ID], LastSeenAt: u.LastSeenAt, CreatedAt: m.CreatedAt}
		if info.Teams == nil {
			info.Teams = []string{}
		}
		if seeEmail {
			info.Email = u.Email
		}
		for _, b := range bindings {
			if b.SubjectID != u.ID || (b.ProjectID != uuid.Nil() && !visible.Has(b.ProjectID)) {
				continue
			}
			r := mcp.MemberRole{}
			if j := slices.IndexFunc(roles, func(x domain.Role) bool { return x.ID == b.RoleID }); j >= 0 {
				r.Role, r.Scope = roles[j].Name, roles[j].Scope
			}
			if j := slices.IndexFunc(projects, func(p domain.Project) bool { return p.ID == b.ProjectID }); j >= 0 {
				r.Project = projects[j].Name
			}
			info.Roles = append(info.Roles, r)
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b mcp.MemberInfo) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.UserID.String(), b.UserID.String()))
	})
	return out, nil
}

// teamsOfUsers returns the slugs of the teams each user is in.
func (s *Service) teamsOfUsers(ctx context.Context, accountID uuid.UUID) (map[uuid.UUID][]string, error) {
	teams, err := s.cfg.Store.Teams().List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]string{}
	for _, t := range teams {
		members, err := s.cfg.Store.Teams().Members(ctx, accountID, t.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			out[m.UserID] = append(out[m.UserID], t.Slug)
		}
	}
	return out, nil
}

// member finds the membership and description of the member named by ref,
// who must hold nothing the caller lacks (No escalation).
func (s *Service) member(ctx context.Context, by domain.Actor, ref string) (mcp.MemberInfo, error) {
	id, err := parseUser(ref)
	if err != nil {
		return mcp.MemberInfo{}, err
	}
	infos, err := s.members(ctx, by, id)
	if err != nil {
		return mcp.MemberInfo{}, err
	}
	if len(infos) == 0 {
		return mcp.MemberInfo{}, fmt.Errorf("member %s: %w", ref, mcp.ErrNotFound)
	}
	if err := s.coversHolder(ctx, by, domain.SubjectUser, id); err != nil {
		return mcp.MemberInfo{}, err
	}
	return infos[0], nil
}

// endAccess ends the sessions of a member in the organization and revokes
// their personal tokens there, in the transaction of the change that needs it.
func endAccess(ctx context.Context, tx store.Store, accountID, userID uuid.UUID) error {
	if err := tx.Sessions().RevokeByUser(ctx, userID, accountID); err != nil {
		return err
	}
	return tx.MCPTokens().RevokeByOwner(ctx, accountID, domain.SubjectUser, userID)
}

// SetMemberSuspended implements mcp.ToolService.
func (s *Service) SetMemberSuspended(ctx context.Context, by domain.Actor, in mcp.SetMemberInput) (mcp.MemberInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermMemberManage); err != nil {
		return mcp.MemberInfo{}, err
	}
	m, err := s.member(ctx, by, in.User)
	if err != nil {
		return m, err
	}
	status, action := domain.MembershipActive, "member.resumed"
	if in.Suspended {
		status, action = domain.MembershipSuspended, "member.suspended"
	} else if m.Status != domain.MembershipSuspended {
		return m, fmt.Errorf("%s is not suspended: %w", m.Name, mcp.ErrConflict)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Memberships().SetStatus(ctx, by.AccountID, m.UserID, status); err != nil {
			return err
		}
		if in.Suspended {
			if err := endAccess(ctx, tx, by.AccountID, m.UserID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, by, uuid.Nil(), action, "user", m.UserID, map[string]any{"name": m.Name})
	})
	if err != nil {
		return m, notFound(conflict(err, "member "+in.User), "member "+in.User)
	}
	m.Status = status
	return m, nil
}

// RemoveMember implements mcp.ToolService.
func (s *Service) RemoveMember(ctx context.Context, by domain.Actor, in mcp.MemberRefInput) (mcp.MemberInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermMemberManage); err != nil {
		return mcp.MemberInfo{}, err
	}
	m, err := s.member(ctx, by, in.User)
	if err != nil {
		return m, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Memberships().Remove(ctx, by.AccountID, m.UserID); err != nil {
			return err
		}
		if err := endAccess(ctx, tx, by.AccountID, m.UserID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "member.removed", "user", m.UserID, map[string]any{"name": m.Name})
	})
	if err != nil {
		return m, notFound(conflict(err, "member "+in.User), "member "+in.User)
	}
	return m, nil
}

// UpdateOrganization implements mcp.ToolService.
func (s *Service) UpdateOrganization(ctx context.Context, by domain.Actor, in mcp.UpdateOrganizationInput) (domain.Account, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgManage); err != nil {
		return domain.Account{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" && in.Slug == "" {
		return domain.Account{}, fmt.Errorf("name or slug is required: %w", mcp.ErrInvalidInput)
	}
	a, err := s.cfg.Store.Accounts().Get(ctx, by.AccountID)
	if err != nil {
		return a, notFound(err, "organization")
	}
	was := a.Slug
	a.Name, a.Slug = cmp.Or(name, a.Name), cmp.Or(in.Slug, a.Slug)
	if err := checkSlug(a.Slug); err != nil {
		return a, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Accounts().Rename(ctx, a.ID, a.Name, a.Slug); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "organization.updated", "organization", a.ID, map[string]any{"name": a.Name, "slug": a.Slug, "was_slug": was})
	})
	return a, conflict(err, "an organization with slug "+a.Slug+" exists")
}

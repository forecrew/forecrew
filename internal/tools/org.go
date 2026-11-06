package tools

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
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
	seeEmail, err := s.authz.Can(ctx, by, domain.PermMemberManage, authz.Organization(by.AccountID))
	if err != nil {
		return nil, err
	}
	d, err := s.memberData(ctx, by, only)
	if err != nil {
		return nil, err
	}
	memberships, users, bindings, roles, projects, visible, teams := d.memberships, d.users, d.bindings, d.roles, d.projects, d.visible, d.teams
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
			if b.SubjectID == u.ID && (b.ProjectID == uuid.Nil() || visible.Has(b.ProjectID)) {
				info.Roles = append(info.Roles, memberRole(b, roles, projects))
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b mcp.MemberInfo) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.UserID.String(), b.UserID.String()))
	})
	return out, nil
}

// memberData is what describes the members of an organization.
type memberData struct {
	memberships []domain.Membership
	users       []domain.User
	bindings    []domain.RoleBinding
	roles       []domain.Role
	projects    []domain.Project
	visible     authz.Visible
	teams       map[uuid.UUID][]string
}

// memberData reads the memberships, or only the one of only, with their users,
// bindings, roles, projects and teams.
func (s *Service) memberData(ctx context.Context, by domain.Actor, only uuid.UUID) (d memberData, err error) {
	st := s.cfg.Store
	if d.memberships, err = st.Memberships().List(ctx, by.AccountID, store.MembershipFilter{}); err != nil {
		return d, err
	}
	if only != uuid.Nil() {
		d.memberships = slices.DeleteFunc(d.memberships, func(m domain.Membership) bool { return m.UserID != only })
	}
	ids := make([]uuid.UUID, len(d.memberships))
	for i, m := range d.memberships {
		ids[i] = m.UserID
	}
	if d.users, err = st.Users().ListByIDs(ctx, ids); err != nil {
		return d, err
	}
	if d.bindings, err = st.RoleBindings().List(ctx, by.AccountID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser}); err != nil {
		return d, err
	}
	if d.roles, err = st.Roles().List(ctx, by.AccountID); err != nil {
		return d, err
	}
	if d.projects, err = st.Projects().List(ctx, by.AccountID); err != nil {
		return d, err
	}
	if d.visible, err = s.authz.Projects(ctx, by, domain.PermProjectRead); err != nil {
		return d, err
	}
	d.teams, err = s.teamsOfUsers(ctx, by.AccountID)
	return d, err
}

// memberRole names the role and project of a binding.
func memberRole(b domain.RoleBinding, roles []domain.Role, projects []domain.Project) mcp.MemberRole {
	r := mcp.MemberRole{}
	if j := slices.IndexFunc(roles, func(x domain.Role) bool { return x.ID == b.RoleID }); j >= 0 {
		r.Role, r.Scope = roles[j].Name, roles[j].Scope
	}
	if j := slices.IndexFunc(projects, func(p domain.Project) bool { return p.ID == b.ProjectID }); j >= 0 {
		r.Project = projects[j].Name
	}
	return r
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
	if err := s.authz.CoversHolder(ctx, by, domain.SubjectUser, id); err != nil {
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

// Sizes of list_audit.
const (
	DefaultAuditLimit = 100
	MaxAuditLimit     = 500
)

func parseTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return t, fmt.Errorf("%s %q is not an RFC 3339 time: %w", name, v, mcp.ErrInvalidInput)
	}
	return t, nil
}

// ListAudit implements mcp.ToolService.
func (s *Service) ListAudit(ctx context.Context, by domain.Actor, in mcp.ListAuditInput) ([]mcp.AuditInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermAuditRead); err != nil {
		return nil, err
	}
	f := store.AuditFilter{Actor: in.Actor, Action: in.Action, SubjectType: in.SubjectType, Limit: DefaultAuditLimit}
	var err error
	if f.Since, err = parseTime("since", in.Since); err != nil {
		return nil, err
	}
	if f.Until, err = parseTime("until", in.Until); err != nil {
		return nil, err
	}
	if in.Subject != "" {
		if f.SubjectID, err = uuid.Parse(in.Subject); err != nil {
			return nil, fmt.Errorf("subject %q is not a UUID: %w", in.Subject, mcp.ErrInvalidInput)
		}
	}
	if in.Limit < 0 || in.Limit > MaxAuditLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d: %w", MaxAuditLimit, mcp.ErrInvalidInput)
	}
	if in.Limit > 0 {
		f.Limit = in.Limit
	}
	entries, err := s.cfg.Store.Audit().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	names, err := s.actorNames(ctx, by.AccountID, entries)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.AuditInfo, len(entries))
	for i, e := range entries {
		out[i] = mcp.AuditInfo{AuditEntry: e, ActorName: names[e.Actor]}
	}
	return out, nil
}

// actorNames names the people and service accounts that acted in entries.
func (s *Service) actorNames(ctx context.Context, accountID uuid.UUID, entries []domain.AuditEntry) (map[string]string, error) {
	names := map[string]string{}
	var users []uuid.UUID
	var accounts []domain.ServiceAccount
	for _, e := range entries {
		a, err := domain.ParseActor(e.Actor)
		if err != nil {
			continue
		}
		id, ok := a.SubjectID()
		switch {
		case !ok:
		case a.Kind == domain.ActorUser:
			users = append(users, id)
		case a.Kind == domain.ActorServiceAccount && accounts == nil:
			if accounts, err = s.cfg.Store.ServiceAccounts().List(ctx, accountID); err != nil {
				return nil, err
			}
		}
	}
	found, err := s.cfg.Store.Users().ListByIDs(ctx, users)
	if err != nil {
		return nil, err
	}
	for _, u := range found {
		names["user:"+u.ID.String()] = u.Name
	}
	for _, a := range accounts {
		names["service_account:"+a.ID.String()] = a.Name
	}
	return names, nil
}

// connectionInfo describes c. An instance connection comes from the server
// file and does not tell its client ID and secret reference.
func (s *Service) connectionInfo(c domain.Connection, tested map[uuid.UUID]bool) mcp.ConnectionInfo {
	base := strings.TrimRight(s.cfg.PublicURL, "/") + "/auth/" + c.ID.String()
	info := mcp.ConnectionInfo{Connection: c, Instance: c.AccountID == uuid.Nil(), Tested: tested[c.ID], RedirectURL: base + "/callback"}
	if info.Instance {
		info.ClientID, info.ClientSecretRef = "", ""
	} else {
		info.TestURL = base + "/login?test=1"
	}
	return info
}

// testedConnections returns the connections of the organization with a
// test sign-in that worked.
func (s *Service) testedConnections(ctx context.Context, accountID uuid.UUID) (map[uuid.UUID]bool, error) {
	entries, err := s.cfg.Store.Audit().List(ctx, accountID, store.AuditFilter{Action: "connection.tested"})
	if err != nil {
		return nil, err
	}
	tested := map[uuid.UUID]bool{}
	for _, e := range entries {
		tested[e.SubjectID] = true
	}
	return tested, nil
}

// ListConnections implements mcp.ToolService.
func (s *Service) ListConnections(ctx context.Context, by domain.Actor) ([]mcp.ConnectionInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermConnectionManage); err != nil {
		return nil, err
	}
	own, err := s.cfg.Store.Connections().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	instance, err := s.cfg.Store.Connections().List(ctx, uuid.Nil())
	if err != nil {
		return nil, err
	}
	tested, err := s.testedConnections(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ConnectionInfo, 0, len(instance)+len(own))
	for _, c := range slices.Concat(instance, own) {
		out = append(out, s.connectionInfo(c, tested))
	}
	return out, nil
}

// CreateConnection implements mcp.ToolService. It never sets JIT or a group
// claim: those are enterprise, and what a connection trusts is subject to No
// escalation.
func (s *Service) CreateConnection(ctx context.Context, by domain.Actor, in mcp.CreateConnectionInput) (mcp.ConnectionInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.ConnectionInfo{}, err
	}
	c := domain.Connection{AccountID: by.AccountID, Kind: in.Kind, Name: in.Name, Issuer: strings.TrimSpace(in.Issuer), ClientID: strings.TrimSpace(in.ClientID), ClientSecretRef: in.ClientSecretRef}
	if err := c.Validate(); err != nil {
		return mcp.ConnectionInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().Create(ctx, &c); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "connection.created", "connection", c.ID, map[string]any{"name": c.Name, "kind": c.Kind, "issuer": c.Issuer})
	})
	if err != nil {
		return mcp.ConnectionInfo{}, conflict(err, "a connection named "+c.Name+" exists")
	}
	return s.connectionInfo(c, nil), nil
}

// SetConnectionEnabled implements mcp.ToolService.
func (s *Service) SetConnectionEnabled(ctx context.Context, by domain.Actor, in mcp.SetConnectionInput) (mcp.ConnectionInfo, error) {
	if err := s.allowOrg(ctx, by, domain.PermConnectionManage); err != nil {
		return mcp.ConnectionInfo{}, err
	}
	if in.Connection == "" {
		return mcp.ConnectionInfo{}, fmt.Errorf("connection is required: %w", mcp.ErrInvalidInput)
	}
	own, err := s.cfg.Store.Connections().List(ctx, by.AccountID)
	if err != nil {
		return mcp.ConnectionInfo{}, err
	}
	i := slices.IndexFunc(own, func(c domain.Connection) bool { return c.ID.String() == in.Connection || c.Name == in.Connection })
	if i < 0 {
		instance, err := s.cfg.Store.Connections().List(ctx, uuid.Nil())
		if err != nil {
			return mcp.ConnectionInfo{}, err
		}
		if slices.ContainsFunc(instance, func(c domain.Connection) bool { return c.ID.String() == in.Connection || c.Name == in.Connection }) {
			return mcp.ConnectionInfo{}, fmt.Errorf("connection %s comes from the server file and is not changed here: %w", in.Connection, mcp.ErrInvalidInput)
		}
		return mcp.ConnectionInfo{}, fmt.Errorf("connection %s: %w", in.Connection, mcp.ErrNotFound)
	}
	c := own[i]
	tested, err := s.testedConnections(ctx, by.AccountID)
	if err != nil {
		return mcp.ConnectionInfo{}, err
	}
	if in.Enabled && !tested[c.ID] {
		return mcp.ConnectionInfo{}, fmt.Errorf("connection %s has no test sign-in that worked yet: open its test_url and sign in once: %w", c.Name, mcp.ErrConflict)
	}
	c.Enabled = in.Enabled
	action := "connection.disabled"
	if in.Enabled {
		action = "connection.enabled"
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Connections().Update(ctx, &c); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), action, "connection", c.ID, map[string]any{"name": c.Name})
	})
	if err != nil {
		return mcp.ConnectionInfo{}, err
	}
	return s.connectionInfo(c, tested), nil
}

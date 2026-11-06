package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// conflict makes a store conflict or the last Owner an mcp.ErrConflict with
// a message for the caller; other errors pass through.
func conflict(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrLastOwner):
		return fmt.Errorf("%w: %w", err, mcp.ErrConflict)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%s: %w", what, mcp.ErrConflict)
	}
	return err
}

// notFound makes a missing row mcp.ErrNotFound.
func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s: %w", what, mcp.ErrNotFound)
	}
	return err
}

// ListRoles implements mcp.ToolService.
func (s *Service) ListRoles(ctx context.Context, by domain.Actor) ([]domain.Role, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.Roles().List(ctx, by.AccountID)
}

// ListTeams implements mcp.ToolService.
func (s *Service) ListTeams(ctx context.Context, by domain.Actor) ([]domain.Team, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.Teams().List(ctx, by.AccountID)
}

// team finds a team by UUID or slug.
func (s *Service) team(ctx context.Context, accountID uuid.UUID, ref string) (domain.Team, error) {
	if ref == "" {
		return domain.Team{}, fmt.Errorf("team is required: %w", mcp.ErrInvalidInput)
	}
	var t domain.Team
	var err error
	if id, perr := uuid.Parse(ref); perr == nil {
		t, err = s.cfg.Store.Teams().Get(ctx, accountID, id)
	} else {
		t, err = s.cfg.Store.Teams().GetBySlug(ctx, accountID, ref)
	}
	return t, notFound(err, "team "+ref)
}

// checkSlug refuses a slug that cannot be stored.
func checkSlug(slug string) error {
	if !domain.ValidSlug(slug) {
		return fmt.Errorf("slug %q must be lowercase letters, digits and -: %w", slug, mcp.ErrInvalidInput)
	}
	return nil
}

// CreateTeam implements mcp.ToolService.
func (s *Service) CreateTeam(ctx context.Context, by domain.Actor, in mcp.CreateTeamInput) (domain.Team, error) {
	if err := s.allowOrg(ctx, by, domain.PermTeamManage); err != nil {
		return domain.Team{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return domain.Team{}, fmt.Errorf("name is required: %w", mcp.ErrInvalidInput)
	}
	t := domain.Team{AccountID: by.AccountID, Name: name, Slug: cmp.Or(in.Slug, domain.Slugify(name)), Source: domain.SourceManual}
	if err := checkSlug(t.Slug); err != nil {
		return domain.Team{}, err
	}
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Teams().Create(ctx, &t); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "team.created", "team", t.ID, map[string]any{"name": t.Name, "slug": t.Slug})
	})
	return t, conflict(err, "a team with slug "+t.Slug+" exists")
}

// UpdateTeam implements mcp.ToolService.
func (s *Service) UpdateTeam(ctx context.Context, by domain.Actor, in mcp.UpdateTeamInput) (domain.Team, error) {
	if err := s.allowOrg(ctx, by, domain.PermTeamManage); err != nil {
		return domain.Team{}, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return domain.Team{}, err
	}
	t.Name, t.Slug = cmp.Or(strings.TrimSpace(in.Name), t.Name), cmp.Or(in.Slug, t.Slug)
	if err := checkSlug(t.Slug); err != nil {
		return domain.Team{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Teams().Update(ctx, &t); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "team.updated", "team", t.ID, map[string]any{"name": t.Name, "slug": t.Slug})
	})
	return t, conflict(err, "a team with slug "+t.Slug+" exists")
}

// DeleteTeam implements mcp.ToolService. Deleting a team removes its roles,
// so it needs what removing them needs.
func (s *Service) DeleteTeam(ctx context.Context, by domain.Actor, in mcp.TeamRefInput) (domain.Team, error) {
	if err := s.allowOrg(ctx, by, domain.PermTeamManage); err != nil {
		return domain.Team{}, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return domain.Team{}, err
	}
	if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectTeam, t.ID); err != nil {
		return domain.Team{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Teams().Delete(ctx, by.AccountID, t.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "team.deleted", "team", t.ID, map[string]any{"name": t.Name, "slug": t.Slug})
	})
	return t, conflict(err, "team "+t.Slug)
}

// ListTeamMembers implements mcp.ToolService.
func (s *Service) ListTeamMembers(ctx context.Context, by domain.Actor, in mcp.TeamRefInput) ([]domain.TeamMember, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgRead); err != nil {
		return nil, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return nil, err
	}
	return s.cfg.Store.Teams().Members(ctx, by.AccountID, t.ID)
}

func parseUser(ref string) (uuid.UUID, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("user %q is not a UUID: %w", ref, mcp.ErrInvalidInput)
	}
	return id, nil
}

// AddTeamMember implements mcp.ToolService. The new member gets the team's
// roles, so the caller needs every permission of them.
func (s *Service) AddTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error) {
	if err := s.allowOrg(ctx, by, domain.PermTeamManage); err != nil {
		return domain.TeamMember{}, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return domain.TeamMember{}, err
	}
	user, err := parseUser(in.User)
	if err != nil {
		return domain.TeamMember{}, err
	}
	if err := s.authz.CoversBindingsOf(ctx, by, domain.SubjectTeam, t.ID); err != nil {
		return domain.TeamMember{}, err
	}
	m := domain.TeamMember{TeamID: t.ID, UserID: user, AccountID: by.AccountID, Source: domain.SourceManual}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Teams().AddMember(ctx, &m); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "team.member_added", "team", t.ID, map[string]any{"user": user})
	})
	return m, notFound(err, "member "+in.User)
}

// RemoveTeamMember implements mcp.ToolService.
func (s *Service) RemoveTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error) {
	if err := s.allowOrg(ctx, by, domain.PermTeamManage); err != nil {
		return domain.TeamMember{}, err
	}
	t, err := s.team(ctx, by.AccountID, in.Team)
	if err != nil {
		return domain.TeamMember{}, err
	}
	user, err := parseUser(in.User)
	if err != nil {
		return domain.TeamMember{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Teams().RemoveMember(ctx, by.AccountID, t.ID, user); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "team.member_removed", "team", t.ID, map[string]any{"user": user})
	})
	return domain.TeamMember{TeamID: t.ID, UserID: user, AccountID: by.AccountID}, notFound(err, "member "+in.User+" of team "+t.Slug)
}

// resource is where a binding applies.
func resource(b domain.RoleBinding) authz.Resource {
	return authz.Resource{AccountID: b.AccountID, ProjectID: b.ProjectID}
}

// CheckHolder is the rule of No escalation for acting on a subject that
// holds roles (resource.Access).
func (s *Service) CheckHolder(ctx context.Context, by domain.Actor, kind domain.SubjectKind, id uuid.UUID) error {
	return s.authz.CoversHolder(ctx, by, kind, id)
}

// bindingPermission is what changing a binding needs: project.manage on its
// project for a project binding, else the manage permission of its subject.
func bindingPermission(b domain.RoleBinding) domain.Permission {
	if b.ProjectID != uuid.Nil() {
		return domain.PermProjectManage
	}
	switch b.SubjectKind {
	case domain.SubjectTeam:
		return domain.PermTeamManage
	case domain.SubjectServiceAccount:
		return domain.PermServiceAccountManage
	}
	return domain.PermMemberManage
}

// checkBindingChange is the No escalation rule for granting or removing the
// binding b: its permission, every permission of its role at its scope, and,
// on the organization, everything a member or service account already holds.
func (s *Service) checkBindingChange(ctx context.Context, by domain.Actor, b domain.RoleBinding, role domain.Role) error {
	if err := s.authz.Authorize(ctx, by, bindingPermission(b), resource(b)); err != nil {
		return err
	}
	if err := s.authz.Covers(ctx, by, role, resource(b)); err != nil {
		return err
	}
	if b.ProjectID != uuid.Nil() || b.SubjectKind == domain.SubjectTeam {
		return nil
	}
	return s.authz.CoversHolder(ctx, by, b.SubjectKind, b.SubjectID)
}

// subject finds the subject of a binding: a user by UUID, a team or service
// account by name or UUID.
func (s *Service) subject(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ref string) (uuid.UUID, error) {
	switch kind {
	case domain.SubjectUser:
		return parseUser(ref)
	case domain.SubjectTeam:
		t, err := s.team(ctx, accountID, ref)
		return t.ID, err
	case domain.SubjectServiceAccount:
		a, err := s.serviceAccount(ctx, accountID, ref)
		return a.ID, err
	}
	return uuid.Nil(), fmt.Errorf("subject_kind %q must be user, team or service_account: %w", kind, mcp.ErrInvalidInput)
}

// roleFor finds the role called name for a binding: a project role on a
// project; on the organization an organization role, or else a project role,
// which then applies to every project.
func (s *Service) roleFor(ctx context.Context, accountID uuid.UUID, name string, project uuid.UUID) (domain.Role, error) {
	roles := s.cfg.Store.Roles()
	var r domain.Role
	var err error
	if project == uuid.Nil() {
		r, err = roles.GetByName(ctx, accountID, domain.RoleScopeOrganization, name)
	}
	if project != uuid.Nil() || errors.Is(err, store.ErrNotFound) {
		r, err = roles.GetByName(ctx, accountID, domain.RoleScopeProject, name)
	}
	return r, notFound(err, "role "+name)
}

// bindingInfo adds the names of the role and project to b.
func bindingInfo(b domain.RoleBinding, role domain.Role, project string) mcp.RoleBindingInfo {
	return mcp.RoleBindingInfo{RoleBinding: b, Role: role.Name, RoleScope: role.Scope, Project: project}
}

// ListRoleBindings implements mcp.ToolService.
func (s *Service) ListRoleBindings(ctx context.Context, by domain.Actor, in mcp.ListRoleBindingsInput) ([]mcp.RoleBindingInfo, error) {
	f := store.RoleBindingFilter{SubjectKind: in.SubjectKind}
	var err error
	if f.ProjectID, err = s.optionalProject(ctx, by.AccountID, in.Project); err != nil {
		return nil, err
	}
	if f.ProjectID != uuid.Nil() {
		err = s.allow(ctx, by, domain.PermProjectRead, f.ProjectID)
	} else {
		err = s.allowOrg(ctx, by, domain.PermOrgRead)
	}
	if err != nil {
		return nil, err
	}
	if in.Subject != "" {
		if f.SubjectID, err = s.subject(ctx, by.AccountID, in.SubjectKind, in.Subject); err != nil {
			return nil, err
		}
	}
	visible, err := s.authz.Projects(ctx, by, domain.PermProjectRead)
	if err != nil {
		return nil, err
	}
	bindings, err := s.cfg.Store.RoleBindings().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	// org.read shows who holds what on the organization; a project stays
	// hidden from who cannot read it, as in list_projects.
	bindings = slices.DeleteFunc(bindings, func(b domain.RoleBinding) bool {
		return b.ProjectID != uuid.Nil() && !visible.Has(b.ProjectID)
	})
	roles, err := s.cfg.Store.Roles().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	projects, err := s.cfg.Store.Projects().List(ctx, by.AccountID)
	if err != nil {
		return nil, err
	}
	names, err := s.subjectNames(ctx, by.AccountID, bindings)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RoleBindingInfo, len(bindings))
	for i, b := range bindings {
		var role domain.Role
		if j := slices.IndexFunc(roles, func(r domain.Role) bool { return r.ID == b.RoleID }); j >= 0 {
			role = roles[j]
		}
		var project string
		if j := slices.IndexFunc(projects, func(p domain.Project) bool { return p.ID == b.ProjectID }); j >= 0 {
			project = projects[j].Name
		}
		out[i] = bindingInfo(b, role, project)
		out[i].SubjectName = names[b.SubjectID]
	}
	return out, nil
}

// subjectNames names the holders of bindings: users, teams and service
// accounts of the organization.
func (s *Service) subjectNames(ctx context.Context, accountID uuid.UUID, bindings []domain.RoleBinding) (map[uuid.UUID]string, error) {
	names := map[uuid.UUID]string{}
	var users []uuid.UUID
	var teams, accounts bool
	for _, b := range bindings {
		switch b.SubjectKind {
		case domain.SubjectUser:
			users = append(users, b.SubjectID)
		case domain.SubjectTeam:
			teams = true
		case domain.SubjectServiceAccount:
			accounts = true
		}
	}
	found, err := s.cfg.Store.Users().ListByIDs(ctx, users)
	if err != nil {
		return nil, err
	}
	for _, u := range found {
		names[u.ID] = u.Name
	}
	if teams {
		list, err := s.cfg.Store.Teams().List(ctx, accountID)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			names[t.ID] = t.Name
		}
	}
	if accounts {
		list, err := s.cfg.Store.ServiceAccounts().List(ctx, accountID)
		if err != nil {
			return nil, err
		}
		for _, a := range list {
			names[a.ID] = a.Name
		}
	}
	return names, nil
}

// CreateRoleBinding implements mcp.ToolService.
func (s *Service) CreateRoleBinding(ctx context.Context, by domain.Actor, in mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error) {
	b := domain.RoleBinding{AccountID: by.AccountID, SubjectKind: in.SubjectKind, CreatedBy: by.String()}
	var err error
	if b.SubjectID, err = s.subject(ctx, by.AccountID, in.SubjectKind, in.Subject); err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	if b.ProjectID, err = s.optionalProject(ctx, by.AccountID, in.Project); err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	role, err := s.roleFor(ctx, by.AccountID, in.Role, b.ProjectID)
	if err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	b.RoleID = role.ID
	if err := s.checkBindingChange(ctx, by, b, role); err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	if err := b.ValidateFor(role); err != nil {
		return mcp.RoleBindingInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.RoleBindings().Create(ctx, &b); err != nil {
			return err
		}
		return s.auditBinding(ctx, tx, by, "role_binding.created", b, role)
	})
	if err != nil {
		return mcp.RoleBindingInfo{}, notFound(conflict(err, "the binding exists"), "subject "+in.Subject)
	}
	return bindingInfo(b, role, in.Project), nil
}

func (s *Service) auditBinding(ctx context.Context, tx store.Store, by domain.Actor, action string, b domain.RoleBinding, role domain.Role) error {
	return s.audit(ctx, tx, by, b.ProjectID, action, "role_binding", b.ID,
		map[string]any{"role": role.Name, "subject_kind": b.SubjectKind, "subject": b.SubjectID})
}

// DeleteRoleBinding implements mcp.ToolService.
func (s *Service) DeleteRoleBinding(ctx context.Context, by domain.Actor, in mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error) {
	id, err := uuid.Parse(in.Binding)
	if err != nil {
		return mcp.RoleBindingInfo{}, fmt.Errorf("binding %q is not a UUID: %w", in.Binding, mcp.ErrInvalidInput)
	}
	b, err := s.cfg.Store.RoleBindings().Get(ctx, by.AccountID, id)
	if err != nil {
		return mcp.RoleBindingInfo{}, notFound(err, "binding "+in.Binding)
	}
	role, err := s.cfg.Store.Roles().Get(ctx, by.AccountID, b.RoleID)
	if err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	if err := s.checkBindingChange(ctx, by, b, role); err != nil {
		return mcp.RoleBindingInfo{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.RoleBindings().Delete(ctx, by.AccountID, id); err != nil {
			return err
		}
		return s.auditBinding(ctx, tx, by, "role_binding.deleted", b, role)
	})
	if err != nil {
		return mcp.RoleBindingInfo{}, conflict(err, "binding "+in.Binding)
	}
	return bindingInfo(b, role, ""), nil
}

// serviceAccount finds a service account by UUID or name.
func (s *Service) serviceAccount(ctx context.Context, accountID uuid.UUID, ref string) (domain.ServiceAccount, error) {
	if ref == "" {
		return domain.ServiceAccount{}, fmt.Errorf("service account is required: %w", mcp.ErrInvalidInput)
	}
	var a domain.ServiceAccount
	var err error
	if id, perr := uuid.Parse(ref); perr == nil {
		a, err = s.cfg.Store.ServiceAccounts().Get(ctx, accountID, id)
	} else {
		a, err = s.cfg.Store.ServiceAccounts().GetByName(ctx, accountID, ref)
	}
	return a, notFound(err, "service account "+ref)
}

// ListServiceAccounts implements mcp.ToolService.
func (s *Service) ListServiceAccounts(ctx context.Context, by domain.Actor) ([]domain.ServiceAccount, error) {
	if err := s.allowOrg(ctx, by, domain.PermOrgRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.ServiceAccounts().List(ctx, by.AccountID)
}

func checkServiceAccountName(name string) error {
	if !domain.ValidName(name) {
		return fmt.Errorf("name %q must be a lowercase name: %w", name, mcp.ErrInvalidInput)
	}
	return nil
}

// CreateServiceAccount implements mcp.ToolService.
func (s *Service) CreateServiceAccount(ctx context.Context, by domain.Actor, in mcp.CreateServiceAccountInput) (domain.ServiceAccount, error) {
	if err := s.allowOrg(ctx, by, domain.PermServiceAccountManage); err != nil {
		return domain.ServiceAccount{}, err
	}
	if err := checkServiceAccountName(in.Name); err != nil {
		return domain.ServiceAccount{}, err
	}
	a := domain.ServiceAccount{AccountID: by.AccountID, Name: in.Name, Description: in.Description, CreatedBy: by.String()}
	err := s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ServiceAccounts().Create(ctx, &a); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "service_account.created", "service_account", a.ID, map[string]any{"name": a.Name})
	})
	return a, conflict(err, "a service account named "+a.Name+" exists")
}

// UpdateServiceAccount implements mcp.ToolService.
func (s *Service) UpdateServiceAccount(ctx context.Context, by domain.Actor, in mcp.UpdateServiceAccountInput) (domain.ServiceAccount, error) {
	if err := s.allowOrg(ctx, by, domain.PermServiceAccountManage); err != nil {
		return domain.ServiceAccount{}, err
	}
	a, err := s.serviceAccount(ctx, by.AccountID, in.ServiceAccount)
	if err != nil {
		return domain.ServiceAccount{}, err
	}
	a.Name = cmp.Or(in.Name, a.Name)
	if in.Description != nil {
		a.Description = *in.Description
	}
	if err := checkServiceAccountName(a.Name); err != nil {
		return domain.ServiceAccount{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ServiceAccounts().Update(ctx, &a); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), "service_account.updated", "service_account", a.ID, map[string]any{"name": a.Name})
	})
	return a, conflict(err, "a service account named "+a.Name+" exists")
}

// SetServiceAccountDisabled implements mcp.ToolService. It acts on a holder
// of roles, so it needs every permission of them.
func (s *Service) SetServiceAccountDisabled(ctx context.Context, by domain.Actor, in mcp.SetServiceAccountInput) (domain.ServiceAccount, error) {
	if err := s.allowOrg(ctx, by, domain.PermServiceAccountManage); err != nil {
		return domain.ServiceAccount{}, err
	}
	a, err := s.serviceAccount(ctx, by.AccountID, in.ServiceAccount)
	if err != nil {
		return domain.ServiceAccount{}, err
	}
	if err := s.authz.CoversHolder(ctx, by, domain.SubjectServiceAccount, a.ID); err != nil {
		return domain.ServiceAccount{}, err
	}
	action := "service_account.enabled"
	if in.Disabled {
		action = "service_account.disabled"
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ServiceAccounts().SetDisabled(ctx, by.AccountID, a.ID, in.Disabled); err != nil {
			return err
		}
		if a, err = tx.ServiceAccounts().Get(ctx, by.AccountID, a.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, uuid.Nil(), action, "service_account", a.ID, map[string]any{"name": a.Name})
	})
	return a, err
}

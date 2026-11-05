package testkit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// memberKey names a membership.
type memberKey struct{ account, user uuid.UUID }

// githubLink is a user's GitHub identity.
type githubLink struct {
	id    int64
	login string
}

// accessData is what the access repositories keep.
type accessData struct {
	users       map[uuid.UUID]domain.User
	members     map[memberKey]domain.MembershipStatus
	github      map[uuid.UUID]githubLink // by user
	teams       table[domain.Team]
	teamMembers []domain.TeamMember
	serviceAccs table[domain.ServiceAccount]
	roles       []domain.Role
	bindings    table[domain.RoleBinding]
}

func newAccessData() accessData {
	roles := domain.BuiltInRoles()
	for i := range roles {
		roles[i].ID = domain.NewID()
	}
	return accessData{
		users: map[uuid.UUID]domain.User{}, members: map[memberKey]domain.MembershipStatus{}, github: map[uuid.UUID]githubLink{},
		teams:       table[domain.Team]{key: func(r domain.Team) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		serviceAccs: table[domain.ServiceAccount]{key: func(r domain.ServiceAccount) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		bindings:    table[domain.RoleBinding]{key: func(r domain.RoleBinding) (uuid.UUID, uuid.UUID) { return r.AccountID, r.ID }},
		roles:       roles,
	}
}

func (a accessData) clone() accessData {
	a.users, a.members, a.github = maps.Clone(a.users), maps.Clone(a.members), maps.Clone(a.github)
	a.teams, a.serviceAccs, a.bindings = a.teams.clone(), a.serviceAccs.clone(), a.bindings.clone()
	a.teamMembers, a.roles = slices.Clone(a.teamMembers), slices.Clone(a.roles)
	return a
}

// AddMember stores a user with a membership in the organization, as sign-in
// does; the zero status is active.
func (s *Store) AddMember(accountID uuid.UUID, u domain.User, status domain.MembershipStatus) domain.User {
	u.ID = newID(u.ID)
	if status == "" {
		status = domain.MembershipActive
	}
	_ = s.do(func(d *storeData, now time.Time) error {
		u.CreatedAt, u.UpdatedAt = now, now
		d.access.users[u.ID] = u
		d.access.members[memberKey{accountID, u.ID}] = status
		return nil
	})
	return u
}

// SetMemberStatus changes a membership.
func (s *Store) SetMemberStatus(accountID, userID uuid.UUID, status domain.MembershipStatus) {
	_ = s.do(func(d *storeData, _ time.Time) error {
		d.access.members[memberKey{accountID, userID}] = status
		return nil
	})
}

// LinkGitHub gives a user a GitHub identity.
func (s *Store) LinkGitHub(userID uuid.UUID, id int64, login string) {
	_ = s.do(func(d *storeData, _ time.Time) error {
		d.access.github[userID] = githubLink{id, login}
		return nil
	})
}

// Directory implements store.Store.
func (s *Store) Directory() store.DirectoryRepo { return directoryRepo{s} }

type directoryRepo struct{ s *Store }

func (r directoryRepo) User(_ context.Context, id uuid.UUID) (u domain.User, err error) {
	err = r.s.do(func(d *storeData, _ time.Time) error {
		var ok bool
		if u, ok = d.access.users[id]; !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return u, err
}

func (r directoryRepo) GitHubMembers(_ context.Context, accountID uuid.UUID, id int64, login string) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for user, link := range d.access.github {
			if _, member := d.access.members[memberKey{accountID, user}]; !member {
				continue
			}
			if (id != 0 && link.id == id) || (id == 0 && strings.EqualFold(link.login, login)) {
				out = append(out, user)
			}
		}
		return nil
	})
	return out, err
}

func teamsT(d *storeData) *table[domain.Team]                 { return &d.access.teams }
func serviceAccsT(d *storeData) *table[domain.ServiceAccount] { return &d.access.serviceAccs }
func bindingsT(d *storeData) *table[domain.RoleBinding]       { return &d.access.bindings }

// Teams implements store.Store.
func (s *Store) Teams() store.TeamRepo { return teamRepo{s} }

type teamRepo struct{ s *Store }

func (r teamRepo) Create(_ context.Context, t *domain.Team) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.access.teams.exists(func(x domain.Team) bool {
			return x.AccountID == t.AccountID && (x.Slug == t.Slug || (t.ExternalID != "" && x.ExternalID == t.ExternalID))
		}) {
			return fmt.Errorf("team %s: %w", t.Slug, store.ErrConflict)
		}
		t.ID = newID(t.ID)
		t.CreatedAt, t.UpdatedAt = now, now
		d.access.teams.insert(*t)
		return nil
	})
}

func (r teamRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Team, error) {
	return getRow(r.s, teamsT, accountID, id)
}

func (r teamRepo) GetBySlug(_ context.Context, accountID uuid.UUID, slug string) (domain.Team, error) {
	return findRow(r.s, teamsT, func(t domain.Team) bool { return t.AccountID == accountID && t.Slug == slug })
}

func (r teamRepo) GetByExternalID(_ context.Context, accountID uuid.UUID, externalID string) (domain.Team, error) {
	return findRow(r.s, teamsT, func(t domain.Team) bool { return t.AccountID == accountID && t.ExternalID == externalID })
}

func (r teamRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Team, error) {
	return listRows(r.s, teamsT, 0, func(t domain.Team) bool { return t.AccountID == accountID })
}

func (r teamRepo) Update(_ context.Context, t *domain.Team) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.access.teams.exists(func(x domain.Team) bool {
			return x.AccountID == t.AccountID && x.ID != t.ID && x.Slug == t.Slug
		}) {
			return fmt.Errorf("team %s: %w", t.Slug, store.ErrConflict)
		}
		row, err := d.access.teams.get(t.AccountID, t.ID)
		if err != nil {
			return err
		}
		row.Name, row.Slug, row.ExternalID, row.UpdatedAt = t.Name, t.Slug, t.ExternalID, now
		t.UpdatedAt = now
		return nil
	})
}

func (r teamRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if _, err := d.access.teams.get(accountID, id); err != nil {
			return err
		}
		d.access.teams.rows = slices.DeleteFunc(slices.Clone(d.access.teams.rows), func(t domain.Team) bool { return t.ID == id })
		d.access.teamMembers = slices.DeleteFunc(slices.Clone(d.access.teamMembers), func(m domain.TeamMember) bool { return m.TeamID == id })
		d.access.bindings.rows = slices.DeleteFunc(slices.Clone(d.access.bindings.rows), func(b domain.RoleBinding) bool {
			return b.SubjectKind == domain.SubjectTeam && b.SubjectID == id
		})
		return nil
	})
}

func (r teamRepo) AddMember(_ context.Context, m *domain.TeamMember) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if _, err := d.access.teams.get(m.AccountID, m.TeamID); err != nil {
			return err
		}
		if _, ok := d.access.members[memberKey{m.AccountID, m.UserID}]; !ok {
			return fmt.Errorf("member %s: %w", m.UserID, store.ErrNotFound)
		}
		if i := slices.IndexFunc(d.access.teamMembers, func(x domain.TeamMember) bool { return x.TeamID == m.TeamID && x.UserID == m.UserID }); i >= 0 {
			*m = d.access.teamMembers[i]
			return nil
		}
		m.CreatedAt = now
		d.access.teamMembers = append(d.access.teamMembers, *m)
		return nil
	})
}

func (r teamRepo) RemoveMember(_ context.Context, accountID, teamID, userID uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.access.teamMembers, func(x domain.TeamMember) bool {
			return x.AccountID == accountID && x.TeamID == teamID && x.UserID == userID
		})
		if i < 0 {
			return store.ErrNotFound
		}
		d.access.teamMembers = slices.Delete(slices.Clone(d.access.teamMembers), i, i+1)
		return nil
	})
}

func (r teamRepo) Members(_ context.Context, accountID, teamID uuid.UUID) ([]domain.TeamMember, error) {
	out := []domain.TeamMember{}
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for _, m := range d.access.teamMembers {
			if m.AccountID == accountID && m.TeamID == teamID {
				out = append(out, m)
			}
		}
		return nil
	})
	return out, err
}

func (r teamRepo) TeamsOf(_ context.Context, accountID, userID uuid.UUID) ([]domain.Team, error) {
	out := []domain.Team{}
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for _, t := range d.access.teams.rows {
			if t.AccountID == accountID && d.access.inTeam(t.ID, userID) {
				out = append(out, t)
			}
		}
		return nil
	})
	return out, err
}

func (a accessData) inTeam(teamID, userID uuid.UUID) bool {
	return slices.ContainsFunc(a.teamMembers, func(m domain.TeamMember) bool { return m.TeamID == teamID && m.UserID == userID })
}

// ServiceAccounts implements store.Store.
func (s *Store) ServiceAccounts() store.ServiceAccountRepo { return serviceAccountRepo{s} }

type serviceAccountRepo struct{ s *Store }

func (r serviceAccountRepo) Create(_ context.Context, a *domain.ServiceAccount) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.access.serviceAccs.exists(func(x domain.ServiceAccount) bool { return x.AccountID == a.AccountID && x.Name == a.Name }) {
			return fmt.Errorf("service account %s: %w", a.Name, store.ErrConflict)
		}
		a.ID = newID(a.ID)
		a.CreatedAt = now
		d.access.serviceAccs.insert(*a)
		return nil
	})
}

func (r serviceAccountRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.ServiceAccount, error) {
	return getRow(r.s, serviceAccsT, accountID, id)
}

func (r serviceAccountRepo) GetByName(_ context.Context, accountID uuid.UUID, name string) (domain.ServiceAccount, error) {
	return findRow(r.s, serviceAccsT, func(a domain.ServiceAccount) bool { return a.AccountID == accountID && a.Name == name })
}

func (r serviceAccountRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.ServiceAccount, error) {
	return listRows(r.s, serviceAccsT, 0, func(a domain.ServiceAccount) bool { return a.AccountID == accountID })
}

func (r serviceAccountRepo) Update(_ context.Context, a *domain.ServiceAccount) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if d.access.serviceAccs.exists(func(x domain.ServiceAccount) bool {
			return x.AccountID == a.AccountID && x.ID != a.ID && x.Name == a.Name
		}) {
			return fmt.Errorf("service account %s: %w", a.Name, store.ErrConflict)
		}
		row, err := d.access.serviceAccs.get(a.AccountID, a.ID)
		if err != nil {
			return err
		}
		row.Name, row.Description = a.Name, a.Description
		return nil
	})
}

func (r serviceAccountRepo) SetDisabled(_ context.Context, accountID, id uuid.UUID, disabled bool) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		row, err := d.access.serviceAccs.get(accountID, id)
		if err != nil {
			return err
		}
		switch {
		case !disabled:
			row.DisabledAt = time.Time{}
		case row.DisabledAt.IsZero():
			row.DisabledAt = now
		}
		return nil
	})
}

// Roles implements store.Store.
func (s *Store) Roles() store.RoleRepo { return roleRepo{s} }

type roleRepo struct{ s *Store }

func (a accessData) visibleRole(accountID uuid.UUID, r domain.Role) bool {
	return r.BuiltIn || r.AccountID == accountID
}

func (r roleRepo) find(accountID uuid.UUID, match func(domain.Role) bool) (role domain.Role, err error) {
	err = r.s.do(func(d *storeData, _ time.Time) error {
		i := slices.IndexFunc(d.access.roles, func(x domain.Role) bool { return d.access.visibleRole(accountID, x) && match(x) })
		if i < 0 {
			return store.ErrNotFound
		}
		role = d.access.roles[i]
		return nil
	})
	return role, err
}

func (r roleRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Role, error) {
	return r.find(accountID, func(x domain.Role) bool { return x.ID == id })
}

func (r roleRepo) GetByName(_ context.Context, accountID uuid.UUID, scope domain.RoleScope, name string) (domain.Role, error) {
	return r.find(accountID, func(x domain.Role) bool { return x.Scope == scope && x.Name == name })
}

func (r roleRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Role, error) {
	out := []domain.Role{}
	err := r.s.do(func(d *storeData, _ time.Time) error {
		for _, x := range d.access.roles {
			if d.access.visibleRole(accountID, x) {
				out = append(out, x)
			}
		}
		return nil
	})
	return out, err
}

func (r roleRepo) Create(_ context.Context, role *domain.Role) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if slices.ContainsFunc(d.access.roles, func(x domain.Role) bool {
			return x.AccountID == role.AccountID && x.Scope == role.Scope && x.Name == role.Name
		}) {
			return fmt.Errorf("role %s: %w", role.Name, store.ErrConflict)
		}
		role.ID = newID(role.ID)
		role.Custom, role.BuiltIn, role.CreatedAt, role.UpdatedAt = true, false, now, now
		d.access.roles = append(slices.Clone(d.access.roles), *role)
		return nil
	})
}

func (r roleRepo) Update(_ context.Context, role *domain.Role) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		i := slices.IndexFunc(d.access.roles, func(x domain.Role) bool { return x.Custom && x.AccountID == role.AccountID && x.ID == role.ID })
		if i < 0 {
			return store.ErrNotFound
		}
		role.UpdatedAt = now
		x := &d.access.roles[i]
		x.Name, x.Description, x.Permissions, x.UpdatedAt = role.Name, role.Description, slices.Clone(role.Permissions), now
		return nil
	})
}

func (r roleRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		if d.access.bindings.exists(func(b domain.RoleBinding) bool { return b.AccountID == accountID && b.RoleID == id }) {
			return store.ErrConflict
		}
		i := slices.IndexFunc(d.access.roles, func(x domain.Role) bool { return x.Custom && x.AccountID == accountID && x.ID == id })
		if i < 0 {
			return store.ErrNotFound
		}
		d.access.roles = slices.Delete(slices.Clone(d.access.roles), i, i+1)
		return nil
	})
}

// RoleBindings implements store.Store.
func (s *Store) RoleBindings() store.RoleBindingRepo { return roleBindingRepo{s} }

type roleBindingRepo struct{ s *Store }

// subjectExists says whether the subject of b is in its organization.
func (a accessData) subjectExists(b *domain.RoleBinding) bool {
	switch b.SubjectKind {
	case domain.SubjectUser:
		_, ok := a.members[memberKey{b.AccountID, b.SubjectID}]
		return ok
	case domain.SubjectTeam:
		_, err := a.teams.get(b.AccountID, b.SubjectID)
		return err == nil
	case domain.SubjectServiceAccount:
		_, err := a.serviceAccs.get(b.AccountID, b.SubjectID)
		return err == nil
	}
	return false
}

func (r roleBindingRepo) Create(_ context.Context, b *domain.RoleBinding) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		i := slices.IndexFunc(d.access.roles, func(x domain.Role) bool { return x.ID == b.RoleID && d.access.visibleRole(b.AccountID, x) })
		if i < 0 || !d.access.subjectExists(b) {
			return fmt.Errorf("binding: %w", store.ErrNotFound)
		}
		if err := foreign(&d.projects, b.AccountID, b.ProjectID); err != nil {
			return err
		}
		if err := b.ValidateFor(d.access.roles[i]); err != nil {
			return err
		}
		if d.access.bindings.exists(func(x domain.RoleBinding) bool {
			return x.AccountID == b.AccountID && x.SubjectKind == b.SubjectKind && x.SubjectID == b.SubjectID && x.RoleID == b.RoleID && x.ProjectID == b.ProjectID
		}) {
			return fmt.Errorf("binding: %w", store.ErrConflict)
		}
		b.ID = newID(b.ID)
		b.CreatedAt = now
		d.access.bindings.insert(*b)
		return nil
	})
}

func (r roleBindingRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.RoleBinding, error) {
	return getRow(r.s, bindingsT, accountID, id)
}

func (r roleBindingRepo) List(_ context.Context, accountID uuid.UUID, f store.RoleBindingFilter) ([]domain.RoleBinding, error) {
	return listRows(r.s, bindingsT, 0, func(b domain.RoleBinding) bool {
		return b.AccountID == accountID && zeroOr(f.SubjectKind, b.SubjectKind) && zeroOr(f.SubjectID, b.SubjectID) && zeroOr(f.RoleID, b.RoleID) &&
			zeroOr(f.ProjectID, b.ProjectID) && (!f.OrganizationOnly || b.ProjectID == uuid.Nil())
	})
}

// activeOwner says whether b makes its subject an active Owner.
func (a accessData) activeOwner(b domain.RoleBinding) bool {
	i := slices.IndexFunc(a.roles, func(x domain.Role) bool { return x.ID == b.RoleID })
	return i >= 0 && a.roles[i].BuiltIn && a.roles[i].Name == domain.RoleOwner && a.roles[i].Scope == domain.RoleScopeOrganization &&
		b.SubjectKind == domain.SubjectUser && a.members[memberKey{b.AccountID, b.SubjectID}] == domain.MembershipActive &&
		a.users[b.SubjectID].DisabledAt.IsZero()
}

func (r roleBindingRepo) Delete(_ context.Context, accountID, id uuid.UUID) error {
	return r.s.do(func(d *storeData, _ time.Time) error {
		b, err := d.access.bindings.get(accountID, id)
		if err != nil {
			return err
		}
		if d.access.activeOwner(*b) && !d.access.bindings.exists(func(x domain.RoleBinding) bool {
			return x.AccountID == accountID && x.ID != id && d.access.activeOwner(x)
		}) {
			return store.ErrLastOwner
		}
		d.access.bindings.rows = slices.DeleteFunc(slices.Clone(d.access.bindings.rows), func(x domain.RoleBinding) bool { return x.ID == id })
		return nil
	})
}

func (r roleBindingRepo) Effective(_ context.Context, accountID uuid.UUID, kind domain.SubjectKind, id uuid.UUID) ([]store.BoundRole, error) {
	out := []store.BoundRole{}
	err := r.s.do(func(d *storeData, _ time.Time) error {
		a := d.access
		switch kind {
		case domain.SubjectUser:
			if a.members[memberKey{accountID, id}] != domain.MembershipActive || !a.users[id].DisabledAt.IsZero() {
				return nil
			}
		case domain.SubjectServiceAccount:
			sa, err := a.serviceAccs.get(accountID, id)
			if err != nil || !sa.DisabledAt.IsZero() {
				return nil
			}
		default:
			return nil
		}
		for _, b := range a.bindings.rows {
			own := b.SubjectKind == kind && b.SubjectID == id
			viaTeam := kind == domain.SubjectUser && b.SubjectKind == domain.SubjectTeam && a.inTeam(b.SubjectID, id)
			if b.AccountID != accountID || (!own && !viaTeam) {
				continue
			}
			i := slices.IndexFunc(a.roles, func(x domain.Role) bool { return x.ID == b.RoleID })
			out = append(out, store.BoundRole{Binding: b, Role: a.roles[i]})
		}
		return nil
	})
	return out, err
}

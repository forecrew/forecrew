package testkit

import (
	"errors"
	"slices"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// AccessSeed adds what the access repositories only read: members, and the
// user behind them, which sign-in creates.
type AccessSeed interface {
	// Member adds an active member of the organization.
	Member(t *testing.T, accountID uuid.UUID) uuid.UUID
	// SetStatus changes a membership.
	SetStatus(t *testing.T, accountID, userID uuid.UUID, status domain.MembershipStatus)
	// Link gives a user a GitHub identity.
	Link(t *testing.T, accountID, userID uuid.UUID, id int64, login string)
}

// AccessStoreSuite checks the contract of the team, service account, role,
// role binding and directory repositories on st, the fake and the Postgres
// store alike.
func AccessStoreSuite(t *testing.T, st store.Store, seed AccessSeed, account, project uuid.UUID) {
	t.Helper()
	s := &accessSuite{t: t, st: st, seed: seed, account: account, project: project}
	s.teams()
	s.serviceAccounts()
	s.roles()
	s.bindings()
	s.effective()
	s.directory()
}

type accessSuite struct {
	t                *testing.T
	st               store.Store
	seed             AccessSeed
	account, project uuid.UUID
}

func (s *accessSuite) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

func (s *accessSuite) want(err, target error) {
	s.t.Helper()
	if !errors.Is(err, target) {
		s.t.Fatalf("error = %v, want %v", err, target)
	}
}

func (s *accessSuite) role(scope domain.RoleScope, name string) domain.Role {
	s.t.Helper()
	r, err := s.st.Roles().GetByName(s.t.Context(), s.account, scope, name)
	s.must(err)
	return r
}

func (s *accessSuite) team(slug string) domain.Team {
	s.t.Helper()
	team := domain.Team{AccountID: s.account, Name: slug, Slug: slug, Source: domain.SourceManual}
	s.must(s.st.Teams().Create(s.t.Context(), &team))
	return team
}

func (s *accessSuite) bind(kind domain.SubjectKind, id uuid.UUID, r domain.Role, project uuid.UUID) domain.RoleBinding {
	s.t.Helper()
	b := domain.RoleBinding{AccountID: s.account, SubjectKind: kind, SubjectID: id, RoleID: r.ID, ProjectID: project, CreatedBy: "user:op"}
	s.must(s.st.RoleBindings().Create(s.t.Context(), &b))
	return b
}

func (s *accessSuite) teams() {
	ctx, repo := s.t.Context(), s.st.Teams()
	a := s.team("core")
	if a.ID == uuid.Nil() || a.CreatedAt.IsZero() {
		s.t.Fatalf("created = %+v", a)
	}
	dup := domain.Team{AccountID: s.account, Name: "x", Slug: "core", Source: domain.SourceManual}
	s.want(repo.Create(ctx, &dup), store.ErrConflict)
	got, err := repo.Get(ctx, s.account, a.ID)
	s.must(err)
	if got.Slug != "core" || got.Source != domain.SourceManual {
		s.t.Fatalf("got = %+v", got)
	}
	_, err = repo.Get(ctx, domain.NewID(), a.ID)
	s.want(err, store.ErrNotFound)
	got, err = repo.GetBySlug(ctx, s.account, "core")
	if err != nil || got.ID != a.ID {
		s.t.Fatalf("by slug = %+v, %v", got, err)
	}
	a.Name, a.Slug, a.ExternalID = "Base", "base", "ext"
	s.must(repo.Update(ctx, &a))
	got, err = repo.GetByExternalID(ctx, s.account, "ext")
	if err != nil || got.Name != "Base" || got.Slug != "base" {
		s.t.Fatalf("by external ID = %+v, %v", got, err)
	}
	b := s.team("other")
	b.Slug = "base"
	s.want(repo.Update(ctx, &b), store.ErrConflict)

	user := s.seed.Member(s.t, s.account)
	m := domain.TeamMember{TeamID: a.ID, UserID: user, AccountID: s.account, Source: domain.SourceManual}
	s.must(repo.AddMember(ctx, &m))
	again := domain.TeamMember{TeamID: a.ID, UserID: user, AccountID: s.account, Source: domain.SourceSCIM}
	s.must(repo.AddMember(ctx, &again))
	if again.Source != domain.SourceManual {
		s.t.Errorf("an existing member became %s", again.Source)
	}
	stranger := domain.TeamMember{TeamID: a.ID, UserID: domain.NewID(), AccountID: s.account, Source: domain.SourceManual}
	s.want(repo.AddMember(ctx, &stranger), store.ErrNotFound)
	members, err := repo.Members(ctx, s.account, a.ID)
	if err != nil || len(members) != 1 || members[0].UserID != user {
		s.t.Fatalf("members = %+v, %v", members, err)
	}
	teams, err := repo.TeamsOf(ctx, s.account, user)
	if err != nil || len(teams) != 1 || teams[0].ID != a.ID {
		s.t.Fatalf("teams of = %+v, %v", teams, err)
	}
	s.must(repo.RemoveMember(ctx, s.account, a.ID, user))
	s.want(repo.RemoveMember(ctx, s.account, a.ID, user), store.ErrNotFound)

	s.must(repo.AddMember(ctx, &m))
	bound := s.bind(domain.SubjectTeam, a.ID, s.role(domain.RoleScopeProject, domain.RoleViewer), s.project)
	s.must(repo.Delete(ctx, s.account, a.ID))
	s.want(repo.Delete(ctx, s.account, a.ID), store.ErrNotFound)
	_, err = s.st.RoleBindings().Get(ctx, s.account, bound.ID)
	s.want(err, store.ErrNotFound)
	if teams, _ := repo.TeamsOf(ctx, s.account, user); len(teams) != 0 {
		s.t.Errorf("teams after delete = %+v", teams)
	}
	all, err := repo.List(ctx, s.account)
	if err != nil || len(all) != 1 || all[0].ID != b.ID {
		s.t.Fatalf("teams = %+v, %v", all, err)
	}
}

func (s *accessSuite) serviceAccounts() {
	ctx, repo := s.t.Context(), s.st.ServiceAccounts()
	a := domain.ServiceAccount{AccountID: s.account, Name: "ci", Description: "CI", CreatedBy: "user:op"}
	s.must(repo.Create(ctx, &a))
	dup := domain.ServiceAccount{AccountID: s.account, Name: "ci", CreatedBy: "user:op"}
	s.want(repo.Create(ctx, &dup), store.ErrConflict)
	b := domain.ServiceAccount{AccountID: s.account, Name: "deploy", CreatedBy: "user:op"}
	s.must(repo.Create(ctx, &b))
	b.Name = "ci"
	s.want(repo.Update(ctx, &b), store.ErrConflict)
	a.Name, a.Description = "build", "Builds"
	s.must(repo.Update(ctx, &a))
	got, err := repo.GetByName(ctx, s.account, "build")
	if err != nil || got.ID != a.ID || got.Description != "Builds" || got.CreatedBy != "user:op" {
		s.t.Fatalf("by name = %+v, %v", got, err)
	}
	s.must(repo.SetDisabled(ctx, s.account, a.ID, true))
	if got, _ = repo.Get(ctx, s.account, a.ID); got.DisabledAt.IsZero() {
		s.t.Error("not disabled")
	}
	s.must(repo.SetDisabled(ctx, s.account, a.ID, false))
	if got, _ = repo.Get(ctx, s.account, a.ID); !got.DisabledAt.IsZero() {
		s.t.Error("not enabled")
	}
	s.want(repo.SetDisabled(ctx, s.account, domain.NewID(), true), store.ErrNotFound)
	if all, err := repo.List(ctx, s.account); err != nil || len(all) != 2 {
		s.t.Fatalf("service accounts = %+v, %v", all, err)
	}
}

func (s *accessSuite) roles() {
	ctx, repo := s.t.Context(), s.st.Roles()
	all, err := repo.List(ctx, s.account)
	s.must(err)
	builtIn := domain.BuiltInRoles()
	if len(all) != len(builtIn) {
		s.t.Fatalf("roles = %d, want %d", len(all), len(builtIn))
	}
	for _, want := range builtIn {
		got := s.role(want.Scope, want.Name)
		if !got.BuiltIn || !slices.Equal(got.Permissions, want.Permissions) {
			s.t.Errorf("%s %s = %+v", want.Scope, want.Name, got)
		}
	}
	custom := domain.Role{AccountID: s.account, Name: "reader", Scope: domain.RoleScopeProject, Permissions: []domain.Permission{domain.PermGoalRead}, Custom: true}
	s.must(repo.Create(ctx, &custom))
	dup := custom
	dup.ID = uuid.Nil()
	s.want(repo.Create(ctx, &dup), store.ErrConflict)
	custom.Permissions = []domain.Permission{domain.PermGoalRead, domain.PermJobRead}
	s.must(repo.Update(ctx, &custom))
	got, err := repo.Get(ctx, s.account, custom.ID)
	if err != nil || len(got.Permissions) != 2 || !got.Custom {
		s.t.Fatalf("custom = %+v, %v", got, err)
	}
	_, err = repo.Get(ctx, domain.NewID(), custom.ID)
	s.want(err, store.ErrNotFound)
	owner := s.role(domain.RoleScopeOrganization, domain.RoleOwner)
	s.want(repo.Update(ctx, &owner), store.ErrNotFound)
	s.want(repo.Delete(ctx, s.account, owner.ID), store.ErrNotFound)

	user := s.seed.Member(s.t, s.account)
	b := s.bind(domain.SubjectUser, user, custom, s.project)
	s.want(repo.Delete(ctx, s.account, custom.ID), store.ErrConflict)
	s.must(s.st.RoleBindings().Delete(ctx, s.account, b.ID))
	s.must(repo.Delete(ctx, s.account, custom.ID))
}

func (s *accessSuite) bindings() {
	ctx, repo := s.t.Context(), s.st.RoleBindings()
	user := s.seed.Member(s.t, s.account)
	viewer := s.role(domain.RoleScopeProject, domain.RoleViewer)
	onProject := s.bind(domain.SubjectUser, user, viewer, s.project)
	onOrg := s.bind(domain.SubjectUser, user, viewer, uuid.Nil())
	dup := onProject
	dup.ID = uuid.Nil()
	s.want(repo.Create(ctx, &dup), store.ErrConflict)
	stranger := domain.RoleBinding{AccountID: s.account, SubjectKind: domain.SubjectUser, SubjectID: domain.NewID(), RoleID: viewer.ID, CreatedBy: "user:op"}
	if err := repo.Create(ctx, &stranger); err == nil {
		s.t.Error("bound a stranger")
	}
	got, err := repo.Get(ctx, s.account, onProject.ID)
	if err != nil || got.ProjectID != s.project || got.CreatedBy != "user:op" {
		s.t.Fatalf("got = %+v, %v", got, err)
	}
	mine := store.RoleBindingFilter{SubjectKind: domain.SubjectUser, SubjectID: user}
	if all, err := repo.List(ctx, s.account, mine); err != nil || len(all) != 2 {
		s.t.Fatalf("list = %+v, %v", all, err)
	}
	f := mine
	f.ProjectID = s.project
	if all, _ := repo.List(ctx, s.account, f); len(all) != 1 || all[0].ID != onProject.ID {
		s.t.Errorf("on project = %+v", all)
	}
	f = mine
	f.OrganizationOnly = true
	if all, _ := repo.List(ctx, s.account, f); len(all) != 1 || all[0].ID != onOrg.ID {
		s.t.Errorf("on organization = %+v", all)
	}
	s.must(repo.Delete(ctx, s.account, onProject.ID))
	s.want(repo.Delete(ctx, s.account, onProject.ID), store.ErrNotFound)
	s.must(repo.Delete(ctx, s.account, onOrg.ID))

	first := s.seed.Member(s.t, s.account)
	second := s.seed.Member(s.t, s.account)
	owner := s.role(domain.RoleScopeOrganization, domain.RoleOwner)
	a := s.bind(domain.SubjectUser, first, owner, uuid.Nil())
	b := s.bind(domain.SubjectUser, second, owner, uuid.Nil())
	s.must(repo.Delete(ctx, s.account, a.ID))
	s.want(repo.Delete(ctx, s.account, b.ID), store.ErrLastOwner)
}

func (s *accessSuite) effective() {
	ctx, repo := s.t.Context(), s.st.RoleBindings()
	user := s.seed.Member(s.t, s.account)
	team := s.team("effective")
	viewer := s.role(domain.RoleScopeProject, domain.RoleViewer)
	maintainer := s.role(domain.RoleScopeProject, domain.RoleMaintainer)
	s.bind(domain.SubjectUser, user, viewer, s.project)
	s.bind(domain.SubjectTeam, team.ID, maintainer, s.project)
	count := func(kind domain.SubjectKind, id uuid.UUID) int {
		s.t.Helper()
		got, err := repo.Effective(ctx, s.account, kind, id)
		s.must(err)
		return len(got)
	}
	if n := count(domain.SubjectUser, user); n != 1 {
		s.t.Fatalf("own bindings = %d", n)
	}
	m := domain.TeamMember{TeamID: team.ID, UserID: user, AccountID: s.account, Source: domain.SourceManual}
	s.must(s.st.Teams().AddMember(ctx, &m))
	got, err := repo.Effective(ctx, s.account, domain.SubjectUser, user)
	s.must(err)
	if len(got) != 2 || !slices.ContainsFunc(got, func(b store.BoundRole) bool { return b.Role.Has(domain.PermGateApproveMerge) }) {
		s.t.Fatalf("effective = %+v", got)
	}
	s.seed.SetStatus(s.t, s.account, user, domain.MembershipSuspended)
	if n := count(domain.SubjectUser, user); n != 0 {
		s.t.Errorf("suspended member has %d bindings", n)
	}
	s.seed.SetStatus(s.t, s.account, user, domain.MembershipActive)
	s.must(s.st.Teams().RemoveMember(ctx, s.account, team.ID, user))
	if n := count(domain.SubjectUser, user); n != 1 {
		s.t.Errorf("after leaving the team: %d", n)
	}

	sa := domain.ServiceAccount{AccountID: s.account, Name: "robot", CreatedBy: "user:op"}
	s.must(s.st.ServiceAccounts().Create(ctx, &sa))
	s.bind(domain.SubjectServiceAccount, sa.ID, viewer, s.project)
	if n := count(domain.SubjectServiceAccount, sa.ID); n != 1 {
		s.t.Errorf("service account: %d", n)
	}
	s.must(s.st.ServiceAccounts().SetDisabled(ctx, s.account, sa.ID, true))
	if n := count(domain.SubjectServiceAccount, sa.ID); n != 0 {
		s.t.Errorf("disabled service account: %d", n)
	}
	if n := count(domain.SubjectTeam, team.ID); n != 0 {
		s.t.Errorf("a team has no effective bindings: %d", n)
	}
}

func (s *accessSuite) directory() {
	ctx, repo := s.t.Context(), s.st.Directory()
	user := s.seed.Member(s.t, s.account)
	if _, err := repo.User(ctx, user); err != nil {
		s.t.Fatal(err)
	}
	_, err := repo.User(ctx, domain.NewID())
	s.want(err, store.ErrNotFound)
	s.seed.Link(s.t, s.account, user, 583231, "Octocat")
	for _, q := range []struct {
		id    int64
		login string
		want  int
	}{{583231, "renamed", 1}, {0, "octocat", 1}, {7, "octocat", 0}, {0, "ghost", 0}} {
		got, err := repo.GitHubMembers(ctx, s.account, q.id, q.login)
		s.must(err)
		if len(got) != q.want || (q.want == 1 && got[0] != user) {
			s.t.Errorf("GitHubMembers(%d, %s) = %v", q.id, q.login, got)
		}
	}
	got, err := repo.GitHubMembers(ctx, domain.NewID(), 583231, "")
	if err != nil || len(got) != 0 {
		s.t.Errorf("another organization: %v, %v", got, err)
	}
}

// Bind gives a built-in role to a subject, on project or, when it is zero, on
// the organization.
func Bind(t *testing.T, st store.Store, accountID uuid.UUID, kind domain.SubjectKind, id uuid.UUID, scope domain.RoleScope, role string, project uuid.UUID) domain.RoleBinding {
	t.Helper()
	r, err := st.Roles().GetByName(t.Context(), accountID, scope, role)
	if err != nil {
		t.Fatal(err)
	}
	b := domain.RoleBinding{AccountID: accountID, SubjectKind: kind, SubjectID: id, RoleID: r.ID, ProjectID: project, CreatedBy: "user:test"}
	if err := st.RoleBindings().Create(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

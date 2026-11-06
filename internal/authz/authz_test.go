package authz_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

type fixture struct {
	st       *testkit.Store
	engine   *authz.Engine
	account  uuid.UUID
	app, api domain.Project
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "acme"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	f := &fixture{st: st, engine: authz.New(st, nil), account: acc.ID}
	f.app = domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app"}
	f.api = domain.Project{AccountID: acc.ID, Name: "api", Repo: "acme/api"}
	must(t, st.Projects().Create(t.Context(), &f.app))
	must(t, st.Projects().Create(t.Context(), &f.api))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) user(t *testing.T) (domain.Actor, uuid.UUID) {
	t.Helper()
	u := f.st.AddMember(f.account, domain.User{Name: "someone"}, "")
	return domain.UserActor(f.account, u.ID, domain.Credential{}), u.ID
}

func (f *fixture) can(t *testing.T, by domain.Actor, p domain.Permission, res authz.Resource) bool {
	t.Helper()
	err := f.engine.Authorize(t.Context(), by, p, res)
	if err != nil && !errors.Is(err, authz.ErrDenied) {
		t.Fatal(err)
	}
	return err == nil
}

func TestEntityNames(t *testing.T) {
	id := uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	for got, want := range map[string]string{
		authz.Action(domain.PermGateApproveMerge).String(): `Action::"gate.approve.merge"`,
		authz.Project(id, id).UID().String():               `Project::"018f0000-0000-7000-8000-000000000001"`,
		authz.Organization(id).UID().String():              `Organization::"018f0000-0000-7000-8000-000000000001"`,
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// TestBuiltInRoles samples what each built-in role may and may not do, at
// organization and at project scope.
func TestBuiltInRoles(t *testing.T) {
	const org, project = domain.RoleScopeOrganization, domain.RoleScopeProject
	tests := []struct {
		scope   domain.RoleScope
		role    string
		onOrg   bool // bound on the organization
		allowed []domain.Permission
		refused []domain.Permission
	}{
		{org, domain.RoleOwner, true,
			[]domain.Permission{domain.PermOrgManage, domain.PermBillingManage, domain.PermGateApproveMerge, domain.PermProjectCreate}, nil},
		{org, domain.RoleAdmin, true,
			[]domain.Permission{domain.PermMemberManage, domain.PermTeamManage, domain.PermGateApproveMerge, domain.PermProjectManage},
			[]domain.Permission{domain.PermOrgManage, domain.PermBillingManage}},
		{org, domain.RoleBilling, true,
			[]domain.Permission{domain.PermBillingManage, domain.PermOrgRead},
			[]domain.Permission{domain.PermProjectRead, domain.PermMemberManage, domain.PermGateApproveSpec}},
		{org, domain.RoleMember, true,
			[]domain.Permission{domain.PermOrgRead, domain.PermGoalCreate, domain.PermKnowledgePropose, domain.PermGateRead},
			[]domain.Permission{domain.PermJobDispatch, domain.PermGateApproveSpec, domain.PermTeamManage}},
		{org, domain.RoleViewer, true,
			[]domain.Permission{domain.PermProjectRead, domain.PermWorkerRead},
			[]domain.Permission{domain.PermGoalCreate, domain.PermJobCancel}},
		{project, domain.RoleProjectAdmin, false,
			[]domain.Permission{domain.PermProjectManage, domain.PermGateApproveMerge, domain.PermGoalCreate},
			[]domain.Permission{domain.PermOrgRead, domain.PermMemberManage, domain.PermProjectCreate}},
		{project, domain.RoleMaintainer, false,
			[]domain.Permission{domain.PermGateApproveMerge, domain.PermGateApproveProfile, domain.PermAssumptionResolve},
			[]domain.Permission{domain.PermProjectManage, domain.PermWorkerRead}},
		{project, domain.RoleContributor, false,
			[]domain.Permission{domain.PermGateApproveSpec, domain.PermGateApproveCouncil, domain.PermJobDispatch, domain.PermGoalCreate},
			[]domain.Permission{domain.PermGateApproveMerge, domain.PermGateApproveProfile, domain.PermGateApproveKnowledgeProtected, domain.PermProposalDecide}},
		{project, domain.RoleViewer, false,
			[]domain.Permission{domain.PermProjectRead, domain.PermGateRead},
			[]domain.Permission{domain.PermGoalCreate, domain.PermGateApproveSpec}},
	}
	for _, tt := range tests {
		t.Run(string(tt.scope)+"/"+tt.role, func(t *testing.T) {
			f := newFixture(t)
			by, id := f.user(t)
			on, res := uuid.Nil(), authz.Organization(f.account)
			if !tt.onOrg {
				on, res = f.app.ID, authz.Project(f.account, f.app.ID)
			}
			testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, tt.scope, tt.role, on)
			f.wantAll(t, by, tt.allowed, res, true)
			f.wantAll(t, by, tt.refused, res, false)
			projectScoped := slices.DeleteFunc(slices.Clone(tt.allowed), func(p domain.Permission) bool { return !p.ProjectScoped() })
			// A role on the organization reaches every project; a project
			// role says nothing about another project or the organization.
			f.wantAll(t, by, projectScoped, authz.Project(f.account, f.api.ID), tt.onOrg)
			if !tt.onOrg {
				f.wantAll(t, by, projectScoped, authz.Organization(f.account), false)
			}
		})
	}
}

// wantAll checks by's answer for each permission on res.
func (f *fixture) wantAll(t *testing.T, by domain.Actor, perms []domain.Permission, res authz.Resource, want bool) {
	t.Helper()
	for _, p := range perms {
		if got := f.can(t, by, p, res); got != want {
			t.Errorf("%s on %s = %v, want %v", p, res.UID(), got, want)
		}
	}
}

func TestProjectRoleOnTheOrganization(t *testing.T) {
	f := newFixture(t)
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleContributor, uuid.Nil())
	for _, p := range []domain.Project{f.app, f.api} {
		if !f.can(t, by, domain.PermGateApproveSpec, authz.Project(f.account, p.ID)) {
			t.Errorf("refused on %s", p.Name)
		}
	}
}

func TestTeamBinding(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	team := domain.Team{AccountID: f.account, Name: "Core", Slug: "core", Source: domain.SourceManual}
	must(t, f.st.Teams().Create(ctx, &team))
	testkit.Bind(t, f.st, f.account, domain.SubjectTeam, team.ID, domain.RoleScopeProject, domain.RoleMaintainer, f.app.ID)
	res := authz.Project(f.account, f.app.ID)
	if f.can(t, by, domain.PermGateApproveMerge, res) {
		t.Fatal("allowed before joining the team")
	}
	must(t, f.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: team.ID, UserID: id, AccountID: f.account, Source: domain.SourceManual}))
	if !f.can(t, by, domain.PermGateApproveMerge, res) {
		t.Fatal("refused in the team")
	}
	must(t, f.st.Teams().RemoveMember(ctx, f.account, team.ID, id))
	if f.can(t, by, domain.PermGateApproveMerge, res) {
		t.Fatal("allowed after leaving the team")
	}
}

func TestInactiveSubjects(t *testing.T) {
	f := newFixture(t)
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	res := authz.Organization(f.account)
	if !f.can(t, by, domain.PermOrgRead, res) {
		t.Fatal("refused")
	}
	f.st.SetMemberStatus(f.account, id, domain.MembershipSuspended)
	if f.can(t, by, domain.PermOrgRead, res) {
		t.Error("a suspended member keeps permissions")
	}
	sa := domain.ServiceAccount{AccountID: f.account, Name: "ci", CreatedBy: "user:op"}
	must(t, f.st.ServiceAccounts().Create(t.Context(), &sa))
	testkit.Bind(t, f.st, f.account, domain.SubjectServiceAccount, sa.ID, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	robot := domain.ServiceAccountActor(f.account, sa.ID, uuid.Nil())
	robot.Credential = domain.Credential{}
	if !f.can(t, robot, domain.PermOrgRead, res) {
		t.Error("service account refused")
	}
	must(t, f.st.ServiceAccounts().SetDisabled(t.Context(), f.account, sa.ID, true))
	if f.can(t, robot, domain.PermOrgRead, res) {
		t.Error("a disabled service account keeps permissions")
	}
}

func (f *fixture) token(t *testing.T, tok domain.MCPToken) domain.MCPToken {
	t.Helper()
	tok.AccountID = f.account
	tok.TokenHash = mcp.HashToken(tok.Name)
	must(t, f.st.MCPTokens().Create(t.Context(), &tok))
	return tok
}

// TestTokenIntersection: a token is limited by its scopes and its owner's
// current roles, so a role downgrade takes power from it at once.
func TestTokenIntersection(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, id := f.user(t)
	binding := testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	admin := f.token(t, domain.MCPToken{Name: "admin", OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: []domain.Scope{domain.ScopeAdmin}})
	read := f.token(t, domain.MCPToken{Name: "read", OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: []domain.Scope{domain.ScopeRead}})
	byAdmin, byRead := mcp.TokenActor(admin), mcp.TokenActor(read)
	res := authz.Project(f.account, f.app.ID)
	org := authz.Organization(f.account)

	if !f.can(t, byAdmin, domain.PermMemberManage, org) {
		t.Error("admin token of an Admin refused")
	}
	if !f.can(t, byRead, domain.PermGoalRead, res) {
		t.Error("read token refused a read")
	}
	if f.can(t, byRead, domain.PermGateApproveMerge, res) || f.can(t, byRead, domain.PermMemberManage, org) {
		t.Error("read token allowed beyond its scope")
	}
	if f.can(t, byAdmin, domain.PermOrgManage, org) {
		t.Error("admin token allowed beyond its owner")
	}

	// The downgrade: Admin becomes Viewer.
	must(t, f.st.RoleBindings().Delete(ctx, f.account, binding.ID))
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	if f.can(t, byAdmin, domain.PermMemberManage, org) || f.can(t, byAdmin, domain.PermGateApproveMerge, res) {
		t.Error("the token outlived the role change")
	}
	if !f.can(t, byAdmin, domain.PermGoalRead, res) {
		t.Error("the token lost what its owner kept")
	}
}

func TestTokenStates(t *testing.T) {
	f := newFixture(t)
	_, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	org := authz.Organization(f.account)
	expired := f.token(t, domain.MCPToken{Name: "old", OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: []domain.Scope{domain.ScopeRead}, ExpiresAt: time.Now().Add(-time.Hour)})
	if f.can(t, mcp.TokenActor(expired), domain.PermOrgRead, org) {
		t.Error("expired token allowed")
	}
	other, _ := f.user(t)
	stolen := other
	stolen.Credential = domain.Credential{Kind: domain.CredentialToken, ID: expired.ID}
	if f.can(t, stolen, domain.PermOrgRead, org) {
		t.Error("a token of another owner allowed")
	}
	grant := domain.UserActor(f.account, id, domain.Credential{Kind: domain.CredentialOAuthGrant, ID: domain.NewID()})
	if f.can(t, grant, domain.PermOrgRead, org) {
		t.Error("an OAuth grant is allowed without scopes")
	}
	session := domain.UserActor(f.account, id, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
	if !f.can(t, session, domain.PermOrgRead, org) {
		t.Error("a session is refused")
	}
}

func TestAuthorizeScope(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, id := f.user(t)
	read := f.token(t, domain.MCPToken{Name: "read", OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: []domain.Scope{domain.ScopeRead}})
	by := mcp.TokenActor(read)
	must(t, f.engine.AuthorizeScope(ctx, by, domain.ScopeRead))
	err := f.engine.AuthorizeScope(ctx, by, domain.ScopeWrite)
	if !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "write") {
		t.Fatalf("err = %v", err)
	}
	entries, err := f.st.Audit().List(ctx, f.account, store.AuditFilter{})
	must(t, err)
	if len(entries) != 1 || entries[0].Action != "access.denied" || entries[0].Credential != by.Credential.String() || !strings.Contains(string(entries[0].Data), `"scope":"write"`) {
		t.Fatalf("audit = %+v", entries)
	}
	must(t, f.engine.AuthorizeScope(ctx, domain.UserActor(f.account, id, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()}), domain.ScopeAdmin))

	// A grant has no scopes until T55, and a denied read is not audited.
	grant := domain.UserActor(f.account, id, domain.Credential{Kind: domain.CredentialOAuthGrant, ID: domain.NewID()})
	if err := f.engine.AuthorizeScope(ctx, grant, domain.ScopeRead); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("grant = %v", err)
	}
	if entries, _ = f.st.Audit().List(ctx, f.account, store.AuditFilter{}); len(entries) != 1 {
		t.Errorf("audit = %+v", entries)
	}
}

func TestOwnerlessToken(t *testing.T) {
	f := newFixture(t)
	admin := f.token(t, domain.MCPToken{Name: "legacy"})
	read := f.token(t, domain.MCPToken{Name: "reader", Scopes: []domain.Scope{domain.ScopeRead}})
	org := authz.Organization(f.account)
	if !f.can(t, mcp.TokenActor(admin), domain.PermOrgManage, org) {
		t.Error("ownerless admin token refused")
	}
	if !f.can(t, mcp.TokenActor(read), domain.PermGoalRead, authz.Project(f.account, f.app.ID)) {
		t.Error("ownerless read token refused a read")
	}
	if f.can(t, mcp.TokenActor(read), domain.PermGoalCreate, authz.Project(f.account, f.app.ID)) {
		t.Error("ownerless read token allowed a write")
	}
	ghost := domain.Actor{AccountID: f.account, Kind: domain.ActorMCP, Name: "ghost"}
	if f.can(t, ghost, domain.PermOrgRead, org) {
		t.Error("an unknown token allowed")
	}
	must(t, f.st.MCPTokens().Revoke(t.Context(), f.account, admin.ID))
	if f.can(t, mcp.TokenActor(admin), domain.PermOrgRead, org) {
		t.Error("a revoked token allowed")
	}
	// Limited to its organization, whatever its scopes.
	if f.can(t, mcp.TokenActor(read), domain.PermGoalRead, authz.Organization(domain.NewID())) {
		t.Error("allowed in another organization")
	}
}

func TestOtherActorsHoldNothing(t *testing.T) {
	f := newFixture(t)
	org := authz.Organization(f.account)
	for _, by := range []domain.Actor{
		domain.SystemActor(f.account, domain.ForecrewdName),
		{AccountID: f.account, Kind: domain.ActorWorker, Name: "box"},
		{AccountID: f.account, Kind: domain.ActorGitHub, Name: "octocat"},
		{AccountID: f.account, Kind: domain.ActorUser, Name: "operator"},
	} {
		if f.can(t, by, domain.PermOrgRead, org) {
			t.Errorf("%s allowed", by)
		}
	}
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
	if f.can(t, by, "goal.delete", org) {
		t.Error("a permission outside the catalog allowed")
	}
}

func TestProjectsAndAnywhere(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	got, err := f.engine.Projects(ctx, by, domain.PermGoalRead)
	must(t, err)
	if got.Any() {
		t.Fatalf("no role, projects = %+v", got)
	}
	if err := f.engine.AuthorizeAnywhere(ctx, by, domain.PermGoalRead); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("anywhere = %v", err)
	}
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleViewer, f.app.ID)
	got, err = f.engine.Projects(ctx, by, domain.PermGoalRead)
	must(t, err)
	if got.All || !got.Has(f.app.ID) || got.Has(f.api.ID) {
		t.Fatalf("project viewer = %+v", got)
	}
	must(t, f.engine.AuthorizeAnywhere(ctx, by, domain.PermGoalRead))
	if err := f.engine.AuthorizeAnywhere(ctx, by, domain.PermGoalCreate); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("anywhere without it = %v", err)
	}
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	got, err = f.engine.Projects(ctx, by, domain.PermGoalRead)
	must(t, err)
	if !got.All || !got.Has(f.api.ID) {
		t.Fatalf("organization viewer = %+v", got)
	}
}

func TestGates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleContributor, f.app.ID)
	gate := func(k domain.GateKind, p domain.Project) domain.Gate {
		return domain.Gate{AccountID: f.account, ProjectID: p.ID, Kind: k}
	}
	for _, tt := range []struct {
		g    domain.Gate
		want bool
	}{
		{gate(domain.GateSpecApproval, f.app), true},
		{gate(domain.GateMergeApproval, f.app), false},
		{gate(domain.GateSpecApproval, f.api), false},
		{gate(domain.GateKnowledgeProposal, f.app), false}, // without a Protection every one counts as protected
		{gate("unknown", f.app), false},
	} {
		can, err := f.engine.CanResolve(ctx, by, tt.g)
		must(t, err)
		err = f.engine.AuthorizeGate(ctx, by, tt.g)
		if can != tt.want || (err == nil) != tt.want {
			t.Errorf("%s on %s: can %v, authorize %v", tt.g.Kind, tt.g.ProjectID, can, err)
		}
	}
	// A service account never resolves a gate, whatever role it holds.
	sa := domain.ServiceAccount{AccountID: f.account, Name: "ci", CreatedBy: "user:op"}
	must(t, f.st.ServiceAccounts().Create(ctx, &sa))
	testkit.Bind(t, f.st, f.account, domain.SubjectServiceAccount, sa.ID, domain.RoleScopeProject, domain.RoleMaintainer, f.app.ID)
	tok := f.token(t, domain.MCPToken{Name: "ci", OwnerKind: domain.SubjectServiceAccount, OwnerID: sa.ID, Scopes: []domain.Scope{domain.ScopeWrite}})
	robot := mcp.TokenActor(tok)
	g := gate(domain.GateSpecApproval, f.app)
	if !f.can(t, robot, domain.PermGateApproveSpec, authz.Project(f.account, f.app.ID)) {
		t.Fatal("the role does not reach the service account")
	}
	if can, err := f.engine.CanResolve(ctx, robot, g); can || err != nil {
		t.Errorf("service account can resolve: %v %v", can, err)
	}
	if err := f.engine.AuthorizeGate(ctx, robot, g); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("service account resolves: %v", err)
	}
}

// protectionFunc answers for the proposals of a gate.
type protectionFunc func(domain.Gate) (bool, error)

func (f protectionFunc) GateProtected(_ context.Context, g domain.Gate) (bool, error) { return f(g) }

func TestKnowledgeGatesFollowTheirProposals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleContributor, f.app.ID)
	g := domain.Gate{AccountID: f.account, ProjectID: f.app.ID, Kind: domain.GateKnowledgeProposal}
	for _, tt := range []struct {
		name      string
		protected bool
		err       error
		want      domain.Permission
		can       bool
	}{
		{"not protected", false, nil, domain.PermGateApproveKnowledge, true},
		{"protected", true, nil, domain.PermGateApproveKnowledgeProtected, false},
		{"unknown", false, errors.New("boom"), domain.PermGateApproveKnowledgeProtected, false}, // never the weaker permission
	} {
		e := authz.New(f.st, nil).WithProtection(protectionFunc(func(domain.Gate) (bool, error) { return tt.protected, tt.err }))
		if got := e.GatePermission(ctx, g); got != tt.want {
			t.Errorf("%s: permission = %s", tt.name, got)
		}
		can, err := e.CanResolve(ctx, by, g)
		if err != nil || can != tt.can {
			t.Errorf("%s: can = %v, %v", tt.name, can, err)
		}
		if err := e.AuthorizeGate(ctx, by, g); (err == nil) != tt.can {
			t.Errorf("%s: authorize = %v", tt.name, err)
		}
	}
	// Other kinds never ask.
	e := authz.New(f.st, nil).WithProtection(protectionFunc(func(domain.Gate) (bool, error) { t.Error("asked"); return true, nil }))
	if got := e.GatePermission(ctx, domain.Gate{Kind: domain.GateMergeApproval}); got != domain.PermGateApproveMerge {
		t.Errorf("permission = %s", got)
	}
}

func TestAuthorizeProtectedKnowledge(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	maintainer, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleMaintainer, f.app.ID)
	must(t, f.engine.AuthorizeProtectedKnowledge(ctx, maintainer, f.app.ID))
	if err := f.engine.AuthorizeProtectedKnowledge(ctx, maintainer, f.api.ID); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("another project: %v", err)
	}
	contributor, cid := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, cid, domain.RoleScopeProject, domain.RoleContributor, f.app.ID)
	if err := f.engine.AuthorizeProtectedKnowledge(ctx, contributor, f.app.ID); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("contributor: %v", err)
	}
	sa := domain.ServiceAccount{AccountID: f.account, Name: "ci", CreatedBy: "user:op"}
	must(t, f.st.ServiceAccounts().Create(ctx, &sa))
	testkit.Bind(t, f.st, f.account, domain.SubjectServiceAccount, sa.ID, domain.RoleScopeProject, domain.RoleMaintainer, f.app.ID)
	robot := domain.Actor{AccountID: f.account, Kind: domain.ActorServiceAccount, Name: sa.ID.String()}
	if err := f.engine.AuthorizeProtectedKnowledge(ctx, robot, f.app.ID); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("service account: %v", err)
	}
}

func TestDenialsOfWritesAreAudited(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	cred := domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()}
	u := f.st.AddMember(f.account, domain.User{}, "")
	by := domain.UserActor(f.account, u.ID, cred)
	res := authz.Project(f.account, f.app.ID)
	_ = f.engine.Authorize(ctx, by, domain.PermGoalRead, res)
	entries, err := f.st.Audit().List(ctx, f.account, store.AuditFilter{})
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("a denied read is audited: %+v", entries)
	}
	err = f.engine.Authorize(ctx, by, domain.PermGateApproveMerge, res)
	if !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "gate.approve.merge") {
		t.Fatalf("err = %v", err)
	}
	entries, err = f.st.Audit().List(ctx, f.account, store.AuditFilter{})
	must(t, err)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	var data map[string]string
	must(t, json.Unmarshal(e.Data, &data))
	if e.Action != "access.denied" || e.Actor != by.String() || e.Credential != cred.String() || e.ProjectID != f.app.ID || e.SubjectType != "project" ||
		data["permission"] != "gate.approve.merge" || data["resource"] != res.UID().String() || data["action"] != `Action::"gate.approve.merge"` {
		t.Errorf("entry = %+v, data %v", e, data)
	}
	// On the organization, and in another organization nothing is written there.
	_ = f.engine.Authorize(ctx, by, domain.PermMemberManage, authz.Organization(f.account))
	entries, _ = f.st.Audit().List(ctx, f.account, store.AuditFilter{})
	if len(entries) != 2 || entries[0].SubjectType != "organization" || entries[0].SubjectID != f.account {
		t.Errorf("entries = %+v", entries)
	}
	_ = f.engine.Authorize(ctx, by, domain.PermMemberManage, authz.Organization(domain.NewID()))
	if entries, _ = f.st.Audit().List(ctx, f.account, store.AuditFilter{}); len(entries) != 2 {
		t.Errorf("a denial in another organization is audited here: %+v", entries)
	}
}

// failing is a store whose role bindings cannot be read.
type failing struct {
	store.Store
	bindings store.RoleBindingRepo
	tokens   store.PersonalTokenRepo
}

func (s failing) RoleBindings() store.RoleBindingRepo { return s.bindings }
func (s failing) MCPTokens() store.PersonalTokenRepo  { return s.tokens }

type countingBindings struct {
	store.RoleBindingRepo
	reads int
	err   error
}

func (c *countingBindings) Effective(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, id uuid.UUID) ([]store.BoundRole, error) {
	c.reads++
	if c.err != nil {
		return nil, c.err
	}
	return c.RoleBindingRepo.Effective(ctx, accountID, kind, id)
}

type failingTokens struct {
	store.PersonalTokenRepo
	err error
}

func (f failingTokens) List(context.Context, uuid.UUID) ([]domain.MCPToken, error) {
	return nil, f.err
}

func TestCachePerRequest(t *testing.T) {
	f := newFixture(t)
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	counting := &countingBindings{RoleBindingRepo: f.st.RoleBindings()}
	e := authz.New(failing{f.st, counting, f.st.MCPTokens()}, nil)
	org := authz.Organization(f.account)

	ctx := e.Request(t.Context())
	for range 3 {
		must(t, e.Authorize(ctx, by, domain.PermOrgRead, org))
	}
	if counting.reads != 1 {
		t.Errorf("reads in one request = %d", counting.reads)
	}
	// A new request sees a change of bindings.
	must(t, e.Authorize(e.Request(t.Context()), by, domain.PermOrgRead, org))
	if counting.reads != 2 {
		t.Errorf("reads in two requests = %d", counting.reads)
	}
	// Without a request context nothing is cached.
	must(t, e.Authorize(t.Context(), by, domain.PermOrgRead, org))
	if counting.reads != 3 {
		t.Errorf("reads without a request = %d", counting.reads)
	}
}

func TestStoreErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	boom := errors.New("boom")
	by, _ := f.user(t)
	e := authz.New(failing{f.st, &countingBindings{RoleBindingRepo: f.st.RoleBindings(), err: boom}, f.st.MCPTokens()}, nil)
	org := authz.Organization(f.account)
	if err := e.Authorize(ctx, by, domain.PermOrgRead, org); !errors.Is(err, boom) || errors.Is(err, authz.ErrDenied) {
		t.Errorf("Authorize = %v", err)
	}
	if _, err := e.Projects(ctx, by, domain.PermOrgRead); !errors.Is(err, boom) {
		t.Errorf("Projects = %v", err)
	}
	if err := e.AuthorizeAnywhere(ctx, by, domain.PermOrgRead); !errors.Is(err, boom) {
		t.Errorf("AuthorizeAnywhere = %v", err)
	}
	if _, err := e.EffectiveOf(ctx, by); !errors.Is(err, boom) {
		t.Errorf("EffectiveOf = %v", err)
	}
	if err := e.AuthorizeScope(ctx, by, domain.ScopeRead); !errors.Is(err, boom) {
		t.Errorf("AuthorizeScope = %v", err)
	}
	e = authz.New(failing{f.st, f.st.RoleBindings(), failingTokens{f.st.MCPTokens(), boom}}, nil)
	ownerless := domain.Actor{AccountID: f.account, Kind: domain.ActorMCP, Name: "x"}
	if _, err := e.Can(ctx, ownerless, domain.PermOrgRead, org); !errors.Is(err, boom) {
		t.Errorf("Can = %v", err)
	}
	token := domain.UserActor(f.account, domain.NewID(), domain.Credential{Kind: domain.CredentialToken, ID: domain.NewID()})
	if _, err := e.Can(ctx, token, domain.PermOrgRead, org); !errors.Is(err, boom) {
		t.Errorf("Can with a token = %v", err)
	}
}

// evaluatorFunc is a policy engine in one function.
type evaluatorFunc func(authz.Request, []store.BoundRole) bool

func (f evaluatorFunc) IsAuthorized(r authz.Request, b []store.BoundRole) bool { return f(r, b) }

func TestEvaluatorIsReplaceable(t *testing.T) {
	f := newFixture(t)
	by, id := f.user(t)
	var seen authz.Request
	e := authz.New(f.st, nil).WithEvaluator(evaluatorFunc(func(r authz.Request, _ []store.BoundRole) bool {
		seen = r
		return r.Action.ID == "goal.read"
	}))
	res := authz.Project(f.account, f.app.ID)
	must(t, e.Authorize(t.Context(), by, domain.PermGoalRead, res))
	if seen.Principal.String() != `User::"`+id.String()+`"` || seen.Action.String() != `Action::"goal.read"` || seen.Resource != res.UID() {
		t.Errorf("request = %+v", seen)
	}
	if err := e.Authorize(t.Context(), by, domain.PermJobRead, res); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("err = %v", err)
	}
}

func TestCovers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	role := func(scope domain.RoleScope, name string) domain.Role {
		r, ok := domain.BuiltInRole(scope, name)
		if !ok {
			t.Fatal(name)
		}
		return r
	}
	org := authz.Organization(f.account)
	must(t, f.engine.Covers(ctx, by, role(domain.RoleScopeOrganization, domain.RoleMember), org))
	err := f.engine.Covers(ctx, by, role(domain.RoleScopeOrganization, domain.RoleBilling), org)
	if !errors.Is(err, authz.ErrDenied) || !strings.Contains(err.Error(), "billing") {
		t.Errorf("Admin covers Billing: %v", err)
	}
}

func TestEffectiveOf(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.user(t)
	team := domain.Team{AccountID: f.account, Name: "Core", Slug: "core", Source: domain.SourceManual}
	must(t, f.st.Teams().Create(ctx, &team))
	must(t, f.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: team.ID, UserID: id, AccountID: f.account, Source: domain.SourceManual}))
	testkit.Bind(t, f.st, f.account, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	testkit.Bind(t, f.st, f.account, domain.SubjectTeam, team.ID, domain.RoleScopeProject, domain.RoleContributor, f.app.ID)
	got, err := f.engine.EffectiveOf(ctx, by)
	must(t, err)
	if len(got.Roles) != 2 || got.Roles[0].Role != domain.RoleBilling || got.Roles[1].TeamID != team.ID || got.Roles[1].ProjectID != f.app.ID {
		t.Fatalf("roles = %+v", got.Roles)
	}
	has := func(perms []domain.Permission, p domain.Permission) bool {
		return slices.Contains(perms, p)
	}
	if !has(got.Permissions, domain.PermBillingManage) || has(got.Permissions, domain.PermGoalRead) {
		t.Errorf("organization permissions = %v", got.Permissions)
	}
	app := got.Projects[f.app.ID.String()]
	if !has(app, domain.PermGateApproveSpec) || has(app, domain.PermGateApproveMerge) {
		t.Errorf("project permissions = %v", app)
	}
	if _, ok := got.Projects[f.api.ID.String()]; ok {
		t.Errorf("a project without a role is listed: %v", got.Projects)
	}

	read := f.token(t, domain.MCPToken{Name: "read", Scopes: []domain.Scope{domain.ScopeRead}})
	got, err = f.engine.EffectiveOf(ctx, mcp.TokenActor(read))
	must(t, err)
	if has(got.Permissions, domain.PermGoalCreate) || !has(got.Permissions, domain.PermAuditRead) || len(got.Projects) != 2 || len(got.Roles) != 0 {
		t.Errorf("read token = %+v", got)
	}
}

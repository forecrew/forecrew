//go:build enterprise

package ee

import (
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestCustomRoleLifecycle(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	engine := authz.New(e.st, nil)
	project := domain.Project{AccountID: e.org.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main"}
	must(t, e.st.Projects().Create(ctx, &project))
	r, err := e.svc.CreateRole(ctx, e.owner, mcp.CreateRoleInput{Name: "release-manager", Scope: domain.RoleScopeProject, Description: "Merges",
		Permissions: []domain.Permission{domain.PermGateApproveMerge, domain.PermJobRead}})
	must(t, err)
	if !r.Custom || r.BuiltIn || r.Permissions[0] != domain.PermGateApproveMerge {
		t.Fatalf("role = %+v", r)
	}

	// Bind it and enforce through Authorize.
	u := domain.User{Name: "Ann"}
	must(t, e.st.Users().Create(ctx, &u))
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	b := domain.RoleBinding{AccountID: e.org.ID, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: r.ID, ProjectID: project.ID, CreatedBy: "user:x"}
	must(t, e.st.RoleBindings().Create(ctx, &b))
	by := domain.UserActor(e.org.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
	if err := engine.Authorize(ctx, by, domain.PermGateApproveMerge, authz.Project(e.org.ID, project.ID)); err != nil {
		t.Fatalf("bound: %v", err)
	}
	if err := engine.Authorize(ctx, by, domain.PermJobCancel, authz.Project(e.org.ID, project.ID)); !isDenied(err) {
		t.Fatalf("not in the role: %v", err)
	}

	// A change reaches who holds it.
	perms := []domain.Permission{domain.PermJobCancel}
	text := "Cancels"
	got, err := e.svc.UpdateRole(ctx, e.owner, mcp.UpdateRoleInput{Role: "release-manager", Name: "canceller", Description: &text, Permissions: &perms})
	must(t, err)
	if got.Name != "canceller" || got.Description != text {
		t.Fatalf("updated = %+v", got)
	}
	if err := engine.Authorize(ctx, by, domain.PermJobCancel, authz.Project(e.org.ID, project.ID)); err != nil {
		t.Fatalf("after the change: %v", err)
	}

	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "canceller"}); !isConflict(err) {
		t.Fatalf("in use: %v", err)
	}
	must(t, e.st.RoleBindings().Delete(ctx, e.org.ID, b.ID))
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: r.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if !e.audited("role.created") || !e.audited("role.updated") || !e.audited("role.deleted") {
		t.Error("not audited")
	}
}

func TestRoleInUseByInvitationOrConnection(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	r, err := e.svc.CreateRole(ctx, e.owner, mcp.CreateRoleInput{Name: "reader", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermOrgRead}})
	must(t, err)
	inv := domain.Invitation{AccountID: e.org.ID, Email: "a@b.example", TokenHash: make([]byte, 32), InvitedBy: "user:x", Grants: []domain.Grant{{RoleID: r.ID}}, ExpiresAt: t0.Add(time.Hour)}
	must(t, e.st.Invitations().Create(ctx, &inv))
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "reader"}); !isConflict(err) {
		t.Fatalf("pending invitation: %v", err)
	}
	must(t, e.st.Invitations().Revoke(ctx, e.org.ID, inv.ID))
	c := e.connection("okta")
	c.JIT = domain.JIT{Enabled: true, DefaultRoles: []uuid.UUID{r.ID}}
	must(t, e.st.Connections().Update(ctx, &c))
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "reader"}); !isConflict(err) {
		t.Fatalf("JIT default: %v", err)
	}
	c.JIT.DefaultRoles = nil
	must(t, e.st.Connections().Update(ctx, &c))
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "reader"}); err != nil {
		t.Fatal(err)
	}
}

func TestCustomRoleRefusals(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	admin := e.person("admin@example.com", domain.RoleAdmin)
	viewer := e.person("viewer@example.com", domain.RoleViewer)
	in := mcp.CreateRoleInput{Name: "x", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermOrgRead}}
	if _, err := e.svc.CreateRole(ctx, viewer, in); !isDenied(err) {
		t.Errorf("no role.manage: %v", err)
	}
	if _, err := e.svc.UpdateRole(ctx, viewer, mcp.UpdateRoleInput{Role: "x"}); !isDenied(err) {
		t.Errorf("no role.manage: %v", err)
	}
	if _, err := e.svc.DeleteRole(ctx, viewer, mcp.RoleRefInput{Role: "x"}); !isDenied(err) {
		t.Errorf("no role.manage: %v", err)
	}
	// No escalation: an Admin lacks billing.manage.
	billing := []domain.Permission{domain.PermBillingManage}
	if _, err := e.svc.CreateRole(ctx, admin, mcp.CreateRoleInput{Name: "pay", Scope: domain.RoleScopeOrganization, Permissions: billing}); !isDenied(err) {
		t.Errorf("above the caller: %v", err)
	}
	r, err := e.svc.CreateRole(ctx, admin, in)
	must(t, err)
	if _, err := e.svc.UpdateRole(ctx, admin, mcp.UpdateRoleInput{Role: r.Name, Permissions: &billing}); !isDenied(err) {
		t.Errorf("raised above the caller: %v", err)
	}
	high, err := e.svc.CreateRole(ctx, e.owner, mcp.CreateRoleInput{Name: "pay", Scope: domain.RoleScopeOrganization, Permissions: billing})
	must(t, err)
	if _, err := e.svc.UpdateRole(ctx, admin, mcp.UpdateRoleInput{Role: high.Name, Name: "other"}); !isDenied(err) {
		t.Errorf("a role above the caller: %v", err)
	}
	if _, err := e.svc.DeleteRole(ctx, admin, mcp.RoleRefInput{Role: high.Name}); !isDenied(err) {
		t.Errorf("a role above the caller: %v", err)
	}
	// Bad input and built-in roles.
	for name, bad := range map[string]mcp.CreateRoleInput{
		"unknown permission": {Name: "y", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{"nope"}},
		"none":               {Name: "y", Scope: domain.RoleScopeOrganization},
		"org in project":     {Name: "y", Scope: domain.RoleScopeProject, Permissions: []domain.Permission{domain.PermOrgRead}},
		"bad name":           {Name: "Not Valid", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermOrgRead}},
	} {
		if _, err := e.svc.CreateRole(ctx, e.owner, bad); !isInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.CreateRole(ctx, e.owner, in); !isConflict(err) {
		t.Errorf("name taken: %v", err)
	}
	if _, err := e.svc.UpdateRole(ctx, e.owner, mcp.UpdateRoleInput{Role: domain.RoleOwner, Name: "boss"}); !isNotFound(err) {
		t.Errorf("built-in: %v", err)
	}
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: domain.RoleOwner}); !isNotFound(err) {
		t.Errorf("built-in: %v", err)
	}
	empty := []domain.Permission{}
	if _, err := e.svc.UpdateRole(ctx, e.owner, mcp.UpdateRoleInput{Role: r.Name, Permissions: &empty}); !isInvalid(err) {
		t.Errorf("no permissions: %v", err)
	}
	// One name, two scopes: the UUID decides.
	if _, err := e.svc.CreateRole(ctx, e.owner, mcp.CreateRoleInput{Name: "x", Scope: domain.RoleScopeProject, Permissions: []domain.Permission{domain.PermJobRead}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "x"}); !isInvalid(err) {
		t.Errorf("ambiguous: %v", err)
	}
}

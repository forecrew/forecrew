package api_test

import (
	"context"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func orPanic(err error) {
	if err != nil {
		panic(err)
	}
}

// ownedBy makes the token of the fixtures belong to a new member, who holds
// the built-in role (on the project app for a project role), so that /me and
// the lists answer as the engine does for them.
func ownedBy(name string, scope domain.RoleScope, role string, onProject bool) func(*env) {
	return func(e *env) {
		ctx := context.Background()
		u := e.store.AddMember(e.account.ID, domain.User{ID: fid(300), Name: name, Email: name + "@acme.test", EmailVerified: true}, "")
		project := uuid.Nil()
		if onProject {
			p := domain.Project{ID: fid(10), AccountID: e.account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
			orPanic(e.store.Projects().Create(ctx, &p))
			project = p.ID
		}
		r, err := e.store.Roles().GetByName(ctx, e.account.ID, scope, role)
		orPanic(err)
		orPanic(e.store.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: e.account.ID, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: r.ID, ProjectID: project, CreatedBy: "user:test"}))
		_, err = e.store.MCPTokens().AssignOwner(ctx, e.account.ID, domain.SubjectUser, u.ID)
		orPanic(err)
	}
}

// withSessions gives the owner of the token two sessions and an identity.
func withSessions(e *env) {
	ownedBy("ana", domain.RoleScopeOrganization, domain.RoleOwner, false)(e)
	ctx := context.Background()
	conn := domain.Connection{ID: fid(90), Kind: domain.ConnectionGitHub, Name: "github", ClientID: "Iv1.abc", ClientSecretRef: "env:GITHUB_SECRET", Enabled: true}
	orPanic(e.store.Connections().UpsertInstance(ctx, &conn))
	for i, agent := range []string{"Firefox on Linux", "Safari on iPhone"} {
		s := domain.Session{ID: fid(310 + i), TokenHash: []byte(agent), UserID: fid(300), AccountID: e.account.ID, ConnectionID: conn.ID,
			UserAgent: agent, IP: "203.0.113.7", IdleExpiresAt: t0.Add(1000 * 24 * 60 * 60e9), ExpiresAt: t0.Add(1000 * 24 * 60 * 60e9)}
		orPanic(e.store.Sessions().Create(ctx, &s))
	}
	orPanic(e.store.Identities().Create(ctx, &domain.Identity{ID: fid(320), UserID: fid(300), ConnectionID: conn.ID, Subject: "583231", Login: "ana", Email: "ana@acme.test"}))
}

func orgFixtureCases() []fixtureCase {
	members := []mcp.MemberInfo{
		{UserID: fid(300), Name: "Gianmaria", Email: "gianmaria@acme.test", Status: domain.MembershipActive, Source: domain.SourceManual,
			Roles: []mcp.MemberRole{{Role: "owner", Scope: domain.RoleScopeOrganization}}, Teams: []string{}, LastSeenAt: at(-1), CreatedAt: at(-5000)},
		{UserID: fid(301), Name: "Marta Rossi", Email: "marta@acme.test", Status: domain.MembershipActive, Source: domain.SourceManual,
			Roles: []mcp.MemberRole{{Role: "member", Scope: domain.RoleScopeOrganization}, {Role: "maintainer", Scope: domain.RoleScopeProject, Project: "app"}}, Teams: []string{"mobile"}, LastSeenAt: at(-60), CreatedAt: at(-4000)},
		{UserID: fid(302), Name: "Luca Bianchi", Email: "luca@acme.test", Status: domain.MembershipSuspended, Source: domain.SourceManual,
			Roles: []mcp.MemberRole{{Role: "viewer", Scope: domain.RoleScopeOrganization}}, Teams: []string{}, CreatedAt: at(-3000)},
		{UserID: fid(303), Name: "Sam <b>Lee</b>", Email: "sam@acme.test", Status: domain.MembershipActive, Source: domain.SourceSCIM,
			Roles: []mcp.MemberRole{{Role: "admin", Scope: domain.RoleScopeOrganization}}, Teams: []string{"mobile"}, CreatedAt: at(-2000)},
	}
	team := domain.Team{ID: fid(330), AccountID: fid(1), Name: "Mobile", Slug: "mobile", Source: domain.SourceManual, CreatedAt: at(-3000), UpdatedAt: at(-3000)}
	idpTeam := domain.Team{ID: fid(331), AccountID: fid(1), Name: "Platform", Slug: "platform", Source: domain.SourceIDP, CreatedAt: at(-2000), UpdatedAt: at(-2000)}
	account := domain.ServiceAccount{ID: fid(340), AccountID: fid(1), Name: "ci", Description: "Builds and releases", CreatedBy: "user:" + fid(300).String(), CreatedAt: at(-1000)}
	disabled := domain.ServiceAccount{ID: fid(341), AccountID: fid(1), Name: "old-bot", CreatedBy: "user:" + fid(300).String(), DisabledAt: at(-100), CreatedAt: at(-2000)}
	invitations := []domain.Invitation{
		{ID: fid(350), AccountID: fid(1), Email: "new@acme.test", Grants: []domain.Grant{{RoleID: fid(1)}}, ExpiresAt: at(60 * 24), InvitedBy: "user:" + fid(300).String(), CreatedAt: at(-30)},
		{ID: fid(351), AccountID: fid(1), GitHubLogin: "octo", GitHubUserID: 583231, ExpiresAt: at(-60), InvitedBy: "user:" + fid(300).String(), CreatedAt: at(-60 * 24 * 8)},
		{ID: fid(352), AccountID: fid(1), Email: "gone@acme.test", ExpiresAt: at(60 * 24), RevokedAt: at(-10), InvitedBy: "user:" + fid(300).String(), CreatedAt: at(-90)},
	}
	connections := []mcp.ConnectionInfo{
		{ID: fid(90), Kind: domain.ConnectionGitHub, Name: "github", Enabled: true, CreatedAt: at(-9000), UpdatedAt: at(-9000), Instance: true,
			RedirectURL: "https://forecrew.example.com/auth/" + fid(90).String() + "/callback"},
		{ID: fid(91), AccountID: fid(1), Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "0oa1", ClientSecretRef: "env:FORECREW_CONN_OKTA", Enabled: true, CreatedAt: at(-900), UpdatedAt: at(-800),
			Tested: true, RedirectURL: "https://forecrew.example.com/auth/" + fid(91).String() + "/callback", TestURL: "https://forecrew.example.com/auth/" + fid(91).String() + "/login?test=1"},
		{ID: fid(92), AccountID: fid(1), Kind: domain.ConnectionGoogle, Name: "google", ClientID: "123.apps.googleusercontent.com", ClientSecretRef: "secret:conn-google", CreatedAt: at(-90), UpdatedAt: at(-90),
			RedirectURL: "https://forecrew.example.com/auth/" + fid(92).String() + "/callback", TestURL: "https://forecrew.example.com/auth/" + fid(92).String() + "/login?test=1"},
	}
	audit := []mcp.AuditInfo{
		{ID: fid(360), AccountID: fid(1), Actor: "user:" + fid(300).String(), Credential: "session:" + fid(310).String(), Action: "member.suspended", SubjectType: "user", SubjectID: fid(302),
			Data: []byte(`{"name":"Luca Bianchi"}`), CreatedAt: at(-5), ActorName: "Gianmaria"},
		{ID: fid(361), AccountID: fid(1), Actor: "service_account:" + fid(340).String(), Credential: "token:" + fid(90).String(), Action: "team.created", SubjectType: "team", SubjectID: fid(330),
			Data: []byte(`{"name":"<img src=x onerror=alert(1)>","slug":"mobile"}`), CreatedAt: at(-120), ActorName: "ci"},
		{ID: fid(362), AccountID: fid(1), Actor: "github:octo", Action: "gate.resolved", SubjectType: "gate", SubjectID: fid(30), ProjectID: fid(10), CreatedAt: at(-1500)},
	}
	tokensWithScopes := func(e *env) {
		e.svc.ListTokensFunc = func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error) {
			return []mcp.TokenInfo{
				{ID: fid(90), Name: "laptop", CreatedAt: at(-2000), LastUsedAt: at(-1), Current: true, Scopes: []domain.Scope{domain.ScopeWrite}, ExpiresAt: at(60 * 24 * 60)},
				{ID: fid(91), Name: "old-laptop", CreatedAt: at(-3000), LastUsedAt: at(-1500), Scopes: []domain.Scope{domain.ScopeRead}, ExpiresAt: at(-100)},
				{ID: fid(92), Name: "ci-box", CreatedAt: at(-2500), RevokedAt: at(-700), OwnerKind: domain.SubjectServiceAccount, OwnerID: fid(340), Scopes: []domain.Scope{domain.ScopeAdmin}},
			}, nil
		}
	}
	return []fixtureCase{
		{"me-viewer", "GET", "/api/v1/me", "", 200, "bearer", ownedBy("vera", domain.RoleScopeOrganization, domain.RoleViewer, false)},
		{"me-contributor", "GET", "/api/v1/me", "", 200, "bearer", ownedBy("carl", domain.RoleScopeProject, domain.RoleContributor, true)},
		{"me-admin", "GET", "/api/v1/me", "", 200, "bearer", ownedBy("ana", domain.RoleScopeOrganization, domain.RoleAdmin, false)},
		{"tokens-scopes", "GET", "/api/v1/tokens", "", 200, "bearer", tokensWithScopes},
		{"token-create", "POST", "/api/v1/tokens", `{"name":"ci-laptop","scopes":["write"],"ttl":"720h"}`, 201, "bearer", func(e *env) {
			e.svc.CreateTokenFunc = func(_ context.Context, _ domain.Actor, in mcp.CreateTokenInput) (mcp.CreateTokenOutput, error) {
				return mcp.CreateTokenOutput{Token: mcp.TokenInfo{ID: fid(93), Name: in.Name, CreatedAt: at(0), Scopes: in.Scopes, ExpiresAt: at(30 * 24 * 60)}, Secret: "fcm_Zm9yZWNyZXctZXhhbXBsZS1zZWNyZXQ"}, nil
			}
		}},
		{"members", "GET", "/api/v1/members", "", 200, "bearer", func(e *env) {
			e.svc.ListMembersFunc = func(context.Context, domain.Actor, any) ([]mcp.MemberInfo, error) { return members, nil }
		}},
		{"member-suspend", "POST", "/api/v1/members/" + fid(301).String() + "/suspend", "", 200, "bearer", func(e *env) {
			e.svc.SetMemberSuspendedFunc = func(_ context.Context, _ domain.Actor, _ mcp.SetMemberInput) (mcp.MemberInfo, error) {
				m := members[1]
				m.Status = domain.MembershipSuspended
				return m, nil
			}
		}},
		{"member-last-owner", "DELETE", "/api/v1/members/" + fid(300).String(), "", 409, "bearer", func(e *env) {
			e.svc.RemoveMemberFunc = func(context.Context, domain.Actor, mcp.MemberRefInput) (mcp.MemberInfo, error) {
				return mcp.MemberInfo{}, fmt.Errorf("an organization must keep an active owner: %w", mcp.ErrConflict)
			}
		}},
		{"organization", "PATCH", "/api/v1/organization", `{"name":"Acme Inc"}`, 200, "bearer", func(e *env) {
			e.svc.UpdateOrganizationFunc = func(_ context.Context, _ domain.Actor, in mcp.UpdateOrganizationInput) (domain.Account, error) {
				return domain.Account{ID: fid(1), Name: in.Name, Slug: "acme", CreatedAt: at(-9000)}, nil
			}
		}},
		{"teams", "GET", "/api/v1/teams", "", 200, "bearer", func(e *env) {
			e.svc.ListTeamsFunc = func(context.Context, domain.Actor, any) ([]domain.Team, error) {
				return []domain.Team{team, idpTeam}, nil
			}
		}},
		{"teams-empty", "GET", "/api/v1/teams", "", 200, "bearer", func(e *env) {
			e.svc.ListTeamsFunc = func(context.Context, domain.Actor, any) ([]domain.Team, error) { return []domain.Team{}, nil }
		}},
		{"team-create", "POST", "/api/v1/teams", `{"name":"Mobile"}`, 201, "bearer", func(e *env) {
			e.svc.CreateTeamFunc = func(context.Context, domain.Actor, mcp.CreateTeamInput) (domain.Team, error) { return team, nil }
		}},
		{"team-members", "GET", "/api/v1/teams/mobile/members", "", 200, "bearer", func(e *env) {
			e.svc.ListTeamMembersFunc = func(context.Context, domain.Actor, mcp.TeamRefInput) ([]domain.TeamMember, error) {
				return []domain.TeamMember{{TeamID: team.ID, UserID: fid(301), AccountID: fid(1), Source: domain.SourceManual, CreatedAt: at(-2000)}}, nil
			}
		}},
		{"roles", "GET", "/api/v1/roles", "", 200, "bearer", func(e *env) {
			e.svc.ListRolesFunc = func(context.Context, domain.Actor, any) ([]domain.Role, error) { return domain.BuiltInRoles(), nil }
		}},
		{"role-bindings", "GET", "/api/v1/role-bindings", "", 200, "bearer", func(e *env) {
			e.svc.ListRoleBindingsFunc = func(context.Context, domain.Actor, mcp.ListRoleBindingsInput) ([]mcp.RoleBindingInfo, error) {
				return []mcp.RoleBindingInfo{
					{ID: fid(370), AccountID: fid(1), SubjectKind: domain.SubjectUser, SubjectID: fid(300), CreatedBy: "user:x", CreatedAt: at(-5000), Role: "owner", RoleScope: domain.RoleScopeOrganization, SubjectName: "Gianmaria"},
					{ID: fid(371), AccountID: fid(1), SubjectKind: domain.SubjectUser, SubjectID: fid(301), ProjectID: fid(10), CreatedBy: "user:x", CreatedAt: at(-4000), Role: "maintainer", RoleScope: domain.RoleScopeProject, Project: "app", SubjectName: "Marta Rossi"},
					{ID: fid(372), AccountID: fid(1), SubjectKind: domain.SubjectTeam, SubjectID: fid(330), ProjectID: fid(10), CreatedBy: "user:x", CreatedAt: at(-3000), Role: "contributor", RoleScope: domain.RoleScopeProject, Project: "app", SubjectName: "Mobile"},
					{ID: fid(373), AccountID: fid(1), SubjectKind: domain.SubjectTeam, SubjectID: fid(330), CreatedBy: "user:x", CreatedAt: at(-3000), Role: "member", RoleScope: domain.RoleScopeOrganization, SubjectName: "Mobile"},
				}, nil
			}
		}},
		{"role-binding-create", "POST", "/api/v1/role-bindings", `{"subject_kind":"user","subject":"` + fid(301).String() + `","role":"viewer","project":"app"}`, 201, "bearer", func(e *env) {
			e.svc.CreateRoleBindingFunc = func(context.Context, domain.Actor, mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error) {
				return mcp.RoleBindingInfo{ID: fid(374), AccountID: fid(1), SubjectKind: domain.SubjectUser, SubjectID: fid(301), ProjectID: fid(10), CreatedAt: at(0), Role: "viewer", RoleScope: domain.RoleScopeProject, Project: "app"}, nil
			}
		}},
		{"role-binding-delete", "DELETE", "/api/v1/role-bindings/" + fid(371).String(), "", 200, "bearer", func(e *env) {
			e.svc.DeleteRoleBindingFunc = func(context.Context, domain.Actor, mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error) {
				return mcp.RoleBindingInfo{ID: fid(371), AccountID: fid(1), Role: "maintainer"}, nil
			}
		}},
		{"service-accounts", "GET", "/api/v1/service-accounts", "", 200, "bearer", func(e *env) {
			e.svc.ListServiceAccountsFunc = func(context.Context, domain.Actor, any) ([]domain.ServiceAccount, error) {
				return []domain.ServiceAccount{account, disabled}, nil
			}
		}},
		{"service-account-create", "POST", "/api/v1/service-accounts", `{"name":"ci"}`, 201, "bearer", func(e *env) {
			e.svc.CreateServiceAccountFunc = func(context.Context, domain.Actor, mcp.CreateServiceAccountInput) (domain.ServiceAccount, error) {
				return account, nil
			}
		}},
		{"invitations", "GET", "/api/v1/invitations", "", 200, "bearer", func(e *env) {
			e.svc.ListInvitationsFunc = func(context.Context, domain.Actor, any) ([]domain.Invitation, error) { return invitations, nil }
		}},
		{"invitation-create", "POST", "/api/v1/invitations", `{"email":"new@acme.test","roles":[{"role":"member"}]}`, 201, "bearer", func(e *env) {
			e.svc.CreateInvitationFunc = func(context.Context, domain.Actor, mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error) {
				return mcp.CreateInvitationOutput{Invitation: invitations[0], URL: "https://forecrew.example.com/login?invite=fci_ZXhhbXBsZS1pbnZpdGF0aW9u"}, nil
			}
		}},
		{"invitation-revoke", "POST", "/api/v1/invitations/" + fid(350).String() + "/revoke", "", 200, "bearer", func(e *env) {
			e.svc.RevokeInvitationFunc = func(context.Context, domain.Actor, mcp.InvitationRefInput) (domain.Invitation, error) {
				inv := invitations[0]
				inv.RevokedAt = at(0)
				return inv, nil
			}
		}},
		{"connections", "GET", "/api/v1/connections", "", 200, "bearer", func(e *env) {
			e.svc.ListConnectionsFunc = func(context.Context, domain.Actor, any) ([]mcp.ConnectionInfo, error) { return connections, nil }
		}},
		{"connection-create", "POST", "/api/v1/connections", `{"kind":"google","name":"google","client_id":"123.apps.googleusercontent.com","client_secret_ref":"secret:conn-google"}`, 201, "bearer", func(e *env) {
			e.svc.CreateConnectionFunc = func(context.Context, domain.Actor, mcp.CreateConnectionInput) (mcp.ConnectionInfo, error) {
				return connections[2], nil
			}
		}},
		{"connection-enable", "POST", "/api/v1/connections/okta/enable", "", 200, "bearer", func(e *env) {
			e.svc.SetConnectionFunc = func(context.Context, domain.Actor, mcp.SetConnectionInput) (mcp.ConnectionInfo, error) {
				return connections[1], nil
			}
		}},
		{"connection-enable-untested", "POST", "/api/v1/connections/google/enable", "", 409, "bearer", func(e *env) {
			e.svc.SetConnectionFunc = func(context.Context, domain.Actor, mcp.SetConnectionInput) (mcp.ConnectionInfo, error) {
				return mcp.ConnectionInfo{}, fmt.Errorf("connection google has no test sign-in that worked yet: open its test_url and sign in once: %w", mcp.ErrConflict)
			}
		}},
		{"audit", "GET", "/api/v1/audit", "", 200, "bearer", func(e *env) {
			e.svc.ListAuditFunc = func(context.Context, domain.Actor, mcp.ListAuditInput) ([]mcp.AuditInfo, error) { return audit, nil }
		}},
		{"audit-empty", "GET", "/api/v1/audit", "", 200, "bearer", func(e *env) {
			e.svc.ListAuditFunc = func(context.Context, domain.Actor, mcp.ListAuditInput) ([]mcp.AuditInfo, error) {
				return []mcp.AuditInfo{}, nil
			}
		}},
		{"sessions", "GET", "/api/v1/sessions", "", 200, "bearer", withSessions},
		{"identities", "GET", "/api/v1/identities", "", 200, "bearer", withSessions},
	}
}

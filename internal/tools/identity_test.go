package tools_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/forecrew/forecrew/internal/tools"
)

// person adds a user with an organization role and returns their actor.
func (f *fixture) person(t *testing.T, email, role string) domain.Actor {
	t.Helper()
	ctx := t.Context()
	u := domain.User{Name: email, Email: email, EmailVerified: true}
	must(t, f.st.Users().Create(ctx, &u))
	must(t, f.st.Memberships().Create(ctx, &domain.Membership{AccountID: f.account.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	r, err := f.st.Roles().GetByName(ctx, f.account.ID, domain.RoleScopeOrganization, role)
	must(t, err)
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: r.ID, CreatedBy: "user:x"}))
	return domain.UserActor(f.account.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
}

func scopes(s ...domain.Scope) []domain.Scope { return s }

func TestCreatePersonalToken(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "ana@example.com", domain.RoleAdmin)
	out, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "laptop", Scopes: scopes(domain.ScopeWrite)})
	must(t, err)
	if !strings.HasPrefix(out.Secret, "fcm_") || out.Token.Name != "laptop" || out.Token.ExpiresAt.IsZero() {
		t.Fatalf("out = %+v", out)
	}
	owner, _ := by.SubjectID()
	stored, err := f.st.MCPTokens().GetByTokenHash(ctx, mcp.HashToken(out.Secret))
	must(t, err)
	if stored.OwnerKind != domain.SubjectUser || stored.OwnerID != owner || !slices.Equal(stored.Scopes, scopes(domain.ScopeWrite)) {
		t.Errorf("stored = %+v", stored)
	}
	// Shown once: nothing the API returns later holds it.
	list, err := f.svc.ListTokens(ctx, by)
	must(t, err)
	for _, info := range list {
		if strings.Contains(info.Name, out.Secret) {
			t.Error("secret listed")
		}
	}
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{SubjectID: out.Token.ID})
	must(t, err)
	if len(entries) != 1 || entries[0].Action != "token.created" || entries[0].Actor != by.String() || entries[0].Credential != by.Credential.String() || strings.Contains(string(entries[0].Data), out.Secret) {
		t.Errorf("audit = %+v", entries)
	}
	_, err = f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "laptop", Scopes: scopes(domain.ScopeRead)})
	wantErr(t, err, mcp.ErrConflict)
	if code, ok := mcp.ErrorCode(err); !ok || code != "conflict" {
		t.Errorf("code = %s", code)
	}
}

func TestTokenTTL(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "ana@example.com", domain.RoleOwner)
	short, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "short", Scopes: scopes(domain.ScopeRead), TTL: "1h"})
	must(t, err)
	long, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "default", Scopes: scopes(domain.ScopeRead)})
	must(t, err)
	if got := long.Token.ExpiresAt.Sub(short.Token.ExpiresAt); got < 89*24*time.Hour {
		t.Errorf("default expiry is %s after 1h", got)
	}
	at, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "limit", Scopes: scopes(domain.ScopeRead), TTL: tools.MaxTokenTTL.String()})
	must(t, err)
	if at.Token.ExpiresAt.IsZero() {
		t.Errorf("token = %+v", at.Token)
	}
	for _, ttl := range []string{"soon", "-1h", "0s", (tools.MaxTokenTTL + time.Hour).String()} {
		_, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "x", Scopes: scopes(domain.ScopeRead), TTL: ttl})
		wantErr(t, err, mcp.ErrInvalidInput)
	}
}

func TestTokenInputRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "ana@example.com", domain.RoleOwner)
	for name, in := range map[string]mcp.CreateTokenInput{
		"no name":   {Scopes: scopes(domain.ScopeRead)},
		"bad name":  {Name: "A B", Scopes: scopes(domain.ScopeRead)},
		"no scopes": {Name: "x"},
		"bad scope": {Name: "x", Scopes: scopes("root")},
	} {
		if _, err := f.svc.CreateToken(ctx, by, in); !isCode(err, "invalid_input") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A personal token is for a person.
	_, err := f.svc.CreateToken(ctx, f.by, mcp.CreateTokenInput{Name: "x", Scopes: scopes(domain.ScopeRead)})
	wantErr(t, err, mcp.ErrInvalidInput)
}

func isCode(err error, want string) bool {
	code, ok := mcp.ErrorCode(err)
	return ok && code == want
}

func TestScopeAboveRolesRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	tests := []struct {
		role  string
		scope domain.Scope
		ok    bool
	}{
		{domain.RoleViewer, domain.ScopeRead, true},
		{domain.RoleViewer, domain.ScopeWrite, false},
		{domain.RoleMember, domain.ScopeWrite, true},
		{domain.RoleMember, domain.ScopeAdmin, false},
		{domain.RoleOwner, domain.ScopeAdmin, true},
	}
	for _, tt := range tests {
		by := f.person(t, tt.role+string(tt.scope)+"@example.com", tt.role)
		_, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "t", Scopes: scopes(tt.scope)})
		if tt.ok {
			must(t, err)
		} else {
			wantErr(t, err, authz.ErrDenied)
		}
	}
}

func TestTokenCannotCreateAboveItself(t *testing.T) {
	f := newFixture(t)
	by := f.person(t, "ana@example.com", domain.RoleOwner)
	ctx := mcp.WithCaller(t.Context(), mcp.Caller{Actor: by, Scopes: scopes(domain.ScopeWrite)})
	_, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "up", Scopes: scopes(domain.ScopeAdmin)})
	wantErr(t, err, authz.ErrDenied)
	if _, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "same", Scopes: scopes(domain.ScopeRead)}); err != nil {
		t.Fatal(err)
	}
}

// A read token cannot make another: it would outlive its own revocation.
func TestReadTokenCreatesNoToken(t *testing.T) {
	f := newFixture(t)
	by := f.person(t, "ana@example.com", domain.RoleOwner)
	id, _ := by.SubjectID()
	read := domain.MCPToken{AccountID: f.account.ID, Name: "read", TokenHash: mcp.HashToken("fcm_read"), OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: scopes(domain.ScopeRead)}
	must(t, f.st.MCPTokens().Create(t.Context(), &read))
	_, err := f.svc.CreateToken(t.Context(), mcp.TokenActor(read), mcp.CreateTokenInput{Name: "copy", Scopes: scopes(domain.ScopeRead)})
	wantErr(t, err, authz.ErrDenied)
}

// Granting a project role needs project.manage on that project, and every
// permission of the role there.
func TestInvitationProjectRoles(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	second := f.secondProject(t)
	custom := domain.Role{AccountID: f.account.ID, Name: "inviter", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermMemberManage}}
	must(t, f.st.Roles().Create(ctx, &custom))
	lead, id := f.member(t, domain.RoleScopeProject, domain.RoleMaintainer, second.ID)
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, id, domain.RoleScopeOrganization, "inviter", uuid.Nil())
	grant := func(project string) error {
		_, err := f.svc.CreateInvitation(ctx, lead, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: domain.RoleMaintainer, Project: project}}})
		return err
	}
	// It holds every permission of the role there, but not project.manage.
	wantErr(t, grant(second.Name), authz.ErrDenied)
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleProjectAdmin, second.ID)
	must(t, grant(second.Name))
	wantErr(t, grant(f.project.Name), authz.ErrDenied)
}

func TestServiceAccountToken(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.person(t, "admin@example.com", domain.RoleAdmin)
	member := f.person(t, "member@example.com", domain.RoleMember)
	sa := domain.ServiceAccount{AccountID: f.account.ID, Name: "ci", CreatedBy: "user:x"}
	must(t, f.st.ServiceAccounts().Create(ctx, &sa))
	role, err := f.st.Roles().GetByName(ctx, f.account.ID, domain.RoleScopeOrganization, domain.RoleMember)
	must(t, err)
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectServiceAccount, SubjectID: sa.ID, RoleID: role.ID, CreatedBy: "user:x"}))

	in := mcp.CreateTokenInput{Name: "deploy", Scopes: scopes(domain.ScopeWrite), ServiceAccount: "ci"}
	_, err = f.svc.CreateToken(ctx, member, in)
	wantErr(t, err, authz.ErrDenied)
	out, err := f.svc.CreateToken(ctx, admin, in)
	must(t, err)
	if out.Token.OwnerKind != domain.SubjectServiceAccount || out.Token.OwnerID != sa.ID {
		t.Errorf("token = %+v", out.Token)
	}
	in.Name, in.Scopes = "top", scopes(domain.ScopeAdmin)
	_, err = f.svc.CreateToken(ctx, admin, in)
	wantErr(t, err, authz.ErrDenied) // above the service account's roles
	in.ServiceAccount = "ghost"
	_, err = f.svc.CreateToken(ctx, admin, in)
	wantErr(t, err, mcp.ErrNotFound)

	// No escalation: a role the caller lacks.
	billing, err := f.st.Roles().GetByName(ctx, f.account.ID, domain.RoleScopeOrganization, domain.RoleBilling)
	must(t, err)
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectServiceAccount, SubjectID: sa.ID, RoleID: billing.ID, CreatedBy: "user:x"}))
	_, err = f.svc.CreateToken(ctx, admin, mcp.CreateTokenInput{Name: "esc", Scopes: scopes(domain.ScopeRead), ServiceAccount: "ci"})
	wantErr(t, err, authz.ErrDenied)
}

// A token works until it expires, as its owner, on the real middleware.
func TestTokenExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "ana@example.com", domain.RoleOwner)
	now := time.Now()
	out, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "short", Scopes: scopes(domain.ScopeRead), TTL: "1h"})
	must(t, err)
	var clock time.Time
	var got domain.Actor
	h := mcp.Authenticate(f.st, func() time.Time { return clock }, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = mcp.ActorFromContext(r.Context())
	}))
	call := func() int {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+out.Secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	clock = now.Add(30 * time.Minute)
	if code := call(); code != http.StatusOK || got.Kind != domain.ActorUser || got.Name != by.Name || got.Credential.Kind != domain.CredentialToken {
		t.Fatalf("before expiry = %d, %+v", code, got)
	}
	clock = now.Add(2 * time.Hour)
	if code := call(); code != http.StatusUnauthorized {
		t.Errorf("after expiry = %d", code)
	}
}

func TestInvitations(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "owner@example.com", domain.RoleOwner)
	in := mcp.CreateInvitationInput{Email: "bea@example.com", Roles: []mcp.RoleGrant{{Role: domain.RoleMember}, {Role: domain.RoleMaintainer, Project: f.project.Name}, {Role: domain.RoleContributor}}, TTL: "1h"}
	out, err := f.svc.CreateInvitation(ctx, by, in)
	must(t, err)
	if !strings.Contains(out.URL, "/login?invite=fci_") || len(out.Invitation.Grants) != 3 || out.Invitation.Grants[1].ProjectID != f.project.ID ||
		out.Invitation.Grants[2].ProjectID != uuid.Nil() || out.Invitation.InvitedBy != by.String() {
		t.Fatalf("out = %+v", out)
	}
	list, err := f.svc.ListInvitations(ctx, by)
	must(t, err)
	if len(list) != 1 || list[0].ID != out.Invitation.ID {
		t.Fatalf("list = %+v", list)
	}
	got, err := f.svc.RevokeInvitation(ctx, by, mcp.InvitationRefInput{Invitation: out.Invitation.ID.String()})
	must(t, err)
	if got.RevokedAt.IsZero() {
		t.Errorf("revoked = %+v", got)
	}
	_, err = f.svc.RevokeInvitation(ctx, by, mcp.InvitationRefInput{Invitation: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.RevokeInvitation(ctx, by, mcp.InvitationRefInput{Invitation: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)

	gh, err := f.svc.CreateInvitation(ctx, by, mcp.CreateInvitationInput{GitHubLogin: "bea", Roles: []mcp.RoleGrant{{Role: domain.RoleViewer}}})
	must(t, err)
	if gh.Invitation.GitHubUserID != 583231 {
		t.Errorf("invitation = %+v", gh.Invitation)
	}
	if d := time.Until(gh.Invitation.ExpiresAt); d < 6*24*time.Hour {
		t.Errorf("default expiry in %s", d)
	}
}

func TestInvitationRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	owner := f.person(t, "owner@example.com", domain.RoleOwner)
	admin := f.person(t, "admin@example.com", domain.RoleAdmin)
	member := f.person(t, "member@example.com", domain.RoleMember)
	roles := []mcp.RoleGrant{{Role: domain.RoleMember}}

	for name, call := range map[string]func() error{
		"create": func() error {
			_, err := f.svc.CreateInvitation(ctx, member, mcp.CreateInvitationInput{Email: "a@b.c", Roles: roles})
			return err
		},
		"list": func() error { _, err := f.svc.ListInvitations(ctx, member); return err },
		"revoke": func() error {
			_, err := f.svc.RevokeInvitation(ctx, member, mcp.InvitationRefInput{Invitation: domain.NewID().String()})
			return err
		},
	} {
		if err := call(); !isCode(err, "forbidden") {
			t.Errorf("%s by a member: %v", name, err)
		}
	}
	// No escalation: an Admin does not give the Owner role.
	_, err := f.svc.CreateInvitation(ctx, admin, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: domain.RoleOwner}}})
	wantErr(t, err, authz.ErrDenied)
	// A read token of an Owner cannot: its scopes meet the Owner's bindings.
	ownerID, _ := owner.SubjectID()
	read := domain.MCPToken{AccountID: f.account.ID, Name: "read", TokenHash: mcp.HashToken("fcm_read"), OwnerKind: domain.SubjectUser, OwnerID: ownerID, Scopes: scopes(domain.ScopeRead)}
	must(t, f.st.MCPTokens().Create(ctx, &read))
	_, err = f.svc.CreateInvitation(ctx, mcp.TokenActor(read), mcp.CreateInvitationInput{Email: "a@b.c", Roles: roles})
	wantErr(t, err, authz.ErrDenied)
	// Nor can an ownerless read token, bounded by its scopes alone.
	legacy := domain.MCPToken{AccountID: f.account.ID, Name: "legacy", TokenHash: mcp.HashToken("fcm_legacy"), Scopes: scopes(domain.ScopeRead)}
	must(t, f.st.MCPTokens().Create(ctx, &legacy))
	_, err = f.svc.CreateInvitation(ctx, mcp.TokenActor(legacy), mcp.CreateInvitationInput{Email: "a@b.c", Roles: roles})
	wantErr(t, err, authz.ErrDenied)
	// An instance admin recovering access can.
	glass := mcp.WithCaller(ctx, mcp.Caller{Actor: member, BreakGlass: true})
	if _, err := f.svc.CreateInvitation(glass, member, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: domain.RoleOwner}}}); err != nil {
		t.Errorf("break glass: %v", err)
	}

	for name, in := range map[string]mcp.CreateInvitationInput{
		"neither":      {Roles: roles},
		"both":         {Email: "a@b.c", GitHubLogin: "a", Roles: roles},
		"not an email": {Email: "nope", Roles: roles},
		"bad ttl":      {Email: "a@b.c", Roles: roles, TTL: "soon"},
		"long ttl":     {Email: "a@b.c", Roles: roles, TTL: (tools.MaxInvitationTTL + time.Hour).String()},
	} {
		if _, err := f.svc.CreateInvitation(ctx, owner, in); !isCode(err, "invalid_input") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = f.svc.CreateInvitation(ctx, owner, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: domain.RoleViewer, Project: "ghost"}}})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateInvitation(ctx, owner, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: "ghost"}}})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateInvitation(ctx, owner, mcp.CreateInvitationInput{GitHubLogin: "ghost", Roles: roles})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.CreateInvitation(ctx, owner, mcp.CreateInvitationInput{GitHubLogin: "down", Roles: roles})
	wantErr(t, err, mcp.ErrUnavailable)
	noLookup := tools.New(tools.Config{Store: f.st})
	_, err = noLookup.CreateInvitation(ctx, owner, mcp.CreateInvitationInput{GitHubLogin: "bea", Roles: roles})
	wantErr(t, err, mcp.ErrUnavailable)
}

func TestTokenCapUnderEnforcement(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by := f.person(t, "ana@acme.example", domain.RoleAdmin)
	d := domain.VerifiedDomain{AccountID: f.account.ID, Domain: "acme.example", VerificationToken: "t"}
	must(t, f.st.VerifiedDomains().Create(ctx, &d))
	must(t, f.st.VerifiedDomains().SetVerified(ctx, f.account.ID, d.ID, true))
	conn := domain.Connection{AccountID: f.account.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://idp.example", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_X", Enabled: true}
	must(t, f.st.Connections().Create(ctx, &conn))
	must(t, f.st.VerifiedDomains().SetEnforcement(ctx, f.account.ID, d.ID, true, conn.ID))
	must(t, f.st.Accounts().SetMaxTokenTTL(ctx, f.account.ID, domain.DefaultSSOTokenTTL))

	out, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "default", Scopes: scopes(domain.ScopeRead)})
	must(t, err)
	if left := time.Until(out.Token.ExpiresAt); left > domain.DefaultSSOTokenTTL {
		t.Errorf("a default token lasts %s", left)
	}
	if _, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "long", Scopes: scopes(domain.ScopeRead), TTL: "720h"}); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("above the cap: %v", err)
	}
	if _, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "short", Scopes: scopes(domain.ScopeRead), TTL: "24h"}); err != nil {
		t.Errorf("within the cap: %v", err)
	}
	sa := domain.ServiceAccount{AccountID: f.account.ID, Name: "ci", CreatedBy: "user:x"}
	must(t, f.st.ServiceAccounts().Create(ctx, &sa))
	viewer, err := f.st.Roles().GetByName(ctx, f.account.ID, domain.RoleScopeOrganization, domain.RoleViewer)
	must(t, err)
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectServiceAccount, SubjectID: sa.ID, RoleID: viewer.ID, CreatedBy: "user:x"}))
	if _, err := f.svc.CreateToken(ctx, by, mcp.CreateTokenInput{Name: "bot", ServiceAccount: "ci", Scopes: scopes(domain.ScopeRead), TTL: "720h"}); err != nil {
		t.Errorf("a service account is not capped: %v", err)
	}
}

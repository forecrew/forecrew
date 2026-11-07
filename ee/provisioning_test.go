//go:build enterprise

package ee

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const clientSecret = "s3cr3t"

// signInEnv is sign-in behind TLS with the enterprise policy and a fake OIDC
// provider, through an organization's connection.
type signInEnv struct {
	*env
	oidc *testkit.FakeOIDC
	web  *httptest.Server
	conn domain.Connection
}

func newSignIn(t *testing.T) *signInEnv {
	t.Helper()
	e := newEnv(t)
	s := &signInEnv{env: e, oidc: testkit.NewFakeOIDC(t, "client")}
	s.oidc.Now = e.clock.Now
	mux := http.NewServeMux()
	s.web = httptest.NewUnstartedServer(mux)
	s.web.StartTLS()
	t.Cleanup(s.web.Close)
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the fakes' test certificates
	svc := auth.New(auth.Config{Store: e.st, Auth: config.Auth{SessionIdle: time.Hour, SessionAbsolute: 24 * time.Hour}, BaseURL: s.web.URL, Now: e.clock.Now, HTTP: insecure, OrgHTTP: insecure,
		Policy: e.svc, Logger: slog.New(slog.DiscardHandler), Getenv: func(string) string { return clientSecret }})
	mux.Handle("/auth/", svc.Handler())
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) {})
	s.conn = domain.Connection{AccountID: e.org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: s.oidc.URL, ClientID: "client", ClientSecretRef: "env:FORECREW_CONN_X", GroupClaim: "groups", Enabled: true}
	must(t, e.st.Connections().Create(t.Context(), &s.conn))
	return s
}

// signIn follows the flow in a new browser and returns the status.
func (s *signInEnv) signIn(sub, email string, groups ...string) int {
	s.t.Helper()
	jar, err := cookiejar.New(nil)
	must(s.t, err)
	browser := &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the fakes' test certificates
	s.oidc.Claims = map[string]any{"sub": sub, "email": email, "email_verified": true, "name": "Ana", "groups": groups}
	resp, err := browser.Get(s.web.URL + "/auth/" + s.conn.ID.String() + "/login")
	must(s.t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (s *signInEnv) setJIT(on bool, roles ...string) {
	s.t.Helper()
	_, err := s.svc.SetConnectionProvisioning(s.t.Context(), s.owner, mcp.SetProvisioningInput{Connection: s.conn.Name, JITEnabled: &on, DefaultRoles: &roles})
	must(s.t, err)
}

func (s *signInEnv) userOf(sub string) uuid.UUID {
	s.t.Helper()
	id, err := s.st.Identities().GetBySubject(s.t.Context(), s.conn.ID, sub)
	must(s.t, err)
	return id.UserID
}

func (s *signInEnv) teamsOf(user uuid.UUID) []string {
	s.t.Helper()
	teams, err := s.st.Teams().TeamsOf(s.t.Context(), s.org.ID, user)
	must(s.t, err)
	out := []string{}
	for _, x := range teams {
		out = append(out, x.Slug)
	}
	slices.Sort(out)
	return out
}

func TestJITCreatesTheMembership(t *testing.T) {
	ctx := t.Context()
	s := newSignIn(t)
	s.verified("acme.example")
	if status := s.signIn("sub-1", "new@acme.example"); status != http.StatusForbidden {
		t.Fatalf("without JIT = %d", status)
	}
	s.setJIT(true, domain.RoleMember)
	if status := s.signIn("sub-1", "new@acme.example"); status != http.StatusOK {
		t.Fatalf("with JIT = %d", status)
	}
	user := s.userOf("sub-1")
	m, err := s.st.Memberships().Get(ctx, s.org.ID, user)
	must(t, err)
	if m.Source != domain.SourceJIT || m.Status != domain.MembershipActive {
		t.Fatalf("membership = %+v", m)
	}
	bindings, err := s.st.RoleBindings().List(ctx, s.org.ID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser, SubjectID: user})
	must(t, err)
	role, err := s.st.Roles().GetByName(ctx, s.org.ID, domain.RoleScopeOrganization, domain.RoleMember)
	must(t, err)
	if len(bindings) != 1 || bindings[0].RoleID != role.ID || bindings[0].CreatedBy != "system:jit" {
		t.Fatalf("bindings = %+v", bindings)
	}
	// Only the first sign-in: a role taken away later does not come back.
	must(t, s.st.RoleBindings().Delete(ctx, s.org.ID, bindings[0].ID))
	if status := s.signIn("sub-1", "new@acme.example"); status != http.StatusOK {
		t.Fatalf("again = %d", status)
	}
	if bindings, _ = s.st.RoleBindings().List(ctx, s.org.ID, store.RoleBindingFilter{SubjectKind: domain.SubjectUser, SubjectID: user}); len(bindings) != 0 {
		t.Fatalf("bindings = %+v", bindings)
	}
	// A suspended member stays out.
	must(t, s.st.Memberships().SetStatus(ctx, s.org.ID, user, domain.MembershipSuspended))
	if status := s.signIn("sub-1", "new@acme.example"); status != http.StatusForbidden {
		t.Fatalf("suspended = %d", status)
	}
}

func TestGroupMappingFollowsTheProvider(t *testing.T) {
	ctx := t.Context()
	s := newSignIn(t)
	s.verified("acme.example")
	s.setJIT(true)
	devs, ops, manual := s.team("Devs", domain.SourceIDP), s.team("Ops", domain.SourceIDP), s.team("Hand", domain.SourceManual)
	for group, x := range map[string]domain.Team{"devs": devs, "ops": ops, "hand": manual} {
		_, err := s.svc.AddGroupMapping(ctx, s.owner, mcp.AddGroupMappingInput{Connection: s.conn.Name, Group: group, Team: x.Slug})
		must(t, err)
	}
	if status := s.signIn("sub-1", "ana@acme.example", "devs", "ops", "unmapped"); status != http.StatusOK {
		t.Fatalf("sign-in = %d", status)
	}
	user := s.userOf("sub-1")
	if got := s.teamsOf(user); !slices.Equal(got, []string{"devs", "ops"}) {
		t.Fatalf("teams = %v", got)
	}
	members, err := s.st.Teams().Members(ctx, s.org.ID, devs.ID)
	must(t, err)
	if len(members) != 1 || members[0].Source != domain.SourceIDP {
		t.Fatalf("members = %+v", members)
	}
	// A manual member of a mapped team keeps their place; a group removed at
	// the provider takes the member out at the next sign-in.
	must(t, s.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: manual.ID, UserID: user, AccountID: s.org.ID, Source: domain.SourceManual}))
	if status := s.signIn("sub-1", "ana@acme.example", "devs"); status != http.StatusOK {
		t.Fatalf("sign-in = %d", status)
	}
	if got := s.teamsOf(user); !slices.Equal(got, []string{"devs", "hand"}) {
		t.Fatalf("teams = %v", got)
	}
	if !s.audited("team.member_removed") {
		t.Error("not audited")
	}
	// No claim at all: the provider names no group.
	s.oidc.Claims = nil
	if status := s.signIn("sub-1", "ana@acme.example"); status != http.StatusOK {
		t.Fatalf("sign-in = %d", status)
	}
	if got := s.teamsOf(user); !slices.Equal(got, []string{"hand"}) {
		t.Fatalf("teams = %v", got)
	}
}

// A user SCIM created signs in through the organization's connection, and an
// instance connection does not touch teams of an organization that enforces
// another one.
func TestSCIMUserSignsIn(t *testing.T) {
	ctx := t.Context()
	s := newSignIn(t)
	s.verified("acme.example")
	status, body := s.scim().do(http.MethodPost, "/Users", userBody("ann@acme.example"))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v", status, body)
	}
	if status := s.signIn("sub-1", "ann@acme.example"); status != http.StatusOK {
		t.Fatalf("sign-in = %d", status)
	}
	user := s.userOf("sub-1")
	if user.String() != body["id"] {
		t.Fatalf("user = %s, want %v", user, body["id"])
	}

	if _, err := s.svc.SetSSOEnforcement(ctx, s.owner, mcp.SetSSOEnforcementInput{Domain: "acme.example", Enforce: true, Connection: s.conn.Name}); err != nil {
		t.Fatal(err)
	}
	inst := domain.Connection{Kind: domain.ConnectionGitHub, Name: "hub", ClientID: "c", ClientSecretRef: "env:X", GroupClaim: "groups", Enabled: true}
	must(t, s.st.Connections().UpsertInstance(ctx, &inst))
	x := s.team("Devs", domain.SourceIDP)
	if _, err := s.svc.AddGroupMapping(ctx, s.owner, mcp.AddGroupMappingInput{Connection: "hub", Group: "devs", Team: x.Slug}); err != nil {
		t.Fatal(err)
	}
	u, err := s.st.Users().Get(ctx, user)
	must(t, err)
	must(t, s.svc.Admit(ctx, s.st, inst, auth.Claims{Groups: []string{"devs"}}, u))
	if got := s.teamsOf(user); len(got) != 0 {
		t.Fatalf("teams = %v", got)
	}
}

func TestSyncSkipsWhoIsNotActiveAndUnknownGroups(t *testing.T) {
	ctx := t.Context()
	s := newSignIn(t)
	x := s.team("Devs", domain.SourceIDP)
	_, err := s.svc.AddGroupMapping(ctx, s.owner, mcp.AddGroupMappingInput{Connection: s.conn.Name, Group: "devs", Team: x.Slug})
	must(t, err)
	u := domain.User{Name: "Ann"}
	must(t, s.st.Users().Create(ctx, &u))
	// Not a member: nothing to do.
	must(t, s.svc.Admit(ctx, s.st, s.conn, auth.Claims{Groups: []string{"devs"}}, u))
	must(t, s.st.Memberships().Create(ctx, &domain.Membership{AccountID: s.org.ID, UserID: u.ID, Status: domain.MembershipSuspended, Source: domain.SourceManual}))
	must(t, s.svc.Admit(ctx, s.st, s.conn, auth.Claims{Groups: []string{"devs"}}, u))
	if got := s.teamsOf(u.ID); len(got) != 0 {
		t.Fatalf("teams = %v", got)
	}
	must(t, s.st.Memberships().SetStatus(ctx, s.org.ID, u.ID, domain.MembershipActive))
	must(t, s.svc.Admit(ctx, s.st, s.conn, auth.Claims{Groups: []string{"devs"}}, u))
	// Unknown groups (nil) leave the team as it is.
	must(t, s.svc.Admit(ctx, s.st, s.conn, auth.Claims{}, u))
	if got := s.teamsOf(u.ID); len(got) != 1 {
		t.Fatalf("teams = %v", got)
	}
	if !s.svc.Provisions(domain.Connection{JIT: domain.JIT{Enabled: true}}) || s.svc.Provisions(domain.Connection{}) {
		t.Error("Provisions does not follow the connection")
	}
}

func TestGroupMappingTools(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	c := e.connection("okta")
	admin := e.person("admin@example.com", domain.RoleAdmin)
	viewer := e.person("viewer@example.com", domain.RoleViewer)
	x := e.team("Pay", domain.SourceManual)
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectTeam, x.ID, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	in := mcp.AddGroupMappingInput{Connection: c.Name, Group: "finance", Team: x.Slug}
	if _, err := e.svc.AddGroupMapping(ctx, viewer, in); !isDenied(err) {
		t.Errorf("viewer: %v", err)
	}
	if _, err := e.svc.AddGroupMapping(ctx, admin, in); !isDenied(err) {
		t.Errorf("a team above the Admin: %v", err)
	}
	if _, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: c.Name, Team: x.Slug}); !isInvalid(err) {
		t.Errorf("no group: %v", err)
	}
	if _, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: "nope", Group: "g", Team: x.Slug}); !isNotFound(err) {
		t.Errorf("unknown connection: %v", err)
	}
	if _, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: c.Name, Group: "g", Team: "nope"}); !isNotFound(err) {
		t.Errorf("unknown team: %v", err)
	}
	m, err := e.svc.AddGroupMapping(ctx, e.owner, in)
	must(t, err)
	if _, err := e.svc.AddGroupMapping(ctx, e.owner, in); !isConflict(err) {
		t.Errorf("twice: %v", err)
	}
	second, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: c.ID.String(), Group: "payroll", Team: x.ID.String()})
	must(t, err)
	list, err := e.svc.ListGroupMappings(ctx, e.owner)
	must(t, err)
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := e.svc.ListGroupMappings(ctx, viewer); !isDenied(err) {
		t.Errorf("viewer list: %v", err)
	}
	if _, err := e.svc.RemoveGroupMapping(ctx, viewer, mcp.GroupMappingRefInput{Mapping: m.ID.String()}); !isDenied(err) {
		t.Errorf("viewer remove: %v", err)
	}
	if _, err := e.svc.RemoveGroupMapping(ctx, e.owner, mcp.GroupMappingRefInput{Mapping: "nope"}); !isNotFound(err) {
		t.Errorf("unknown: %v", err)
	}
	// The members mapping added leave with the last mapping, others stay.
	u, hand := domain.User{Name: "I"}, domain.User{Name: "H"}
	for _, p := range []*domain.User{&u, &hand} {
		must(t, e.st.Users().Create(ctx, p))
		must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: p.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	}
	must(t, e.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: x.ID, UserID: u.ID, AccountID: e.org.ID, Source: domain.SourceIDP}))
	must(t, e.st.Teams().AddMember(ctx, &domain.TeamMember{TeamID: x.ID, UserID: hand.ID, AccountID: e.org.ID, Source: domain.SourceManual}))
	if _, err := e.svc.RemoveGroupMapping(ctx, e.owner, mcp.GroupMappingRefInput{Mapping: m.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if members, _ := e.st.Teams().Members(ctx, e.org.ID, x.ID); len(members) != 2 {
		t.Fatalf("one mapping left: %+v", members)
	}
	if _, err := e.svc.RemoveGroupMapping(ctx, e.owner, mcp.GroupMappingRefInput{Mapping: second.ID.String()}); err != nil {
		t.Fatal(err)
	}
	members, err := e.st.Teams().Members(ctx, e.org.ID, x.ID)
	must(t, err)
	if len(members) != 1 || members[0].UserID != hand.ID {
		t.Fatalf("members = %+v", members)
	}
}

func TestProvisioningNoEscalation(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	c := e.connection("okta")
	admin := e.person("admin@example.com", domain.RoleAdmin)
	viewer := e.person("viewer@example.com", domain.RoleViewer)
	on := true
	claim := "groups"
	if _, err := e.svc.SetConnectionProvisioning(ctx, viewer, mcp.SetProvisioningInput{Connection: c.Name}); !isDenied(err) {
		t.Errorf("viewer: %v", err)
	}
	roles := []string{domain.RoleOwner}
	if _, err := e.svc.SetConnectionProvisioning(ctx, admin, mcp.SetProvisioningInput{Connection: c.Name, JITEnabled: &on, DefaultRoles: &roles}); !isDenied(err) {
		t.Errorf("a default above the Admin: %v", err)
	}
	roles = []string{"nope"}
	if _, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: c.Name, DefaultRoles: &roles}); !isInvalid(err) {
		t.Errorf("unknown role: %v", err)
	}
	roles = []string{domain.RoleBilling}
	p, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: c.Name, JITEnabled: &on, DefaultRoles: &roles, GroupClaim: &claim})
	must(t, err)
	if !p.JIT.Enabled || len(p.JIT.DefaultRoles) != 1 || p.GroupClaim != "groups" {
		t.Fatalf("provisioning = %+v", p)
	}
	// The new provider would decide who gets the Billing role.
	empty := ""
	if _, err := e.svc.SetConnectionProvisioning(ctx, admin, mcp.SetProvisioningInput{Connection: c.Name, GroupClaim: &empty}); !isDenied(err) {
		t.Errorf("what it trusts is above the Admin: %v", err)
	}
	roles = []string{}
	if _, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: c.Name, DefaultRoles: &roles}); err != nil {
		t.Fatal(err)
	}
	x := e.team("Pay", domain.SourceManual)
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectTeam, x.ID, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	if _, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: c.Name, Group: "g", Team: x.Slug}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetConnectionProvisioning(ctx, admin, mcp.SetProvisioningInput{Connection: c.Name, GroupClaim: &claim}); !isDenied(err) {
		t.Errorf("a mapped team above the Admin: %v", err)
	}
	// An instance connection comes from the server file.
	inst := domain.Connection{Kind: domain.ConnectionGitHub, Name: "hub", ClientID: "c", ClientSecretRef: "env:X", Enabled: true}
	must(t, e.st.Connections().UpsertInstance(ctx, &inst))
	if _, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: "hub", JITEnabled: &on}); !isInvalid(err) {
		t.Errorf("instance connection: %v", err)
	}
	if _, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: "nope"}); !isNotFound(err) {
		t.Errorf("unknown: %v", err)
	}
}

func TestStoreErrorsPassThrough(t *testing.T) {
	boom := errors.New("boom")
	if err := storeErr(boom, "x"); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if err := storeErr(fmt.Errorf("x: %w", store.ErrLastOwner), "x"); !isConflict(err) {
		t.Errorf("err = %v", err)
	}
	if err := storeErr(store.ErrNotFound, "x"); !isNotFound(err) {
		t.Errorf("err = %v", err)
	}
	if err := scimStoreErr(boom); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if err := scimStoreErr(store.ErrConflict); err == nil {
		t.Error("no error")
	}
}

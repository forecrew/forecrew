package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// enforced gives f.org the domain acme.example, enforced through conn.
func (f *fixture) enforced(conn domain.Connection) domain.VerifiedDomain {
	f.t.Helper()
	d := f.verifyDomain(f.org.ID, "acme.example")
	must(f.t, f.st.VerifiedDomains().SetEnforcement(f.t.Context(), f.org.ID, d.ID, true, conn.ID))
	return d
}

func (f *fixture) gitHubUser(u domain.User, id int64, email string) {
	f.t.Helper()
	f.github.ID, f.github.Login, f.github.Email, f.github.EmailVerified = id, "ana", email, true
	must(f.t, f.st.Identities().Create(f.t.Context(), &domain.Identity{UserID: u.ID, ConnectionID: f.gh.ID, Subject: "77", Login: "ana", Email: email}))
}

func TestEnforcedDomainRefusesOtherConnections(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@acme.example")
	f.gitHubUser(u, 77, "ana@acme.example")
	conn := f.orgConnection()
	f.enforced(conn)

	status, body := f.signIn(f.gh, "")
	if status != http.StatusForbidden || !strings.Contains(body, "single sign-on") {
		t.Fatalf("through GitHub = %d %s", status, body)
	}
	if f.sessionToken() != "" {
		t.Fatal("a refused sign-in left a session")
	}

	f.claims("sub-1", "ana@acme.example", true)
	if status, body := f.signIn(conn, ""); status != http.StatusOK {
		t.Fatalf("through the organization's connection = %d %s", status, body)
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != f.org.ID {
		t.Errorf("caller = %+v", c)
	}
}

// A person's other organizations are not concerned: only the one that
// enforces the domain closes.
func TestEnforcementConcernsOneOrganization(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@acme.example")
	f.gitHubUser(u, 77, "ana@acme.example")
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))
	must(t, f.st.Memberships().Create(t.Context(), &domain.Membership{AccountID: other.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	f.enforced(f.orgConnection())

	if status, body := f.signIn(f.gh, "?org="+f.org.Slug); status != http.StatusForbidden || !strings.Contains(body, "single sign-on") {
		t.Fatalf("to the enforcing organization = %d %s", status, body)
	}
	f.newBrowser()
	if status, body := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatalf("to the other = %d %s", status, body)
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != other.ID {
		t.Fatalf("caller = %+v", c)
	}
	if _, err := f.svc.Switch(t.Context(), c, f.org.Slug); !errors.Is(err, auth.ErrSSORequired) {
		t.Fatalf("switch = %v", err)
	}
}

// An email an identity reports counts as much as the primary one.
func TestEnforcementFollowsIdentityEmails(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@home.example")
	f.gitHubUser(u, 77, "ana@acme.example")
	f.enforced(f.orgConnection())
	if status, _ := f.signIn(f.gh, ""); status != http.StatusForbidden {
		t.Fatalf("sign-in = %d", status)
	}
}

func TestSessionEndsWhenEnforcementStarts(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@acme.example")
	f.gitHubUser(u, 77, "ana@acme.example")
	if status, _ := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d", status)
	}
	token := f.sessionToken()
	if _, err := f.svc.Resolve(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	f.enforced(f.orgConnection())
	if _, err := f.svc.Resolve(t.Context(), token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("resolve = %v", err)
	}
	sessions, err := f.st.Sessions().ListByUser(t.Context(), u.ID)
	must(t, err)
	if len(sessions) != 1 || sessions[0].RevokedAt.IsZero() {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestInstanceAdminIsExemptAndAudited(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"github:77"} })
	u := f.member("ana@acme.example")
	f.gitHubUser(u, 77, "ana@acme.example")
	f.enforced(f.orgConnection())

	if status, body := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if _, err := f.svc.Resolve(t.Context(), f.sessionToken()); err != nil {
		t.Fatal(err)
	}
	entries, err := f.st.Audit().List(t.Context(), f.org.ID, store.AuditFilter{})
	must(t, err)
	if !slices.ContainsFunc(entries, func(e domain.AuditEntry) bool {
		return e.Action == "sso.exempted" && e.SubjectID == u.ID && strings.Contains(string(e.Data), f.gh.ID.String())
	}) {
		t.Errorf("audit = %+v", entries)
	}
}

func TestBreakGlassSessionIsExempt(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@acme.example")
	f.enforced(f.orgConnection())
	token, err := auth.NewToken(auth.PrefixSession)
	must(t, err)
	s := domain.Session{TokenHash: mcp.HashToken(token), UserID: u.ID, AccountID: f.org.ID, IdleExpiresAt: t0.Add(time.Hour), ExpiresAt: t0.Add(time.Hour)}
	must(t, f.st.Sessions().Create(t.Context(), &s))
	if _, err := f.svc.Resolve(t.Context(), token); err != nil {
		t.Fatalf("resolve = %v", err)
	}
}

func TestAcceptanceChecksTheInviterStillCovers(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	owner, err := f.st.Roles().GetByName(ctx, f.org.ID, domain.RoleScopeOrganization, domain.RoleOwner)
	must(t, err)
	viewer, err := f.st.Roles().GetByName(ctx, f.org.ID, domain.RoleScopeOrganization, domain.RoleViewer)
	must(t, err)
	inviter := f.member("boss@example.com")
	b := domain.RoleBinding{AccountID: f.org.ID, SubjectKind: domain.SubjectUser, SubjectID: inviter.ID, RoleID: viewer.ID, CreatedBy: "user:x"}
	must(t, f.st.RoleBindings().Create(ctx, &b))
	invite := func(email string, role domain.Role) string {
		token, err := auth.NewToken(auth.PrefixInvitation)
		must(t, err)
		inv := domain.Invitation{AccountID: f.org.ID, Email: email, TokenHash: mcp.HashToken(token), InvitedBy: domain.UserActor(f.org.ID, inviter.ID, domain.Credential{}).String(),
			Grants: []domain.Grant{{RoleID: role.ID}}, ExpiresAt: t0.Add(time.Hour)}
		must(t, f.st.Invitations().Create(ctx, &inv))
		return token
	}
	f.claims("sub-1", "bea@example.com", true)
	status, body := f.signIn(f.inst, "?invite="+invite("bea@example.com", owner))
	if status != http.StatusForbidden || !strings.Contains(body, "cannot be used") {
		t.Fatalf("above the inviter = %d %s", status, body)
	}
	f.newBrowser()
	if status, body := f.signIn(f.inst, "?invite="+invite("bea@example.com", viewer)); status != http.StatusOK {
		t.Fatalf("within the inviter = %d %s", status, body)
	}
}

// admitted is a Policy that remembers what it was given.
type admitted struct {
	provisions bool
	account    domain.Account // joined when provisions
	claims     []auth.Claims
	err        error
}

func (a *admitted) Provisions(domain.Connection) bool { return a.provisions }

func (a *admitted) Admit(ctx context.Context, tx store.Store, _ domain.Connection, c auth.Claims, u domain.User) error {
	a.claims = append(a.claims, c)
	if a.err != nil || !a.provisions {
		return a.err
	}
	return auth.Join(ctx, tx, a.account.ID, u.ID, domain.SourceJIT, nil, domain.SystemActor(a.account.ID, "jit"))
}

func TestPolicySeesTheGroupClaim(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{"a list", map[string]any{"groups": []string{"devs", "ops"}}, []string{"devs", "ops"}},
		{"one group", map[string]any{"groups": "devs"}, []string{"devs"}},
		{"no claim", nil, []string{}},
		{"too many for the token", map[string]any{"_claim_names": map[string]string{"groups": "src1"}, "_claim_sources": map[string]any{"src1": map[string]string{"endpoint": "https://graph.example/groups"}}}, nil},
		{"not groups", map[string]any{"groups": 7}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			p := &admitted{}
			f.setPolicy(p)
			f.member("ana@example.com")
			conn := f.inst
			conn.GroupClaim = "groups"
			must(t, f.st.Connections().Update(t.Context(), &conn))
			f.claims("sub-1", "ana@example.com", true)
			for k, v := range tt.claims {
				f.oidc.Claims[k] = v
			}
			if status, body := f.signIn(conn, ""); status != http.StatusOK {
				t.Fatalf("sign-in = %d %s", status, body)
			}
			if len(p.claims) != 1 {
				t.Fatalf("claims = %+v", p.claims)
			}
			got := p.claims[0].Groups
			if (got == nil) != (tt.want == nil) || !slices.Equal(got, tt.want) {
				t.Errorf("groups = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPolicyProvisionsAndRefuses(t *testing.T) {
	f := newFixture(t)
	f.claims("sub-new", "new@example.com", true)
	if status, _ := f.signIn(f.inst, ""); status != http.StatusForbidden {
		t.Fatalf("open core = %d", status)
	}
	p := &admitted{provisions: true, account: f.org, err: errors.New("no")}
	f.setPolicy(p)
	f.newBrowser()
	if status, _ := f.signIn(f.inst, ""); status == http.StatusOK {
		t.Fatalf("a policy error = %d", status)
	}
	if users, err := f.st.Users().ListByEmail(t.Context(), "new@example.com"); err != nil || len(users) != 0 {
		t.Fatalf("a refused sign-in kept %+v, %v", users, err)
	}
	p.err = nil
	f.newBrowser()
	f.signIn(f.inst, "")
	users, err := f.st.Users().ListByEmail(t.Context(), "new@example.com")
	must(t, err)
	if len(users) != 1 {
		t.Fatalf("users = %+v", users)
	}
}

// setPolicy rebuilds the service of f with p, behind the same web server.
func (f *fixture) setPolicy(p auth.Policy) {
	f.t.Helper()
	f.svc = auth.New(auth.Config{
		Store: f.st, Auth: f.cfg, BaseURL: f.web.URL, Now: f.clock.Now, HTTP: f.http, OrgHTTP: f.http, Policy: p,
		Logger: slog.New(slog.DiscardHandler), GitHubURL: f.github.URL, GitHubAPIURL: f.github.URL,
		Getenv: func(string) string { return clientSecret },
	})
	f.handler.set(f.svc.Handler())
}

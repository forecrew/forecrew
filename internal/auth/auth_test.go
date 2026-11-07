package auth_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

const (
	clientID     = "client-1"
	clientSecret = "s3cr3t-client-secret"
	secretEnv    = "FORECREW_CONN_TEST"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fixture is forecrewd's sign-in behind TLS, with a fake OIDC provider, a
// fake GitHub and a browser with a cookie jar.
type fixture struct {
	t       *testing.T
	st      *testkit.Store
	clock   *testkit.Clock
	svc     *auth.Service
	oidc    *testkit.FakeOIDC
	github  *testkit.FakeGitHub
	web     *httptest.Server
	browser *http.Client
	logs    *bytes.Buffer
	org     domain.Account
	cfg     config.Auth
	inst    domain.Connection // instance OIDC connection
	gh      domain.Connection // instance GitHub connection
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, change ...func(*config.Auth)) *fixture {
	t.Helper()
	clock := testkit.NewClock(t0)
	f := &fixture{t: t, clock: clock, st: testkit.NewStore(clock.Now), logs: &bytes.Buffer{}}
	f.oidc = testkit.NewFakeOIDC(t, clientID)
	f.oidc.Now = clock.Now
	f.github = testkit.NewFakeGitHub(t)
	f.org = domain.Account{Name: "Acme"}
	must(t, f.st.Accounts().Create(t.Context(), &f.org))

	cfg := config.Auth{SessionIdle: time.Hour, SessionAbsolute: 24 * time.Hour}
	for _, c := range change {
		c(&cfg)
	}
	cfg.Connections = map[string]config.Connection{
		"oidc":   {Kind: domain.ConnectionOIDC, Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv},
		"github": {Kind: domain.ConnectionGitHub, ClientID: clientID, ClientSecretRef: "env:" + secretEnv},
	}
	f.cfg = cfg
	mux := http.NewServeMux()
	f.web = httptest.NewUnstartedServer(mux)
	f.web.StartTLS()
	t.Cleanup(f.web.Close)
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the fakes' test certificates
	f.svc = auth.New(auth.Config{
		Store: f.st, Auth: cfg, BaseURL: f.web.URL, Now: clock.Now, HTTP: insecure, OrgHTTP: insecure,
		Logger: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), GitHubURL: f.github.URL, GitHubAPIURL: f.github.URL,
		Getenv: func(k string) string {
			if k == secretEnv {
				return clientSecret
			}
			return ""
		},
	})
	must(t, f.svc.Start(t.Context()))
	mux.Handle("/auth/", f.svc.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "the app") })
	conns, err := f.st.Connections().List(t.Context(), uuid.Nil())
	must(t, err)
	for _, c := range conns {
		if c.Kind == domain.ConnectionGitHub {
			f.gh = c
		} else {
			f.inst = c
		}
	}
	f.newBrowser()
	return f
}

func (f *fixture) newBrowser() {
	jar, err := cookiejar.New(nil)
	must(f.t, err)
	f.browser = &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the fakes' test certificates
}

// signIn follows the whole flow in the browser.
func (f *fixture) signIn(c domain.Connection, query string) (status int, body string) {
	f.t.Helper()
	resp, err := f.browser.Get(f.web.URL + "/auth/" + c.ID.String() + "/login" + query)
	must(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	must(f.t, err)
	return resp.StatusCode, string(b)
}

// sessionToken returns the session cookie the browser holds.
func (f *fixture) sessionToken() string {
	f.t.Helper()
	u, err := url.Parse(f.web.URL)
	must(f.t, err)
	for _, c := range f.browser.Jar.Cookies(u) {
		if c.Name == auth.CookieName {
			return c.Value
		}
	}
	return ""
}

func (f *fixture) claims(sub, email string, verified bool) {
	f.oidc.Claims = map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "Ana"}
}

// member stores a user with a verified email and an active membership.
func (f *fixture) member(email string) domain.User {
	f.t.Helper()
	u := domain.User{Name: email, Email: email, EmailVerified: true}
	must(f.t, f.st.Users().Create(f.t.Context(), &u))
	must(f.t, f.st.Memberships().Create(f.t.Context(), &domain.Membership{AccountID: f.org.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	return u
}

func (f *fixture) invite(in domain.Invitation) string {
	f.t.Helper()
	token, err := auth.NewToken(auth.PrefixInvitation)
	must(f.t, err)
	role, err := f.st.Roles().GetByName(f.t.Context(), f.org.ID, domain.RoleScopeOrganization, domain.RoleMember)
	must(f.t, err)
	in.AccountID, in.TokenHash, in.InvitedBy, in.Grants = f.org.ID, mcp.HashToken(token), "user:x", []domain.Grant{{RoleID: role.ID}}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = t0.Add(time.Hour)
	}
	must(f.t, f.st.Invitations().Create(f.t.Context(), &in))
	return token
}

func TestSignInLinksByVerifiedEmail(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@example.com")
	f.claims("sub-1", "ANA@example.com", true)
	status, body := f.signIn(f.inst, "")
	if status != http.StatusOK || body != "the app" {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	id, err := f.st.Identities().GetBySubject(t.Context(), f.inst.ID, "sub-1")
	must(t, err)
	if id.UserID != u.ID {
		t.Errorf("identity = %+v", id)
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if got, _ := c.Actor.SubjectID(); got != u.ID || c.Actor.AccountID != f.org.ID || c.Actor.Kind != domain.ActorUser || c.Actor.Credential.Kind != domain.CredentialSession {
		t.Errorf("caller = %+v", c)
	}
	entries, err := f.st.Audit().List(t.Context(), f.org.ID, store.AuditFilter{})
	must(t, err)
	if len(entries) == 0 || entries[0].Action != "session.created" || entries[0].Credential != c.Actor.Credential.String() {
		t.Errorf("audit = %+v", entries)
	}

	// The second sign-in finds the identity, and gets a new session.
	first := f.sessionToken()
	f.newBrowser()
	f.oidc.Claims["email"] = "renamed@example.com"
	if status, _ := f.signIn(f.inst, ""); status != http.StatusOK || f.sessionToken() == first {
		t.Errorf("second sign-in = %d", status)
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/callback") {
			return nil
		}
		if req.URL.Path == "/" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := f.browser.Get(f.web.URL + "/auth/" + f.inst.ID.String() + "/login")
	must(t, err)
	must(t, resp.Body.Close())
	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			got = c
		}
	}
	if got == nil || !got.HttpOnly || !got.Secure || got.SameSite != http.SameSiteStrictMode || got.Path != "/" || !strings.HasPrefix(got.Value, "fcs_") {
		t.Fatalf("cookie = %+v", got)
	}
	sessions, err := f.st.Sessions().GetByTokenHash(t.Context(), mcp.HashToken(got.Value))
	must(t, err)
	if !sessions.ExpiresAt.Equal(t0.Add(24*time.Hour)) || !sessions.IdleExpiresAt.Equal(t0.Add(time.Hour)) || sessions.ConnectionID != f.inst.ID {
		t.Errorf("session = %+v", sessions)
	}
}

// A callback is only taken with the state of the browser that started it.
func TestWrongState(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			q := req.URL.Query()
			q.Set("state", "forged")
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	status, body := f.signIn(f.inst, "")
	if status != http.StatusBadRequest || !strings.Contains(body, "expired") || f.sessionToken() != "" {
		t.Errorf("forged state = %d %s", status, body)
	}
	// Without the cookie at all.
	f.newBrowser()
	resp, err := f.browser.Get(f.web.URL + "/auth/" + f.inst.ID.String() + "/callback?code=c&state=s")
	must(t, err)
	must(t, resp.Body.Close())
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no cookie = %d", resp.StatusCode)
	}
}

// A code works once, even with the flow cookie saved and sent again.
func TestReplayedCode(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	var callback string
	var flow []*http.Cookie
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			callback = req.URL.String()
			flow = f.browser.Jar.Cookies(req.URL)
		}
		return nil
	}
	if status, _ := f.signIn(f.inst, ""); status != http.StatusOK || f.sessionToken() == "" {
		t.Fatalf("first use = %d", status)
	}
	again, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback, nil)
	must(t, err)
	for _, c := range flow {
		again.AddCookie(c)
	}
	plain := &http.Client{Transport: f.browser.Transport}
	resp, err := plain.Do(again)
	must(t, err)
	must(t, resp.Body.Close())
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("replay = %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			t.Errorf("replay set a session: %v", c)
		}
	}
}

func TestForgedIDTokens(t *testing.T) {
	tests := []struct {
		name  string
		claim string
		value func(f *fixture) any
	}{
		{"bad nonce", "nonce", func(*fixture) any { return "other" }},
		{"wrong audience", "aud", func(*fixture) any { return "someone-else" }},
		{"expired", "exp", func(*fixture) any { return t0.Add(-time.Minute).Unix() }},
		{"wrong issuer", "iss", func(*fixture) any { return "https://evil.example" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.member("ana@example.com")
			f.claims("sub-1", "ana@example.com", true)
			f.oidc.Claims[tt.claim] = tt.value(f)
			status, body := f.signIn(f.inst, "")
			if status != http.StatusForbidden || !strings.Contains(body, "Sign-in failed") || f.sessionToken() != "" {
				t.Errorf("sign-in = %d %s", status, body)
			}
		})
	}
}

func TestUnknownUserRefused(t *testing.T) {
	f := newFixture(t)
	f.claims("sub-9", "stranger@example.com", true)
	status, body := f.signIn(f.inst, "")
	if status != http.StatusForbidden || !strings.Contains(body, "do not have access") || f.sessionToken() != "" {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if _, err := f.st.Identities().GetBySubject(t.Context(), f.inst.ID, "sub-9"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("identity kept: %v", err)
	}
	if users, _ := f.st.Users().ListByEmail(t.Context(), "stranger@example.com"); len(users) != 0 {
		t.Errorf("user kept: %+v", users)
	}
}

func TestUnverifiedEmailDoesNotLink(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", false)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || f.sessionToken() != "" {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if _, err := f.st.Identities().GetBySubject(t.Context(), f.inst.ID, "sub-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("linked: %v", err)
	}
}

// orgConnection adds an organization's own connection to the fake provider.
func (f *fixture) orgConnection() domain.Connection {
	f.t.Helper()
	c := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv, Enabled: true}
	must(f.t, f.st.Connections().Create(f.t.Context(), &c))
	return c
}

func (f *fixture) verifyDomain(account uuid.UUID, name string) domain.VerifiedDomain {
	f.t.Helper()
	d := domain.VerifiedDomain{AccountID: account, Domain: name, VerificationToken: "t"}
	must(f.t, f.st.VerifiedDomains().Create(f.t.Context(), &d))
	must(f.t, f.st.VerifiedDomains().SetVerified(f.t.Context(), account, d.ID, true))
	return d
}

// An organization's connection claims only the emails of its verified
// domains: otherwise it could become anyone.
func TestOrganizationConnectionLinking(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@acme.example")
	conn := f.orgConnection()
	f.claims("sub-1", "ana@acme.example", true)
	if status, _ := f.signIn(conn, ""); status != http.StatusForbidden {
		t.Fatalf("domain not verified = %d", status)
	}

	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))
	d := f.verifyDomain(other.ID, "acme.example")
	f.newBrowser()
	if status, _ := f.signIn(conn, ""); status != http.StatusForbidden {
		t.Fatalf("domain of another organization = %d", status)
	}

	must(t, f.st.VerifiedDomains().Delete(t.Context(), other.ID, d.ID))
	f.verifyDomain(f.org.ID, "acme.example")
	f.newBrowser()
	if status, _ := f.signIn(conn, ""); status != http.StatusOK {
		t.Fatalf("verified domain = %d", status)
	}
	if id, err := f.st.Identities().GetBySubject(t.Context(), conn.ID, "sub-1"); err != nil || id.UserID != u.ID {
		t.Errorf("identity = %+v, %v", id, err)
	}
}

func TestInvitationAccepted(t *testing.T) {
	f := newFixture(t)
	token := f.invite(domain.Invitation{Email: "bea@example.com"})
	f.claims("sub-2", "bea@example.com", true)
	status, body := f.signIn(f.inst, "?invite="+token)
	if status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	users, err := f.st.Users().ListByEmail(t.Context(), "bea@example.com")
	must(t, err)
	if len(users) != 1 || !users[0].EmailVerified || users[0].Name != "Ana" {
		t.Fatalf("users = %+v", users)
	}
	m, err := f.st.Memberships().Get(t.Context(), f.org.ID, users[0].ID)
	must(t, err)
	if m.Status != domain.MembershipActive || m.Source != domain.SourceInvitation {
		t.Errorf("membership = %+v", m)
	}
	eff, err := f.st.RoleBindings().Effective(t.Context(), f.org.ID, domain.SubjectUser, users[0].ID)
	must(t, err)
	if len(eff) != 1 || eff[0].Role.Name != domain.RoleMember {
		t.Errorf("roles = %+v", eff)
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != f.org.ID {
		t.Errorf("organization = %s", c.Actor.AccountID)
	}

	// The link works once.
	f.newBrowser()
	f.claims("sub-3", "bea@example.com", true)
	if status, body := f.signIn(f.inst, "?invite="+token); status != http.StatusForbidden || !strings.Contains(body, "cannot be used") {
		t.Errorf("second use = %d %s", status, body)
	}
}

func TestInvitationRefusals(t *testing.T) {
	tests := []struct {
		name   string
		inv    domain.Invitation
		query  string
		email  string
		verify bool
		want   string
	}{
		{name: "another person", inv: domain.Invitation{Email: "bea@example.com"}, email: "eve@example.com", verify: true, want: "someone else"},
		{name: "unverified email", inv: domain.Invitation{Email: "bea@example.com"}, email: "bea@example.com", want: "someone else"},
		{name: "expired", inv: domain.Invitation{Email: "bea@example.com", ExpiresAt: t0.Add(-time.Minute)}, email: "bea@example.com", verify: true, want: "cannot be used"},
		{name: "revoked", inv: domain.Invitation{Email: "bea@example.com", RevokedAt: t0}, email: "bea@example.com", verify: true, want: "cannot be used"},
		{name: "unknown token", inv: domain.Invitation{Email: "bea@example.com"}, query: "?invite=fci_nope", email: "bea@example.com", verify: true, want: "cannot be used"},
		{name: "github login through oidc", inv: domain.Invitation{GitHubLogin: "bea", GitHubUserID: 9}, email: "bea@example.com", verify: true, want: "someone else"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			token := f.invite(tt.inv)
			f.claims("sub-2", tt.email, tt.verify)
			q := tt.query
			if q == "" {
				q = "?invite=" + token
			}
			status, body := f.signIn(f.inst, q)
			if status != http.StatusForbidden || !strings.Contains(body, tt.want) || f.sessionToken() != "" {
				t.Errorf("sign-in = %d %s", status, body)
			}
		})
	}
}

// An existing user accepts through the link: same user, new organization.
func TestInvitationOfExistingUser(t *testing.T) {
	f := newFixture(t)
	u := domain.User{Name: "Bea", Email: "bea@example.com", EmailVerified: true}
	must(t, f.st.Users().Create(t.Context(), &u))
	token := f.invite(domain.Invitation{Email: "BEA@example.com"})
	f.claims("sub-2", "bea@example.com", true)
	if status, body := f.signIn(f.inst, "?invite="+token); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if m, err := f.st.Memberships().Get(t.Context(), f.org.ID, u.ID); err != nil || m.Status != domain.MembershipActive {
		t.Errorf("membership = %+v, %v", m, err)
	}
}

// Through an organization's connection without a verified domain, the
// email of an invitation of that organization is accepted, but it proves
// nothing elsewhere: the new user's email stays unverified.
func TestInvitationThroughOwnConnection(t *testing.T) {
	f := newFixture(t)
	existing := domain.User{Email: "bea@example.com", EmailVerified: true}
	must(t, f.st.Users().Create(t.Context(), &existing))
	conn := f.orgConnection()
	token := f.invite(domain.Invitation{Email: "bea@example.com"})
	f.claims("sub-2", "bea@example.com", true)
	if status, body := f.signIn(conn, "?invite="+token); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	id, err := f.st.Identities().GetBySubject(t.Context(), conn.ID, "sub-2")
	must(t, err)
	if id.UserID == existing.ID {
		t.Fatal("the connection took over the existing user")
	}
	u, err := f.st.Users().Get(t.Context(), id.UserID)
	must(t, err)
	if u.EmailVerified {
		t.Errorf("user = %+v", u)
	}
}

func TestSuspendedMemberKeepsOut(t *testing.T) {
	f := newFixture(t)
	u := f.member("bea@example.com")
	must(t, f.st.Memberships().SetStatus(t.Context(), f.org.ID, u.ID, domain.MembershipSuspended))
	f.claims("sub-2", "bea@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "do not have access") {
		t.Errorf("suspended sign-in = %d %s", status, body)
	}
	f.newBrowser()
	token := f.invite(domain.Invitation{Email: "bea@example.com"})
	if status, body := f.signIn(f.inst, "?invite="+token); status != http.StatusForbidden || !strings.Contains(body, "suspended") {
		t.Errorf("invitation of a suspended member = %d %s", status, body)
	}
	if status, _ := f.signIn(f.inst, "?invite="+token); status != http.StatusForbidden {
		t.Errorf("repeat = %d", status)
	}
}

func TestDisabledUserRefused(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@example.com")
	must(t, f.st.Users().SetDisabled(t.Context(), u.ID, true))
	f.claims("sub-1", "ana@example.com", true)
	if status, body := f.signIn(f.inst, ""); status != http.StatusForbidden || !strings.Contains(body, "disabled") {
		t.Errorf("sign-in = %d %s", status, body)
	}
}

// Entra's email claim proves nothing: only xms_edov does.
func TestMicrosoftEmail(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	conn := domain.Connection{Kind: domain.ConnectionMicrosoft, Name: "entra", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv, Enabled: true}
	must(t, f.st.Connections().UpsertInstance(t.Context(), &conn))
	f.claims("sub-1", "ana@example.com", true)
	if status, _ := f.signIn(conn, ""); status != http.StatusForbidden {
		t.Fatalf("email_verified alone = %d", status)
	}
	f.newBrowser()
	f.oidc.Claims["xms_edov"] = "1"
	if status, body := f.signIn(conn, ""); status != http.StatusOK {
		t.Fatalf("xms_edov = %d %s", status, body)
	}
}

func TestGitHubSignIn(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@example.com")
	f.github.ID, f.github.Login, f.github.Name, f.github.Email, f.github.EmailVerified = 583231, "ana", "Ana", "ana@example.com", true
	if status, body := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	id, err := f.st.Identities().GetBySubject(t.Context(), f.gh.ID, "583231")
	must(t, err)
	if id.UserID != u.ID || id.Login != "ana" {
		t.Errorf("identity = %+v", id)
	}
	// An unverified primary email links nobody.
	f.newBrowser()
	f.github.ID, f.github.EmailVerified = 7, false
	if status, _ := f.signIn(f.gh, ""); status != http.StatusForbidden {
		t.Errorf("unverified = %d", status)
	}
}

func TestGitHubLoginInvitation(t *testing.T) {
	f := newFixture(t)
	token := f.invite(domain.Invitation{GitHubLogin: "Bea", GitHubUserID: 9})

	// Someone who took the login after a rename is not whom it was sent to.
	f.github.ID, f.github.Login, f.github.NoEmails = 10, "Bea", true
	if status, body := f.signIn(f.gh, "?invite="+token); status != http.StatusForbidden || !strings.Contains(body, "someone else") {
		t.Fatalf("same login, other user = %d %s", status, body)
	}
	f.newBrowser()

	// The person it was sent to is, under whatever login they have now.
	f.github.ID, f.github.Login = 9, "bea"
	if status, body := f.signIn(f.gh, "?invite="+token); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	id, err := f.st.Identities().GetBySubject(t.Context(), f.gh.ID, "9")
	must(t, err)
	u, err := f.st.Users().Get(t.Context(), id.UserID)
	must(t, err)
	if u.Name != "bea" || u.Email != "" {
		t.Errorf("user = %+v", u)
	}
}

func TestGitHubUserID(t *testing.T) {
	f := newFixture(t)
	f.github.ID, f.github.Login = 583231, "octocat"
	id, err := f.svc.GitHubUserID(t.Context(), "octocat")
	must(t, err)
	if id != 583231 {
		t.Errorf("id = %d", id)
	}
	if _, err := f.svc.GitHubUserID(t.Context(), "ghost"); !errors.Is(err, auth.ErrNoGitHubUser) {
		t.Errorf("unknown = %v", err)
	}
}

func TestProviderErrors(t *testing.T) {
	f := newFixture(t)
	if resp, err := f.browser.Get(f.web.URL + "/auth/nope/login"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad id = %v, %v", resp, err)
	}
	if resp, err := f.browser.Get(f.web.URL + "/auth/" + domain.NewID().String() + "/login"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id = %v, %v", resp, err)
	}
	off := f.inst
	off.Enabled = false
	must(t, f.st.Connections().Update(t.Context(), &off))
	if status, _ := f.signIn(f.inst, ""); status != http.StatusNotFound {
		t.Errorf("turned off = %d", status)
	}

	// The provider answers with an error instead of a code.
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			q := req.URL.Query()
			q.Del("code")
			q.Set("error", "access_denied")
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	if status, body := f.signIn(f.gh, ""); status != http.StatusForbidden || !strings.Contains(body, "canceled") {
		t.Errorf("denied = %d %s", status, body)
	}

	// The issuer is down.
	broken := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionOIDC, Name: "down", Issuer: "https://127.0.0.1:1", ClientID: "x", ClientSecretRef: "env:" + secretEnv, Enabled: true}
	must(t, f.st.Connections().Create(t.Context(), &broken))
	if status, body := f.signIn(broken, ""); status != http.StatusBadGateway || !strings.Contains(body, "not available") {
		t.Errorf("issuer down = %d %s", status, body)
	}
	// The secret is missing.
	unset := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionGitHub, Name: "unset", ClientID: "x", ClientSecretRef: "env:FORECREW_CONN_UNSET", Enabled: true}
	must(t, f.st.Connections().Create(t.Context(), &unset))
	if status, _ := f.signIn(unset, ""); status != http.StatusBadGateway {
		t.Errorf("no secret = %d", status)
	}
}

func TestFileSecret(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "s"), []byte(clientSecret+"\n"), 0o600))
	f := newFixture(t)
	conn := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionGitHub, Name: "file", ClientID: "x", ClientSecretRef: "secret:conn-missing", Enabled: true}
	must(t, f.st.Connections().Create(t.Context(), &conn))
	// The real /run/secrets has no such file.
	if status, _ := f.signIn(conn, ""); status != http.StatusBadGateway {
		t.Errorf("missing file = %d", status)
	}
	got := auth.New(auth.Config{Store: f.st, BaseURL: "https://x", GitHubURL: f.github.URL, ReadFile: func(string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, "s")) }})
	rec := httptest.NewRecorder()
	got.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/"+conn.ID.String()+"/login", nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "code_challenge=") {
		t.Errorf("login = %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestInstanceAdmin(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"root@example.com", "github:77"} })
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(t.Context(), &other))

	// No membership anywhere, and still in: without an organization.
	f.claims("sub-root", "root@example.com", true)
	root := domain.User{Name: "root", Email: "root@example.com", EmailVerified: true}
	must(t, f.st.Users().Create(t.Context(), &root))
	if status, body := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	u, err := f.st.Users().Get(t.Context(), root.ID)
	must(t, err)
	if !u.InstanceAdmin {
		t.Error("the flag does not follow config")
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != uuid.Nil() || c.BreakGlass {
		t.Errorf("caller = %+v", c)
	}

	acc, err := f.svc.Switch(t.Context(), c, other.Slug)
	must(t, err)
	if acc.ID != other.ID {
		t.Errorf("account = %+v", acc)
	}
	entries, err := f.st.Audit().List(t.Context(), other.ID, store.AuditFilter{})
	must(t, err)
	if len(entries) == 0 || entries[0].Action != "instance_admin.entered" || entries[0].Credential != c.Actor.Credential.String() {
		t.Errorf("audit = %+v", entries)
	}
	c, err = f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if !c.BreakGlass || c.Actor.AccountID != other.ID {
		t.Errorf("caller = %+v", c)
	}

	// A GitHub admin is named by user ID, and the flag goes away with the config.
	gh := f.member("gh@example.com")
	must(t, f.st.Identities().Create(t.Context(), &domain.Identity{UserID: gh.ID, ConnectionID: f.gh.ID, Subject: "77", Login: "gh"}))
	f.github.ID, f.github.Login = 77, "gh"
	f.newBrowser()
	if status, _ := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatal("github admin refused")
	}
	if u, _ = f.st.Users().Get(t.Context(), gh.ID); !u.InstanceAdmin {
		t.Error("github admin not flagged")
	}
	noLonger := domain.User{Name: "was", Email: "was@example.com", EmailVerified: true, InstanceAdmin: true}
	must(t, f.st.Users().Create(t.Context(), &noLonger))
	must(t, f.st.Memberships().Create(t.Context(), &domain.Membership{AccountID: f.org.ID, UserID: noLonger.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	f.claims("sub-was", "was@example.com", true)
	f.newBrowser()
	if status, _ := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatal("former admin refused")
	}
	if u, _ = f.st.Users().Get(t.Context(), noLonger.ID); u.InstanceAdmin {
		t.Error("the flag stayed")
	}
}

// restarted is forecrewd started again with the instance admins given.
func (f *fixture) restarted(admins ...string) *auth.Service {
	f.t.Helper()
	cfg := f.cfg
	cfg.InstanceAdmins = admins
	svc := auth.New(auth.Config{Store: f.st, Auth: cfg, Now: f.clock.Now})
	must(f.t, svc.Start(f.t.Context()))
	return svc
}

// An admin taken out of config loses break-glass at the next start, in the
// sessions they already have.
func TestInstanceAdminRemovedFromConfig(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"root@example.com", "github:77"} })
	ctx := t.Context()
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	root := domain.User{Name: "root", Email: "root@example.com", EmailVerified: true}
	must(t, f.st.Users().Create(ctx, &root))
	f.claims("sub-root", "root@example.com", true)
	if status, _ := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatal("admin refused")
	}
	token := f.sessionToken()
	c, err := f.svc.Resolve(ctx, token)
	must(t, err)
	_, err = f.svc.Switch(ctx, c, other.Slug)
	must(t, err)
	if c, err = f.svc.Resolve(ctx, token); err != nil || !c.BreakGlass {
		t.Fatalf("before = %+v, %v", c, err)
	}
	// A second session, without an organization.
	f.newBrowser()
	f.signIn(f.inst, "")
	idle := f.sessionToken()

	// Still listed: nothing changes.
	svc := f.restarted("root@example.com")
	if c, err = svc.Resolve(ctx, token); err != nil || !c.BreakGlass {
		t.Fatalf("still listed = %+v, %v", c, err)
	}

	svc = f.restarted("someone@example.com")
	u, err := f.st.Users().Get(ctx, root.ID)
	must(t, err)
	if u.InstanceAdmin {
		t.Fatal("the flag stayed")
	}
	c, err = svc.Resolve(ctx, token)
	if !errors.Is(err, auth.ErrNoSession) || c.BreakGlass {
		t.Errorf("after = %+v, %v", c, err)
	}
	c, err = svc.Resolve(ctx, idle)
	must(t, err)
	if c.BreakGlass {
		t.Errorf("caller = %+v", c)
	}
	if _, err := svc.Switch(ctx, c, other.Slug); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("switch = %v", err)
	}
}

func TestGitHubInstanceAdminRemovedFromConfig(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"github:77"} })
	ctx := t.Context()
	u := f.member("gh@example.com")
	must(t, f.st.Identities().Create(ctx, &domain.Identity{UserID: u.ID, ConnectionID: f.gh.ID, Subject: "77", Login: "gh"}))
	f.github.ID, f.github.Login = 77, "gh"
	if status, _ := f.signIn(f.gh, ""); status != http.StatusOK {
		t.Fatal("admin refused")
	}
	f.restarted("github:77")
	if got, _ := f.st.Users().Get(ctx, u.ID); !got.InstanceAdmin {
		t.Fatal("cleared while listed")
	}
	f.restarted("github:78")
	if got, _ := f.st.Users().Get(ctx, u.ID); got.InstanceAdmin {
		t.Error("the flag stayed")
	}
}

// An organization's connection, even for its verified domain, makes nobody
// an instance admin.
func TestOrganizationConnectionGrantsNoAdmin(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"root@acme.example"} })
	u := f.member("root@acme.example")
	conn := f.orgConnection()
	f.verifyDomain(f.org.ID, "acme.example")
	f.claims("sub-1", "root@acme.example", true)
	if status, body := f.signIn(conn, ""); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	if got, _ := f.st.Users().Get(t.Context(), u.ID); got.InstanceAdmin {
		t.Fatal("granted through an organization's connection")
	}
	// Through an instance connection it is, and the other then keeps it.
	f.newBrowser()
	f.claims("sub-2", "root@acme.example", true)
	f.signIn(f.inst, "")
	if got, _ := f.st.Users().Get(t.Context(), u.ID); !got.InstanceAdmin {
		t.Fatal("not granted through an instance connection")
	}
	f.newBrowser()
	f.claims("sub-1", "root@acme.example", true)
	f.signIn(conn, "")
	if got, _ := f.st.Users().Get(t.Context(), u.ID); !got.InstanceAdmin {
		t.Error("cleared by a sign-in while listed")
	}
}

// A session from an organization's connection stays in that organization:
// its provider vouches for nothing else, even for a member of both.
func TestOrganizationConnectionStaysInItsOrganization(t *testing.T) {
	f := newFixture(t, func(a *config.Auth) { a.InstanceAdmins = []string{"ana@acme.example"} })
	ctx := t.Context()
	u := f.member("ana@acme.example")
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	must(t, f.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	conn := f.orgConnection()
	f.verifyDomain(f.org.ID, "acme.example")
	f.claims("sub-1", "ana@acme.example", true)

	if status, _ := f.signIn(conn, "?org="+other.Slug); status != http.StatusForbidden {
		t.Errorf("org of another organization = %d", status)
	}
	f.newBrowser()
	if status, body := f.signIn(conn, "?org="+f.org.Slug); status != http.StatusOK {
		t.Fatalf("own org = %d %s", status, body)
	}
	c, err := f.svc.Resolve(ctx, f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != f.org.ID {
		t.Fatalf("organization = %s", c.Actor.AccountID)
	}
	if _, err := f.svc.Switch(ctx, c, other.Slug); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("switch away = %v", err)
	}
	// Not even for an instance admin.
	must(t, f.st.Users().Update(ctx, &domain.User{ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: true, InstanceAdmin: true}))
	if _, err := f.svc.Switch(ctx, c, other.Slug); !errors.Is(err, auth.ErrNotMember) {
		t.Errorf("admin switch away = %v", err)
	}
	if _, err := f.svc.Switch(ctx, c, f.org.Slug); err != nil {
		t.Errorf("switch to its own: %v", err)
	}
	// An instance session moves freely.
	f.newBrowser()
	f.claims("sub-2", "ana@acme.example", true)
	f.signIn(f.inst, "?org="+f.org.Slug)
	c, err = f.svc.Resolve(ctx, f.sessionToken())
	must(t, err)
	if _, err := f.svc.Switch(ctx, c, other.Slug); err != nil {
		t.Errorf("instance session: %v", err)
	}
	// A gone session has nothing to move.
	gone := mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})}
	if _, err := f.svc.Switch(ctx, gone, other.Slug); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("gone session = %v", err)
	}
}

// An invitation of one organization is not accepted through the connection
// of another, even for an email that connection proves.
func TestOrganizationConnectionOpensOnlyItsInvitations(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	c := domain.Connection{AccountID: other.ID, Kind: domain.ConnectionOIDC, Name: "other", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv, Enabled: true}
	must(t, f.st.Connections().Create(ctx, &c))
	f.verifyDomain(other.ID, "other.example")
	token := f.invite(domain.Invitation{Email: "bea@other.example"})
	f.claims("sub-1", "bea@other.example", true)
	if status, body := f.signIn(c, "?invite="+token); status != http.StatusForbidden || !strings.Contains(body, "someone else") {
		t.Errorf("sign-in = %d %s", status, body)
	}
}

func TestOrganizationChoice(t *testing.T) {
	f := newFixture(t)
	u := f.member("ana@example.com")
	second := domain.Account{Name: "Second"}
	must(t, f.st.Accounts().Create(t.Context(), &second))
	must(t, f.st.Memberships().Create(t.Context(), &domain.Membership{AccountID: second.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	f.claims("sub-1", "ana@example.com", true)

	if status, _ := f.signIn(f.inst, ""); status != http.StatusOK {
		t.Fatal("no choice")
	}
	c, err := f.svc.Resolve(t.Context(), f.sessionToken())
	must(t, err)
	if c.Actor.AccountID != uuid.Nil() {
		t.Errorf("two organizations, none picked: %s", c.Actor.AccountID)
	}
	_, orgs, err := f.svc.Profile(t.Context(), mcp.Caller{Actor: domain.UserActor(f.org.ID, u.ID, domain.Credential{})})
	must(t, err)
	if len(orgs) != 2 {
		t.Errorf("organizations = %+v", orgs)
	}

	f.newBrowser()
	if status, _ := f.signIn(f.inst, "?org="+second.Slug); status != http.StatusOK {
		t.Fatal("with slug")
	}
	if c, err = f.svc.Resolve(t.Context(), f.sessionToken()); err != nil || c.Actor.AccountID != second.ID {
		t.Errorf("caller = %+v, %v", c, err)
	}

	f.newBrowser()
	if status, _ := f.signIn(f.inst, "?org=nowhere"); status != http.StatusForbidden {
		t.Errorf("unknown organization = %d", status)
	}
}

func TestDiscover(t *testing.T) {
	f := newFixture(t)
	conn := f.orgConnection()
	ctx := t.Context()
	names := func(d auth.Discovery) string {
		out := []string{}
		for _, c := range d.Connections {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	d, err := f.svc.Discover(ctx, auth.DiscoverInput{})
	must(t, err)
	if strings.Contains(names(d), "okta") || len(d.Connections) != 2 || d.Redirect != "" || d.TokenLogin {
		t.Errorf("instance only = %+v", d)
	}
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Org: f.org.Slug})
	must(t, err)
	if !strings.Contains(names(d), "okta") || !strings.Contains(d.Connections[2].LoginURL, "org="+f.org.Slug) {
		t.Errorf("by slug = %+v", d)
	}
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Org: "nowhere"})
	must(t, err)
	if strings.Contains(names(d), "okta") {
		t.Errorf("unknown slug = %+v", d)
	}
	token := f.invite(domain.Invitation{Email: "bea@example.com"})
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Invite: token})
	must(t, err)
	if !strings.Contains(names(d), "okta") || !strings.Contains(d.Connections[0].LoginURL, "invite=") {
		t.Errorf("by invitation = %+v", d)
	}
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Invite: "fci_nope"})
	must(t, err)
	if strings.Contains(names(d), "okta") {
		t.Errorf("unknown invitation = %+v", d)
	}

	v := f.verifyDomain(f.org.ID, "acme.example")
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Email: "ana@ACME.example"})
	must(t, err)
	if !strings.Contains(names(d), "okta") || d.Redirect != "" {
		t.Errorf("verified domain = %+v", d)
	}
	must(t, f.st.VerifiedDomains().SetEnforcement(ctx, f.org.ID, v.ID, true, conn.ID))
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Email: "ana@acme.example"})
	must(t, err)
	if d.Redirect != "/auth/"+conn.ID.String()+"/login" {
		t.Errorf("enforced = %+v", d)
	}
	d, err = f.svc.Discover(ctx, auth.DiscoverInput{Email: "ana@elsewhere.example"})
	must(t, err)
	if d.Redirect != "" || len(d.Connections) != 2 {
		t.Errorf("other domain = %+v", d)
	}
}

func TestStart(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	stale := domain.Connection{Kind: domain.ConnectionGitHub, Name: "removed", ClientID: "x", ClientSecretRef: "env:X", Enabled: true}
	must(t, f.st.Connections().UpsertInstance(ctx, &stale))
	legacy := domain.MCPToken{AccountID: f.org.ID, Name: "laptop", TokenHash: mcp.HashToken("fcm_legacy")}
	must(t, f.st.MCPTokens().Create(ctx, &legacy))

	// No Owner yet: the token waits.
	must(t, f.svc.Start(ctx))
	got, err := f.st.Connections().Get(ctx, stale.ID)
	must(t, err)
	if got.Enabled {
		t.Error("a connection gone from the file is still on")
	}
	if tok, _ := f.st.MCPTokens().GetByTokenHash(ctx, legacy.TokenHash); tok.OwnerID != uuid.Nil() {
		t.Fatalf("assigned without an Owner: %+v", tok)
	}

	u := f.member("owner@example.com")
	role, err := f.st.Roles().GetByName(ctx, f.org.ID, domain.RoleScopeOrganization, domain.RoleOwner)
	must(t, err)
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.org.ID, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: role.ID, CreatedBy: "user:x"}))
	must(t, f.svc.Start(ctx))
	tok, err := f.st.MCPTokens().GetByTokenHash(ctx, legacy.TokenHash)
	must(t, err)
	if tok.OwnerKind != domain.SubjectUser || tok.OwnerID != u.ID {
		t.Fatalf("token = %+v", tok)
	}
	entries, err := f.st.Audit().List(ctx, f.org.ID, store.AuditFilter{})
	must(t, err)
	if len(entries) == 0 || entries[0].Action != "token.assigned" {
		t.Errorf("audit = %+v", entries)
	}
}

// A change to an instance connection is audited in every organization; a
// start that changes nothing records nothing.
func TestStartAuditsConnections(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	actions := func() map[string]int {
		entries, err := f.st.Audit().List(ctx, f.org.ID, store.AuditFilter{})
		must(t, err)
		out := map[string]int{}
		for _, e := range entries {
			out[e.Action]++
		}
		return out
	}
	if got := actions(); got["connection.created"] != 2 {
		t.Fatalf("audit = %v", got)
	}
	must(t, f.svc.Start(ctx))
	if got := actions(); got["connection.created"] != 2 || got["connection.updated"] != 0 {
		t.Fatalf("audit after a quiet start = %v", got)
	}
	cfg := f.cfg
	cfg.Connections = map[string]config.Connection{"oidc": {Kind: domain.ConnectionOIDC, Issuer: f.oidc.URL, ClientID: "other", ClientSecretRef: "env:" + secretEnv}}
	must(t, auth.New(auth.Config{Store: f.st, Auth: cfg, Now: f.clock.Now}).Start(ctx))
	if got := actions(); got["connection.updated"] != 1 || got["connection.disabled"] != 1 {
		t.Errorf("audit = %v", got)
	}
}

// Joining is audited, membership and roles.
func TestInvitationAudited(t *testing.T) {
	f := newFixture(t)
	token := f.invite(domain.Invitation{Email: "bea@example.com"})
	f.claims("sub-2", "bea@example.com", true)
	if status, body := f.signIn(f.inst, "?invite="+token); status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	entries, err := f.st.Audit().List(t.Context(), f.org.ID, store.AuditFilter{})
	must(t, err)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Action] = true
	}
	for _, want := range []string{"membership.created", "binding.created", "invitation.accepted", "session.created"} {
		if !got[want] {
			t.Errorf("no %s in %v", want, got)
		}
	}
}

// /me of a session from an organization's connection shows only that one.
func TestProfileOfOrganizationSession(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := f.member("ana@acme.example")
	other := domain.Account{Name: "Other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	must(t, f.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	conn := f.orgConnection()
	f.verifyDomain(f.org.ID, "acme.example")
	f.claims("sub-1", "ana@acme.example", true)
	if status, _ := f.signIn(conn, ""); status != http.StatusOK {
		t.Fatal("sign-in")
	}
	c, err := f.svc.Resolve(ctx, f.sessionToken())
	must(t, err)
	_, orgs, err := f.svc.Profile(ctx, c)
	must(t, err)
	if len(orgs) != 1 || orgs[0].ID != f.org.ID {
		t.Errorf("organizations = %+v", orgs)
	}
	f.newBrowser()
	f.claims("sub-2", "ana@acme.example", true)
	f.signIn(f.inst, "?org="+f.org.Slug)
	c, err = f.svc.Resolve(ctx, f.sessionToken())
	must(t, err)
	if _, orgs, err = f.svc.Profile(ctx, c); err != nil || len(orgs) != 2 {
		t.Errorf("organizations of an instance session = %+v, %v", orgs, err)
	}
}

func TestSweep(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	f.signIn(f.inst, "")
	f.clock.Advance(48 * time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		f.svc.RunSweep(ctx, time.Millisecond)
		close(done)
	}()
	for {
		if _, err := f.st.Sessions().GetByTokenHash(t.Context(), mcp.HashToken(f.sessionToken())); errors.Is(err, store.ErrNotFound) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// Nothing secret reaches the log, on success or on any refusal.
func TestNoSecretsInLogs(t *testing.T) {
	f := newFixture(t)
	f.member("ana@example.com")
	f.claims("sub-1", "ana@example.com", true)
	var code string
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			code = req.URL.Query().Get("code")
		}
		return nil
	}
	f.signIn(f.inst, "")
	session := f.sessionToken()

	f.newBrowser()
	f.oidc.Claims["nonce"] = "other"
	f.signIn(f.inst, "")

	f.newBrowser()
	f.claims("sub-9", "stranger@example.com", true)
	invite := f.invite(domain.Invitation{Email: "bea@example.com"})
	f.signIn(f.inst, "?invite="+invite)

	f.newBrowser()
	f.github.ID, f.github.Login, f.github.Email = 5, "x", "x@example.com"
	f.signIn(f.gh, "")

	// A failed token exchange: the code was spent.
	f.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/callback") {
			q := req.URL.Query()
			q.Set("code", code)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	f.newBrowser2KeepRedirect()
	f.signIn(f.inst, "")

	logs := f.logs.String()
	if !strings.Contains(logs, "sign-in refused") {
		t.Fatalf("nothing logged: %s", logs)
	}
	secrets := append([]string{clientSecret, code, session, invite}, f.oidc.IDTokens...)
	secrets = append(secrets, f.github.AccessTokens...)
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			t.Errorf("log holds %q:\n%s", s, logs)
		}
	}
}

// newBrowser2KeepRedirect clears the cookies but keeps the redirect hook.
func (f *fixture) newBrowser2KeepRedirect() {
	hook := f.browser.CheckRedirect
	f.newBrowser()
	f.browser.CheckRedirect = hook
}

// A connection is tested before it is enabled: an administrator of its
// organization signs in through it, sees what the provider says, nobody is
// signed in, and the test is audited.
func TestConnectionTestSignIn(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.member("ana@acme.example")
	testkit.Bind(t, f.st, f.org.ID, domain.SubjectUser, admin.ID, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	plain := f.member("pia@acme.example")
	conn := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv}
	must(t, f.st.Connections().Create(ctx, &conn))

	// Nobody is signed in: refused, and a disabled connection is no sign-in.
	if status, _ := f.signIn(conn, "?test=1"); status != http.StatusForbidden {
		t.Fatalf("test without a session = %d", status)
	}
	if status, _ := f.signIn(conn, ""); status != http.StatusNotFound {
		t.Fatalf("a disabled connection signs in = %d", status)
	}

	// An administrator signs in through the instance connection first.
	f.claims("sub-admin", "ana@acme.example", true)
	f.signIn(f.inst, "")
	session := f.sessionToken()
	f.claims("sub-test", "someone@okta.example", true)
	status, body := f.signIn(conn, "?test=1")
	if status != http.StatusOK || !strings.Contains(body, "Sign-in works") || !strings.Contains(body, "someone@okta.example") {
		t.Fatalf("test = %d %s", status, body)
	}
	if f.sessionToken() != session {
		t.Error("the test changed the session")
	}
	if _, err := f.st.Identities().GetBySubject(ctx, conn.ID, "sub-test"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the test linked an identity: %v", err)
	}
	entries, err := f.st.Audit().List(ctx, f.org.ID, store.AuditFilter{Action: "connection.tested"})
	if err != nil || len(entries) != 1 || entries[0].SubjectID != conn.ID || entries[0].Actor != "user:"+admin.ID.String() {
		t.Fatalf("audit = %+v, %v", entries, err)
	}

	// A member without connection.manage, and a connection of the instance, are refused.
	f.newBrowser()
	f.claims("sub-plain", "pia@acme.example", true)
	f.signIn(f.inst, "")
	if status, _ := f.signIn(conn, "?test=1"); status != http.StatusForbidden {
		t.Errorf("test by a member = %d", status)
	}
	_ = plain
	f.newBrowser()
	f.claims("sub-admin", "ana@acme.example", true)
	f.signIn(f.inst, "")
	if status, _ := f.signIn(f.inst, "?test=1"); status != http.StatusForbidden {
		t.Errorf("test of an instance connection = %d", status)
	}
}

// After signing in the person goes back to the page they came from, if it is
// a page of this site.
func TestSignInReturnsToThePage(t *testing.T) {
	for redirect, want := range map[string]string{"/inbox?project=app": "/inbox", "//evil.example/x": "/", "https://evil.example/": "/", "/\\evil.example": "/", "/\t/evil.example": "/", "/\n/evil.example": "/", "/a\\b": "/", "/a\x00b": "/", "": "/"} {
		f := newFixture(t)
		f.member("ana@acme.example")
		f.claims("sub-1", "ana@acme.example", true)
		resp, err := f.browser.Get(f.web.URL + "/auth/" + f.inst.ID.String() + "/login?redirect=" + url.QueryEscape(redirect))
		must(t, err)
		_ = resp.Body.Close()
		if resp.Request.URL.Path != want || resp.Request.URL.Host != strings.TrimPrefix(f.web.URL, "https://") {
			t.Errorf("redirect %q ended at %s, want %s on this site", redirect, resp.Request.URL, want)
		}
	}
}

// The cookie of a flow is not signed: whatever it says is checked again when
// the sign-in ends.
func TestForgedReturnInTheFlowCookie(t *testing.T) {
	for _, forged := range []string{"//evil.example/x", "/\t/evil.example", "https://evil.example/"} {
		f := newFixture(t)
		f.member("ana@acme.example")
		f.claims("sub-1", "ana@acme.example", true)
		next := f.startFlow("/auth/"+f.inst.ID.String()+"/login", func(flow map[string]any) { flow["r"] = forged })
		resp, err := f.browser.Get(next)
		must(t, err)
		_ = resp.Body.Close()
		if resp.Request.URL.Host != strings.TrimPrefix(f.web.URL, "https://") || resp.Request.URL.Path != "/" {
			t.Errorf("forged %q ended at %s", forged, resp.Request.URL)
		}
	}
}

// startFlow begins a sign-in without following it to the provider: it returns
// the address of the provider, and edit may change the flow cookie first.
func (f *fixture) startFlow(path string, edit func(flow map[string]any)) string {
	f.t.Helper()
	f.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { f.browser.CheckRedirect = nil }()
	resp, err := f.browser.Get(f.web.URL + path)
	must(f.t, err)
	_ = resp.Body.Close()
	u, _ := url.Parse(f.web.URL + "/auth/x")
	for _, c := range f.browser.Jar.Cookies(u) {
		if c.Name != "forecrew_login" || edit == nil {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(c.Value)
		must(f.t, err)
		var flow map[string]any
		must(f.t, json.Unmarshal(raw, &flow))
		edit(flow)
		raw, err = json.Marshal(flow)
		must(f.t, err)
		c.Value, c.Path, c.Secure = base64.RawURLEncoding.EncodeToString(raw), "/auth/", true
		f.browser.Jar.SetCookies(u, []*http.Cookie{c})
	}
	return resp.Header.Get("Location")
}

// Who tests a connection is what the server remembers: a cookie that says it
// is a test, or names a tester, makes nothing a test.
func TestForgedTestFlow(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	plain := f.member("pia@acme.example")
	conn := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv, Enabled: true}
	must(t, f.st.Connections().Create(ctx, &conn))
	f.claims("sub-forged", "pia@acme.example", true)
	next := f.startFlow("/auth/"+conn.ID.String()+"/login", func(flow map[string]any) { flow["t"] = "1" })
	resp, err := f.browser.Get(next)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a forged test = %d", resp.StatusCode)
	}
	entries, _ := f.st.Audit().List(ctx, f.org.ID, store.AuditFilter{Action: "connection.tested"})
	if len(entries) != 0 || f.sessionToken() != "" {
		t.Errorf("audit %+v, session %q", entries, f.sessionToken())
	}
	_ = plain
}

// A tester who lost connection.manage while the test was on the provider's
// page cannot finish it.
func TestTesterLosesTheRightMidTest(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.member("ana@acme.example")
	b := testkit.Bind(t, f.st, f.org.ID, domain.SubjectUser, admin.ID, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	conn := domain.Connection{AccountID: f.org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: f.oidc.URL, ClientID: clientID, ClientSecretRef: "env:" + secretEnv}
	must(t, f.st.Connections().Create(ctx, &conn))
	f.claims("sub-admin", "ana@acme.example", true)
	f.signIn(f.inst, "")
	f.claims("sub-test", "someone@okta.example", true)
	next := f.startFlow("/auth/"+conn.ID.String()+"/login?test=1", nil)
	must(t, f.st.RoleBindings().Delete(ctx, f.org.ID, b.ID))
	resp, err := f.browser.Get(next)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the test of someone who lost the right = %d", resp.StatusCode)
	}
	if entries, _ := f.st.Audit().List(ctx, f.org.ID, store.AuditFilter{Action: "connection.tested"}); len(entries) != 0 {
		t.Errorf("audit = %+v", entries)
	}
}

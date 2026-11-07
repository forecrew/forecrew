package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// sessionEnv is the API with sessions, and a member with a session.
type sessionEnv struct {
	st     *testkit.Store
	clock  *testkit.Clock
	svc    *testkit.ToolService
	server *httptest.Server
	org    domain.Account
	user   domain.User
	cookie string
}

func newSessionEnv(t *testing.T) *sessionEnv {
	t.Helper()
	clock := testkit.NewClock(t0)
	e := &sessionEnv{st: testkit.NewStore(clock.Now), clock: clock, svc: &testkit.ToolService{}, org: domain.Account{Name: "Acme"}}
	ctx := t.Context()
	must(t, e.st.Accounts().Create(ctx, &e.org))
	e.user = domain.User{Name: "Ana", Email: "ana@example.com", EmailVerified: true}
	must(t, e.st.Users().Create(ctx, &e.user))
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: e.org.ID, UserID: e.user.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	e.cookie = e.session(t, e.org)
	svc := auth.New(auth.Config{Store: e.st, Auth: config.Auth{SessionIdle: time.Hour, SessionAbsolute: 24 * time.Hour}, Now: clock.Now})
	logger := slog.New(slog.DiscardHandler)
	e.server = httptest.NewServer(api.New(api.Config{Tools: e.svc, Store: e.st, Authz: authz.New(e.st, nil), Auth: svc, Logger: logger, PublicURL: "https://forecrew.example.com", Now: clock.Now}).Handler())
	t.Cleanup(e.server.Close)
	return e
}

// session stores a session of the user in the organization and returns its
// token.
func (e *sessionEnv) session(t *testing.T, org domain.Account) string {
	t.Helper()
	token, err := auth.NewToken(auth.PrefixSession)
	must(t, err)
	must(t, e.st.Sessions().Create(t.Context(), &domain.Session{TokenHash: mcp.HashToken(token), UserID: e.user.ID, AccountID: org.ID,
		IdleExpiresAt: t0.Add(time.Hour), ExpiresAt: t0.Add(24 * time.Hour)}))
	return token
}

func (e *sessionEnv) do(t *testing.T, method, path, body, cookie string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.server.URL+path, strings.NewReader(body))
	must(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: api.CookieName, Value: cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { must(t, resp.Body.Close()) }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

func TestSessionCookie(t *testing.T) {
	e := newSessionEnv(t)
	resp, body := e.do(t, "GET", "/api/v1/me", "", e.cookie, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me = %d %s", resp.StatusCode, body)
	}
	var s api.Session
	must(t, json.Unmarshal([]byte(body), &s))
	if s.User == nil || s.User.ID != e.user.ID || s.Account.ID != e.org.ID || len(s.Organizations) != 1 || s.Token != "" {
		t.Errorf("me = %s", body)
	}
	if resp, body := e.do(t, "POST", "/api/v1/gates/GATE-1/approve", `{}`, e.cookie, nil); resp.StatusCode != 200 {
		t.Fatalf("approve = %d %s", resp.StatusCode, body)
	}
	calls := e.svc.Calls()
	if id, _ := calls[0].Actor.SubjectID(); len(calls) != 1 || id != e.user.ID || calls[0].Actor.Credential.Kind != domain.CredentialSession {
		t.Errorf("calls = %+v", calls)
	}
}

func TestDeadSessionCookieIsCleared(t *testing.T) {
	e := newSessionEnv(t)
	e.clock.Advance(2 * time.Hour)
	resp, _ := e.do(t, "GET", "/api/v1/me", "", e.cookie, nil)
	if resp.StatusCode != 401 || len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge >= 0 {
		t.Errorf("expired = %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
}

func TestCookieRules(t *testing.T) {
	e := newSessionEnv(t)
	tests := []struct {
		name   string
		body   string
		header map[string]string
		status int
		code   string
	}{
		{name: "not JSON", header: map[string]string{"Content-Type": "text/plain"}, status: 415, code: "unsupported_media_type"},
		{name: "other site", body: "{}", header: map[string]string{"Origin": "https://evil.example"}, status: 403, code: "bad_origin"},
		{name: "null origin", body: "{}", header: map[string]string{"Origin": "null"}, status: 403, code: "bad_origin"},
		{name: "public origin", body: "{}", header: map[string]string{"Origin": "https://forecrew.example.com"}, status: 200},
		{name: "same host", body: "{}", header: map[string]string{"Origin": "PLACEHOLDER"}, status: 200},
		{name: "http on the public host", body: "{}", header: map[string]string{"Origin": "http://forecrew.example.com"}, status: 403, code: "bad_origin"},
		{name: "no origin", body: "{}", status: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.header["Origin"] == "PLACEHOLDER" {
				tt.header["Origin"] = e.server.URL
			}
			resp, body := e.do(t, "POST", "/api/v1/gates/GATE-1/approve", tt.body, e.cookie, tt.header)
			if resp.StatusCode != tt.status || !strings.Contains(body, tt.code) {
				t.Errorf("= %d %s", resp.StatusCode, body)
			}
		})
	}
	// A bearer request is not one a browser sends on its own.
	if resp, _ := e.do(t, "GET", "/api/v1/me", "", "", map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 401 {
		t.Errorf("no credential = %d", resp.StatusCode)
	}
}

func TestSignOutEndpoints(t *testing.T) {
	e := newSessionEnv(t)
	if resp, _ := e.do(t, "DELETE", "/api/v1/session", "", e.cookie, nil); resp.StatusCode != 415 {
		t.Errorf("sign out without JSON = %d", resp.StatusCode)
	}
	resp, _ := e.do(t, "DELETE", "/api/v1/session", "{}", e.cookie, nil)
	if resp.StatusCode != 204 || len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("sign out = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", "", e.cookie, nil); resp.StatusCode != 401 {
		t.Errorf("after sign out = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/session", "", "", nil); resp.StatusCode != 204 {
		t.Errorf("sign out without a cookie = %d", resp.StatusCode)
	}

	one, two := e.session(t, e.org), e.session(t, e.org)
	if resp, _ := e.do(t, "DELETE", "/api/v1/sessions", "{}", one, nil); resp.StatusCode != 204 {
		t.Fatalf("everywhere = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", "", two, nil); resp.StatusCode != 401 {
		t.Errorf("other session = %d", resp.StatusCode)
	}
}

func TestSwitchOrganization(t *testing.T) {
	e := newSessionEnv(t)
	ctx := t.Context()
	other := domain.Account{Name: "Other"}
	must(t, e.st.Accounts().Create(ctx, &other))
	resp, body := e.do(t, "PUT", "/api/v1/session/organization", `{"organization":"`+other.Slug+`"}`, e.cookie, nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "forbidden") {
		t.Fatalf("not a member = %d %s", resp.StatusCode, body)
	}
	must(t, e.st.Memberships().Create(ctx, &domain.Membership{AccountID: other.ID, UserID: e.user.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	resp, body = e.do(t, "PUT", "/api/v1/session/organization", `{"organization":"`+other.Slug+`"}`, e.cookie, nil)
	var s api.Session
	must(t, json.Unmarshal([]byte(body), &s))
	if resp.StatusCode != 200 || s.Account.ID != other.ID || len(s.Organizations) != 2 {
		t.Fatalf("switch = %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "PUT", "/api/v1/session/organization", `{"nope":1}`, e.cookie, nil); resp.StatusCode != 400 {
		t.Errorf("bad body = %d", resp.StatusCode)
	}

	tok := domain.MCPToken{AccountID: e.org.ID, Name: "t", TokenHash: mcp.HashToken("fcm_x"), Scopes: []domain.Scope{domain.ScopeAdmin}}
	must(t, e.st.MCPTokens().Create(ctx, &tok))
	bearer := map[string]string{"Authorization": "Bearer fcm_x"}
	if resp, _ := e.do(t, "PUT", "/api/v1/session/organization", `{"organization":"`+other.Slug+`"}`, "", bearer); resp.StatusCode != 400 {
		t.Errorf("token switch = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/sessions", "{}", "", bearer); resp.StatusCode != 400 {
		t.Errorf("ownerless token everywhere = %d", resp.StatusCode)
	}
}

func TestSessionWithoutOrganization(t *testing.T) {
	e := newSessionEnv(t)
	cookie := e.session(t, domain.Account{})
	resp, body := e.do(t, "GET", "/api/v1/me", "", cookie, nil)
	var s api.Session
	must(t, json.Unmarshal([]byte(body), &s))
	if resp.StatusCode != 200 || strings.Contains(body, `"account"`) || s.User == nil {
		t.Errorf("me = %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/api/v1/projects", "", cookie, nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "no_organization") {
		t.Errorf("projects = %d %s", resp.StatusCode, body)
	}
}

func TestTokenLoginOffByDefault(t *testing.T) {
	e := newSessionEnv(t)
	tok := domain.MCPToken{AccountID: e.org.ID, Name: "t", TokenHash: mcp.HashToken("fcm_x")}
	must(t, e.st.MCPTokens().Create(t.Context(), &tok))
	resp, body := e.do(t, "POST", "/api/v1/session", `{"token":"fcm_x"}`, "", nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "token_login_disabled") || len(resp.Cookies()) != 0 {
		t.Errorf("login = %d %s", resp.StatusCode, body)
	}
	// A cookie holding a token is no session without token_login.
	if resp, _ := e.do(t, "GET", "/api/v1/me", "", "fcm_x", nil); resp.StatusCode != 401 {
		t.Errorf("token in a cookie = %d", resp.StatusCode)
	}
}

func TestDiscoverEndpoint(t *testing.T) {
	e := newSessionEnv(t)
	c := domain.Connection{Kind: domain.ConnectionGitHub, Name: "github", ClientID: "secret-client", ClientSecretRef: "env:X", Enabled: true}
	must(t, e.st.Connections().UpsertInstance(t.Context(), &c))
	resp, body := e.do(t, "POST", "/api/v1/auth/discover", `{"org":"acme"}`, "", nil)
	var d auth.Discovery
	must(t, json.Unmarshal([]byte(body), &d))
	if resp.StatusCode != 200 || len(d.Connections) != 1 || d.Connections[0].Kind != domain.ConnectionGitHub || strings.Contains(body, "secret-client") || strings.Contains(body, "env:X") {
		t.Errorf("discover = %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/discover", `{"nope":1}`, "", nil); resp.StatusCode != 400 {
		t.Errorf("bad body = %d", resp.StatusCode)
	}
}

func TestNoSessionService(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, e.server.URL+"/api/v1/me", nil)
	req.RequestURI = ""
	req.AddCookie(&http.Cookie{Name: api.CookieName, Value: "fcs_x"})
	if r := e.send(t, req); r.status != 401 {
		t.Errorf("session without a service = %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/auth/discover", `{}`, ""); r.status != 200 || !strings.Contains(r.body, `"connections":[]`) {
		t.Errorf("discover = %d %s", r.status, r.body)
	}
}

// TestApplyFromASessionIsFromTheBrowser: a browser session is how a person
// confirms a plan a model wrote.
func TestApplyFromASessionIsFromTheBrowser(t *testing.T) {
	e := newSessionEnv(t)
	testkit.Bind(t, e.st, e.org.ID, domain.SubjectUser, e.user.ID, domain.RoleScopeOrganization, domain.RoleOwner, uuid.Nil())
	var got mcp.ConfigApplyInput
	e.svc.ConfigApplyFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
		got = in
		return mcp.ConfigApplied{}, nil
	}
	resp, body := e.do(t, http.MethodPost, "/api/v1/config/plans/p/apply", `{"word":"acme"}`, e.cookie, nil)
	if resp.StatusCode != http.StatusOK || !got.Browser || got.Word != "acme" {
		t.Fatalf("%d %s: %+v", resp.StatusCode, body, got)
	}
}

// TestDiscoverIsRateLimited lets one address only so far, per window, without
// touching another address.
func TestDiscoverIsRateLimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := api.New(api.Config{Tools: &testkit.ToolService{}, Store: testkit.NewStore(func() time.Time { return now }), Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }})
	h := s.Handler()
	call := func(addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/discover", strings.NewReader(`{}`))
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := range 120 {
		if w := call("192.0.2.1:1000"); w.Code != http.StatusOK {
			t.Fatalf("call %d = %d", i, w.Code)
		}
	}
	w := call("192.0.2.1:1001")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("over the limit = %d %s", w.Code, w.Body)
	}
	if w := call("192.0.2.2:1000"); w.Code != http.StatusOK {
		t.Fatalf("another address = %d", w.Code)
	}
	now = now.Add(time.Minute)
	if w := call("192.0.2.1:1000"); w.Code != http.StatusOK {
		t.Fatalf("after the window = %d", w.Code)
	}
}

// Under SSO enforcement a token proves no connection: token login is refused
// and a cookie from an earlier one dies, while the token stays a bearer token.
func TestTokenLoginUnderEnforcement(t *testing.T) {
	ctx := t.Context()
	st := testkit.NewStore(time.Now)
	org := domain.Account{Name: "Acme"}
	must(t, st.Accounts().Create(ctx, &org))
	u := domain.User{Name: "Ana", Email: "ana@acme.example", EmailVerified: true}
	must(t, st.Users().Create(ctx, &u))
	must(t, st.Memberships().Create(ctx, &domain.Membership{AccountID: org.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	must(t, st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: org.ID, Name: "t", TokenHash: mcp.HashToken("fcm_ana"), OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeRead}}))
	server := httptest.NewServer(api.New(api.Config{Tools: &testkit.ToolService{}, Store: st, Authz: authz.New(st, nil), Logger: slog.New(slog.DiscardHandler), TokenLogin: true}).Handler())
	t.Cleanup(server.Close)
	call := func(method, path, body, cookie, bearer string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		must(t, err)
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: api.CookieName, Value: cookie})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		must(t, err)
		return resp.StatusCode, string(b)
	}
	login := func() (int, string) { return call(http.MethodPost, "/api/v1/session", `{"token":"fcm_ana"}`, "", "") }
	if status, body := login(); status != http.StatusOK {
		t.Fatalf("before enforcement = %d %s", status, body)
	}
	if status, _ := call(http.MethodGet, "/api/v1/me", "", "fcm_ana", ""); status != http.StatusOK {
		t.Fatalf("cookie before enforcement = %d", status)
	}

	d := domain.VerifiedDomain{AccountID: org.ID, Domain: "acme.example", VerificationToken: "t"}
	must(t, st.VerifiedDomains().Create(ctx, &d))
	must(t, st.VerifiedDomains().SetVerified(ctx, org.ID, d.ID, true))
	conn := domain.Connection{AccountID: org.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://idp.example", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_X", Enabled: true}
	must(t, st.Connections().Create(ctx, &conn))
	must(t, st.VerifiedDomains().SetEnforcement(ctx, org.ID, d.ID, true, conn.ID))

	if status, body := login(); status != http.StatusForbidden || !strings.Contains(body, "sso_required") {
		t.Fatalf("login = %d %s", status, body)
	}
	if status, _ := call(http.MethodGet, "/api/v1/me", "", "fcm_ana", ""); status != http.StatusUnauthorized {
		t.Fatalf("old cookie = %d", status)
	}
	if status, _ := call(http.MethodGet, "/api/v1/me", "", "", "fcm_ana"); status != http.StatusOK {
		t.Fatalf("bearer = %d", status)
	}

	// An instance admin is exempt, and the login is audited.
	u.InstanceAdmin = true
	must(t, st.Users().Update(ctx, &u))
	if status, body := login(); status != http.StatusOK {
		t.Fatalf("instance admin = %d %s", status, body)
	}
	if status, _ := call(http.MethodGet, "/api/v1/me", "", "fcm_ana", ""); status != http.StatusOK {
		t.Fatalf("instance admin cookie = %d", status)
	}
	entries, err := st.Audit().List(ctx, org.ID, store.AuditFilter{})
	must(t, err)
	if !slices.ContainsFunc(entries, func(e domain.AuditEntry) bool { return e.Action == "sso.exempted" }) {
		t.Errorf("audit = %+v", entries)
	}
}

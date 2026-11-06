package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

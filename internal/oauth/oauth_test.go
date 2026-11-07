package oauth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/oauth"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const (
	verifier = "a-verifier-that-is-long-enough-to-be-a-real-pkce-verifier-0123456789"
	callback = "https://app.example/callback"
	cookie   = "fcs_test-session"
)

func challenge(v string) string {
	h := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// documents answers client ID metadata document fetches.
type documents struct {
	mu   sync.Mutex
	docs map[string]string
}

func (d *documents) set(url, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs[url] = body
}

func (d *documents) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	body, ok := d.docs[r.URL.String()]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// env is forecrewd's OAuth and MCP endpoints with a member of two
// organizations who has a session.
type env struct {
	st     *testkit.Store
	tools  *testkit.ToolService
	server *oauth.Server
	base   string
	http   *http.Client
	docs   *documents
	user   domain.User
	a, b   domain.Account
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	st := testkit.NewStore(nil)
	e := &env{st: st, tools: &testkit.ToolService{}, docs: &documents{docs: map[string]string{}}, now: time.Now()}
	e.a, e.b = domain.Account{Name: "Acme", Slug: "acme"}, domain.Account{Name: "Bravo", Slug: "bravo"}
	must(t, st.Accounts().Create(ctx, &e.a))
	must(t, st.Accounts().Create(ctx, &e.b))
	e.user = st.AddMember(e.a.ID, domain.User{Name: "Ana"}, "")
	m := domain.Membership{AccountID: e.b.ID, UserID: e.user.ID, Status: domain.MembershipActive, Source: domain.SourceManual}
	must(t, st.Memberships().Create(ctx, &m))
	testkit.Bind(t, st, e.a.ID, domain.SubjectUser, e.user.ID, domain.RoleScopeOrganization, domain.RoleAdmin, uuid.Nil())
	e.session(t, cookie, e.user.ID)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	e.base = "http://" + l.Addr().String()
	logger := slog.New(slog.DiscardHandler)
	signIn := auth.New(auth.Config{Store: st, BaseURL: e.base, Logger: logger})
	e.server = oauth.New(oauth.Config{Store: st, Auth: signIn, BaseURL: e.base, Logger: logger, Now: func() time.Time { return e.now }, HTTP: &http.Client{Transport: e.docs}})
	mux := http.NewServeMux()
	h := e.server.Handler()
	for _, p := range oauth.Paths() {
		mux.Handle(p, h)
	}
	mux.Handle("/mcp", mcp.NewHandler(e.tools, st, authz.New(st, nil), "test", mcp.NewResource(e.base), logger))
	srv := httptest.NewUnstartedServer(mux)
	must(t, srv.Listener.Close())
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	e.http = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) session(t *testing.T, token string, user uuid.UUID) {
	t.Helper()
	exp := time.Now().Add(time.Hour)
	must(t, e.st.Sessions().Create(t.Context(), &domain.Session{TokenHash: mcp.HashToken(token), UserID: user, IdleExpiresAt: exp, ExpiresAt: exp}))
}

// register registers a public client.
func (e *env) register(t *testing.T, uris ...string) string {
	t.Helper()
	status, out := e.postJSON(t, "/oauth/register", map[string]any{"client_name": "Scripted", "redirect_uris": uris, "token_endpoint_auth_method": "none"})
	if status != http.StatusCreated {
		t.Fatalf("register = %d %v", status, out)
	}
	return out["client_id"].(string)
}

func (e *env) postJSON(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+path, bytes.NewReader(raw))
	must(t, err)
	return e.answer(t, req)
}

func (e *env) answer(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	res, err := e.http.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	out := map[string]any{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("status %d, not JSON: %v", res.StatusCode, err)
	}
	return res.StatusCode, out
}

// params is a good authorization request of client.
func (e *env) params(client string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {callback}, "response_type": {"code"}, "state": {"xyz"}, "scope": {"read write"},
		"resource": {e.base + "/mcp"}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}}
}

// get visits the authorization endpoint with the session cookie.
func (e *env) get(t *testing.T, q url.Values, sessionCookie string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.base+"/oauth/authorize?"+q.Encode(), nil)
	must(t, err)
	if sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionCookie})
	}
	res, err := e.http.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	return res, string(body)
}

var hiddenField = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// post answers the consent form of page.
func (e *env) post(t *testing.T, page string, change func(url.Values)) *http.Response {
	t.Helper()
	form := url.Values{"decision": {"allow"}}
	for _, m := range hiddenField.FindAllStringSubmatch(page, -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	if form.Get("organization") == "" {
		form.Set("organization", e.a.Slug)
	}
	if change != nil {
		change(form)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+"/oauth/authorize", strings.NewReader(form.Encode()))
	must(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	res, err := e.http.Do(req)
	must(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// code goes through consent and returns the authorization code.
func (e *env) code(t *testing.T, q url.Values, org string) string {
	t.Helper()
	res, page := e.get(t, q, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d: %s", res.StatusCode, page)
	}
	back := e.post(t, page, func(f url.Values) { f.Set("organization", org) })
	if back.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent = %d", back.StatusCode)
	}
	u, err := back.Location()
	must(t, err)
	if u.Query().Get("state") != q.Get("state") || u.Query().Get("iss") != e.base || u.Query().Get("code") == "" {
		t.Fatalf("redirect = %s", u)
	}
	return u.Query().Get("code")
}

func (e *env) token(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+"/oauth/token", strings.NewReader(form.Encode()))
	must(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.answer(t, req)
}

func (e *env) exchange(t *testing.T, client, code string) (int, map[string]any) {
	t.Helper()
	return e.token(t, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {callback}, "resource": {e.base + "/mcp"}})
}

// tokens goes through the whole flow and returns the answer of the token
// endpoint.
func (e *env) tokens(t *testing.T, client string) map[string]any {
	t.Helper()
	status, out := e.exchange(t, client, e.code(t, e.params(client), e.a.Slug))
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v", status, out)
	}
	return out
}

func (e *env) refresh(t *testing.T, client, token string) (int, map[string]any) {
	t.Helper()
	return e.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {token}, "resource": {e.base + "/mcp"}})
}

func wantError(t *testing.T, status int, out map[string]any, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || out["error"] != code {
		t.Fatalf("answer = %d %v, want %d %s", status, out, wantStatus, code)
	}
}

// bearerClient is the HTTP client of an MCP session with a fixed token.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// listProjects calls a tool on /mcp and reports whether the token got in.
func (e *env) listProjects(t *testing.T, token string) error {
	t.Helper()
	tr := &sdk.StreamableClientTransport{Endpoint: e.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "script", Version: "1"}, nil).Connect(t.Context(), tr, nil)
	if err != nil {
		return err
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: mcp.ToolListProjects, Arguments: map[string]any{}})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("tool error: %v", res.Content)
	}
	return nil
}

// status is the status of a bare call of /mcp with the token.
func (e *env) status(t *testing.T, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	must(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := e.http.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode, res.Header.Get("WWW-Authenticate")
}

// consentScreen checks what the screen says and that it cannot be framed.
func (e *env) consentScreen(t *testing.T, q url.Values) string {
	t.Helper()
	res, page := e.get(t, q, cookie)
	for _, want := range []string{"Scripted", "app.example", "Acme", "Bravo", "<select", "You have not connected this app before", "Read projects", "resolve gates"} {
		if !strings.Contains(page, want) {
			t.Errorf("the consent screen lacks %q", want)
		}
	}
	if res.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("consent can be framed: %v", res.Header)
	}
	return page
}

// checkIssued checks the rows of a first access and refresh token, in Bravo.
func (e *env) checkIssued(t *testing.T, access, refresh string) {
	t.Helper()
	g, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(access))
	must(t, err)
	if g.AccountID != e.b.ID || g.UserID != e.user.ID || g.Resource != e.base+"/mcp" || g.ExpiresAt.Sub(e.now) != time.Hour {
		t.Fatalf("grant = %+v", g)
	}
	r, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(refresh))
	must(t, err)
	if r.ExpiresAt.Sub(e.now) != 30*24*time.Hour || r.FamilyID != g.FamilyID {
		t.Fatalf("refresh = %+v", r)
	}
}

func (e *env) wantAudit(t *testing.T, account uuid.UUID, want ...string) {
	t.Helper()
	entries, err := e.st.Audit().List(t.Context(), account, store.AuditFilter{})
	must(t, err)
	for _, action := range want {
		if !slices.ContainsFunc(entries, func(x domain.AuditEntry) bool { return x.Action == action }) {
			t.Errorf("audit lacks %s: %+v", action, entries)
		}
	}
}

// discover follows the metadata from a 401, as a client does.
func (e *env) discover(t *testing.T) {
	t.Helper()
	status, challengeHeader := e.status(t, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d", status)
	}
	cs, err := oauthex.ParseWWWAuthenticate([]string{challengeHeader})
	must(t, err)
	resourceMeta := cs[0].Params["resource_metadata"]
	if resourceMeta != e.base+"/.well-known/oauth-protected-resource/mcp" || cs[0].Params["scope"] != "read write" {
		t.Fatalf("challenge = %+v", cs)
	}
	prm, err := oauthex.GetProtectedResourceMetadata(t.Context(), resourceMeta, e.base+"/mcp", http.DefaultClient)
	must(t, err)
	if len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != e.base {
		t.Fatalf("resource metadata = %+v", prm)
	}
	asm, err := sdkauth.GetAuthServerMetadata(t.Context(), e.base, http.DefaultClient)
	must(t, err)
	if asm.Issuer != e.base || asm.RegistrationEndpoint != e.base+"/oauth/register" || !asm.ClientIDMetadataDocumentSupported ||
		len(asm.CodeChallengeMethodsSupported) != 1 || asm.CodeChallengeMethodsSupported[0] != "S256" {
		t.Fatalf("server metadata = %+v", asm)
	}

}

// TestFullFlow is a client that knows only the URL: discovery from a 401,
// registration, consent with an organization choice, the code exchange, an MCP
// call, a refresh, and a reused refresh token ending the family.
func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	e.discover(t)
	client := e.register(t, callback)
	q := e.params(client)
	page := e.consentScreen(t, q)
	back := e.post(t, page, func(f url.Values) { f.Set("organization", e.b.Slug) })
	u, err := back.Location()
	must(t, err)
	if back.StatusCode != http.StatusSeeOther || !strings.HasPrefix(u.String(), callback+"?") {
		t.Fatalf("consent = %d %s", back.StatusCode, u)
	}
	status, out := e.exchange(t, client, u.Query().Get("code"))
	if status != http.StatusOK || out["token_type"] != "Bearer" || out["expires_in"] != float64(3600) || out["scope"] != "read write" {
		t.Fatalf("exchange = %d %v", status, out)
	}
	access, refresh := out["access_token"].(string), out["refresh_token"].(string)
	if !strings.HasPrefix(access, "fca_") || !strings.HasPrefix(refresh, "fcr_") || !strings.HasPrefix(u.Query().Get("code"), "fco_") {
		t.Fatalf("prefixes: %v", out)
	}
	e.checkIssued(t, access, refresh)

	// The member of Bravo holds no role there, so the grant reaches the tool
	// layer only to be refused by ToolAccess.
	if err := e.listProjects(t, access); err == nil {
		t.Error("a grant for an organization without roles listed projects")
	}
	testkit.Bind(t, e.st, e.b.ID, domain.SubjectUser, e.user.ID, domain.RoleScopeOrganization, domain.RoleViewer, uuid.Nil())
	must(t, e.listProjects(t, access))
	calls := e.tools.Calls()
	if last := calls[len(calls)-1]; last.Tool != mcp.ToolListProjects || last.Actor.AccountID != e.b.ID || last.Actor.Credential.Kind != domain.CredentialOAuthGrant {
		t.Fatalf("call = %+v", last)
	}

	status, out = e.refresh(t, client, refresh)
	if status != http.StatusOK || out["refresh_token"] == refresh {
		t.Fatalf("refresh = %d %v", status, out)
	}
	newAccess, newRefresh := out["access_token"].(string), out["refresh_token"].(string)
	must(t, e.listProjects(t, newAccess))

	status, out = e.refresh(t, client, refresh) // the old one again
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
	if s, _ := e.status(t, newAccess); s != http.StatusUnauthorized {
		t.Errorf("access token of a revoked family = %d", s)
	}
	status, out = e.refresh(t, client, newRefresh)
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
	e.wantAudit(t, e.b.ID, "oauth.authorized", "oauth.reuse_detected")
}

func TestPKCE(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	for name, change := range map[string]func(url.Values){
		"missing challenge": func(q url.Values) { q.Del("code_challenge") },
		"plain":             func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"no method":         func(q url.Values) { q.Del("code_challenge_method") },
		"short challenge":   func(q url.Values) { q.Set("code_challenge", "abc") },
	} {
		t.Run(name, func(t *testing.T) {
			q := e.params(client)
			change(q)
			res, _ := e.get(t, q, cookie)
			u, err := res.Location()
			must(t, err)
			if res.StatusCode != http.StatusSeeOther || u.Query().Get("error") != "invalid_request" || u.Query().Get("state") != "xyz" || u.Query().Get("iss") != e.base {
				t.Fatalf("answer = %d %s", res.StatusCode, u)
			}
		})
	}
	for name, v := range map[string]string{"missing verifier": "", "wrong verifier": "not-the-" + verifier} {
		t.Run(name, func(t *testing.T) {
			code := e.code(t, e.params(client), e.a.Slug)
			form := url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {code}, "code_verifier": {v}, "redirect_uri": {callback}}
			status, out := e.token(t, form)
			wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
			// A failed attempt burns the code: the right verifier comes too late.
			status, out = e.exchange(t, client, code)
			wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
		})
	}
}

func TestCodeIsSingleUse(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	code := e.code(t, e.params(client), e.a.Slug)
	status, out := e.exchange(t, client, code)
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v", status, out)
	}
	access := out["access_token"].(string)
	status, out = e.exchange(t, client, code)
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
	if s, _ := e.status(t, access); s != http.StatusUnauthorized {
		t.Errorf("a reused code left the family alive: %d", s)
	}
}

func TestCodeBindings(t *testing.T) {
	e := newEnv(t)
	client, other := e.register(t, callback), e.register(t, callback)
	code := e.code(t, e.params(client), e.a.Slug)
	status, out := e.exchange(t, other, code) // not its code, and it stays usable
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
	status, out = e.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {code}})
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
	status, out = e.token(t, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {"https://app.example/other"}})
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")

	code = e.code(t, e.params(client), e.a.Slug)
	status, out = e.token(t, url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {code}, "code_verifier": {verifier}, "resource": {"https://other.example/mcp"}})
	wantError(t, status, out, http.StatusBadRequest, "invalid_target")

	code = e.code(t, e.params(client), e.a.Slug)
	e.now = e.now.Add(6 * time.Minute)
	status, out = e.exchange(t, client, code)
	wantError(t, status, out, http.StatusBadRequest, "invalid_grant")
}

func TestAuthorizeRefuses(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback, "http://127.0.0.1:1234/cb")

	q := e.params(client)
	q.Set("redirect_uri", "https://evil.example/callback")
	res, page := e.get(t, q, cookie)
	if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(page, "not one it registered") {
		t.Errorf("unregistered redirect URI: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	q = e.params("nobody")
	if res, page := e.get(t, q, cookie); res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(page, "not registered") {
		t.Errorf("unknown client: %d", res.StatusCode)
	}
	// A loopback port is free; the host and path are not.
	q = e.params(client)
	q.Set("redirect_uri", "http://127.0.0.1:5555/cb")
	if res, page := e.get(t, q, cookie); res.StatusCode != http.StatusOK || !strings.Contains(page, "an app on this computer") {
		t.Errorf("loopback with another port: %d", res.StatusCode)
	}
	q.Set("redirect_uri", "http://127.0.0.1:5555/other")
	if res, _ := e.get(t, q, cookie); res.StatusCode != http.StatusBadRequest {
		t.Errorf("loopback with another path: %d", res.StatusCode)
	}

	for name, tt := range map[string]struct {
		change func(url.Values)
		code   string
	}{
		"token for another resource": {func(q url.Values) { q.Set("resource", "https://other.example/mcp") }, "invalid_target"},
		"unknown scope":              {func(q url.Values) { q.Set("scope", "read everything") }, "invalid_scope"},
		"implicit flow":              {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
	} {
		t.Run(name, func(t *testing.T) {
			q := e.params(client)
			tt.change(q)
			res, _ := e.get(t, q, cookie)
			u, err := res.Location()
			must(t, err)
			if u.Query().Get("error") != tt.code {
				t.Fatalf("redirect = %s", u)
			}
		})
	}
}

func TestScopes(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	for _, tt := range []struct{ asked, granted string }{
		{"", "read"},
		{"offline_access", "read"},
		{"admin read", "read admin"},
		{"write offline_access", "write"},
	} {
		q := e.params(client)
		q.Set("scope", tt.asked)
		status, out := e.exchange(t, client, e.code(t, q, e.a.Slug))
		if status != http.StatusOK || out["scope"] != tt.granted {
			t.Errorf("%q: %d %v", tt.asked, status, out)
		}
	}

	// A refresh narrows and never widens.
	q := e.params(client)
	q.Set("scope", "write")
	_, out := e.exchange(t, client, e.code(t, q, e.a.Slug))
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {out["refresh_token"].(string)}, "scope": {"admin"}}
	status, res := e.token(t, form)
	wantError(t, status, res, http.StatusBadRequest, "invalid_scope")
	form.Set("scope", "read")
	status, res = e.token(t, form)
	if status != http.StatusOK || res["scope"] != "read" {
		t.Fatalf("narrowed = %d %v", status, res)
	}
	form.Set("refresh_token", res["refresh_token"].(string))
	form.Set("resource", "https://other.example/mcp")
	status, res = e.token(t, form)
	wantError(t, status, res, http.StatusBadRequest, "invalid_target")
}

func TestConsent(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	q := e.params(client)
	_, page := e.get(t, q, cookie)

	if res := e.post(t, page, func(f url.Values) { f.Set("csrf", "forged") }); res.StatusCode != http.StatusForbidden {
		t.Errorf("forged csrf = %d", res.StatusCode)
	}
	if res := e.post(t, page, func(f url.Values) { f.Set("organization", "nowhere") }); res.StatusCode != http.StatusForbidden {
		t.Errorf("foreign organization = %d", res.StatusCode)
	}
	if res := e.post(t, page, func(f url.Values) { f.Set("redirect_uri", "https://evil.example/") }); res.StatusCode != http.StatusBadRequest {
		t.Errorf("changed redirect URI = %d", res.StatusCode)
	}
	if res := e.post(t, page, func(f url.Values) { f.Set("client_id", "nobody") }); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown client = %d", res.StatusCode)
	}
	res := e.post(t, page, func(f url.Values) { f.Set("scope", "everything") })
	if u, _ := res.Location(); u == nil || u.Query().Get("error") != "invalid_scope" {
		t.Errorf("scope = %d %v", res.StatusCode, u)
	}
	res = e.post(t, page, func(f url.Values) { f.Set("decision", "deny") })
	u, err := res.Location()
	must(t, err)
	if u.Query().Get("error") != "access_denied" || u.Query().Get("code") != "" {
		t.Errorf("deny = %s", u)
	}

	// The second time the app is known, and one organization needs no picker.
	e.code(t, q, e.a.Slug)
	if _, page := e.get(t, q, cookie); strings.Contains(page, "not connected this app before") {
		t.Error("a known client is called new")
	}
	must(t, e.st.Memberships().Remove(t.Context(), e.b.ID, e.user.ID))
	_, page = e.get(t, q, cookie)
	if strings.Contains(page, "<select") || !strings.Contains(page, "Acme") {
		t.Error("one organization needs no picker")
	}
}

func TestAuthorizeNeedsASignIn(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	q := e.params(client)
	res, _ := e.get(t, q, "")
	loc, err := res.Location()
	must(t, err)
	if res.StatusCode != http.StatusFound || loc.Path != "/login" || !strings.HasPrefix(loc.Query().Get("redirect"), "/oauth/authorize?") {
		t.Fatalf("no session: %d %s", res.StatusCode, loc)
	}
	if res, _ := e.get(t, q, "fcs_dead"); res.StatusCode != http.StatusFound {
		t.Errorf("dead session = %d", res.StatusCode)
	}
	res, page := e.get(t, q, "fcm_legacy-token-cookie")
	if res.StatusCode != http.StatusForbidden || !strings.Contains(page, "cannot authorize") {
		t.Errorf("a token cookie consents: %d", res.StatusCode)
	}
	loner := e.st.AddMember(e.a.ID, domain.User{Name: "Lee"}, "")
	must(t, e.st.Memberships().SetStatus(t.Context(), e.a.ID, loner.ID, domain.MembershipSuspended))
	e.session(t, "fcs_loner", loner.ID)
	if res, _ := e.get(t, q, "fcs_loner"); res.StatusCode != http.StatusForbidden {
		t.Errorf("no organization = %d", res.StatusCode)
	}
}

func TestLostMembershipEndsRefresh(t *testing.T) {
	for name, lose := range map[string]func(*testing.T, *env) error{
		"suspended": func(t *testing.T, e *env) error {
			return e.st.Memberships().SetStatus(t.Context(), e.a.ID, e.user.ID, domain.MembershipSuspended)
		},
		"disabled": func(t *testing.T, e *env) error { return e.st.Users().SetDisabled(t.Context(), e.user.ID, true) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			client := e.register(t, callback)
			out := e.tokens(t, client)
			must(t, lose(t, e))
			status, res := e.refresh(t, client, out["refresh_token"].(string))
			wantError(t, status, res, http.StatusBadRequest, "invalid_grant")
			if s, _ := e.status(t, out["access_token"].(string)); s != http.StatusUnauthorized {
				t.Errorf("access token = %d", s)
			}
		})
	}
}

func TestRevokedAppStopsAtOnce(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	out := e.tokens(t, client)
	g, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(out["access_token"].(string)))
	must(t, err)
	must(t, e.st.OAuth().RevokeFamily(t.Context(), g.FamilyID))
	if s, _ := e.status(t, out["access_token"].(string)); s != http.StatusUnauthorized {
		t.Errorf("access token = %d", s)
	}
	status, res := e.refresh(t, client, out["refresh_token"].(string))
	wantError(t, status, res, http.StatusBadRequest, "invalid_grant")
}

func TestExpiredCredentials(t *testing.T) {
	e := newEnv(t)
	client := e.register(t, callback)
	out := e.tokens(t, client)
	e.now = e.now.Add(31 * 24 * time.Hour)
	status, res := e.refresh(t, client, out["refresh_token"].(string))
	wantError(t, status, res, http.StatusBadRequest, "invalid_grant")
}

func TestConfidentialClient(t *testing.T) {
	e := newEnv(t)
	status, reg := e.postJSON(t, "/oauth/register", map[string]any{"client_name": "Server", "redirect_uris": []string{callback}, "token_endpoint_auth_method": "client_secret_basic"})
	if status != http.StatusCreated || reg["client_secret"] == "" {
		t.Fatalf("register = %d %v", status, reg)
	}
	client, secret := reg["client_id"].(string), reg["client_secret"].(string)
	code := e.code(t, e.params(client), e.a.Slug)
	status, out := e.exchange(t, client, code) // no secret
	wantError(t, status, out, http.StatusUnauthorized, "invalid_client")

	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {callback}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+"/oauth/token", strings.NewReader(form.Encode()))
	must(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(client), url.QueryEscape(secret))
	status, out = e.answer(t, req)
	if status != http.StatusOK {
		t.Fatalf("basic = %d %v", status, out)
	}
	status, out = e.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "client_secret": {secret}, "refresh_token": {out["refresh_token"].(string)}})
	if status != http.StatusOK {
		t.Fatalf("post = %d %v", status, out)
	}
	status, out = e.token(t, url.Values{"grant_type": {"password"}, "client_id": {client}, "client_secret": {secret}})
	wantError(t, status, out, http.StatusBadRequest, "unsupported_grant_type")
	status, out = e.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"nobody"}})
	wantError(t, status, out, http.StatusUnauthorized, "invalid_client")
}

func TestRegistrationRules(t *testing.T) {
	e := newEnv(t)
	for name, tt := range map[string]struct {
		body map[string]any
		code string
	}{
		"no redirect URI": {map[string]any{"client_name": "x"}, "invalid_redirect_uri"},
		"http":            {map[string]any{"redirect_uris": []string{"http://app.example/cb"}}, "invalid_redirect_uri"},
		"custom scheme":   {map[string]any{"redirect_uris": []string{"cursor://cb"}}, "invalid_redirect_uri"},
		"fragment":        {map[string]any{"redirect_uris": []string{"https://app.example/cb#x"}}, "invalid_redirect_uri"},
		"credentials":     {map[string]any{"redirect_uris": []string{"https://a:b@app.example/cb"}}, "invalid_redirect_uri"},
		"too many":        {map[string]any{"redirect_uris": strings.Split(strings.Repeat(callback+" ", 11), " ")[:11]}, "invalid_redirect_uri"},
		"grant":           {map[string]any{"redirect_uris": []string{callback}, "grant_types": []string{"password"}}, "invalid_client_metadata"},
		"response":        {map[string]any{"redirect_uris": []string{callback}, "response_types": []string{"token"}}, "invalid_client_metadata"},
		"auth method":     {map[string]any{"redirect_uris": []string{callback}, "token_endpoint_auth_method": "private_key_jwt"}, "invalid_client_metadata"},
	} {
		t.Run(name, func(t *testing.T) {
			status, out := e.postJSON(t, "/oauth/register", tt.body)
			wantError(t, status, out, http.StatusBadRequest, tt.code)
		})
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.base+"/oauth/register", strings.NewReader("not json"))
	must(t, err)
	status, out := e.answer(t, req)
	wantError(t, status, out, http.StatusBadRequest, "invalid_client_metadata")

	status, out = e.postJSON(t, "/oauth/register", map[string]any{"client_name": " \u0007 ", "redirect_uris": []string{"http://localhost:9/cb", "http://[::1]/cb"}})
	if status != http.StatusCreated || out["client_name"] != "Unnamed app" || out["token_endpoint_auth_method"] != "none" || out["client_secret"] != nil {
		t.Fatalf("register = %d %v", status, out)
	}
	status, out = e.postJSON(t, "/oauth/register", map[string]any{"client_name": strings.Repeat("n", 500), "redirect_uris": []string{callback}})
	if status != http.StatusCreated || len(out["client_name"].(string)) != 100 {
		t.Fatalf("long name = %d %v", status, out)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"redirect_uris": []string{callback}}
	var status int
	var out map[string]any
	for range 21 {
		status, out = e.postJSON(t, "/oauth/register", body)
	}
	wantError(t, status, out, http.StatusTooManyRequests, "temporarily_unavailable")
	e.now = e.now.Add(time.Minute)
	if status, out = e.postJSON(t, "/oauth/register", body); status != http.StatusCreated {
		t.Fatalf("next minute = %d %v", status, out)
	}
	for range 241 {
		status, out = e.token(t, url.Values{"grant_type": {"refresh_token"}})
	}
	wantError(t, status, out, http.StatusTooManyRequests, "temporarily_unavailable")
}

func TestMetadataDocumentClient(t *testing.T) {
	e := newEnv(t)
	id := "https://app.example/oauth/client.json"
	q := e.params(id)
	refused := func(name, body string) {
		t.Helper()
		e.docs.set(id, body)
		if res, _ := e.get(t, q, cookie); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	if res, _ := e.get(t, q, cookie); res.StatusCode != http.StatusBadRequest {
		t.Errorf("no document = %d", res.StatusCode)
	}
	refused("not JSON", "<html>")
	refused("another client_id", `{"client_id":"https://evil.example/c.json","client_name":"App","redirect_uris":["`+callback+`"]}`)
	refused("no name", `{"client_id":"`+id+`","redirect_uris":["`+callback+`"]}`)
	refused("a secret", `{"client_id":"`+id+`","client_name":"App","client_secret":"s","redirect_uris":["`+callback+`"]}`)
	refused("shared secret method", `{"client_id":"`+id+`","client_name":"App","token_endpoint_auth_method":"client_secret_post","redirect_uris":["`+callback+`"]}`)
	refused("bad redirect", `{"client_id":"`+id+`","client_name":"App","redirect_uris":["http://app.example/cb"]}`)
	refused("too large", `{"client_id":"`+id+`","client_name":"`+strings.Repeat("x", 20<<10)+`","redirect_uris":["`+callback+`"]}`)

	for _, bad := range []string{"https://app.example", "https://app.example/", "https://app.example/a/../b", "https://u:p@app.example/c", "https://app.example/c#f", "http://app.example/c"} {
		if res, _ := e.get(t, e.params(bad), cookie); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, res.StatusCode)
		}
	}

	e.docs.set(id, `{"client_id":"`+id+`","client_name":"Doc App","redirect_uris":["`+callback+`"]}`)
	res, page := e.get(t, q, cookie)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "Doc App") {
		t.Fatalf("document = %d %s", res.StatusCode, page)
	}
	status, out := e.exchange(t, id, e.code(t, q, e.a.Slug))
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v", status, out)
	}

	// The document decides at every visit: a renamed app, a dropped address.
	e.docs.set(id, `{"client_id":"`+id+`","client_name":"Renamed","redirect_uris":["https://app.example/new"]}`)
	if res, _ := e.get(t, q, cookie); res.StatusCode != http.StatusBadRequest {
		t.Errorf("dropped redirect URI = %d", res.StatusCode)
	}
	c, err := e.st.OAuth().GetClient(t.Context(), id)
	must(t, err)
	if c.Name != "Renamed" || c.RedirectURIs[0] != "https://app.example/new" {
		t.Errorf("client = %+v", c)
	}
}

func TestMetadataFetchIsLimited(t *testing.T) {
	e := newEnv(t)
	id := "https://app.example/client.json"
	e.docs.set(id, `{"client_id":"`+id+`","client_name":"App","redirect_uris":["`+callback+`"]}`)
	var res *http.Response
	for range 31 {
		res, _ = e.get(t, e.params(id), cookie)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", res.StatusCode)
	}
}

func TestSweep(t *testing.T) {
	e := newEnv(t)
	unused := e.register(t, callback)
	used := e.register(t, callback)
	out := e.tokens(t, used)
	e.now = e.now.Add(2 * time.Hour) // the access token expired, the refresh token lives
	must(t, e.server.Sweep(t.Context()))
	if _, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(out["access_token"].(string))); err == nil {
		t.Error("the expired access token was kept")
	}
	if _, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(out["refresh_token"].(string))); err != nil {
		t.Errorf("refresh token: %v", err)
	}
	for _, id := range []string{unused, used} {
		if _, err := e.st.OAuth().GetClient(t.Context(), id); err != nil {
			t.Errorf("a client younger than 30 days was deleted: %v", err)
		}
	}
	e.now = e.now.Add(oauth.UnusedClientTTL + time.Hour) // the store's clock stays real
	must(t, e.server.Sweep(t.Context()))
	for _, id := range []string{unused, used} {
		if _, err := e.st.OAuth().GetClient(t.Context(), id); err == nil {
			t.Errorf("client %s outlived its 30 days without grants", id)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.server.RunSweep(ctx, time.Millisecond)
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done
}

// conformance runs the MCP SDK's own client against the server: it finds
// everything from the 401, gets a client ID in the way cfg sets up, and
// connects. The browser is the fetcher, which answers the consent screen.
func (e *env) conformance(t *testing.T, cfg *sdkauth.AuthorizationCodeHandlerConfig) {
	t.Helper()
	cfg.RedirectURL = callback
	cfg.Client = &http.Client{}
	cfg.AuthorizationCodeFetcher = func(_ context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		res, page := e.get(t, u.Query(), cookie)
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("authorize = %d: %s", res.StatusCode, page)
		}
		back, err := e.post(t, page, nil).Location()
		if err != nil {
			return nil, err
		}
		q := back.Query()
		return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(cfg)
	must(t, err)
	tr := &sdk.StreamableClientTransport{Endpoint: e.base + "/mcp", OAuthHandler: handler}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "conformance", Version: "1"}, nil).Connect(t.Context(), tr, nil)
	must(t, err)
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: mcp.ToolListProjects, Arguments: map[string]any{}})
	must(t, err)
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	ts, err := handler.TokenSource(t.Context())
	must(t, err)
	tok, err := ts.Token()
	must(t, err)
	g, err := e.st.OAuth().GetGrantByTokenHash(t.Context(), mcp.HashToken(tok.AccessToken))
	must(t, err)
	if len(g.Scopes) != 2 || g.Scopes[1] != domain.ScopeWrite {
		t.Errorf("the client asked for %v, want what the challenge says", g.Scopes)
	}
}

func TestConformanceDynamicRegistration(t *testing.T) {
	e := newEnv(t)
	e.conformance(t, &sdkauth.AuthorizationCodeHandlerConfig{DynamicClientRegistrationConfig: &sdkauth.DynamicClientRegistrationConfig{
		Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "SDK", RedirectURIs: []string{callback}, GrantTypes: []string{"authorization_code", "refresh_token"}, TokenEndpointAuthMethod: "none"},
	}})
}

func TestConformanceClientMetadataDocument(t *testing.T) {
	e := newEnv(t)
	id := "https://app.example/client.json"
	e.docs.set(id, `{"client_id":"`+id+`","client_name":"SDK","redirect_uris":["`+callback+`"],"grant_types":["authorization_code","refresh_token"],"token_endpoint_auth_method":"none"}`)
	e.conformance(t, &sdkauth.AuthorizationCodeHandlerConfig{ClientIDMetadataDocumentConfig: &sdkauth.ClientIDMetadataDocumentConfig{URL: id}})
}

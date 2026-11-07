package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const testToken = "fcm_test-token"

// failingTokens fails every lookup like a database outage.
type failingTokens struct{ store.PersonalTokenRepo }

// failingStore is a store whose tokens fail.
type failingStore struct{ *testkit.Store }

func (s failingStore) MCPTokens() store.PersonalTokenRepo {
	return failingTokens{s.Store.MCPTokens()}
}

func (failingTokens) GetByTokenHash(context.Context, []byte) (domain.MCPToken, error) {
	return domain.MCPToken{}, errors.New("disk on fire")
}

type authEnv struct {
	store *testkit.Store
	actor domain.Actor
}

func newAuthEnv(t *testing.T) authEnv {
	t.Helper()
	st := testkit.NewStore(nil)
	account := domain.Account{ID: domain.NewID(), Name: "acme"}
	if err := st.Accounts().Create(t.Context(), &account); err != nil {
		t.Fatal(err)
	}
	tok := domain.MCPToken{AccountID: account.ID, Name: "laptop", TokenHash: mcp.HashToken(testToken)}
	if err := st.MCPTokens().Create(t.Context(), &tok); err != nil {
		t.Fatal(err)
	}
	return authEnv{st, domain.Actor{AccountID: account.ID, Kind: domain.ActorMCP, Name: "laptop", Credential: domain.Credential{Kind: domain.CredentialToken, ID: tok.ID}}}
}

func TestAuthenticateRejects(t *testing.T) {
	e := newAuthEnv(t)
	revoked := domain.MCPToken{AccountID: e.actor.AccountID, Name: "old", TokenHash: mcp.HashToken("fcm_old")}
	if err := e.store.MCPTokens().Create(t.Context(), &revoked); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MCPTokens().Revoke(t.Context(), revoked.AccountID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	reached := false
	h := mcp.Authenticate(e.store, nil, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	tests := []struct{ name, header string }{
		{"missing", ""},
		{"wrong scheme", "Basic " + testToken},
		{"empty token", "Bearer "},
		{"wrong token", "Bearer fcm_nope"},
		{"revoked token", "Bearer fcm_old"},
	}
	for _, tt := range tests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader("{}"))
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out struct {
			Error struct{ Code, Message string }
		}
		if rec.Code != http.StatusUnauthorized || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Error.Code != "unauthorized" ||
			rec.Header().Get("WWW-Authenticate") != "Bearer" || strings.Contains(rec.Body.String(), testToken) {
			t.Errorf("%s: status %d body %s", tt.name, rec.Code, rec.Body)
		}
	}
	if reached {
		t.Error("next handler reached without a valid token")
	}
}

func TestAuthenticateSetsActorAndRecordsUse(t *testing.T) {
	e := newAuthEnv(t)
	var got domain.Actor
	var ok bool
	h := mcp.Authenticate(e.store, nil, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = mcp.ActorFromContext(r.Context())
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/x", nil)
	req.Header.Set("Authorization", "bearer "+testToken)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ok || got != e.actor {
		t.Errorf("actor = %+v, %v; want %+v", got, ok, e.actor)
	}
	if _, ok := mcp.ActorFromContext(t.Context()); ok {
		t.Error("actor found in a bare context")
	}
	toks, err := e.store.MCPTokens().List(t.Context(), e.actor.AccountID)
	if err != nil || len(toks) != 1 || toks[0].LastUsedAt.IsZero() {
		t.Errorf("tokens = %+v, %v", toks, err)
	}
}

func TestAuthenticateStoreError(t *testing.T) {
	e := newAuthEnv(t)
	var logs bytes.Buffer
	h := mcp.Authenticate(failingStore{e.store}, nil, slog.New(slog.NewJSONHandler(&logs, nil)))(http.NotFoundHandler())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk") || !strings.Contains(logs.String(), "disk") {
		t.Errorf("status %d body %s logs %s", rec.Code, rec.Body, logs.String())
	}
}

// A token stops with its owner, and carries its scopes and credential.
func TestAuthenticateChecksTheOwner(t *testing.T) {
	e := newAuthEnv(t)
	ctx := t.Context()
	account := e.actor.AccountID
	u := domain.User{Name: "Ana"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.store.Users().Create(ctx, &u))
	must(e.store.Memberships().Create(ctx, &domain.Membership{AccountID: account, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	sa := domain.ServiceAccount{AccountID: account, Name: "ci", CreatedBy: "user:x"}
	must(e.store.ServiceAccounts().Create(ctx, &sa))
	personal := domain.MCPToken{AccountID: account, Name: "p", TokenHash: mcp.HashToken("fcm_p"), OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeRead}}
	must(e.store.MCPTokens().Create(ctx, &personal))
	bot := domain.MCPToken{AccountID: account, Name: "b", TokenHash: mcp.HashToken("fcm_b"), OwnerKind: domain.SubjectServiceAccount, OwnerID: sa.ID, Scopes: []domain.Scope{domain.ScopeWrite}, ExpiresAt: t0.Add(time.Hour)}
	must(e.store.MCPTokens().Create(ctx, &bot))

	now := t0
	var got mcp.Caller
	h := mcp.Authenticate(e.store, func() time.Time { return now }, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = mcp.CallerFromContext(r.Context())
	}))
	call := func(token string) int {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("fcm_p"); code != 200 || got.Actor != domain.UserActor(account, u.ID, domain.Credential{Kind: domain.CredentialToken, ID: personal.ID}) ||
		got.TokenName != "p" || len(got.Scopes) != 1 {
		t.Fatalf("personal = %d %+v", code, got)
	}
	if code := call("fcm_b"); code != 200 || got.Actor != domain.ServiceAccountActor(account, sa.ID, bot.ID) {
		t.Fatalf("service account = %d %+v", code, got)
	}
	now = t0.Add(2 * time.Hour)
	if code := call("fcm_b"); code != 401 {
		t.Errorf("expired = %d", code)
	}
	now = t0
	must(e.store.ServiceAccounts().SetDisabled(ctx, account, sa.ID, true))
	if code := call("fcm_b"); code != 401 {
		t.Errorf("disabled service account = %d", code)
	}
	must(e.store.Memberships().SetStatus(ctx, account, u.ID, domain.MembershipSuspended))
	if code := call("fcm_p"); code != 401 {
		t.Errorf("suspended = %d", code)
	}
	must(e.store.Memberships().SetStatus(ctx, account, u.ID, domain.MembershipActive))
	if code := call("fcm_p"); code != 200 {
		t.Errorf("active again = %d", code)
	}
	must(e.store.Users().SetDisabled(ctx, u.ID, true))
	if code := call("fcm_p"); code != 401 {
		t.Errorf("disabled user = %d", code)
	}
	must(e.store.Users().SetDisabled(ctx, u.ID, false))
	must(e.store.Memberships().Remove(ctx, account, u.ID))
	if code := call("fcm_p"); code != 401 {
		t.Errorf("removed = %d", code)
	}
}

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// grantEnv is a member with an access token for the resource.
type grantEnv struct {
	authEnv
	res   mcp.Resource
	user  domain.User
	grant domain.OAuthGrant
}

const accessToken = "fca_test-access"

func newGrantEnv(t *testing.T, change ...func(*domain.OAuthGrant)) grantEnv {
	t.Helper()
	e := grantEnv{authEnv: newAuthEnv(t), res: mcp.NewResource("https://forecrew.example/")}
	e.user = e.store.AddMember(e.actor.AccountID, domain.User{Name: "ana"}, "")
	client := domain.OAuthClient{ClientID: "app", Name: "App", RedirectURIs: []string{"https://app.example/cb"}}
	if err := e.store.OAuth().CreateClient(t.Context(), &client); err != nil {
		t.Fatal(err)
	}
	e.grant = domain.OAuthGrant{Kind: domain.OAuthAccess, TokenHash: mcp.HashToken(accessToken), FamilyID: domain.NewID(), ClientID: client.ID, UserID: e.user.ID,
		AccountID: e.actor.AccountID, Scopes: []domain.Scope{domain.ScopeRead}, Resource: e.res.URL, ExpiresAt: time.Now().Add(time.Hour)}
	for _, c := range change {
		c(&e.grant)
	}
	if err := e.store.OAuth().CreateGrant(t.Context(), &e.grant); err != nil {
		t.Fatal(err)
	}
	return e
}

// call sends the token through the middleware and returns the answer and the
// caller that reached the handler.
func (e grantEnv) call(t *testing.T, token string) (*httptest.ResponseRecorder, *mcp.Caller) {
	t.Helper()
	var got *mcp.Caller
	h := mcp.AuthenticateResource(e.store, nil, slog.New(slog.DiscardHandler), e.res)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		c, _ := mcp.CallerFromContext(r.Context())
		got = &c
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got
}

func TestResourceChallenge(t *testing.T) {
	e := newGrantEnv(t)
	if e.res.URL != "https://forecrew.example/mcp" || e.res.Metadata != "https://forecrew.example/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("resource = %+v", e.res)
	}
	want := `Bearer resource_metadata="https://forecrew.example/.well-known/oauth-protected-resource/mcp", scope="read write"`
	rec, _ := e.call(t, "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want {
		t.Errorf("missing token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	rec, _ = e.call(t, "fcm_nope")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want+`, error="invalid_token"` {
		t.Errorf("bad token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestAuthenticateResourceTakesGrantsAndTokens(t *testing.T) {
	e := newGrantEnv(t)
	rec, c := e.call(t, accessToken)
	if rec.Code != http.StatusOK || c == nil {
		t.Fatalf("status %d, caller %+v", rec.Code, c)
	}
	want := domain.UserActor(e.actor.AccountID, e.user.ID, domain.Credential{Kind: domain.CredentialOAuthGrant, ID: e.grant.ID})
	if c.Actor != want || len(c.Scopes) != 1 || c.Scopes[0] != domain.ScopeRead {
		t.Errorf("caller = %+v", c)
	}
	if _, c := e.call(t, testToken); c == nil || c.Actor != e.actor {
		t.Errorf("personal token: caller = %+v", c)
	}
}

func TestAuthenticateResourceRefusesGrants(t *testing.T) {
	tests := []struct {
		name   string
		change func(*domain.OAuthGrant)
	}{
		{"another resource", func(g *domain.OAuthGrant) { g.Resource = "https://other.example/mcp" }},
		{"expired", func(g *domain.OAuthGrant) { g.ExpiresAt = time.Now().Add(-time.Second) }},
		{"revoked", func(g *domain.OAuthGrant) { g.RevokedAt = time.Now() }},
		{"a refresh token", func(g *domain.OAuthGrant) { g.Kind = domain.OAuthRefresh }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newGrantEnv(t, tt.change)
			if tt.name == "revoked" {
				if err := e.store.OAuth().RevokeFamily(t.Context(), e.grant.FamilyID); err != nil {
					t.Fatal(err)
				}
			}
			if rec, c := e.call(t, accessToken); rec.Code != http.StatusUnauthorized || c != nil {
				t.Errorf("status %d, caller %+v", rec.Code, c)
			}
		})
	}
}

// TestAuthenticateNeverTakesGrants is /api/v1: an access token is for /mcp.
func TestAuthenticateNeverTakesGrants(t *testing.T) {
	e := newGrantEnv(t)
	reached := false
	h := mcp.Authenticate(e.store, nil, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/images", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("status %d, reached %v", rec.Code, reached)
	}
}

func TestLostAccessRevokesGrants(t *testing.T) {
	tests := []struct {
		name string
		lose func(t *testing.T, e grantEnv) error
	}{
		{"suspended", func(t *testing.T, e grantEnv) error {
			return e.store.Memberships().SetStatus(t.Context(), e.actor.AccountID, e.user.ID, domain.MembershipSuspended)
		}},
		{"removed", func(t *testing.T, e grantEnv) error {
			return e.store.Memberships().Remove(t.Context(), e.actor.AccountID, e.user.ID)
		}},
		{"disabled", func(t *testing.T, e grantEnv) error { return e.store.Users().SetDisabled(t.Context(), e.user.ID, true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newGrantEnv(t)
			if err := tt.lose(t, e); err != nil {
				t.Fatal(err)
			}
			if rec, _ := e.call(t, accessToken); rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d", rec.Code)
			}
			if g, err := e.store.OAuth().GetGrant(t.Context(), e.grant.ID); err != nil || g.RevokedAt.IsZero() {
				t.Errorf("grant = %+v, %v", g, err)
			}
		})
	}
}

// Under SSO enforcement a personal token ends max_token_ttl after its
// creation, whatever its own expiry says; a service account's does not.
func TestPersonalTokenCapApplies(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	account := e.actor.AccountID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	u := domain.User{Name: "Ana", Email: "ana@acme.example", EmailVerified: true}
	must(e.store.Users().Create(ctx, &u))
	must(e.store.Memberships().Create(ctx, &domain.Membership{AccountID: account, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	must(e.store.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: account, Name: "p", TokenHash: mcp.HashToken("fcm_p"), OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeRead}}))
	d := domain.VerifiedDomain{AccountID: account, Domain: "acme.example", VerificationToken: "t"}
	must(e.store.VerifiedDomains().Create(ctx, &d))
	must(e.store.VerifiedDomains().SetVerified(ctx, account, d.ID, true))
	conn := domain.Connection{AccountID: account, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://idp.example", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_X", Enabled: true}
	must(e.store.Connections().Create(ctx, &conn))
	must(e.store.VerifiedDomains().SetEnforcement(ctx, account, d.ID, true, conn.ID))
	must(e.store.Accounts().SetMaxTokenTTL(ctx, account, time.Hour))

	stored, err := e.store.MCPTokens().GetByTokenHash(ctx, mcp.HashToken("fcm_p"))
	must(err)
	now := stored.CreatedAt.Add(59 * time.Minute)
	h := mcp.Authenticate(e.store, func() time.Time { return now }, slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	call := func() int {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer fcm_p")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call(); code != 200 {
		t.Fatalf("within the cap = %d", code)
	}
	now = stored.CreatedAt.Add(time.Hour)
	if code := call(); code != 401 {
		t.Fatalf("past the cap = %d", code)
	}
	must(e.store.VerifiedDomains().SetEnforcement(ctx, account, d.ID, false, uuid.Nil()))
	if code := call(); code != 200 {
		t.Fatalf("without enforcement = %d", code)
	}
}

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

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const testToken = "fcm_test-token"

// failingTokens fails every lookup like a database outage.
type failingTokens struct{ store.MCPTokenRepo }

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
	return authEnv{st, domain.Actor{AccountID: account.ID, Kind: domain.ActorMCP, Name: "laptop"}}
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
	h := mcp.Authenticate(e.store.MCPTokens(), slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
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
	h := mcp.Authenticate(e.store.MCPTokens(), slog.New(slog.DiscardHandler))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
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
	h := mcp.Authenticate(failingTokens{e.store.MCPTokens()}, slog.New(slog.NewJSONHandler(&logs, nil)))(http.NotFoundHandler())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk") || !strings.Contains(logs.String(), "disk") {
		t.Errorf("status %d body %s logs %s", rec.Code, rec.Body, logs.String())
	}
}

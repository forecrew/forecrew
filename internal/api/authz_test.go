package api_test

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

const userToken = "fcm_user"

// userEnv is the API with a user's token and no role.
func userEnv(t *testing.T) (*env, uuid.UUID) {
	t.Helper()
	e := newEnv(t)
	u := e.store.AddMember(e.account.ID, domain.User{Name: "Ana", Email: "ana@example.com"}, "")
	must(t, e.store.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: e.account.ID, Name: "user", TokenHash: mcp.HashToken(userToken),
		OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeAdmin}}))
	return e, u.ID
}

func (e *env) as(t *testing.T, tokenValue, method, path, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.server.URL+path, strings.NewReader(body))
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+tokenValue)
	req.Header.Set("Content-Type", "application/json")
	return e.send(t, req)
}

var pathVar = regexp.MustCompile(`\{[a-z_]+(\.\.\.)?\}`)

// TestEveryRouteChecksItsPermission calls each route with a permission as a
// user who holds none: the guard refuses it before the tool runs.
func TestEveryRouteChecksItsPermission(t *testing.T) {
	e, _ := userEnv(t)
	for _, pattern := range slices.Sorted(maps.Keys(api.RouteAccess())) {
		a := api.RouteAccess()[pattern]
		if a.Permission == "" || a.Own {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		before := len(e.svc.Calls())
		r := e.as(t, userToken, method, pathVar.ReplaceAllString(path, "x"), `{}`)
		if r.status != http.StatusForbidden || errorCode(t, r) != "forbidden" || !strings.Contains(r.body, string(a.Permission)) {
			t.Errorf("%s = %d %s", pattern, r.status, r.body)
		}
		if len(e.svc.Calls()) != before {
			t.Errorf("%s reached the tool", pattern)
		}
	}
}

func TestGuardLetsAProjectRoleThrough(t *testing.T) {
	e, id := userEnv(t)
	p := domain.Project{AccountID: e.account.ID, Name: "app", Repo: "acme/app"}
	must(t, e.store.Projects().Create(t.Context(), &p))
	testkit.Bind(t, e.store, e.account.ID, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleViewer, p.ID)
	if r := e.as(t, userToken, "GET", "/api/v1/goals", ""); r.status != http.StatusOK {
		t.Errorf("goals = %d %s", r.status, r.body)
	}
	if r := e.as(t, userToken, "GET", "/api/v1/workers", ""); r.status != http.StatusForbidden {
		t.Errorf("workers = %d %s", r.status, r.body)
	}
	calls := e.svc.Calls()
	want := domain.UserActor(e.account.ID, id, calls[0].Actor.Credential)
	if len(calls) != 1 || calls[0].Actor != want || calls[0].Actor.Credential.Kind != domain.CredentialToken {
		t.Errorf("calls = %+v", calls)
	}
}

func TestGateAndOwnRoutesReachTheTool(t *testing.T) {
	e, _ := userEnv(t)
	for _, c := range [][2]string{{"POST", "/api/v1/gates/GATE-1/approve"}, {"GET", "/api/v1/tokens"}, {"POST", "/api/v1/tokens/x/revoke"}, {"GET", "/api/v1/role-bindings"}} {
		if r := e.as(t, userToken, c[0], c[1], ""); r.status != http.StatusOK {
			t.Errorf("%s %s = %d %s", c[0], c[1], r.status, r.body)
		}
	}
}

func TestPublicAndSignedInRoutes(t *testing.T) {
	e := newEnv(t)
	if r := e.do(t, "DELETE", "/api/v1/session", "", ""); r.status != http.StatusNoContent {
		t.Errorf("logout = %d", r.status)
	}
	if r := e.do(t, "GET", "/api/v1/me", "", ""); r.status != http.StatusUnauthorized {
		t.Errorf("me without a token = %d", r.status)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	e := newEnv(t)
	must(t, e.store.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: e.account.ID, Name: "old2", TokenHash: mcp.HashToken("fcm_old2"), ExpiresAt: time.Now().Add(-time.Minute)}))
	if r := e.as(t, "fcm_old2", "GET", "/api/v1/me", ""); r.status != http.StatusUnauthorized {
		t.Errorf("status = %d", r.status)
	}
}

func TestMeOfAUser(t *testing.T) {
	e, id := userEnv(t)
	p := domain.Project{AccountID: e.account.ID, Name: "app", Repo: "acme/app"}
	must(t, e.store.Projects().Create(t.Context(), &p))
	testkit.Bind(t, e.store, e.account.ID, domain.SubjectUser, id, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	testkit.Bind(t, e.store, e.account.ID, domain.SubjectUser, id, domain.RoleScopeProject, domain.RoleContributor, p.ID)
	r := e.as(t, userToken, "GET", "/api/v1/me", "")
	if r.status != http.StatusOK {
		t.Fatalf("me = %d %s", r.status, r.body)
	}
	var got api.Session
	must(t, json.Unmarshal([]byte(r.body), &got))
	if got.User == nil || got.User.ID != id || got.User.Email != "ana@example.com" || got.Account.ID != e.account.ID {
		t.Fatalf("session = %s", r.body)
	}
	if len(got.Roles) != 2 || !slices.Contains(got.Permissions, domain.PermBillingManage) || slices.Contains(got.Permissions, domain.PermGoalCreate) {
		t.Errorf("session = %s", r.body)
	}
	perms := got.Projects[p.ID.String()]
	if !slices.Contains(perms, domain.PermGateApproveSpec) || slices.Contains(perms, domain.PermGateApproveMerge) {
		t.Errorf("project permissions = %v", perms)
	}
	// An ownerless token has no user, and what its scopes allow.
	r = e.do(t, "GET", "/api/v1/me", "", "bearer")
	var legacy api.Session
	must(t, json.Unmarshal([]byte(r.body), &legacy))
	if legacy.User != nil || !slices.Contains(legacy.Permissions, domain.PermOrgManage) {
		t.Errorf("ownerless = %s", r.body)
	}
}

func TestMeOfAMissingUser(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "acme"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	must(t, st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: acc.ID, Name: "t", TokenHash: mcp.HashToken("fcm_t"),
		OwnerKind: domain.SubjectUser, OwnerID: domain.NewID(), Scopes: []domain.Scope{domain.ScopeRead}}))
	srv := api.New(api.Config{Tools: &testkit.ToolService{}, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Authz: authz.New(st, nil), Directory: st.Directory(), Logger: slog.New(slog.DiscardHandler)})
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer fcm_t")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d %s", rec.Code, rec.Body)
	}
}

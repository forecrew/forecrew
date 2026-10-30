//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// raw sends a request with the headers given and returns the response; the
// body is closed.
func (w *world) raw(method, path, body string, header map[string]string) *http.Response {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.url+path, strings.NewReader(body))
	must(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	must(w.t, err)
	must(w.t, resp.Body.Close())
	return resp
}

// A token that is revoked through the API stops at once for MCP, the API
// and the web session it backs, and the revocation is audited.
func TestRevokeTokens(t *testing.T) {
	w := newWorld(t, true)
	const other = "fcm_other-token"
	must(t, w.st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: w.account.ID, Name: "other", TokenHash: mcp.HashToken(other)}))

	var list []mcp.TokenInfo
	if code := w.api(http.MethodGet, "/tokens", nil, &list); code != http.StatusOK || len(list) != 2 {
		t.Fatalf("list tokens = %d %+v", code, list)
	}
	if list[0].Name != "e2e" || !list[0].Current || list[1].Name != "other" || list[1].Current {
		t.Errorf("tokens = %+v", list)
	}

	// The browser session of "other": login sets the cookie.
	login := w.raw(http.MethodPost, "/api/v1/session", `{"token":"`+other+`"}`, nil)
	cookies := login.Cookies()
	if login.StatusCode != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login = %d, cookies %v", login.StatusCode, cookies)
	}
	session := map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value}
	if r := w.raw(http.MethodGet, "/api/v1/me", "", session); r.StatusCode != http.StatusOK {
		t.Fatalf("me = %d", r.StatusCode)
	}
	bearer := map[string]string{"Authorization": "Bearer " + other}

	var out mcp.RevokeTokenOutput
	if code := w.api(http.MethodPost, "/tokens/other/revoke", nil, &out); code != http.StatusOK || out.Self || out.Token.RevokedAt.IsZero() {
		t.Fatalf("revoke other = %d %+v", code, out)
	}
	if r := w.raw(http.MethodGet, "/api/v1/me", "", session); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("session of a revoked token = %d, want 401", r.StatusCode)
	}
	for _, path := range []string{"/api/v1/projects", "/mcp"} {
		if r := w.raw(http.MethodGet, path, "", bearer); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with a revoked token = %d, want 401", path, r.StatusCode)
		}
	}
	if code := w.api(http.MethodPost, "/tokens/other/revoke", nil, nil); code != http.StatusNotFound {
		t.Errorf("revoke again = %d, want 404", code)
	}

	// The caller may revoke its own token; it is the last request it makes.
	if code := w.api(http.MethodPost, "/tokens/"+list[0].ID.String()+"/revoke", nil, &out); code != http.StatusOK || !out.Self {
		t.Fatalf("revoke own token = %d %+v", code, out)
	}
	if code := w.api(http.MethodGet, "/tokens", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("own token after revoke = %d, want 401", code)
	}

	w.wantRevocationAudit()
}

func (w *world) wantRevocationAudit() {
	t := w.t
	entries, err := w.st.Audit().List(t.Context(), w.account.ID, store.AuditFilter{})
	must(t, err)
	var revoked []string
	for _, e := range entries {
		if e.Action == "token.revoked" {
			revoked = append(revoked, e.Actor)
		}
	}
	if len(revoked) != 2 || revoked[0] != "mcp:e2e" {
		t.Errorf("token.revoked audit actors = %v", revoked)
	}
}

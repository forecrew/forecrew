package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestSessionsAndIdentities(t *testing.T) {
	e := newSessionEnv(t)
	other := e.session(t, e.org)
	resp, body := e.do(t, "GET", "/api/v1/sessions", "", e.cookie, nil)
	var list []struct {
		ID           string `json:"id"`
		Current      bool   `json:"current"`
		Organization string `json:"organization"`
	}
	must(t, json.Unmarshal([]byte(body), &list))
	if resp.StatusCode != 200 || len(list) != 2 || list[0].Organization != e.org.Slug || list[0].Current == list[1].Current {
		t.Fatalf("sessions = %d %s", resp.StatusCode, body)
	}
	var otherID string
	for _, s := range list {
		if !s.Current {
			otherID = s.ID
		}
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/sessions/"+otherID, "{}", e.cookie, nil); resp.StatusCode != 204 {
		t.Fatalf("end one = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", "", other, nil); resp.StatusCode != 401 {
		t.Errorf("the ended session = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/sessions/"+otherID, "{}", e.cookie, nil); resp.StatusCode != 404 {
		t.Errorf("end it again = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/sessions/nope", "{}", e.cookie, nil); resp.StatusCode != 404 && resp.StatusCode != 400 {
		t.Errorf("end a bad ID = %d", resp.StatusCode)
	}
	conn := domain.Connection{Kind: domain.ConnectionGitHub, Name: "github", ClientID: "c", ClientSecretRef: "env:X", Enabled: true}
	must(t, e.st.Connections().UpsertInstance(t.Context(), &conn))
	must(t, e.st.Identities().Create(t.Context(), &domain.Identity{UserID: e.user.ID, ConnectionID: conn.ID, Subject: "1", Login: "ana"}))
	resp, body = e.do(t, "GET", "/api/v1/identities", "", e.cookie, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"connection":"github"`) || !strings.Contains(body, `"login":"ana"`) {
		t.Fatalf("identities = %d %s", resp.StatusCode, body)
	}
	// Ending the session of the request clears its cookie.
	own := e.session(t, e.org)
	_, body = e.do(t, "GET", "/api/v1/sessions", "", own, nil)
	must(t, json.Unmarshal([]byte(body), &list))
	for _, s := range list {
		if s.Current {
			if resp, _ := e.do(t, "DELETE", "/api/v1/sessions/"+s.ID, "{}", own, nil); resp.StatusCode != 204 || len(resp.Cookies()) != 1 {
				t.Errorf("end own = %d %v", resp.StatusCode, resp.Cookies())
			}
		}
	}
}

func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestCLILogin(t *testing.T) {
	e := newSessionEnv(t)
	var names []string
	e.svc.CreateTokenFunc = func(_ context.Context, by domain.Actor, in mcp.CreateTokenInput) (mcp.CreateTokenOutput, error) {
		names = append(names, in.Name+":"+string(in.Scopes[0]))
		if in.Scopes[0] == domain.ScopeAdmin {
			return mcp.CreateTokenOutput{}, authz.ErrDenied
		}
		if in.Name == "cli-laptop" {
			return mcp.CreateTokenOutput{}, mcp.ErrConflict
		}
		return mcp.CreateTokenOutput{Token: mcp.TokenInfo{Name: in.Name}, Secret: "fcm_secret"}, nil
	}
	verifier, challenge := pkce(t)
	authorize := func(body map[string]string) (int, string) {
		b, _ := json.Marshal(body)
		resp, out := e.do(t, "POST", "/api/v1/cli/authorize", string(b), e.cookie, nil)
		return resp.StatusCode, out
	}
	good := map[string]string{"redirect_uri": "http://127.0.0.1:5555/callback", "state": "st", "code_challenge": challenge}
	for name, bad := range map[string]map[string]string{
		"host":      {"redirect_uri": "http://example.com:5555/callback", "code_challenge": challenge},
		"scheme":    {"redirect_uri": "https://127.0.0.1:5555/callback", "code_challenge": challenge},
		"port":      {"redirect_uri": "http://127.0.0.1/callback", "code_challenge": challenge},
		"challenge": {"redirect_uri": good["redirect_uri"], "code_challenge": "short"},
	} {
		if status, body := authorize(bad); status != 400 {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}
	if status, _ := authorize(map[string]string{"redirect_uri": good["redirect_uri"], "code_challenge": challenge, "organization": "nope"}); status != 403 {
		t.Errorf("unknown organization = %d", status)
	}
	status, body := authorize(good)
	var out struct{ Redirect, Code string }
	must(t, json.Unmarshal([]byte(body), &out))
	u, err := url.Parse(out.Redirect)
	if status != 200 || err != nil || u.Host != "127.0.0.1:5555" || u.Query().Get("state") != "st" || u.Query().Get("code") != out.Code {
		t.Fatalf("authorize = %d %s", status, body)
	}

	exchange := func(code, verifier string) (int, string) {
		b, _ := json.Marshal(api.CLITokenInput{Code: code, CodeVerifier: verifier, Name: "CLI Laptop!"})
		resp, out := e.do(t, "POST", "/api/v1/cli/token", string(b), "", nil)
		return resp.StatusCode, out
	}
	// A wrong verifier burns the code.
	if status, _ := exchange(out.Code, "wrong"); status != 400 {
		t.Errorf("wrong verifier = %d", status)
	}
	if status, _ := exchange(out.Code, verifier); status != 400 {
		t.Errorf("burned code = %d", status)
	}
	_, body = authorize(good)
	must(t, json.Unmarshal([]byte(body), &out))
	status, body = exchange(out.Code, verifier)
	var got api.CLIToken
	must(t, json.Unmarshal([]byte(body), &got))
	if status != 200 || got.Secret != "fcm_secret" || got.Account.ID != e.org.ID || got.User.ID != e.user.ID || got.Token.Name != "cli-laptop-2" {
		t.Fatalf("token = %d %s (calls %v)", status, body, names)
	}
	if want := "cli-laptop:admin cli-laptop:write cli-laptop-2:admin cli-laptop-2:write"; strings.Join(names[max(0, len(names)-4):], " ") != want {
		t.Errorf("calls = %v", names)
	}
	if status, _ := exchange(out.Code, verifier); status != 400 {
		t.Errorf("used code = %d", status)
	}
	// Only a person with a session allows the CLI.
	if resp, _ := e.do(t, "POST", "/api/v1/cli/authorize", "{}", "", map[string]string{"Authorization": "Bearer " + uuid.New().String()}); resp.StatusCode != 401 {
		t.Errorf("without a session = %d", resp.StatusCode)
	}
}

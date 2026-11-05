package tools_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestCancelJob(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t, "one")
	first, second := f.newJob(t, "reviewer", g, domain.Task{}, 0), f.newJob(t, "reviewer", g, domain.Task{}, 0)
	w := domain.Worker{AccountID: f.account.ID, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	leased, err := f.st.Jobs().LeaseNext(ctx, f.account.ID, w.ID, []string{"runtime:claude-code"}, domain.LeaseDuration)
	must(t, err)

	if leased.ID != first.ID || leased.Status != domain.JobRunning {
		t.Fatalf("leased %s (%s), want %s running", leased.Ref(), leased.Status, first.Ref())
	}
	// One job runs, the other is queued.
	for _, j := range []domain.Job{first, second} {
		info, err := f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: j.Ref()})
		must(t, err)
		if info.Ref != j.Ref() || info.Status != domain.JobCanceled || info.FinishedAt.IsZero() {
			t.Errorf("canceled %s = %+v", j.Ref(), info)
		}
	}
	// A finished job cannot be canceled again.
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: second.Ref()})
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: "JOB-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)
	other, _, otherJob := f.otherAccount(t)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: otherJob.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	if got, _ := f.st.Jobs().Get(ctx, other.AccountID, otherJob.ID); got.Status != domain.JobQueued {
		t.Errorf("job of another account is %s", got.Status)
	}

	want := []string{"mcp:laptop job.canceled", "mcp:laptop job.canceled"}
	if got := f.auditActions(t); !slices.Equal(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other, _, _ := f.otherAccount(t)
	for _, tok := range []domain.MCPToken{
		{AccountID: f.account.ID, Name: "ci", TokenHash: mcp.HashToken("fcm_b")},
		{AccountID: other.AccountID, Name: "ci", TokenHash: mcp.HashToken("fcm_c")},
	} {
		must(t, f.st.MCPTokens().Create(ctx, &tok))
	}
	list, err := f.svc.ListTokens(ctx, f.by)
	must(t, err)
	if len(list) != 2 || list[0].Name != "laptop" || !list[0].Current || list[1].Current || !list[1].RevokedAt.IsZero() {
		t.Fatalf("tokens = %+v", list)
	}
	if raw, _ := json.Marshal(list); strings.Contains(string(raw), "hash") || strings.Contains(string(raw), "account") {
		t.Errorf("tokens leak internals: %s", raw)
	}

	// By name, then the second one is revoked already, then by ID.
	out, err := f.svc.RevokeToken(ctx, f.by, mcp.RevokeTokenInput{Token: "ci"})
	must(t, err)
	if out.Self || out.Token.Name != "ci" || out.Token.RevokedAt.IsZero() || out.Token.Current {
		t.Errorf("revoke ci = %+v", out)
	}
	_, err = f.svc.RevokeToken(ctx, f.by, mcp.RevokeTokenInput{Token: "ci"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.RevokeToken(ctx, f.by, mcp.RevokeTokenInput{Token: ""})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.RevokeToken(ctx, f.by, mcp.RevokeTokenInput{Token: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	// The other account's token with the same name is untouched.
	if others, _ := f.st.MCPTokens().List(ctx, other.AccountID); len(others) != 2 || !others[1].RevokedAt.IsZero() {
		t.Errorf("other account tokens = %+v", others)
	}

	out, err = f.svc.RevokeToken(ctx, f.by, mcp.RevokeTokenInput{Token: list[0].ID.String()})
	must(t, err)
	if !out.Self || !strings.Contains(out.Message, "token you are using") || out.Token.Current {
		t.Errorf("revoke own token = %+v", out)
	}
	list, err = f.svc.ListTokens(ctx, f.by)
	must(t, err)
	if list[0].RevokedAt.IsZero() || list[0].Current {
		t.Errorf("own token after revoke = %+v", list[0])
	}
	want := []string{"mcp:laptop token.revoked", "mcp:laptop token.revoked"}
	if got := f.auditActions(t); !slices.Equal(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

// TestRevokedTokenEndsSessions revokes the token behind a browser session
// through the session itself: the very next request, with the cookie or
// the bearer header, is refused and the cookie is dropped.
func TestRevokedTokenEndsSessions(t *testing.T) {
	e := newE2E(t)
	jar, err := cookiejar.New(nil)
	must(t, err)
	client := &http.Client{Jar: jar}
	client.Jar = jar
	do := func(method, path, body string, bearer bool) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, e.apiURL+path, strings.NewReader(body))
		must(t, err)
		req.Header.Set("Content-Type", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+e2eToken)
		}
		resp, err := client.Do(req)
		must(t, err)
		must(t, resp.Body.Close())
		return resp
	}
	if r := do("POST", "/api/v1/session", `{"token":"`+e2eToken+`"}`, false); r.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", r.StatusCode)
	}
	if r := do("GET", "/api/v1/me", "", false); r.StatusCode != http.StatusOK {
		t.Fatalf("me = %d", r.StatusCode)
	}
	var out mcp.RevokeTokenOutput
	if code := e.apiCall(t, "POST", "/api/v1/tokens/e2e/revoke", "", &out); code != http.StatusOK || !out.Self {
		t.Fatalf("revoke own token = %d %+v", code, out)
	}
	if r := do("GET", "/api/v1/me", "", false); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("session after revoke = %d, want 401", r.StatusCode)
	}
	if r := do("GET", "/api/v1/tokens", "", true); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bearer after revoke = %d, want 401", r.StatusCode)
	}
}

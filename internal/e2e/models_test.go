//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/secrets"
)

// fakeProvider is a model API that lists its models to the key it was given.
func fakeProvider(t *testing.T, key string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// browser is the person in the web UI: an Owner's session, which alone
// confirms a plan that lowers protection.
type browser struct {
	w      *world
	cookie string
}

func (w *world) browser() *browser {
	w.t.Helper()
	ctx := w.t.Context()
	u := domain.User{Name: "Ana", Email: "ana@acme.test", EmailVerified: true}
	must(w.t, w.st.Users().Create(ctx, &u))
	must(w.t, w.st.Memberships().Create(ctx, &domain.Membership{AccountID: w.account.ID, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	owner, err := w.st.Roles().GetByName(ctx, w.account.ID, domain.RoleScopeOrganization, domain.RoleOwner)
	must(w.t, err)
	must(w.t, w.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: w.account.ID, SubjectKind: domain.SubjectUser, SubjectID: u.ID, RoleID: owner.ID, CreatedBy: "e2e"}))
	token, err := auth.NewToken(auth.PrefixSession)
	must(w.t, err)
	now := time.Now()
	must(w.t, w.st.Sessions().Create(ctx, &domain.Session{TokenHash: mcp.HashToken(token), UserID: u.ID, AccountID: w.account.ID,
		IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(24 * time.Hour)}))
	return &browser{w: w, cookie: token}
}

func (b *browser) call(method, path string, in, out any) int {
	b.w.t.Helper()
	data, err := json.Marshal(in)
	must(b.w.t, err)
	req, err := http.NewRequestWithContext(b.w.t.Context(), method, b.w.url+"/api/v1"+path, bytes.NewReader(data))
	must(b.w.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: b.cookie})
	resp, err := http.DefaultClient.Do(req)
	must(b.w.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	must(b.w.t, err)
	if resp.StatusCode >= 300 {
		b.w.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, body)
	} else if out != nil {
		must(b.w.t, json.Unmarshal(body, out))
	}
	return resp.StatusCode
}

// confirm fills the secure fields of a plan and confirms it as the person
// does, with the second confirmation when the plan asks for it.
func (b *browser) confirm(p mcp.ConfigPlanView, secrets map[string]string) {
	b.w.t.Helper()
	for _, s := range p.Steps {
		if s.Kind == domain.ConfigStepSecret {
			if status := b.call(http.MethodPut, "/config/plans/"+p.ID.String()+"/secrets/"+url.PathEscape(s.Secret), map[string]string{"value": secrets[s.Secret]}, nil); status != http.StatusOK {
				b.w.t.Fatalf("secret %s: %d", s.Secret, status)
			}
		}
	}
	var applied mcp.ConfigApplied
	if status := b.call(http.MethodPost, "/config/plans/"+p.ID.String()+"/apply", map[string]string{"word": p.ConfirmWord}, &applied); status != http.StatusOK || applied.Plan.Status != domain.ConfigPlanApplied {
		b.w.t.Fatalf("apply %s: %d %+v", p.Title, status, applied.Plan)
	}
}

// TestAIModelsFromKeyToJob is the AI models page from end to end: a key goes
// in a secure field, its check runs on a control worker against the
// provider, a preset puts the roles on the connection, and the developer's
// job runs with that model, endpoint and key.
func TestAIModelsFromKeyToJob(t *testing.T) {
	const key = "sk-fake-provider-key"
	provider := fakeProvider(t, key)
	cfg := testConfig()
	cfg.SecretKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, secrets.KeySize))
	w := newWorldOn(t, true, nil, cfg)
	labels := []string{"linux", "runtime:claude-code", "runtime:codex", "runtime:opencode"}
	w.startWorker("box", labels...)
	w.startWorkerIn(domain.PoolControl, controlBox, labels...)
	b := w.browser()

	// Add provider: the plan has a secure field for the key.
	base := provider.URL + "/v1"
	var plan mcp.ConfigPlanView
	if status := b.call(http.MethodPost, "/config/plans", map[string]any{"title": "Add AI provider DeepSeek API", "source": "form", "changes": []map[string]any{{
		"kind": "model_connection", "name": "deepseek",
		"spec": map[string]any{"vendor": "deepseek", "adapter": "opencode", "base_url": base, "key_env": "DEEPSEEK_API_KEY", "secret_ref": "store:models/deepseek", "enabled": true},
	}}}, &plan); status != http.StatusCreated {
		t.Fatalf("plan: %d", status)
	}
	b.confirm(plan, map[string]string{"models/deepseek": key})

	// Saving checks the key where the model runs.
	var checks []mcp.ModelCheckView
	eventually(t, "the key check", func() bool {
		checks = nil
		w.api(http.MethodGet, "/model-checks?connection=deepseek", nil, &checks)
		return len(checks) == 1 && checks[0].Status == domain.ModelCheckReachable
	})
	if checks[0].Key == nil || !slices.Contains(checks[0].Key.Models, "deepseek-chat") {
		t.Fatalf("check = %+v", checks[0])
	}

	// The suitability test is an agent job of its own (internal/models): here
	// it passed.
	conn, err := w.st.ModelConnections().List(t.Context(), w.account.ID)
	must(t, err)
	must(t, w.st.ModelChecks().Put(t.Context(), &domain.ModelCheck{AccountID: w.account.ID, ConnectionID: conn[0].ID, Connection: "deepseek", Model: "deepseek/deepseek-chat",
		JobID: checks[0].JobID, Status: domain.SuitabilityDone, CreatedBy: "e2e", Test: &domain.SuitabilityResult{Level: domain.LevelCode}}))

	// Low cost puts the developer on it.
	var sg mcp.ModelsSuggestion
	if status := b.call(http.MethodPost, "/models/suggest", map[string]string{"preset": "low_cost", "source": "form"}, &sg); status != http.StatusOK || sg.Plan == nil {
		t.Fatalf("suggest: %d %+v", status, sg)
	}
	if sg.Roles["developer"].Connection != "deepseek" {
		t.Fatalf("suggestion = %+v", sg)
	}
	b.confirm(*sg.Plan, nil)

	var mu sync.Mutex
	calls := map[string]call{}
	w.ag.Before = func(_ context.Context, c call) error {
		mu.Lock()
		defer mu.Unlock()
		calls[c.Role] = c
		return nil
	}
	goal := w.createGoal(contributing, "Contributors do not know how to run the tests.")
	w.decide(w.gate(domain.GateSpecApproval), "approve", "")
	w.mergeTask()
	w.waitGoal(goal.ID, domain.GoalDone)

	mu.Lock()
	defer mu.Unlock()
	dev, ok := calls["developer"]
	if !ok || dev.Model != "deepseek/deepseek-chat" || dev.Endpoint == nil || dev.Endpoint.BaseURL != base || !dev.Endpoint.Organization {
		t.Fatalf("developer ran with %+v (ok %v)", dev, ok)
	}
	for _, j := range w.jobs("developer") {
		if j.Runtime != "opencode" || !j.Spec.OrganizationModel || strings.Contains(string(mustJSON(t, j.Spec)), key) {
			t.Errorf("job %s: runtime %s, organization %v, or the key is stored with it", j.Ref(), j.Runtime, j.Spec.OrganizationModel)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

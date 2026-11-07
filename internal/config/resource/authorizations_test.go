package resource_test

import (
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func (w *world) authorizations() []domain.ModelAuthorization {
	w.t.Helper()
	rows, err := w.st.ModelAuthorizations().List(w.t.Context(), w.account.ID)
	check(w.t, err)
	return rows
}

func cloudConn(ref string) map[string]any {
	return map[string]any{"vendor": "anthropic", "adapter": "claude-code", "secret_ref": ref}
}

func TestAssigningAProjectAuthorizesItsCode(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	check(t, w.vault.Put(ctx, w.owner, w.account.ID, "models/cloud", []byte("k")))
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "cloud", cloudConn("store:models/cloud"))))

	p := w.plan(w.owner, wanted(t, "model_assignment", "project:app", assignment("cloud")))
	i := slices.IndexFunc(p.Changes, func(c domain.ConfigChange) bool { return c.Kind == "model_authorization" })
	if i < 0 || p.Changes[i].Name != "cloud@app" {
		t.Fatalf("changes = %+v", p.Changes)
	}
	if !strings.Contains(p.Affects, "The code of acme/app goes to Anthropic API (in US) through connection cloud") {
		t.Errorf("affects = %q", p.Affects)
	}
	if !slices.ContainsFunc(p.LowersProtection, func(s string) bool { return strings.Contains(s, "private code of project app") }) {
		t.Errorf("lowers protection = %v", p.LowersProtection)
	}
	// A token, or anyone without the browser and the word, cannot confirm it.
	_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord})
	wantErr(t, err, mcp.ErrConflict)
	w.apply(w.owner, p)
	rows := w.authorizations()
	if len(rows) != 1 || rows[0].ProjectID != w.project.ID || rows[0].Connection != "cloud" || rows[0].AuthorizedBy != w.owner.String() || rows[0].Adapter != "claude-code" {
		t.Fatalf("authorizations = %+v", rows)
	}
	if got := w.spec("model_authorization", "cloud@app"); got != "{}" {
		t.Errorf("read = %s", got)
	}

	// The same connection again needs nothing more.
	again := assignment("cloud")
	again["tiers"] = map[string]any{"strong": map[string]any{"acme": "m9"}}
	if p := w.plan(w.owner, wanted(t, "model_assignment", "project:app", again)); slices.ContainsFunc(p.Changes, func(c domain.ConfigChange) bool { return c.Kind == "model_authorization" }) {
		t.Errorf("authorized twice: %+v", p.Changes)
	}

	// A new endpoint ends the authorization.
	moved := cloudConn("store:models/cloud")
	moved["base_url"] = "https://proxy.example.com"
	p = w.plan(w.owner, wanted(t, "model_connection", "cloud", moved))
	if !strings.Contains(p.Affects, "authorizations for private code end") {
		t.Errorf("affects = %q", p.Affects)
	}
	w.apply(w.owner, p)
	if rows := w.authorizations(); len(rows) != 0 {
		t.Fatalf("after the move: %+v", rows)
	}

	// Revoking is a delete, which lowers nothing.
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_authorization", "cloud@app", map[string]any{})))
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "model_authorization", Name: "cloud@app", Delete: true})
	if len(p.LowersProtection) != 0 {
		t.Errorf("revoke lowers protection: %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	if rows := w.authorizations(); len(rows) != 0 {
		t.Fatalf("after the revoke: %+v", rows)
	}
}

func TestALocalConnectionNeedsNoAuthorization(t *testing.T) {
	w := newWorld(t)
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/local", []byte("k")))
	local := map[string]any{"vendor": "acme", "adapter": "opencode", "key_env": "OLLAMA_API_KEY", "base_url": "http://127.0.0.1:11434/v1", "secret_ref": "store:models/local"}
	tiers := map[string]any{"strong": map[string]any{"acme": "acme/m1"}}
	p := w.plan(w.owner, wanted(t, "model_connection", "local", local),
		wanted(t, "model_assignment", "project:app", map[string]any{"tiers": tiers, "roles": map[string]any{"developer": map[string]any{"connection": "local", "tier": "strong"}}}))
	if slices.ContainsFunc(p.Changes, func(c domain.ConfigChange) bool { return c.Kind == "model_authorization" }) {
		t.Errorf("a local connection was authorized: %+v", p.Changes)
	}
}

func TestAuthorizingEveryProjectTakesModelsAuthorize(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	check(t, w.vault.Put(ctx, w.owner, w.account.ID, "models/cloud", []byte("k")))
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "cloud", cloudConn("store:models/cloud"))))
	p := w.plan(w.owner, wanted(t, "model_authorization", "cloud@*", map[string]any{}))
	if !slices.Contains(p.Needs, domain.PermModelsAuthorize) || len(p.LowersProtection) != 1 || !strings.Contains(p.Affects, "every project (acme/app)") {
		t.Fatalf("plan = %+v", p)
	}
	// A member may plan it and not confirm it.
	_, err := w.engine.Apply(ctx, w.person(domain.RoleMember), resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
	wantErr(t, err, authz.ErrDenied)
	w.apply(w.owner, p)
	if rows := w.authorizations(); len(rows) != 1 || rows[0].ProjectID != uuid.Nil() {
		t.Fatalf("authorizations = %+v", rows)
	}
}

func TestAuthorizationNames(t *testing.T) {
	w := newWorld(t)
	w.refused(w.owner, "<connection>@<project>", wanted(t, "model_authorization", "cloud", map[string]any{}))
	w.refused(w.owner, "not one of the organization's", wanted(t, "model_authorization", "nothing@app", map[string]any{}))
	w.refused(w.owner, "does not exist", wanted(t, "model_authorization", "cloud@nowhere", map[string]any{}))
	w.refused(w.owner, "unknown field", wanted(t, "model_authorization", "cloud@app", map[string]any{"who": "me"}))
}

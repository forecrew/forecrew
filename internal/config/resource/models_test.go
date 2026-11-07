package resource_test

import (
	"encoding/json"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func modelConn(ref string) map[string]any {
	return map[string]any{"vendor": "acme", "adapter": "claude-code", "secret_ref": ref}
}

func assignment(connection string, extra ...string) map[string]any {
	roles := map[string]any{"developer": map[string]any{"connection": connection, "tier": "strong"}}
	for _, role := range extra {
		roles[role] = map[string]any{"connection": connection, "tier": "strong"}
	}
	return map[string]any{"tiers": map[string]any{"strong": map[string]any{"acme": "m1", "zeta": "m2"}}, "roles": roles}
}

// connect makes a connection with its key and gives it the developer role.
func (w *world) connect(name string) {
	w.t.Helper()
	ref := "models/" + name
	p := w.plan(w.owner, wanted(w.t, "model_connection", name, modelConn("store:"+ref)), wanted(w.t, "model_assignment", "organization", assignment(name)))
	_, err := w.engine.PutSecret(w.t.Context(), w.owner, p.ID, ref, []byte("sk-value"))
	check(w.t, err)
	w.apply(w.owner, p)
}

func (w *world) refused(by domain.Actor, want string, changes ...mcp.ConfigWanted) {
	w.t.Helper()
	_, err := w.engine.Plan(w.t.Context(), by, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: changes})
	wantErr(w.t, err, mcp.ErrInvalidInput)
	if !strings.Contains(err.Error(), want) {
		w.t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestModelConnectionValidation(t *testing.T) {
	w := newWorld(t)
	base := func(k, v string) map[string]any {
		s := modelConn("store:models/a")
		s[k] = v
		return s
	}
	tests := []struct {
		name string
		res  string
		spec map[string]any
		want string
	}{
		{"bad name", "Bad Name", modelConn("store:models/a"), "lowercase"},
		{"no vendor", "a", base("vendor", ""), "vendor"},
		{"unknown adapter", "a", base("adapter", "other"), "adapter"},
		{"plain http", "a", base("base_url", "http://models.example.com"), "base_url"},
		{"another key variable", "a", base("key_env", "OTHER_KEY"), "key_env"},
		{"opencode without a variable", "a", base("adapter", "opencode"), "key_env"},
		{"an env reference", "a", base("secret_ref", "env:FORECREW_DATABASE_URL"), "secret_ref"},
		{"another namespace", "a", base("secret_ref", "store:conn-sso"), "secret_ref"},
		{"no reference", "a", base("secret_ref", ""), "secret_ref"},
		{"a runtime of the file", "claude", modelConn("store:models/a"), "server file"},
		{"a staged value", "a", base("secret_ref", "store:staged/x/y"), "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w.refused(w.owner, tt.want, wanted(t, "model_connection", tt.res, tt.spec))
		})
	}
	bad := base("adapter", "opencode")
	bad["key_env"] = "PATH"
	w.refused(w.owner, "key_env", wanted(t, "model_connection", "a", bad))
	bad["key_env"] = "FORECREW_SECRET_KEY"
	w.refused(w.owner, "key_env", wanted(t, "model_connection", "a", bad))
	w.refused(w.owner, "spec", mcp.ConfigWanted{Kind: "model_connection", Name: "a", Spec: json.RawMessage(`[]`)})
	w.refused(w.owner, "spec", wanted(t, "model_connection", "a", map[string]any{"vendor": "acme", "nope": 1}))
}

func TestModelKeysStayWithModelConnections(t *testing.T) {
	w := newWorld(t)
	oidc := map[string]any{"kind": "oidc", "issuer": "https://evil.example.com", "client_id": "abc", "client_secret_ref": "store:models/acme", "jit": map[string]any{"enabled": false}, "enabled": true}
	w.refused(w.owner, "only a model connection", wanted(t, "connection", "evil", oidc))
}

func TestAConnectionStagesItsKeyAndLowersProtection(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	p := w.plan(w.owner, wanted(t, "model_connection", "acme", modelConn("store:models/acme")))
	if len(p.LowersProtection) != 1 || p.ConfirmWord != "acme" || len(p.Steps) != 1 || p.Steps[0].Kind != domain.ConfigStepSecret {
		t.Fatalf("plan = %+v", p)
	}
	_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
	wantErr(t, err, mcp.ErrConflict) // the key is still empty
	_, err = w.engine.PutSecret(ctx, w.owner, p.ID, "models/acme", []byte("sk-value"))
	check(t, err)
	if has, _ := w.vault.Has(ctx, w.account.ID, "models/acme"); has {
		t.Fatal("the key is live before anyone confirmed")
	}
	_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord})
	wantErr(t, err, mcp.ErrConflict) // a token does not confirm it
	w.apply(w.owner, p)
	if got, _ := w.vault.Get(ctx, w.account.ID, "models/acme"); string(got) != "sk-value" {
		t.Fatalf("key = %q", got)
	}
	if got := w.spec("model_connection", "acme"); strings.Contains(got, "sk-value") || !strings.Contains(got, `"enabled":true`) {
		t.Fatalf("spec = %s", got)
	}
}

func TestEveryChangeOfEndpointOrKeyNeedsTheBrowser(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/other", []byte("k")))
	change := func(k string, v any) map[string]any {
		s := modelConn("store:models/acme")
		s[k] = v
		return s
	}
	for k, v := range map[string]any{"base_url": "https://models.example.com", "secret_ref": "store:models/other", "adapter": "codex"} {
		if p := w.plan(w.owner, wanted(t, "model_connection", "acme", change(k, v))); len(p.LowersProtection) == 0 {
			t.Errorf("%s: plan = %+v", k, p)
		}
	}
	// Turning off stops traffic; turning on starts it again. Here the role
	// that used it moves back to the server file, which is a move.
	off := w.plan(w.owner, wanted(t, "model_assignment", "organization", map[string]any{}), wanted(t, "model_connection", "acme", change("enabled", false)))
	if len(off.LowersProtection) != 1 || !strings.Contains(off.LowersProtection[0], "developer") {
		t.Fatalf("turning off = %v", off.LowersProtection)
	}
	w.apply(w.owner, off)
	if p := w.plan(w.owner, wanted(t, "model_connection", "acme", change("enabled", true))); len(p.LowersProtection) != 1 {
		t.Fatalf("turning on = %v", p.LowersProtection)
	}
	// A vendor of a connection only picks models.
	if p := w.plan(w.owner, wanted(t, "model_connection", "acme", map[string]any{"vendor": "zeta", "adapter": "claude-code", "secret_ref": "store:models/acme", "enabled": false})); len(p.LowersProtection) != 0 {
		t.Fatalf("vendor = %v", p.LowersProtection)
	}
}

func TestRotationKeepsTheOldValue(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.connect("acme")
	p := w.plan(w.owner, wanted(t, "model_connection", "acme", modelConn("store:models/acme-2")))
	_, err := w.engine.PutSecret(ctx, w.owner, p.ID, "models/acme-2", []byte("sk-new"))
	check(t, err)
	w.apply(w.owner, p)
	if got, _ := w.vault.Get(ctx, w.account.ID, "models/acme"); string(got) != "sk-value" {
		t.Fatalf("the old value = %q", got)
	}
	if !strings.Contains(w.spec("model_connection", "acme"), "store:models/acme-2") {
		t.Fatal(w.spec("model_connection", "acme"))
	}
	// A saved name is reused, never replaced.
	q := w.plan(w.owner, wanted(t, "model_connection", "acme", modelConn("store:models/acme")))
	if !q.Steps[0].Reuses {
		t.Fatalf("steps = %+v", q.Steps)
	}
}

func TestDeletingAUsedConnection(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	project := assignment("acme")
	delete(project, "tiers")
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_assignment", "project:app", project)))
	del := mcp.ConfigWanted{Kind: "model_connection", Name: "acme", Delete: true}
	w.refused(w.owner, "does not exist", del)
	off := modelConn("store:models/acme")
	off["enabled"] = false
	w.refused(w.owner, "turned off", wanted(t, "model_connection", "acme", off))
	_, err := w.engine.Plan(t.Context(), w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{del}})
	for _, part := range []string{"the organization", "project app", "developer"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("%q not in %v", part, err)
		}
	}

	// The plan works when it reassigns the roles, and says whom it touches.
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/next", []byte("k")))
	p := w.plan(w.owner, del, wanted(t, "model_connection", "next", modelConn("store:models/next")),
		wanted(t, "model_assignment", "organization", assignment("next")),
		mcp.ConfigWanted{Kind: "model_assignment", Name: "project:app", Delete: true})
	for _, part := range []string{"developer (the organization)", "developer (project app)"} {
		if !strings.Contains(p.Affects, part) {
			t.Fatalf("affects = %q", p.Affects)
		}
	}
	w.apply(w.owner, p)
	if w.spec("model_connection", "acme") != "" || !strings.Contains(w.spec("model_assignment", "organization"), "next") {
		t.Fatal("not applied")
	}
}

func TestARoleAssignedSinceThePlanStopsIt(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/spare", []byte("k")))
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "spare", modelConn("store:models/spare"))))
	stale := w.plan(w.owner, mcp.ConfigWanted{Kind: "model_connection", Name: "spare", Delete: true})
	project := assignment("spare")
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_assignment", "project:app", project)))
	_, err := w.engine.Apply(t.Context(), w.owner, resource.Confirmation{Plan: stale.ID, Word: stale.ConfirmWord, Browser: true})
	wantErr(t, err, mcp.ErrInvalidInput)
	if w.spec("model_connection", "spare") == "" {
		t.Fatal("the connection is gone")
	}
}

func TestAssignmentValidation(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	role := func(tier string) map[string]any {
		return map[string]any{"roles": map[string]any{"developer": map[string]any{"connection": "acme", "tier": tier}}}
	}
	w.refused(w.owner, "the name is", wanted(t, "model_assignment", "app", assignment("acme")))
	w.refused(w.owner, "does not exist", wanted(t, "model_assignment", "project:nope", assignment("acme")))
	w.refused(w.owner, "tier", wanted(t, "model_assignment", "organization", role("huge")))
	w.refused(w.owner, "tier", wanted(t, "model_assignment", "organization", map[string]any{"tiers": map[string]any{"huge": map[string]any{"a": "b"}}}))
	w.refused(w.owner, "a vendor and a model", wanted(t, "model_assignment", "organization", map[string]any{"tiers": map[string]any{"fast": map[string]any{"a": ""}}}))
	w.refused(w.owner, "connection is required", wanted(t, "model_assignment", "organization", map[string]any{"roles": map[string]any{"developer": map[string]any{"tier": "fast"}}}))
	w.refused(w.owner, "no strong model", wanted(t, "model_assignment", "organization", role("strong")))
	w.refused(w.owner, "a policy of the organization", wanted(t, "model_assignment", "project:app", map[string]any{"project_overrides": true}))
}

func TestAssignmentOfRolesKnowsTheRoles(t *testing.T) {
	w := newWorld(t)
	w.engine = resource.NewEngine(resource.Config{Store: w.st, Secrets: w.vault, Roles: func(role string) (bool, bool) {
		return role == "security", role == "security" || role == "developer"
	}})
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/oc", []byte("k")))
	oc := map[string]any{"vendor": "acme", "adapter": "opencode", "key_env": "ACME_KEY", "secret_ref": "store:models/oc"}
	on := func(role string) map[string]any {
		return map[string]any{"tiers": map[string]any{"fast": map[string]any{"acme": "acme/m"}}, "roles": map[string]any{role: map[string]any{"connection": "oc", "tier": "fast"}}}
	}
	w.refused(w.owner, "there is no role", wanted(t, "model_connection", "oc", oc), wanted(t, "model_assignment", "organization", on("ghost")))
	w.refused(w.owner, "opencode does not run in a toolbox", wanted(t, "model_connection", "oc", oc), wanted(t, "model_assignment", "organization", on("security")))
	w.plan(w.owner, wanted(t, "model_connection", "oc", oc), wanted(t, "model_assignment", "organization", on("developer")))
}

func TestSeparationOfVendors(t *testing.T) {
	w := newWorld(t)
	strict := *w.server
	strict.Policy.AllowSameVendorReview = false
	w.engine = resource.NewEngine(resource.Config{Store: w.st, Server: &strict, Getenv: env, Secrets: w.vault})
	check(t, w.vault.Put(t.Context(), w.owner, w.account.ID, "models/a", []byte("k")))
	w.refused(w.owner, "same", wanted(t, "model_connection", "a", modelConn("store:models/a")), wanted(t, "model_assignment", "organization", assignment("a", "reviewer")))
	other := modelConn("store:models/a")
	other["vendor"] = "zeta"
	w.plan(w.owner, wanted(t, "model_connection", "a", modelConn("store:models/a")), wanted(t, "model_connection", "b", other),
		wanted(t, "model_assignment", "organization", map[string]any{"tiers": assignment("a")["tiers"], "roles": map[string]any{
			"developer": map[string]any{"connection": "a", "tier": "strong"}, "reviewer": map[string]any{"connection": "b", "tier": "strong"}}}))
}

func TestReviewerThroughARouterOfTheDevelopersMaker(t *testing.T) {
	w := newWorld(t)
	strict := *w.server
	strict.Policy.AllowSameVendorReview = false
	w.engine = resource.NewEngine(resource.Config{Store: w.st, Server: &strict, Getenv: env, Secrets: w.vault})
	direct := map[string]any{"vendor": "anthropic", "adapter": "claude-code", "secret_ref": "store:models/a"}
	router := map[string]any{"vendor": "openrouter", "adapter": "opencode", "key_env": "OPENROUTER_API_KEY", "secret_ref": "store:models/r"}
	roles := map[string]any{"developer": map[string]any{"connection": "a", "tier": "strong"}, "reviewer": map[string]any{"connection": "r", "tier": "best"}}
	tiers := func(reviewer string) map[string]any {
		return map[string]any{"strong": map[string]any{"anthropic": "claude-sonnet-4-5"}, "best": map[string]any{"openrouter": reviewer}}
	}
	w.refused(w.owner, `models of "anthropic"`, wanted(t, "model_connection", "a", direct), wanted(t, "model_connection", "r", router),
		wanted(t, "model_assignment", "organization", map[string]any{"tiers": tiers("anthropic/claude-opus-4.5"), "roles": roles}))
	w.plan(w.owner, wanted(t, "model_connection", "a", direct), wanted(t, "model_connection", "r", router),
		wanted(t, "model_assignment", "organization", map[string]any{"tiers": tiers("openai/gpt-5"), "roles": roles}))
}

func TestProjectOverrides(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.connect("acme")

	// A project admin sets the override of their project and nothing else.
	admin := w.person(domain.RoleMember)
	testkit.Bind(t, w.st, w.account.ID, domain.SubjectUser, mustUUID(t, admin.Name), domain.RoleScopeProject, domain.RoleProjectAdmin, w.project.ID)
	over := wanted(t, "model_assignment", "project:app", assignment("acme"))
	w.apply(admin, w.plan(admin, over))
	if got := w.spec("model_assignment", "project:app"); got == "" {
		t.Fatal("no override")
	}
	org := w.plan(admin, wanted(t, "model_assignment", "organization", assignment("acme", "architect")))
	_, err := w.engine.Apply(ctx, admin, resource.Confirmation{Plan: org.ID})
	if err == nil {
		t.Fatal("a project admin changed the organization's assignment")
	}
	conn := w.plan(admin, mcp.ConfigWanted{Kind: "model_assignment", Name: "project:app", Delete: true})
	member := w.person(domain.RoleMember)
	if _, err := w.engine.Apply(ctx, member, resource.Confirmation{Plan: conn.ID}); err == nil {
		t.Fatal("a member changed a project's models")
	}

	// The policy of the organization stops new overrides.
	closed := assignment("acme")
	closed["project_overrides"] = false
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_assignment", "organization", closed)))
	w.refused(w.owner, "models.project_overrides", wanted(t, "model_assignment", "project:app", assignment("acme", "architect")))
}

func TestRepoFilesDoNotOverrideModels(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	body := "models:\n  developer: {connection: evil, tier: best}\n"
	check(t, w.tracker.SetFile("acme/app", "main", ".forecrew/models.yaml", []byte(body)))
	check(t, w.tracker.SetFile("acme/app", "main", ".forecrew/profile.yaml", []byte(body)))
	if got := w.spec("model_assignment", "project:app"); got != "" {
		t.Fatalf("a file set an override: %s", got)
	}
	if err := analyzer.Validate([]byte(body)); err == nil {
		t.Fatal("a profile holds models")
	}
}

func TestServerFileIsShownAsManaged(t *testing.T) {
	w := newWorld(t)
	rs, err := w.engine.Get(t.Context(), w.owner, "model_connection", "claude")
	check(t, err)
	if len(rs) != 1 || rs[0].Origin != resource.OriginFile || rs[0].Scope != resource.ScopeServer || strings.Contains(string(rs[0].Spec), "value") || !strings.Contains(string(rs[0].Spec), "ANTHROPIC_API_KEY") {
		t.Fatalf("resources = %+v", rs)
	}
	rs, err = w.engine.Get(t.Context(), w.owner, "model_assignment", "organization")
	check(t, err)
	if len(rs) != 1 || rs[0].Origin != resource.OriginFile || !strings.Contains(string(rs[0].Spec), `"connection":"claude"`) {
		t.Fatalf("resources = %+v", rs)
	}
	w.refused(w.owner, "forecrew.yaml", mcp.ConfigWanted{Kind: "model_connection", Name: "claude", Delete: true})
	w.refused(w.owner, "forecrew.yaml", mcp.ConfigWanted{Kind: "model_assignment", Name: "organization", Delete: true})

	// An organization's own row goes over it and comes off again.
	w.connect("acme")
	if !strings.Contains(w.spec("model_assignment", "organization"), `"connection":"acme"`) {
		t.Fatal(w.spec("model_assignment", "organization"))
	}
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "model_assignment", Name: "organization", Delete: true}))
	if !strings.Contains(w.spec("model_assignment", "organization"), `"connection":"claude"`) {
		t.Fatal(w.spec("model_assignment", "organization"))
	}
}

func TestHostedHidesTheServerFile(t *testing.T) {
	w := newWorld(t)
	off := false
	hosted := *w.server
	hosted.Models.ServerKeysForOrganizations = &off
	w.engine = resource.NewEngine(resource.Config{Store: w.st, Server: &hosted, Getenv: env, Secrets: w.vault})
	if got := w.spec("model_connection", "claude"); got != "" {
		t.Fatalf("spec = %s", got)
	}
	if got := w.spec("model_assignment", "organization"); got != "" {
		t.Fatalf("spec = %s", got)
	}
	w.refused(w.owner, "does not exist", wanted(t, "model_assignment", "organization", map[string]any{"roles": map[string]any{"developer": map[string]any{"connection": "claude", "tier": "strong"}}}))
}

func TestModelChangesAreAuditedWithoutValues(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.connect("acme")
	p := w.plan(w.owner, wanted(t, "model_connection", "acme", modelConn("store:models/acme-2")))
	_, err := w.engine.PutSecret(ctx, w.owner, p.ID, "models/acme-2", []byte("sk-rotated"))
	check(t, err)
	w.apply(w.owner, p)
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "model_assignment", Name: "organization", Delete: true}))
	off := modelConn("store:models/acme-2")
	off["enabled"] = false
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "acme", off)))
	off["base_url"] = "https://models.example.com"
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "acme", off)))
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "model_connection", Name: "acme", Delete: true}))

	entries, err := w.st.Audit().List(ctx, w.account.ID, store.AuditFilter{})
	check(t, err)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
		if strings.Contains(string(e.Data), "sk-") {
			t.Fatalf("%s holds a value: %s", e.Action, e.Data)
		}
	}
	for _, action := range []string{"model_connection.created", "model_connection.rotated", "model_connection.disabled", "model_connection.changed", "model_connection.deleted", "model_assignment.changed", "model_assignment.deleted"} {
		if !seen[action] {
			t.Errorf("no %s in %v", action, seen)
		}
	}
}

func TestExportShowsReferences(t *testing.T) {
	w := newWorld(t)
	w.connect("acme")
	out, err := w.engine.Export(t.Context(), w.owner)
	check(t, err)
	if !strings.Contains(out, "store:models/acme") || strings.Contains(out, "sk-value") {
		t.Fatalf("export:\n%s", out)
	}
	p, err := w.engine.Import(t.Context(), w.owner, out, domain.ConfigFromCLI, "")
	check(t, err)
	if len(p.Changes) != 0 {
		t.Fatalf("import of an export changes %+v", p.Changes)
	}
}

func TestReadingModelsTakesThePermission(t *testing.T) {
	w := newWorld(t)
	billing := w.person(domain.RoleBilling)
	if rs, err := w.engine.Get(t.Context(), billing, "", ""); err != nil || strings.Contains(string(mustJSON(t, rs)), "model_connection") {
		t.Fatalf("resources = %v, %v", rs, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	check(t, err)
	return raw
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	check(t, err)
	return id
}

// TestMovingARoleNeedsTheBrowser: a plan that sends a role's code to another
// connection is confirmed only by a person in the web UI, so a token cannot
// apply it, whoever made it.
func TestMovingARoleNeedsTheBrowser(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.connect("acme")
	check(t, w.vault.Put(ctx, w.owner, w.account.ID, "models/other", []byte("k")))
	w.apply(w.owner, w.plan(w.owner, wanted(t, "model_connection", "other", modelConn("store:models/other"))))

	moves := map[string]mcp.ConfigWanted{
		"another connection": wanted(t, "model_assignment", "organization", assignment("other")),
		"a new role":         wanted(t, "model_assignment", "organization", assignment("acme", "architect")),
		"a project override": wanted(t, "model_assignment", "project:app", assignment("other")),
		"a role taken off":   wanted(t, "model_assignment", "organization", map[string]any{"tiers": assignment("acme")["tiers"]}),
	}
	for name, change := range moves {
		t.Run(name, func(t *testing.T) {
			p := w.plan(w.owner, change)
			if len(p.LowersProtection) != 1 || p.ConfirmWord != "acme" {
				t.Fatalf("plan = %+v", p)
			}
			_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord})
			wantErr(t, err, mcp.ErrConflict)
		})
	}
	// Tiers alone move no code.
	tiers := assignment("acme")
	tiers["tiers"] = map[string]any{"strong": map[string]any{"acme": "m3", "zeta": "m2"}}
	if p := w.plan(w.owner, wanted(t, "model_assignment", "organization", tiers)); len(p.LowersProtection) != 0 {
		t.Fatalf("tiers = %v", p.LowersProtection)
	}
	// Taking a project's override off moves its roles back.
	w.apply(w.owner, w.plan(w.owner, moves["a project override"]))
	if p := w.plan(w.owner, mcp.ConfigWanted{Kind: "model_assignment", Name: "project:app", Delete: true}); len(p.LowersProtection) != 1 {
		t.Fatalf("delete = %v", p.LowersProtection)
	}
}

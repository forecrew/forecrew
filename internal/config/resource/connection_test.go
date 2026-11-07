package resource_test

import (
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/signintest"
	"github.com/forecrew/forecrew/internal/testkit"
)

// Changing what a connection trusts lets the new provider decide who joins
// its mapped teams and who gets the roles it gave: it needs every permission
// of them (permissions.md, No escalation).
func TestConnectionTrustNeedsMappedTeamsAndOldDefaults(t *testing.T) {
	ctx := t.Context()
	w := newWorld(t)
	billing, err := w.st.Roles().GetByName(ctx, w.account.ID, domain.RoleScopeOrganization, domain.RoleBilling)
	check(t, err)
	conn := domain.Connection{AccountID: w.account.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://a.example.com", ClientID: "c",
		ClientSecretRef: "env:FORECREW_CONN_OKTA", JIT: domain.JIT{Enabled: true, DefaultRoles: []uuid.UUID{billing.ID}}, Enabled: true}
	check(t, w.st.Connections().Create(ctx, &conn))
	spec := func(issuer string, roles ...string) map[string]any {
		return map[string]any{"kind": "oidc", "issuer": issuer, "client_id": "c", "client_secret_ref": "env:FORECREW_CONN_OKTA",
			"jit": map[string]any{"enabled": true, "default_roles": roles}, "enabled": true}
	}
	admin := w.person(domain.RoleAdmin)
	// The new issuer has a test sign-in, so only trust and roles are in question.
	moved := conn
	moved.Issuer = "https://b.example.com"
	check(t, signintest.Record(ctx, w.st, w.owner, moved))
	apply := func(by domain.Actor, s map[string]any) error {
		t.Helper()
		p := w.plan(by, wanted(t, "connection", "okta", s))
		_, err := w.engine.Apply(ctx, by, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
		return err
	}

	// Dropping a default role the Admin lacks, with a new issuer.
	wantErr(t, apply(admin, spec("https://b.example.com")), authz.ErrDenied)
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", spec("https://a.example.com"))))

	// A team mapped to the connection holds Billing.
	team := domain.Team{AccountID: w.account.ID, Name: "Pay", Slug: "pay", Source: domain.SourceManual}
	check(t, w.st.Teams().Create(ctx, &team))
	testkit.Bind(t, w.st, w.account.ID, domain.SubjectTeam, team.ID, domain.RoleScopeOrganization, domain.RoleBilling, uuid.Nil())
	check(t, w.st.Connections().AddGroupMapping(ctx, &domain.GroupMapping{AccountID: w.account.ID, ConnectionID: conn.ID, ExternalGroup: "finance", TeamID: team.ID}))
	wantErr(t, apply(admin, spec("https://b.example.com")), authz.ErrDenied)
	g := spec("https://a.example.com")
	g["group_claim"] = "groups"
	wantErr(t, apply(admin, g), authz.ErrDenied)

	// Only a field that does not change trust passes, and an Owner passes.
	check(t, apply(w.owner, spec("https://b.example.com")))
}

func oidc(issuer, clientID string, enabled bool) map[string]any {
	return map[string]any{"kind": "oidc", "issuer": issuer, "client_id": clientID, "client_secret_ref": "env:FORECREW_CONN_OKTA",
		"jit": map[string]any{"enabled": false}, "enabled": enabled}
}

// A plan, like the other ways, turns a connection on only after a test
// sign-in worked for its issuer and client ID, and whoever wrote the plan.
func TestPlanEnablesConnectionOnlyAfterTest(t *testing.T) {
	ctx := t.Context()
	w := newWorld(t)
	check(t, w.st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: w.account.ID, Name: "laptop", TokenHash: mcp.HashToken("fcm_laptop")}))
	token := domain.Actor{AccountID: w.account.ID, Kind: domain.ActorMCP, Name: "laptop"}
	planErr := func(by domain.Actor, s map[string]any) error {
		t.Helper()
		_, err := w.engine.Plan(ctx, by, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "connection", "okta", s)}})
		return err
	}
	a := "https://a.example.com"

	// Created on, by a person or by a token: refused. Created off: fine.
	wantErr(t, planErr(w.owner, oidc(a, "c1", true)), mcp.ErrConflict)
	wantErr(t, planErr(token, oidc(a, "c1", true)), mcp.ErrConflict)
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(a, "c1", false))))

	// Turned on with no test, or with a test of other values: refused.
	wantErr(t, planErr(w.owner, oidc(a, "c1", true)), mcp.ErrConflict)
	wantErr(t, planErr(token, oidc(a, "c1", true)), mcp.ErrConflict)
	w.connectionTested("okta")
	wantErr(t, planErr(w.owner, oidc(a, "c2", true)), mcp.ErrConflict)
	check(t, planErr(token, oidc(a, "c1", true)))
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(a, "c1", true))))

	// The issuer of a connection that is on changes: refused while it stays on,
	// the test is gone once it is saved off, also after changing back.
	b := "https://b.example.com"
	wantErr(t, planErr(w.owner, oidc(b, "c1", true)), mcp.ErrConflict)
	wantErr(t, planErr(token, oidc(b, "c1", true)), mcp.ErrConflict)
	wantErr(t, planErr(w.owner, oidc(a, "c2", true)), mcp.ErrConflict)
	valid := func() bool {
		t.Helper()
		rows, err := w.st.Connections().List(ctx, w.account.ID)
		check(t, err)
		ok, err := signintest.Valid(ctx, w.st, rows[0])
		check(t, err)
		return ok
	}
	if !valid() {
		t.Fatal("tested connection is not valid")
	}
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(b, "c1", false))))
	if valid() {
		t.Fatal("test survived a new issuer")
	}
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(a, "c1", false))))
	if valid() {
		t.Fatal("test came back with the old issuer")
	}
	wantErr(t, planErr(w.owner, oidc(a, "c1", true)), mcp.ErrConflict)
	w.connectionTested("okta")
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(a, "c1", true))))

	// Another client ID is the same, and turning it off needs no test.
	wantErr(t, planErr(w.owner, oidc(a, "c3", true)), mcp.ErrConflict)
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", oidc(a, "c3", false))))
	if valid() {
		t.Fatal("test survived a new client ID")
	}
}

// A connection that is on and whose test is not asked about again (it came
// from before the rule) stays on while a plan changes something else.
func TestPlanKeepsUntestedEnabledConnection(t *testing.T) {
	ctx := t.Context()
	w := newWorld(t)
	conn := domain.Connection{AccountID: w.account.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://a.example.com", ClientID: "c1",
		ClientSecretRef: "env:FORECREW_CONN_OKTA", Enabled: true}
	check(t, w.st.Connections().Create(ctx, &conn))
	s := oidc(conn.Issuer, "c1", true)
	s["group_claim"] = "groups"
	w.apply(w.owner, w.plan(w.owner, wanted(t, "connection", "okta", s)))
	_, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI,
		Changes: []mcp.ConfigWanted{wanted(t, "connection", "okta", oidc("https://b.example.com", "c1", true))}})
	wantErr(t, err, mcp.ErrConflict)
}

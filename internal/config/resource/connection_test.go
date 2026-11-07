package resource_test

import (
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
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

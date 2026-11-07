//go:build enterprise && integration

package ee

import (
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestRecordedSequencesOnPostgres replays the sequences on the real store,
// whose triggers and unique indexes the fake only imitates.
func TestRecordedSequencesOnPostgres(t *testing.T) {
	for _, file := range []string{"testdata/scim/okta.json", "testdata/scim/entra.json"} {
		t.Run(file, func(t *testing.T) {
			ctx := t.Context()
			pool := testkit.NewDatabase(t)
			must(t, postgres.Migrate(ctx, pool))
			e := newEnvOn(t, postgres.New(pool), testkit.NewClock(time.Now()))
			e.verified("acme.example")
			var client, grant uuid.UUID
			must(t, pool.QueryRow(ctx, `INSERT INTO oauth_clients (client_id, name, redirect_uris) VALUES ('c', 'c', '{https://x/cb}') RETURNING id`).Scan(&client))
			e.scim().replay(file, e.accessHooks(func(user uuid.UUID) {
				must(t, pool.QueryRow(ctx, `INSERT INTO oauth_grants (kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, expires_at)
					VALUES ('access', $1, uuidv7(), $2, $3, $4, '{read}', 'https://x/mcp', $5) RETURNING id`, mcp.HashToken(user.String()), client, user, e.org.ID, time.Now().Add(time.Hour)).Scan(&grant))
			}, func() bool {
				var revoked bool
				must(t, pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM oauth_grants WHERE id = $1`, grant).Scan(&revoked))
				return revoked
			}))
		})
	}
}

// A mapped team can still be deleted, by SCIM or anyone else.
func TestDeletingAMappedTeamOnPostgres(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(ctx, pool))
	e := newEnvOn(t, postgres.New(pool), testkit.NewClock(time.Now()))
	c := e.connection("okta")
	x := e.team("Core", domain.SourceSCIM)
	_, err := e.svc.AddGroupMapping(ctx, e.owner, mcp.AddGroupMappingInput{Connection: c.Name, Group: "g", Team: x.Slug})
	must(t, err)
	must(t, e.st.Teams().Delete(ctx, e.org.ID, x.ID))
	list, err := e.svc.ListGroupMappings(ctx, e.owner)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("mappings = %+v", list)
	}
}

// Provisioning and custom roles against the real tables and triggers.
func TestRolesAndProvisioningOnPostgres(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(ctx, pool))
	e := newEnvOn(t, postgres.New(pool), testkit.NewClock(time.Now()))
	c := e.connection("okta")
	r, err := e.svc.CreateRole(ctx, e.owner, mcp.CreateRoleInput{Name: "reader", Scope: domain.RoleScopeOrganization, Permissions: []domain.Permission{domain.PermOrgRead}})
	must(t, err)
	on, claim, roles := true, "groups", []string{"reader"}
	p, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: c.Name, JITEnabled: &on, DefaultRoles: &roles, GroupClaim: &claim})
	must(t, err)
	stored, err := e.st.Connections().Get(ctx, c.ID)
	must(t, err)
	if !stored.JIT.Enabled || len(stored.JIT.DefaultRoles) != 1 || stored.JIT.DefaultRoles[0] != r.ID || stored.GroupClaim != p.GroupClaim {
		t.Fatalf("stored = %+v", stored)
	}
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "reader"}); !isConflict(err) {
		t.Fatalf("a JIT default: %v", err)
	}
	u := domain.User{Name: "Ann", Email: "ann@acme.example"}
	must(t, e.st.Users().Create(ctx, &u))
	must(t, e.svc.Admit(ctx, e.st, stored, auth.Claims{}, u))
	m, err := e.st.Memberships().Get(ctx, e.org.ID, u.ID)
	must(t, err)
	if m.Source != domain.SourceJIT {
		t.Fatalf("membership = %+v", m)
	}
	roles = []string{}
	if _, err := e.svc.SetConnectionProvisioning(ctx, e.owner, mcp.SetProvisioningInput{Connection: c.Name, DefaultRoles: &roles}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DeleteRole(ctx, e.owner, mcp.RoleRefInput{Role: "reader"}); !isConflict(err) {
		t.Fatalf("still bound: %v", err)
	}
}

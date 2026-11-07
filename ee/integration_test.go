//go:build enterprise && integration

package ee

import (
	"testing"
	"time"
	"uuid"

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

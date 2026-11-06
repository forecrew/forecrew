//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestIdentityStoreSuite(t *testing.T) {
	f := newFixture(t)
	testkit.IdentityStoreSuite(t, f.st, f.account)
}

func TestOAuthGrantsRevokeByUser(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	u := domain.User{Name: "Ann"}
	must(t, f.st.Users().Create(ctx, &u))
	must(t, f.st.Memberships().Create(ctx, &domain.Membership{AccountID: f.account, UserID: u.ID, Status: domain.MembershipActive, Source: domain.SourceManual}))
	var client, grant string
	must(t, f.pool.QueryRow(ctx, `INSERT INTO oauth_clients (client_id, name, redirect_uris) VALUES ('c', 'c', '{https://x/cb}') RETURNING id`).Scan(&client))
	must(t, f.pool.QueryRow(ctx, `INSERT INTO oauth_grants (kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, expires_at)
		VALUES ('access', $1, uuidv7(), $2, $3, $4, '{read}', 'https://x/mcp', $5) RETURNING id`, make([]byte, 32), client, u.ID, f.account, time.Now().Add(time.Hour)).Scan(&grant))
	must(t, f.st.OAuthGrants().RevokeByUser(ctx, f.account, u.ID))
	var revoked bool
	must(t, f.pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM oauth_grants WHERE id = $1`, grant).Scan(&revoked))
	if !revoked {
		t.Fatal("the grant is not revoked")
	}
}

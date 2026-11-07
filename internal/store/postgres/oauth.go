package postgres

import (
	"context"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// OAuth implements store.Store.
func (s *Store) OAuth() store.OAuthRepo { return oauthRepo{s.db} }

type oauthRepo struct{ db dbtx }

var oauthClientCols = `id, client_id, name, redirect_uris, secret_hash, ` + ctime("last_used_at") + `, created_at`

func scanOAuthClient(row pgx.Row) (c domain.OAuthClient, err error) {
	err = row.Scan(&c.ID, &c.ClientID, &c.Name, &c.RedirectURIs, &c.SecretHash, &c.LastUsedAt, &c.CreatedAt)
	utc(&c.LastUsedAt, &c.CreatedAt)
	return c, err
}

func (r oauthRepo) CreateClient(ctx context.Context, c *domain.OAuthClient) error {
	c.ID = newID(c.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO oauth_clients (id, client_id, name, redirect_uris, secret_hash) VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
		c.ID, c.ClientID, c.Name, c.RedirectURIs, c.SecretHash).Scan(&c.CreatedAt))
}

func (r oauthRepo) GetClient(ctx context.Context, clientID string) (domain.OAuthClient, error) {
	return one(ctx, r.db, scanOAuthClient, `SELECT `+oauthClientCols+` FROM oauth_clients WHERE client_id = $1`, clientID)
}

func (r oauthRepo) GetClientByID(ctx context.Context, id uuid.UUID) (domain.OAuthClient, error) {
	return one(ctx, r.db, scanOAuthClient, `SELECT `+oauthClientCols+` FROM oauth_clients WHERE id = $1`, id)
}

func (r oauthRepo) UpdateClient(ctx context.Context, c *domain.OAuthClient) error {
	return mustUpdate(ctx, r.db, `UPDATE oauth_clients SET name = $2, redirect_uris = $3 WHERE id = $1`, c.ID, c.Name, c.RedirectURIs)
}

func (r oauthRepo) TouchClient(ctx context.Context, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE oauth_clients SET last_used_at = now() WHERE id = $1`, id)
}

func (r oauthRepo) DeleteUnusedClients(ctx context.Context, t time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM oauth_clients c WHERE COALESCE(c.last_used_at, c.created_at) < $1
		AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = c.id)`, t.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

func (r oauthRepo) CountUnusedClients(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM oauth_clients c WHERE c.last_used_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = c.id)`).Scan(&n)
	return n, mapErr(err)
}

var oauthGrantCols = `id, kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, redirect_uri, code_challenge, expires_at, ` +
	ctime("used_at") + `, ` + ctime("revoked_at") + `, created_at`

func scanOAuthGrant(row pgx.Row) (g domain.OAuthGrant, err error) {
	err = row.Scan(&g.ID, &g.Kind, &g.TokenHash, &g.FamilyID, &g.ClientID, &g.UserID, &g.AccountID, &g.Scopes, &g.Resource, &g.RedirectURI, &g.CodeChallenge,
		&g.ExpiresAt, &g.UsedAt, &g.RevokedAt, &g.CreatedAt)
	utc(&g.ExpiresAt, &g.UsedAt, &g.RevokedAt, &g.CreatedAt)
	return g, err
}

func (r oauthRepo) CreateGrant(ctx context.Context, g *domain.OAuthGrant) error {
	g.ID = newID(g.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO oauth_grants (id, kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, redirect_uri, code_challenge, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING created_at`,
		g.ID, g.Kind, g.TokenHash, g.FamilyID, g.ClientID, g.UserID, g.AccountID, g.Scopes, g.Resource, g.RedirectURI, g.CodeChallenge, g.ExpiresAt.UTC()).Scan(&g.CreatedAt))
}

func (r oauthRepo) GetGrant(ctx context.Context, id uuid.UUID) (domain.OAuthGrant, error) {
	return one(ctx, r.db, scanOAuthGrant, `SELECT `+oauthGrantCols+` FROM oauth_grants WHERE id = $1`, id)
}

func (r oauthRepo) GetGrantByTokenHash(ctx context.Context, hash []byte) (domain.OAuthGrant, error) {
	return one(ctx, r.db, scanOAuthGrant, `SELECT `+oauthGrantCols+` FROM oauth_grants WHERE token_hash = $1`, hash)
}

func (r oauthRepo) UseGrant(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE oauth_grants SET used_at = now() WHERE id = $1 AND kind <> 'access' AND used_at IS NULL`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	return nil
}

func (r oauthRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE oauth_grants SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	return mapErr(err)
}

func (r oauthRepo) ListFamilies(ctx context.Context, accountID, userID uuid.UUID) ([]domain.OAuthGrant, error) {
	return many(ctx, r.db, scanOAuthGrant, `SELECT `+oauthGrantCols+` FROM (
		SELECT DISTINCT ON (family_id) * FROM oauth_grants
		WHERE account_id = $1 AND user_id = $2 AND kind <> 'code' AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		ORDER BY family_id, created_at DESC, id DESC
	) f ORDER BY created_at DESC, id DESC`, accountID, userID)
}

func (r oauthRepo) UserAuthorized(ctx context.Context, clientID, userID uuid.UUID) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM oauth_grants WHERE client_id = $1 AND user_id = $2)`, clientID, userID).Scan(&ok)
	return ok, mapErr(err)
}

func (r oauthRepo) RevokeByUser(ctx context.Context, accountID, userID uuid.UUID) error {
	w := (&where{}).and("user_id = ?", userID).and("revoked_at IS NULL AND true = ?", true)
	andNot(w, "account_id = ?", accountID)
	_, err := r.db.Exec(ctx, `UPDATE oauth_grants SET revoked_at = now() WHERE `+strings.Join(w.conds, " AND "), w.args...)
	return mapErr(err)
}

func (r oauthRepo) DeleteExpiredGrants(ctx context.Context, t time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM oauth_grants WHERE expires_at < $1`, t.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

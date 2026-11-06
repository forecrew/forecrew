package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Users implements store.Store.
func (s *Store) Users() store.UserRepo { return userRepo{s.db} }

// Identities implements store.Store.
func (s *Store) Identities() store.IdentityRepo { return identityRepo{s.db} }

// Memberships implements store.Store.
func (s *Store) Memberships() store.MembershipRepo { return membershipRepo{s.db} }

// Invitations implements store.Store.
func (s *Store) Invitations() store.InvitationRepo { return invitationRepo{s.db} }

// Connections implements store.Store.
func (s *Store) Connections() store.ConnectionRepo { return connectionRepo{s.db} }

// VerifiedDomains implements store.Store.
func (s *Store) VerifiedDomains() store.VerifiedDomainRepo { return verifiedDomainRepo{s.db} }

// Sessions implements store.Store.
func (s *Store) Sessions() store.SessionRepo { return sessionRepo{s.db} }

// OAuthGrants implements store.Store.
func (s *Store) OAuthGrants() store.OAuthGrantRepo { return oauthGrantRepo{s.db} }

// SCIMTokens implements store.Store.
func (s *Store) SCIMTokens() store.SCIMTokenRepo { return scimTokenRepo{s.db} }

// ---- users

type userRepo struct{ db dbtx }

func (r userRepo) Create(ctx context.Context, u *domain.User) error {
	u.ID = newID(u.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO users (id, name, email, email_verified, instance_admin) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`, u.ID, u.Name, opt(u.Email), u.EmailVerified, u.InstanceAdmin).Scan(&u.CreatedAt, &u.UpdatedAt))
}

func (r userRepo) Get(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return one(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
}

func (r userRepo) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]domain.User, error) {
	return many(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE id = ANY($1)`, ids)
}

func (r userRepo) GetByVerifiedEmail(ctx context.Context, email string) (domain.User, error) {
	return one(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE lower(email) = lower($1) AND email_verified`, email)
}

func (r userRepo) ListByEmail(ctx context.Context, email string) ([]domain.User, error) {
	return many(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE lower(email) = lower($1) ORDER BY created_at, id`, email)
}

func (r userRepo) ListInstanceAdmins(ctx context.Context) ([]domain.User, error) {
	return many(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE instance_admin ORDER BY created_at, id`)
}

func (r userRepo) Update(ctx context.Context, u *domain.User) error {
	return mustUpdate(ctx, r.db, `UPDATE users SET name = $2, email = $3, email_verified = $4, instance_admin = $5, updated_at = now() WHERE id = $1`,
		u.ID, u.Name, opt(u.Email), u.EmailVerified, u.InstanceAdmin)
}

func (r userRepo) Seen(ctx context.Context, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE users SET last_seen_at = now() WHERE id = $1`, id)
}

func (r userRepo) SetDisabled(ctx context.Context, id uuid.UUID, disabled bool) error {
	return mustUpdate(ctx, r.db, `UPDATE users SET disabled_at = CASE WHEN $2 THEN COALESCE(disabled_at, now()) END, updated_at = now() WHERE id = $1`, id, disabled)
}

// ---- identities

type identityRepo struct{ db dbtx }

var identityCols = `id, user_id, connection_id, subject, login, email, ` + ctime("last_used_at") + `, created_at`

func scanIdentity(row pgx.Row) (i domain.Identity, err error) {
	err = row.Scan(&i.ID, &i.UserID, &i.ConnectionID, &i.Subject, &i.Login, &i.Email, &i.LastUsedAt, &i.CreatedAt)
	utc(&i.LastUsedAt, &i.CreatedAt)
	return i, err
}

func (r identityRepo) Create(ctx context.Context, i *domain.Identity) error {
	i.ID = newID(i.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO identities (id, user_id, connection_id, subject, login, email) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`, i.ID, i.UserID, i.ConnectionID, i.Subject, i.Login, i.Email).Scan(&i.CreatedAt))
}

func (r identityRepo) GetBySubject(ctx context.Context, connectionID uuid.UUID, subject string) (domain.Identity, error) {
	return one(ctx, r.db, scanIdentity, `SELECT `+identityCols+` FROM identities WHERE connection_id = $1 AND subject = $2`, connectionID, subject)
}

func (r identityRepo) ListByLogin(ctx context.Context, connectionID uuid.UUID, login string) ([]domain.Identity, error) {
	return many(ctx, r.db, scanIdentity, `SELECT `+identityCols+` FROM identities WHERE connection_id = $1 AND login <> '' AND lower(login) = lower($2) ORDER BY created_at, id`, connectionID, login)
}

func (r identityRepo) ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Identity, error) {
	return many(ctx, r.db, scanIdentity, `SELECT `+identityCols+` FROM identities WHERE user_id = $1 ORDER BY created_at, id`, userID)
}

func (r identityRepo) Seen(ctx context.Context, id uuid.UUID, login, email string) error {
	return mustUpdate(ctx, r.db, `UPDATE identities SET last_used_at = now(), login = $2, email = $3 WHERE id = $1`, id, login, email)
}

func (r identityRepo) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM identities WHERE user_id = $1 AND id = $2`, userID, id)
}

// ---- memberships

type membershipRepo struct{ db dbtx }

var membershipCols = `id, account_id, user_id, status, source, ` + ctext("external_id") + `, created_at, updated_at`

func scanMembership(row pgx.Row) (m domain.Membership, err error) {
	err = row.Scan(&m.ID, &m.AccountID, &m.UserID, &m.Status, &m.Source, &m.ExternalID, &m.CreatedAt, &m.UpdatedAt)
	utc(&m.CreatedAt, &m.UpdatedAt)
	return m, err
}

func (r membershipRepo) Create(ctx context.Context, m *domain.Membership) error {
	m.ID = newID(m.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO memberships (id, account_id, user_id, status, source, external_id) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`, m.ID, m.AccountID, m.UserID, m.Status, m.Source, opt(m.ExternalID)).Scan(&m.CreatedAt, &m.UpdatedAt))
}

func (r membershipRepo) Get(ctx context.Context, accountID, userID uuid.UUID) (domain.Membership, error) {
	return one(ctx, r.db, scanMembership, `SELECT `+membershipCols+` FROM memberships WHERE account_id = $1 AND user_id = $2`, accountID, userID)
}

func (r membershipRepo) GetByExternalID(ctx context.Context, accountID uuid.UUID, externalID string) (domain.Membership, error) {
	return one(ctx, r.db, scanMembership, `SELECT `+membershipCols+` FROM memberships WHERE account_id = $1 AND external_id = $2`, accountID, externalID)
}

func (r membershipRepo) List(ctx context.Context, accountID uuid.UUID, f store.MembershipFilter) ([]domain.Membership, error) {
	w := newWhere(accountID)
	andNot(w, "status = ?", f.Status)
	return many(ctx, r.db, scanMembership, `SELECT `+membershipCols+` FROM memberships`+w.sql("created_at, id", f.Limit), w.args...)
}

func (r membershipRepo) ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Membership, error) {
	return many(ctx, r.db, scanMembership, `SELECT `+membershipCols+` FROM memberships WHERE user_id = $1 ORDER BY created_at, id`, userID)
}

func (r membershipRepo) SetStatus(ctx context.Context, accountID, userID uuid.UUID, status domain.MembershipStatus) error {
	return mustUpdate(ctx, r.db, `UPDATE memberships SET status = $3, updated_at = now() WHERE account_id = $1 AND user_id = $2`, accountID, userID, status)
}

func (r membershipRepo) SetExternalID(ctx context.Context, accountID, userID uuid.UUID, externalID string) error {
	return mustUpdate(ctx, r.db, `UPDATE memberships SET external_id = $3, updated_at = now() WHERE account_id = $1 AND user_id = $2`, accountID, userID, opt(externalID))
}

func (r membershipRepo) Remove(ctx context.Context, accountID, userID uuid.UUID) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE account_id = $1 AND user_id = $2`, accountID, userID); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE account_id = $1 AND subject_kind = 'user' AND subject_id = $2`, accountID, userID); err != nil {
			return mapErr(err)
		}
		return mustUpdate(ctx, tx, `DELETE FROM memberships WHERE account_id = $1 AND user_id = $2`, accountID, userID)
	})
}

// ---- invitations

type invitationRepo struct{ db dbtx }

var invitationCols = `id, account_id, ` + ctext("email") + `, ` + ctext("github_login") + `, ` + cint("github_user_id") + `, roles, token_hash, expires_at, invited_by, ` +
	cuuid("accepted_by") + `, ` + ctime("accepted_at") + `, ` + ctime("revoked_at") + `, created_at`

func scanInvitation(row pgx.Row) (i domain.Invitation, err error) {
	var roles []byte
	if err = row.Scan(&i.ID, &i.AccountID, &i.Email, &i.GitHubLogin, &i.GitHubUserID, &roles, &i.TokenHash, &i.ExpiresAt, &i.InvitedBy,
		&i.AcceptedBy, &i.AcceptedAt, &i.RevokedAt, &i.CreatedAt); err != nil {
		return i, err
	}
	utc(&i.ExpiresAt, &i.AcceptedAt, &i.RevokedAt, &i.CreatedAt)
	return i, json.Unmarshal(roles, &i.Grants)
}

func (r invitationRepo) Create(ctx context.Context, i *domain.Invitation) error {
	i.ID = newID(i.ID)
	if i.Grants == nil {
		i.Grants = []domain.Grant{}
	}
	roles, err := json.Marshal(i.Grants)
	if err != nil {
		return fmt.Errorf("marshal roles: %w", err)
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO invitations (id, account_id, email, github_login, github_user_id, roles, token_hash, expires_at, invited_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at`,
		i.ID, i.AccountID, opt(i.Email), opt(i.GitHubLogin), opt(i.GitHubUserID), roles, i.TokenHash, i.ExpiresAt.UTC(), i.InvitedBy).Scan(&i.CreatedAt))
}

func (r invitationRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Invitation, error) {
	return one(ctx, r.db, scanInvitation, `SELECT `+invitationCols+` FROM invitations WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r invitationRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.Invitation, error) {
	return one(ctx, r.db, scanInvitation, `SELECT `+invitationCols+` FROM invitations WHERE token_hash = $1`, hash)
}

func (r invitationRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Invitation, error) {
	return many(ctx, r.db, scanInvitation, `SELECT `+invitationCols+` FROM invitations WHERE account_id = $1 ORDER BY created_at DESC, id DESC`, accountID)
}

func (r invitationRepo) Accept(ctx context.Context, accountID, id, userID uuid.UUID) (domain.Invitation, error) {
	inv, err := one(ctx, r.db, scanInvitation, `UPDATE invitations SET accepted_by = $3, accepted_at = now()
		WHERE account_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		RETURNING `+invitationCols, accountID, id, userID)
	if err == nil {
		return inv, nil
	}
	if _, getErr := r.Get(ctx, accountID, id); getErr != nil {
		return domain.Invitation{}, getErr
	}
	return domain.Invitation{}, fmt.Errorf("invitation %s is not pending: %w", id, store.ErrConflict)
}

func (r invitationRepo) Revoke(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE invitations SET revoked_at = COALESCE(revoked_at, now()) WHERE account_id = $1 AND id = $2`, accountID, id)
}

// ---- connections

type connectionRepo struct{ db dbtx }

var connectionCols = `id, ` + cuuid("account_id") + `, kind, name, issuer, client_id, client_secret_ref, jit, group_claim, enabled, created_at, updated_at`

func scanConnection(row pgx.Row) (c domain.Connection, err error) {
	var jit []byte
	if err = row.Scan(&c.ID, &c.AccountID, &c.Kind, &c.Name, &c.Issuer, &c.ClientID, &c.ClientSecretRef, &jit, &c.GroupClaim, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return c, err
	}
	utc(&c.CreatedAt, &c.UpdatedAt)
	return c, json.Unmarshal(jit, &c.JIT)
}

func jitJSON(j domain.JIT) ([]byte, error) {
	if j.DefaultRoles == nil {
		j.DefaultRoles = []uuid.UUID{}
	}
	return json.Marshal(j)
}

func (r connectionRepo) Create(ctx context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	jit, err := jitJSON(c.JIT)
	if err != nil {
		return fmt.Errorf("marshal jit: %w", err)
	}
	c.ID = newID(c.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO connections (id, account_id, kind, name, issuer, client_id, client_secret_ref, jit, group_claim, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at, updated_at`,
		c.ID, opt(c.AccountID), c.Kind, c.Name, c.Issuer, c.ClientID, c.ClientSecretRef, jit, c.GroupClaim, c.Enabled).Scan(&c.CreatedAt, &c.UpdatedAt))
}

func (r connectionRepo) Get(ctx context.Context, id uuid.UUID) (domain.Connection, error) {
	return one(ctx, r.db, scanConnection, `SELECT `+connectionCols+` FROM connections WHERE id = $1`, id)
}

func (r connectionRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Connection, error) {
	return many(ctx, r.db, scanConnection, `SELECT `+connectionCols+` FROM connections WHERE account_id IS NOT DISTINCT FROM $1 ORDER BY created_at, id`, opt(accountID))
}

func (r connectionRepo) UpsertInstance(ctx context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO connections (id, kind, name, issuer, client_id, client_secret_ref, group_claim, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (account_id, name) DO UPDATE SET kind = EXCLUDED.kind, issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id,
			client_secret_ref = EXCLUDED.client_secret_ref, group_claim = EXCLUDED.group_claim, enabled = EXCLUDED.enabled, updated_at = now()
		RETURNING id, created_at, updated_at`,
		newID(c.ID), c.Kind, c.Name, c.Issuer, c.ClientID, c.ClientSecretRef, c.GroupClaim, c.Enabled).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt))
}

func (r connectionRepo) Update(ctx context.Context, c *domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	jit, err := jitJSON(c.JIT)
	if err != nil {
		return fmt.Errorf("marshal jit: %w", err)
	}
	return mustUpdate(ctx, r.db, `UPDATE connections SET name = $2, issuer = $3, client_id = $4, client_secret_ref = $5, jit = $6, group_claim = $7, enabled = $8, updated_at = now()
		WHERE id = $1`, c.ID, c.Name, c.Issuer, c.ClientID, c.ClientSecretRef, jit, c.GroupClaim, c.Enabled)
}

var groupMappingCols = `id, account_id, connection_id, external_group, team_id, created_at`

func scanGroupMapping(row pgx.Row) (m domain.GroupMapping, err error) {
	err = row.Scan(&m.ID, &m.AccountID, &m.ConnectionID, &m.ExternalGroup, &m.TeamID, &m.CreatedAt)
	utc(&m.CreatedAt)
	return m, err
}

func (r connectionRepo) AddGroupMapping(ctx context.Context, m *domain.GroupMapping) error {
	m.ID = newID(m.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO group_mappings (id, account_id, connection_id, external_group, team_id) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`, m.ID, m.AccountID, m.ConnectionID, m.ExternalGroup, m.TeamID).Scan(&m.CreatedAt))
}

func (r connectionRepo) RemoveGroupMapping(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM group_mappings WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r connectionRepo) GroupMappings(ctx context.Context, connectionID uuid.UUID) ([]domain.GroupMapping, error) {
	return many(ctx, r.db, scanGroupMapping, `SELECT `+groupMappingCols+` FROM group_mappings WHERE connection_id = $1 ORDER BY created_at, id`, connectionID)
}

// ---- verified domains

type verifiedDomainRepo struct{ db dbtx }

var domainCols = `id, account_id, domain, verification_token, ` + ctime("verified_at") + `, enforce_sso, ` + cuuid("connection_id") + `, created_at, updated_at`

func scanDomain(row pgx.Row) (d domain.VerifiedDomain, err error) {
	err = row.Scan(&d.ID, &d.AccountID, &d.Domain, &d.VerificationToken, &d.VerifiedAt, &d.EnforceSSO, &d.ConnectionID, &d.CreatedAt, &d.UpdatedAt)
	utc(&d.VerifiedAt, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (r verifiedDomainRepo) Create(ctx context.Context, d *domain.VerifiedDomain) error {
	d.ID = newID(d.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO verified_domains (id, account_id, domain, verification_token) VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at`, d.ID, d.AccountID, d.Domain, d.VerificationToken).Scan(&d.CreatedAt, &d.UpdatedAt))
}

func (r verifiedDomainRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.VerifiedDomain, error) {
	return one(ctx, r.db, scanDomain, `SELECT `+domainCols+` FROM verified_domains WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r verifiedDomainRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.VerifiedDomain, error) {
	return many(ctx, r.db, scanDomain, `SELECT `+domainCols+` FROM verified_domains WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r verifiedDomainRepo) GetVerified(ctx context.Context, name string) (domain.VerifiedDomain, error) {
	return one(ctx, r.db, scanDomain, `SELECT `+domainCols+` FROM verified_domains WHERE domain = $1 AND verified_at IS NOT NULL`, strings.ToLower(name))
}

func (r verifiedDomainRepo) SetVerified(ctx context.Context, accountID, id uuid.UUID, verified bool) error {
	return mustUpdate(ctx, r.db, `UPDATE verified_domains SET verified_at = CASE WHEN $3 THEN COALESCE(verified_at, now()) END,
		enforce_sso = enforce_sso AND $3, updated_at = now() WHERE account_id = $1 AND id = $2`, accountID, id, verified)
}

func (r verifiedDomainRepo) SetEnforcement(ctx context.Context, accountID, id uuid.UUID, enforce bool, connectionID uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE verified_domains SET enforce_sso = $3, connection_id = $4, updated_at = now() WHERE account_id = $1 AND id = $2`,
		accountID, id, enforce, opt(connectionID))
}

func (r verifiedDomainRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM verified_domains WHERE account_id = $1 AND id = $2`, accountID, id)
}

// ---- sessions

type sessionRepo struct{ db dbtx }

var sessionCols = `id, token_hash, user_id, ` + cuuid("account_id") + `, ` + cuuid("connection_id") + `, created_at, last_seen_at, idle_expires_at, expires_at, user_agent, ` +
	`COALESCE(host(ip), ''), ` + ctime("revoked_at")

func scanSession(row pgx.Row) (s domain.Session, err error) {
	err = row.Scan(&s.ID, &s.TokenHash, &s.UserID, &s.AccountID, &s.ConnectionID, &s.CreatedAt, &s.LastSeenAt, &s.IdleExpiresAt, &s.ExpiresAt, &s.UserAgent, &s.IP, &s.RevokedAt)
	utc(&s.CreatedAt, &s.LastSeenAt, &s.IdleExpiresAt, &s.ExpiresAt, &s.RevokedAt)
	return s, err
}

func (r sessionRepo) Create(ctx context.Context, s *domain.Session) error {
	s.ID = newID(s.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO sessions (id, token_hash, user_id, account_id, connection_id, idle_expires_at, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::inet) RETURNING created_at, last_seen_at`,
		s.ID, s.TokenHash, s.UserID, opt(s.AccountID), opt(s.ConnectionID), s.IdleExpiresAt.UTC(), s.ExpiresAt.UTC(), s.UserAgent, opt(s.IP)).Scan(&s.CreatedAt, &s.LastSeenAt))
}

func (r sessionRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.Session, error) {
	return one(ctx, r.db, scanSession, `SELECT `+sessionCols+` FROM sessions WHERE token_hash = $1`, hash)
}

func (r sessionRepo) ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.Session, error) {
	return many(ctx, r.db, scanSession, `SELECT `+sessionCols+` FROM sessions WHERE user_id = $1 ORDER BY created_at, id`, userID)
}

func (r sessionRepo) Touch(ctx context.Context, id uuid.UUID, idleExpiresAt time.Time) error {
	return mustUpdate(ctx, r.db, `UPDATE sessions SET last_seen_at = now(), idle_expires_at = LEAST($2, expires_at) WHERE id = $1`, id, idleExpiresAt.UTC())
}

func (r sessionRepo) SetAccount(ctx context.Context, id, accountID uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE sessions SET account_id = $2 WHERE id = $1`, id, opt(accountID))
}

func (r sessionRepo) Revoke(ctx context.Context, userID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE sessions SET revoked_at = COALESCE(revoked_at, now()) WHERE user_id = $1 AND id = $2`, userID, id)
}

func (r sessionRepo) RevokeByUser(ctx context.Context, userID, accountID uuid.UUID) error {
	w := (&where{}).and("user_id = ?", userID).and("revoked_at IS NULL AND true = ?", true)
	andNot(w, "account_id = ?", accountID)
	_, err := r.db.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE `+strings.Join(w.conds, " AND "), w.args...)
	return mapErr(err)
}

func (r sessionRepo) DeleteExpired(ctx context.Context, t time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < $1 OR idle_expires_at < $1 OR revoked_at < $1`, t.UTC())
	if err != nil {
		return 0, mapErr(err)
	}
	return int(tag.RowsAffected()), nil
}

// ---- personal tokens

func (r mcpTokenRepo) ListByOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) ([]domain.MCPToken, error) {
	return many(ctx, r.db, scanToken, `SELECT `+tokenCols+` FROM mcp_tokens WHERE account_id = $1 AND owner_kind = $2 AND owner_id = $3 ORDER BY created_at, id`, accountID, kind, ownerID)
}

// AssignOwner renames a token whose name the owner uses already, in the
// order the tokens were created.
func (r mcpTokenRepo) AssignOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) (int, error) {
	n := 0
	err := inTx(ctx, r.db, func(tx dbtx) error {
		repo := mcpTokenRepo{tx}
		orphans, err := many(ctx, tx, scanToken, `SELECT `+tokenCols+` FROM mcp_tokens WHERE account_id = $1 AND owner_id IS NULL ORDER BY created_at, id FOR UPDATE`, accountID)
		if err != nil {
			return err
		}
		owned, err := repo.ListByOwner(ctx, accountID, kind, ownerID)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		for _, t := range owned {
			used[t.Name] = true
		}
		for _, t := range orphans {
			name := store.FreeTokenName(t.Name, used)
			used[name] = true
			if err := mustUpdate(ctx, tx, `UPDATE mcp_tokens SET owner_kind = $2, owner_id = $3, name = $4 WHERE id = $1`, t.ID, kind, ownerID, name); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func (r mcpTokenRepo) RevokeByOwner(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, ownerID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE mcp_tokens SET revoked_at = now() WHERE account_id = $1 AND owner_kind = $2 AND owner_id = $3 AND revoked_at IS NULL`, accountID, kind, ownerID)
	return mapErr(err)
}

// ---- OAuth grants

type oauthGrantRepo struct{ db dbtx }

func (r oauthGrantRepo) RevokeByUser(ctx context.Context, accountID, userID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE oauth_grants SET revoked_at = now() WHERE account_id = $1 AND user_id = $2 AND revoked_at IS NULL`, accountID, userID)
	return mapErr(err)
}

// ---- SCIM tokens

type scimTokenRepo struct{ db dbtx }

var scimTokenCols = `id, account_id, name, token_hash, created_by, ` + ctime("last_used_at") + `, ` + ctime("revoked_at") + `, created_at`

func scanSCIMToken(row pgx.Row) (t domain.SCIMToken, err error) {
	err = row.Scan(&t.ID, &t.AccountID, &t.Name, &t.TokenHash, &t.CreatedBy, &t.LastUsedAt, &t.RevokedAt, &t.CreatedAt)
	utc(&t.LastUsedAt, &t.RevokedAt, &t.CreatedAt)
	return t, err
}

func (r scimTokenRepo) Create(ctx context.Context, t *domain.SCIMToken) error {
	t.ID = newID(t.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO scim_tokens (id, account_id, name, token_hash, created_by) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`, t.ID, t.AccountID, t.Name, t.TokenHash, t.CreatedBy).Scan(&t.CreatedAt))
}

func (r scimTokenRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.SCIMToken, error) {
	return one(ctx, r.db, scanSCIMToken, `SELECT `+scimTokenCols+` FROM scim_tokens WHERE token_hash = $1`, hash)
}

func (r scimTokenRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.SCIMToken, error) {
	return many(ctx, r.db, scanSCIMToken, `SELECT `+scimTokenCols+` FROM scim_tokens WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r scimTokenRepo) Touch(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE scim_tokens SET last_used_at = now() WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r scimTokenRepo) Revoke(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE scim_tokens SET revoked_at = COALESCE(revoked_at, now()) WHERE account_id = $1 AND id = $2`, accountID, id)
}

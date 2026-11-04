//go:build integration

package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// TestIdentityMigration applies 00005 on a copy of the schema before it,
// with data in it: the organizations get slugs, tokens become ownerless
// admin tokens that still authenticate, the audit log keeps its rows, and
// the migration rolls back cleanly.
func TestIdentityMigration(t *testing.T) {
	pool := newPoolAt(t, 4)
	ctx := t.Context()
	var acc string
	must(t, pool.QueryRow(ctx, `SELECT id FROM accounts`).Scan(&acc))
	must(t, exec(pool, `INSERT INTO accounts (name) VALUES ('Default'), ('Acme Labs!'), ('***')`))
	must(t, exec(pool, `INSERT INTO projects (account_id, name, repo, status) VALUES ($1, 'p', 'o/p', 'registered')`, acc))
	must(t, exec(pool, `INSERT INTO mcp_tokens (account_id, name, token_hash) VALUES ($1, 'laptop', '\x01'), ($1, 'ci', '\x02')`, acc))
	must(t, exec(pool, `UPDATE mcp_tokens SET revoked_at = now() WHERE name = 'ci'`))
	must(t, exec(pool, `INSERT INTO audit_log (account_id, actor, action, subject_type) VALUES ($1, 'mcp:laptop', 'goal.created', 'goal')`, acc))

	p := provider(t, pool)
	_, err := p.UpTo(ctx, 5)
	must(t, err)

	st := New(pool)
	accounts, err := st.Accounts().List(ctx)
	must(t, err)
	var slugs []string
	for _, a := range accounts {
		slugs = append(slugs, a.Slug)
		if a.CreatedBy != "" {
			t.Errorf("%s: created_by %q", a.Name, a.CreatedBy)
		}
	}
	if !slices.Equal(slugs, []string{"default", "default-2", "acme-labs", "org"}) {
		t.Fatalf("slugs = %v", slugs)
	}
	tok, err := st.MCPTokens().GetByTokenHash(ctx, []byte{1})
	must(t, err)
	if tok.Name != "laptop" || tok.OwnerKind != "" || tok.OwnerID != uuid.Nil() || !slices.Equal(tok.Scopes, []domain.Scope{domain.ScopeAdmin}) ||
		!tok.ExpiresAt.IsZero() || !tok.Usable(time.Now()) {
		t.Fatalf("legacy token = %+v", tok)
	}
	tokens, err := st.MCPTokens().List(ctx, tok.AccountID)
	if err != nil || len(tokens) != 2 || tokens[1].RevokedAt.IsZero() {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
	must(t, st.MCPTokens().Touch(ctx, tok.AccountID, tok.ID))
	dup := domain.MCPToken{AccountID: tok.AccountID, Name: "laptop", TokenHash: []byte{3}}
	wantErr(t, st.MCPTokens().Create(ctx, &dup), store.ErrConflict)
	entries, err := st.Audit().List(ctx, tok.AccountID, store.AuditFilter{})
	if err != nil || len(entries) != 1 || entries[0].Actor != "mcp:laptop" || entries[0].Credential != "" {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
	if _, err := domain.ParseActor(entries[0].Actor); err != nil {
		t.Fatal(err)
	}
	mustFail(t, pool, `UPDATE audit_log SET credential = 'token:x'`)

	_, err = p.DownTo(ctx, 4)
	must(t, err)
	_, err = p.Up(ctx)
	must(t, err)
}

// TestIdentityMigrationSlugs: the slug step cannot clash, whatever the
// existing organizations are called.
func TestIdentityMigrationSlugs(t *testing.T) {
	pool := newPoolAt(t, 4)
	ctx := t.Context()
	long := strings.Repeat("a", 70)
	// One at a time, for a stable oldest-first order; the install's own
	// "default" comes first.
	names := []string{"default", "default-2", "Default!", "default-2", "org", "***", "---", long, long + "b", long + "c"}
	for i, name := range names {
		must(t, exec(pool, `INSERT INTO accounts (name, created_at) VALUES ($1, now() + $2 * interval '1 second')`, name, i+1))
	}
	_, err := provider(t, pool).UpTo(ctx, 5)
	must(t, err)
	accounts, err := New(pool).Accounts().List(ctx)
	must(t, err)
	a63 := strings.Repeat("a", 63)
	a61 := strings.Repeat("a", 61)
	want := []string{"default", "default-2", "default-2-2", "default-3", "default-2-3", "org", "org-2", "org-3", a63, a61 + "-2", a61 + "-3"}
	var got []string
	for _, a := range accounts {
		got = append(got, a.Slug)
		if !domain.ValidSlug(a.Slug) {
			t.Errorf("invalid slug %q", a.Slug)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("slugs = %v, want %v", got, want)
	}
}

// TestBuiltInRolesMatchDomain: the seeded built-in roles are those of
// domain.BuiltInRoles, and they cannot be changed.
func TestBuiltInRolesMatchDomain(t *testing.T) {
	f := newFixture(t)
	rows, err := f.pool.Query(t.Context(), `SELECT name, scope, description, permissions, custom FROM roles WHERE built_in ORDER BY created_at, id`)
	must(t, err)
	var got []domain.Role
	for rows.Next() {
		var r domain.Role
		var perms []string
		must(t, rows.Scan(&r.Name, &r.Scope, &r.Description, &perms, &r.Custom))
		r.Permissions, r.BuiltIn = fromStrings[domain.Permission](perms), true
		got = append(got, r)
	}
	must(t, rows.Err())
	want := domain.BuiltInRoles()
	if len(got) != len(want) {
		t.Fatalf("%d built-in roles, want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Name != w.Name || g.Scope != w.Scope || g.Description != w.Description || !slices.Equal(g.Permissions, w.Permissions) || g.Custom {
			t.Errorf("role %d = %+v, want %+v", i, g, w)
		}
	}
	mustFail(t, f.pool, `UPDATE roles SET permissions = '{org.read}' WHERE name = 'viewer'`)
	mustFail(t, f.pool, `DELETE FROM roles WHERE name = 'owner'`)
	mustFail(t, f.pool, `INSERT INTO roles (name, scope, permissions, built_in, custom) VALUES ('root', 'organization', '{org.read}', true, false)`)
}

// identity is a migrated database with users and helpers for the identity
// tables.
type identity struct {
	*fixture
	t *testing.T
}

func newIdentity(t *testing.T) identity { return identity{newFixture(t), t} }

func (id identity) user(email string) uuid.UUID {
	var u uuid.UUID
	must(id.t, id.pool.QueryRow(id.t.Context(), `INSERT INTO users (name, email, email_verified) VALUES ($1, $1, true) RETURNING id`, email).Scan(&u))
	return u
}

func (id identity) member(acc, user uuid.UUID, status domain.MembershipStatus) {
	must(id.t, exec(id.pool, `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, $3, 'manual')`, acc, user, string(status)))
}

func (id identity) role(scope domain.RoleScope, name string) uuid.UUID {
	var r uuid.UUID
	must(id.t, id.pool.QueryRow(id.t.Context(), `SELECT id FROM roles WHERE built_in AND scope = $1 AND name = $2`, string(scope), name).Scan(&r))
	return r
}

func (id identity) bind(acc uuid.UUID, kind domain.SubjectKind, subject, role uuid.UUID, project any) error {
	return mapErr(exec(id.pool, `INSERT INTO role_bindings (account_id, subject_kind, subject_id, role_id, project_id, created_by) VALUES ($1, $2, $3, $4, $5, 'user:test')`,
		acc, string(kind), subject, role, project))
}

func (id identity) owner() uuid.UUID { return id.role(domain.RoleScopeOrganization, domain.RoleOwner) }

// TestIdentityTables checks the constraints of the identity tables.
func TestIdentityTables(t *testing.T) {
	id := newIdentity(t)
	a := id.account
	other := domain.Account{Name: "other"}
	must(t, id.st.Accounts().Create(t.Context(), &other))
	ana, bob, cy := id.user("ana@example.com"), id.user("bob@example.com"), id.user("cy@example.com")
	id.member(a, ana, domain.MembershipActive)
	id.member(a, cy, domain.MembershipActive)
	must(t, exec(id.pool, `INSERT INTO teams (account_id, name, slug, source) VALUES ($1, 'Platform', 'platform', 'manual')`, a))
	must(t, exec(id.pool, `INSERT INTO teams (account_id, name, slug, source) VALUES ($1, 'Theirs', 'theirs', 'manual')`, other.ID))
	must(t, exec(id.pool, `INSERT INTO service_accounts (account_id, name, created_by) VALUES ($1, 'ci', 'user:test')`, a))
	must(t, exec(id.pool, `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'okta', 'https://acme.okta.com', 'c', 'env:FORECREW_CONN_OKTA')`, a))
	hash := strings.Repeat("ab", 32)
	conn := `(SELECT id FROM connections WHERE name = 'okta')`
	team := `(SELECT id FROM teams WHERE slug = 'platform')`
	tests := []struct {
		name string
		sql  string
		args []any
		ok   bool
	}{
		{"instance github connection", `INSERT INTO connections (kind, name, client_id, client_secret_ref) VALUES ('github', 'github', 'Iv1.x', 'env:FORECREW_GITHUB_CLIENT_SECRET')`, nil, true},
		{"second instance connection of that name", `INSERT INTO connections (kind, name, client_id, client_secret_ref) VALUES ('google', 'github', 'x', 'secret:google')`, nil, false},
		{"same name in an organization", `INSERT INTO connections (account_id, kind, name, client_id, client_secret_ref) VALUES ($1, 'google', 'github', 'x', 'secret:conn-google')`, []any{a}, true},
		{"secret value instead of a reference", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x1', 'https://i', 'c', 'GOCSPX-1a2b3c4d')`, []any{a}, false},
		{"lowercase env reference", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x2', 'https://i', 'c', 'env:secret')`, []any{a}, false},
		{"secret path escape", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x3', 'https://i', 'c', 'secret:a/../b')`, []any{a}, false},
		{"instance secret on an organization connection", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x10', 'https://i', 'c', 'env:FORECREW_DATABASE_URL')`, []any{a}, false},
		{"instance secret file on an organization connection", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x11', 'https://i', 'c', 'secret:forecrew-google')`, []any{a}, false},
		{"secret store reference on an organization connection", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x12', 'https://i', 'c', 'store:okta')`, []any{a}, true},
		{"secret store reference on an instance connection", `INSERT INTO connections (kind, name, issuer, client_id, client_secret_ref) VALUES ('oidc', 'x13', 'https://i', 'c', 'store:okta')`, nil, true},
		{"http issuer", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'oidc', 'x4', 'http://i', 'c', 'secret:conn-x')`, []any{a}, false},
		{"github with issuer", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'github', 'x5', 'https://github.com', 'c', 'secret:conn-x')`, []any{a}, false},
		{"saml", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref) VALUES ($1, 'saml', 'x6', 'https://i', 'c', 'secret:conn-x')`, []any{a}, false},
		{"jit on an instance connection", `INSERT INTO connections (kind, name, issuer, client_id, client_secret_ref, jit) VALUES ('oidc', 'x7', 'https://i', 'c', 'secret:x', '{"enabled": true, "default_roles": []}')`, nil, false},
		{"jit on an organization connection", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref, jit) VALUES ($1, 'oidc', 'x8', 'https://i', 'c', 'secret:conn-x', '{"enabled": true, "default_roles": []}')`, []any{a}, true},
		{"malformed jit", `INSERT INTO connections (account_id, kind, name, issuer, client_id, client_secret_ref, jit) VALUES ($1, 'oidc', 'x9', 'https://i', 'c', 'secret:conn-x', '{"enabled": "yes"}')`, []any{a}, false},
		{"identity", `INSERT INTO identities (user_id, connection_id, subject, login) VALUES ($1, ` + conn + `, 'sub-1', 'ana')`, []any{ana}, true},
		{"same subject twice", `INSERT INTO identities (user_id, connection_id, subject) VALUES ($1, ` + conn + `, 'sub-1')`, []any{bob}, false},
		{"second verified user with the email", `INSERT INTO users (email, email_verified) VALUES ('ANA@example.com', true)`, nil, false},
		{"unverified user with the email", `INSERT INTO users (email) VALUES ('ana@example.com')`, nil, true},
		{"verified without email", `INSERT INTO users (email_verified) VALUES (true)`, nil, false},
		{"second membership", `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, 'invited', 'invitation')`, []any{a, ana}, false},
		{"membership status", `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, 'banned', 'manual')`, []any{a, bob}, false},
		{"membership source", `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, 'active', 'idp')`, []any{a, bob}, false},
		{"scim membership", `INSERT INTO memberships (account_id, user_id, status, source, external_id) VALUES ($1, $2, 'active', 'scim', 'okta-1')`, []any{other.ID, bob}, true},
		{"same external id", `INSERT INTO memberships (account_id, user_id, status, source, external_id) VALUES ($1, $2, 'active', 'scim', 'okta-1')`, []any{other.ID, ana}, false},
		{"invitation by email", `INSERT INTO invitations (account_id, email, token_hash, expires_at, invited_by) VALUES ($1, 'cy@example.com', decode($2, 'hex'), now() + interval '7 days', 'user:x')`, []any{a, hash}, true},
		{"invitation by email and login", `INSERT INTO invitations (account_id, email, github_login, token_hash, expires_at, invited_by) VALUES ($1, 'cy@example.com', 'cy', '\x01', now(), 'user:x')`, []any{a}, false},
		{"invitation with a short hash", `INSERT INTO invitations (account_id, github_login, token_hash, expires_at, invited_by) VALUES ($1, 'cy', '\x01', now(), 'user:x')`, []any{a}, false},
		{"accepted without time", `UPDATE invitations SET accepted_by = $1`, []any{ana}, false},
		{"team member", `INSERT INTO team_members (team_id, user_id, account_id, source) VALUES (` + team + `, $1, $2, 'manual')`, []any{ana, a}, true},
		{"team member without membership", `INSERT INTO team_members (team_id, user_id, account_id, source) VALUES (` + team + `, $1, $2, 'manual')`, []any{bob, a}, false},
		{"team member of another organization", `INSERT INTO team_members (team_id, user_id, account_id, source) VALUES (` + team + `, $1, $2, 'scim')`, []any{bob, other.ID}, false},
		{"team slug", `INSERT INTO teams (account_id, name, slug, source) VALUES ($1, 'x', 'Platform Team', 'manual')`, []any{a}, false},
		{"group mapping", `INSERT INTO group_mappings (account_id, connection_id, external_group, team_id) VALUES ($1, ` + conn + `, 'eng', ` + team + `)`, []any{a}, true},
		{"group mapping to another organization's team", `INSERT INTO group_mappings (account_id, connection_id, external_group, team_id) VALUES ($1, ` + conn + `, 'eng', ` + team + `)`, []any{other.ID}, false},
		{"group mapping on another organization's connection", `INSERT INTO group_mappings (account_id, connection_id, external_group, team_id) SELECT $1, ` + conn + `, 'eng', id FROM teams WHERE account_id = $1`, []any{other.ID}, false},
		{"group mapping on an instance connection", `INSERT INTO group_mappings (account_id, connection_id, external_group, team_id) SELECT $1, (SELECT id FROM connections WHERE account_id IS NULL AND name = 'github'), 'eng', ` + team, []any{a}, true},
		{"custom role", `INSERT INTO roles (account_id, name, scope, permissions, built_in, custom) VALUES ($1, 'release', 'project', '{gate.approve.merge}', false, true)`, []any{a}, true},
		{"custom role without organization", `INSERT INTO roles (name, scope, permissions, built_in, custom) VALUES ('x', 'project', '{gate.approve.merge}', false, true)`, nil, false},
		{"role without permissions", `INSERT INTO roles (account_id, name, scope, permissions, built_in, custom) VALUES ($1, 'x', 'project', '{}', false, true)`, []any{a}, false},
		{"custom role can change", `UPDATE roles SET description = 'Release managers' WHERE name = 'release'`, nil, true},
		{"verified domain", `INSERT INTO verified_domains (account_id, domain, verification_token, verified_at, enforce_sso, connection_id) VALUES ($1, 'acme.com', 't', now(), true, ` + conn + `)`, []any{a}, true},
		{"pending elsewhere", `INSERT INTO verified_domains (account_id, domain, verification_token) VALUES ($1, 'acme.com', 't')`, []any{other.ID}, true},
		{"verified elsewhere", `UPDATE verified_domains SET verified_at = now() WHERE account_id = $1`, []any{other.ID}, false},
		{"enforced without verification", `INSERT INTO verified_domains (account_id, domain, verification_token, enforce_sso, connection_id) VALUES ($1, 'acme.io', 't', true, ` + conn + `)`, []any{a}, false},
		{"uppercase domain", `INSERT INTO verified_domains (account_id, domain, verification_token) VALUES ($1, 'Acme.org', 't')`, []any{a}, false},
		{"session", `INSERT INTO sessions (token_hash, user_id, account_id, connection_id, idle_expires_at, expires_at, ip) VALUES (decode($1, 'hex'), $2, $3, ` + conn + `, now() + interval '1 day', now() + interval '7 days', '192.0.2.1')`, []any{hash, ana, a}, true},
		{"session with a short hash", `INSERT INTO sessions (token_hash, user_id, idle_expires_at, expires_at) VALUES ('\x01', $1, now(), now())`, []any{ana}, false},
		{"idle after absolute expiry", `INSERT INTO sessions (token_hash, user_id, idle_expires_at, expires_at) VALUES (decode($1, 'hex'), $2, now() + interval '2 days', now() + interval '1 day')`, []any{strings.Repeat("cd", 32), ana}, false},
		{"personal token", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes, expires_at) VALUES ($1, 'laptop', '\x10', 'user', $2, '{read,write}', now() + interval '90 days')`, []any{a, ana}, true},
		{"same name, other owner", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) VALUES ($1, 'laptop', '\x11', 'user', $2, '{read}')`, []any{a, cy}, true},
		{"same name, same owner", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) VALUES ($1, 'laptop', '\x12', 'user', $2, '{read}')`, []any{a, cy}, false},
		{"token owner outside the organization", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) VALUES ($1, 'y', '\x17', 'user', $2, '{read}')`, []any{other.ID, ana}, false},
		{"unknown service account owner", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) VALUES ($1, 'y', '\x18', 'service_account', $2, '{read}')`, []any{a, domain.NewID()}, false},
		{"service account token", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) SELECT $1, 'y', '\x19', 'service_account', id, '{read}' FROM service_accounts`, []any{a}, true},
		{"owner kind without id", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, scopes) VALUES ($1, 'x', '\x13', 'user', '{read}')`, []any{a}, false},
		{"team token", `INSERT INTO mcp_tokens (account_id, name, token_hash, owner_kind, owner_id, scopes) VALUES ($1, 'x', '\x14', 'team', $2, '{read}')`, []any{a, ana}, false},
		{"token scope", `INSERT INTO mcp_tokens (account_id, name, token_hash, scopes) VALUES ($1, 'x', '\x15', '{root}')`, []any{a}, false},
		{"token without scopes", `INSERT INTO mcp_tokens (account_id, name, token_hash) VALUES ($1, 'x', '\x16')`, []any{a}, false},
		{"oauth client", `INSERT INTO oauth_clients (client_id, name, redirect_uris) VALUES ('https://claude.ai/oauth/metadata.json', 'Claude', '{https://claude.ai/api/mcp/auth_callback}')`, nil, true},
		{"oauth code", `INSERT INTO oauth_grants (kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, redirect_uri, code_challenge, expires_at) SELECT 'code', decode($1, 'hex'), uuidv7(), id, $2, $3, '{read,write}', 'https://f.example/mcp', 'https://claude.ai/cb', 'abc', now() + interval '1 minute' FROM oauth_clients`, []any{hash, ana, a}, true},
		{"oauth code without challenge", `INSERT INTO oauth_grants (kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, redirect_uri, expires_at) SELECT 'code', decode($1, 'hex'), uuidv7(), id, $2, $3, '{read}', 'r', 'https://claude.ai/cb', now() FROM oauth_clients`, []any{strings.Repeat("ef", 32), ana, a}, false},
		{"used access token", `INSERT INTO oauth_grants (kind, token_hash, family_id, client_id, user_id, account_id, scopes, resource, expires_at, used_at) SELECT 'access', decode($1, 'hex'), uuidv7(), id, $2, $3, '{read}', 'r', now(), now() FROM oauth_clients`, []any{strings.Repeat("ef", 32), ana, a}, false},
		{"scim token", `INSERT INTO scim_tokens (account_id, name, token_hash, created_by) VALUES ($1, 'okta', decode($2, 'hex'), 'user:x')`, []any{a, hash}, true},
		{"scim token with a short hash", `INSERT INTO scim_tokens (account_id, name, token_hash, created_by) VALUES ($1, 'entra', '\x01', 'user:x')`, []any{a}, false},
		{"organization slug", `UPDATE accounts SET slug = 'Acme Labs' WHERE id = $1`, []any{a}, false},
		{"duplicate organization slug", `UPDATE accounts SET slug = 'default' WHERE id = $1`, []any{other.ID}, false},
	}
	for _, tt := range tests {
		err := exec(id.pool, tt.sql, tt.args...)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok %v", tt.name, err, tt.ok)
		}
	}
}

// TestRoleBindings checks the rules the role_bindings trigger enforces.
func TestRoleBindings(t *testing.T) {
	id := newIdentity(t)
	a := id.account
	other := domain.Account{Name: "other"}
	must(t, id.st.Accounts().Create(t.Context(), &other))
	otherProject := id.newProject(t, other.ID, "theirs")
	ana, stranger := id.user("ana@example.com"), id.user("eve@example.com")
	id.member(a, ana, domain.MembershipActive)
	var team, sa, custom, foreign uuid.UUID
	must(t, id.pool.QueryRow(t.Context(), `INSERT INTO teams (account_id, name, slug, source) VALUES ($1, 'Platform', 'platform', 'manual') RETURNING id`, a).Scan(&team))
	must(t, id.pool.QueryRow(t.Context(), `INSERT INTO service_accounts (account_id, name, created_by) VALUES ($1, 'ci', 'user:x') RETURNING id`, a).Scan(&sa))
	must(t, id.pool.QueryRow(t.Context(), `INSERT INTO roles (account_id, name, scope, permissions, built_in, custom) VALUES ($1, 'release', 'project', '{gate.approve.merge}', false, true) RETURNING id`, a).Scan(&custom))
	must(t, id.pool.QueryRow(t.Context(), `INSERT INTO roles (account_id, name, scope, permissions, built_in, custom) VALUES ($1, 'release', 'project', '{gate.approve.merge}', false, true) RETURNING id`, other.ID).Scan(&foreign))
	org, proj := domain.RoleScopeOrganization, domain.RoleScopeProject
	tests := []struct {
		name    string
		kind    domain.SubjectKind
		subject uuid.UUID
		role    uuid.UUID
		project any
		want    error // nil: accepted; errAny: refused with an unmapped error
	}{
		{"user owner", domain.SubjectUser, ana, id.owner(), nil, nil},
		{"same binding twice", domain.SubjectUser, ana, id.owner(), nil, store.ErrConflict},
		{"team as maintainer of a project", domain.SubjectTeam, team, id.role(proj, domain.RoleMaintainer), id.project.ID, nil},
		{"team as maintainer of every project", domain.SubjectTeam, team, id.role(proj, domain.RoleMaintainer), nil, nil},
		{"service account viewer", domain.SubjectServiceAccount, sa, id.role(org, domain.RoleViewer), nil, nil},
		{"custom role", domain.SubjectUser, ana, custom, id.project.ID, nil},
		{"team owner", domain.SubjectTeam, team, id.owner(), nil, errAny},
		{"service account owner", domain.SubjectServiceAccount, sa, id.owner(), nil, errAny},
		{"organization role on a project", domain.SubjectUser, ana, id.role(org, domain.RoleAdmin), id.project.ID, errAny},
		{"another organization's role", domain.SubjectUser, ana, foreign, id.project.ID, store.ErrNotFound},
		{"another organization's project", domain.SubjectUser, ana, custom, otherProject.ID, store.ErrNotFound},
		{"user without membership", domain.SubjectUser, stranger, id.role(org, domain.RoleViewer), nil, store.ErrNotFound},
		{"unknown team", domain.SubjectTeam, domain.NewID(), id.role(org, domain.RoleViewer), nil, store.ErrNotFound},
		{"unknown service account", domain.SubjectServiceAccount, domain.NewID(), id.role(org, domain.RoleViewer), nil, store.ErrNotFound},
	}
	for _, tt := range tests {
		err := id.bind(a, tt.kind, tt.subject, tt.role, tt.project)
		switch {
		case tt.want == nil && err != nil, tt.want != nil && err == nil:
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		case tt.want != nil && tt.want != errAny && !errors.Is(err, tt.want):
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

var errAny = errors.New("any error")

// TestLastOwner: an organization that has an active Owner always keeps
// one; removing or demoting the last Owner is refused with
// store.ErrLastOwner, whichever row the change touches.
func TestLastOwner(t *testing.T) {
	id := newIdentity(t)
	a := id.account
	ana, bob := id.user("ana@example.com"), id.user("bob@example.com")
	id.member(a, ana, domain.MembershipActive)
	id.member(a, bob, domain.MembershipActive)
	admin := id.role(domain.RoleScopeOrganization, domain.RoleAdmin)
	change := func(sql string, args ...any) error { return mapErr(exec(id.pool, sql, args...)) }
	ownerOf := `subject_id = $1 AND role_id = (SELECT id FROM roles WHERE built_in AND name = 'owner')`

	// Without an owner (as after the migration) nothing is guarded.
	must(t, change(`UPDATE memberships SET status = 'suspended' WHERE user_id = $1`, bob))
	must(t, change(`UPDATE memberships SET status = 'active' WHERE user_id = $1`, bob))
	must(t, id.bind(a, domain.SubjectUser, ana, id.owner(), nil))

	carol := id.user("carol@example.com")
	id.member(a, carol, domain.MembershipSuspended)
	refused := []struct {
		name string
		do   func() error
	}{
		{"remove the binding", func() error { return change(`DELETE FROM role_bindings WHERE `+ownerOf, ana) }},
		{"demote to admin", func() error { return change(`UPDATE role_bindings SET role_id = $2 WHERE `+ownerOf, ana, admin) }},
		{"move to a suspended member", func() error { return change(`UPDATE role_bindings SET subject_id = $2 WHERE `+ownerOf, ana, carol) }},
		{"suspend the membership", func() error { return change(`UPDATE memberships SET status = 'suspended' WHERE user_id = $1`, ana) }},
		{"remove the membership", func() error { return change(`DELETE FROM memberships WHERE user_id = $1`, ana) }},
		{"disable the user", func() error { return change(`UPDATE users SET disabled_at = now() WHERE id = $1`, ana) }},
	}
	for _, r := range refused {
		if err := r.do(); !errors.Is(err, store.ErrLastOwner) {
			t.Errorf("%s: err = %v, want ErrLastOwner", r.name, err)
		}
	}
	// Other changes to the owner and to other members are free.
	must(t, change(`UPDATE users SET name = 'Ana' WHERE id = $1`, ana))
	must(t, change(`UPDATE memberships SET status = 'suspended' WHERE user_id = $1`, bob))

	// A second owner who is suspended or disabled does not count...
	must(t, id.bind(a, domain.SubjectUser, bob, id.owner(), nil))
	if err := refused[0].do(); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("with a suspended second owner: %v", err)
	}
	must(t, change(`UPDATE memberships SET status = 'active' WHERE user_id = $1`, bob))
	must(t, change(`UPDATE users SET disabled_at = now() WHERE id = $1`, bob))
	if err := refused[3].do(); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("with a disabled second owner: %v", err)
	}
	// ...an active one does.
	must(t, change(`UPDATE users SET disabled_at = NULL WHERE id = $1`, bob))
	must(t, change(`DELETE FROM role_bindings WHERE `+ownerOf, ana))
	if err := change(`DELETE FROM role_bindings WHERE `+ownerOf, bob); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("bob is the last owner: %v", err)
	}
	// Ana no longer owns it: she may go.
	must(t, change(`DELETE FROM memberships WHERE user_id = $1`, ana))
}

// TestLastOwnerConcurrent: two transactions that each remove one of two
// owners are checked one after the other, so one of them fails.
func TestLastOwnerConcurrent(t *testing.T) {
	id := newIdentity(t)
	ctx := t.Context()
	a := id.account
	ana, bob := id.user("ana@example.com"), id.user("bob@example.com")
	for _, u := range []uuid.UUID{ana, bob} {
		id.member(a, u, domain.MembershipActive)
		must(t, id.bind(a, domain.SubjectUser, u, id.owner(), nil))
	}
	remove := `DELETE FROM role_bindings WHERE subject_id = $1`
	tx1, err := id.pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx1.Rollback(ctx) }()
	_, err = tx1.Exec(ctx, remove, ana)
	must(t, err)
	second := make(chan error, 1)
	go func() {
		second <- mapErr(exec(id.pool, remove, bob))
	}()
	select {
	case err := <-second:
		t.Fatalf("second removal did not wait for the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, tx1.Commit(ctx))
	if err := <-second; !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("second removal: err = %v, want ErrLastOwner", err)
	}
	var owners int
	must(t, id.pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings WHERE subject_id = $1`, bob).Scan(&owners))
	if owners != 1 {
		t.Fatalf("bob's bindings = %d", owners)
	}
}

// TestAccountSlugs: a derived slug takes the first free suffix, also inside
// a transaction, which a clash must not abort, and under concurrent creates;
// a slug that is given and taken is a conflict.
func TestAccountSlugs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	create := func(a domain.Account) domain.Account {
		t.Helper()
		must(t, f.st.Accounts().Create(ctx, &a))
		return a
	}
	if a, b := create(domain.Account{Name: "Same Name"}), create(domain.Account{Name: "same name"}); a.Slug != "same-name" || b.Slug != "same-name-2" {
		t.Fatalf("slugs = %q %q", a.Slug, b.Slug)
	}
	create(domain.Account{Name: "x", Slug: "same-name-3"})
	if got := create(domain.Account{Name: "Same Name"}).Slug; got != "same-name-4" {
		t.Fatalf("past a given slug: %q", got)
	}
	taken := domain.Account{Name: "y", Slug: "same-name"}
	wantErr(t, f.st.Accounts().Create(ctx, &taken), store.ErrConflict)
	first := create(domain.Account{Name: "first"})
	again := domain.Account{ID: first.ID, Name: "second"}
	wantErr(t, f.st.Accounts().Create(ctx, &again), store.ErrConflict)

	must(t, f.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		a := domain.Account{Name: "Same Name"}
		if err := tx.Accounts().Create(ctx, &a); err != nil {
			return err
		}
		_, err := tx.Accounts().Get(ctx, a.ID)
		return err
	}))

	const n = 8
	slugs := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			a := domain.Account{Name: "Race"}
			if err := f.st.Accounts().Create(ctx, &a); err != nil {
				t.Error(err)
			}
			slugs[i] = a.Slug
		})
	}
	wg.Wait()
	slices.Sort(slugs)
	if len(slices.Compact(slugs)) != n {
		t.Fatalf("concurrent slugs = %v", slugs)
	}
}

// TestIdentityRepos: accounts get a slug and created_by, tokens keep their
// owner, scopes and expiry, and audit entries their credential.
func TestIdentityRepos(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	acc := domain.Account{Name: "Acme Labs", CreatedBy: "user:" + domain.NewID().String()}
	must(t, f.st.Accounts().Create(ctx, &acc))
	got, err := f.st.Accounts().Get(ctx, acc.ID)
	if err != nil || got.Slug != "acme-labs" || got.CreatedBy != acc.CreatedBy {
		t.Fatalf("account = %+v, %v", got, err)
	}
	if got.MaxTokenTTL != 0 {
		t.Fatalf("max_token_ttl = %v, want no cap", got.MaxTokenTTL)
	}
	must(t, f.st.Accounts().SetMaxTokenTTL(ctx, acc.ID, domain.DefaultSSOTokenTTL))
	got, err = f.st.Accounts().Get(ctx, acc.ID)
	if err != nil || time.Duration(got.MaxTokenTTL) != domain.DefaultSSOTokenTTL {
		t.Fatalf("capped account = %+v, %v", got, err)
	}
	must(t, f.st.Accounts().SetMaxTokenTTL(ctx, acc.ID, 0))
	if got, err = f.st.Accounts().Get(ctx, acc.ID); err != nil || got.MaxTokenTTL != 0 {
		t.Fatalf("uncapped account = %+v, %v", got, err)
	}
	if err := f.st.Accounts().SetMaxTokenTTL(ctx, acc.ID, -time.Hour); err == nil {
		t.Fatal("negative cap accepted")
	}
	wantErr(t, f.st.Accounts().SetMaxTokenTTL(ctx, domain.NewID(), time.Hour), store.ErrNotFound)
	capped := domain.Account{Name: "capped", MaxTokenTTL: domain.Duration(time.Hour)}
	must(t, f.st.Accounts().Create(ctx, &capped))
	if got, err = f.st.Accounts().Get(ctx, capped.ID); err != nil || time.Duration(got.MaxTokenTTL) != time.Hour {
		t.Fatalf("created with a cap = %+v, %v", got, err)
	}
	bad := domain.Account{Name: "x", Slug: "Not A Slug"}
	if err := f.st.Accounts().Create(ctx, &bad); err == nil {
		t.Fatal("invalid slug accepted")
	}

	owner, sa := domain.NewID(), domain.NewID()
	must(t, exec(f.pool, `INSERT INTO users (id) VALUES ($1)`, owner))
	must(t, exec(f.pool, `INSERT INTO memberships (account_id, user_id, status, source) VALUES ($1, $2, 'active', 'manual')`, f.account, owner))
	must(t, exec(f.pool, `INSERT INTO service_accounts (id, account_id, name, created_by) VALUES ($1, $2, 'ci', 'user:x')`, sa, f.account))
	exp := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Microsecond)
	tok := domain.MCPToken{AccountID: f.account, Name: "laptop", TokenHash: []byte("p1"), OwnerKind: domain.SubjectUser, OwnerID: owner,
		Scopes: []domain.Scope{domain.ScopeRead, domain.ScopeWrite}, ExpiresAt: exp}
	must(t, f.st.MCPTokens().Create(ctx, &tok))
	back, err := f.st.MCPTokens().GetByTokenHash(ctx, []byte("p1"))
	if err != nil || back.OwnerKind != domain.SubjectUser || back.OwnerID != owner || !slices.Equal(back.Scopes, tok.Scopes) || !back.ExpiresAt.Equal(exp) {
		t.Fatalf("token = %+v, %v", back, err)
	}
	legacy := domain.MCPToken{AccountID: f.account, Name: "laptop", TokenHash: []byte("p2")}
	must(t, f.st.MCPTokens().Create(ctx, &legacy)) // another owner (none): same name allowed
	if !slices.Equal(legacy.Scopes, []domain.Scope{domain.ScopeAdmin}) {
		t.Fatalf("legacy scopes = %v", legacy.Scopes)
	}
	noScopes := domain.MCPToken{AccountID: f.account, Name: "x", TokenHash: []byte("p3"), OwnerKind: domain.SubjectServiceAccount, OwnerID: sa}
	if err := f.st.MCPTokens().Create(ctx, &noScopes); err == nil {
		t.Fatal("owned token without scopes accepted")
	}

	stranger := domain.MCPToken{AccountID: f.account, Name: "y", TokenHash: []byte("p4"), OwnerKind: domain.SubjectUser, OwnerID: domain.NewID(), Scopes: []domain.Scope{domain.ScopeRead}}
	wantErr(t, f.st.MCPTokens().Create(ctx, &stranger), store.ErrNotFound)
	// A member who left keeps a revocable token: the owner is only checked when set.
	must(t, f.st.MCPTokens().Touch(ctx, f.account, tok.ID))
	must(t, exec(f.pool, `DELETE FROM memberships WHERE user_id = $1`, owner))
	must(t, f.st.MCPTokens().Touch(ctx, f.account, tok.ID))
	must(t, f.st.MCPTokens().Revoke(ctx, f.account, tok.ID))

	cred := domain.Credential{Kind: domain.CredentialToken, ID: tok.ID}
	by := domain.UserActor(f.account, owner, cred)
	e := domain.AuditEntry{AccountID: f.account, Actor: by.String(), Credential: by.Credential.String(), Action: "goal.created", SubjectType: "goal"}
	must(t, f.st.Audit().Append(ctx, &e))
	list, err := f.st.Audit().List(ctx, f.account, store.AuditFilter{Limit: 1})
	if err != nil || len(list) != 1 || list[0].Credential != "token:"+tok.ID.String() || list[0].Actor != "user:"+owner.String() {
		t.Fatalf("audit = %+v, %v", list, err)
	}
}

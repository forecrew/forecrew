package postgres

import (
	"context"
	"strconv"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ---- teams

type teamRepo struct{ db dbtx }

const teamCols = `id, account_id, name, slug, source, COALESCE(external_id, ''), created_at, updated_at`

func scanTeam(row pgx.Row) (t domain.Team, err error) {
	err = row.Scan(&t.ID, &t.AccountID, &t.Name, &t.Slug, &t.Source, &t.ExternalID, &t.CreatedAt, &t.UpdatedAt)
	utc(&t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (r teamRepo) Create(ctx context.Context, t *domain.Team) error {
	t.ID = newID(t.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO teams (id, account_id, name, slug, source, external_id)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`,
		t.ID, t.AccountID, t.Name, t.Slug, t.Source, opt(t.ExternalID)).Scan(&t.CreatedAt, &t.UpdatedAt))
}

func (r teamRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Team, error) {
	return one(ctx, r.db, scanTeam, `SELECT `+teamCols+` FROM teams WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r teamRepo) GetBySlug(ctx context.Context, accountID uuid.UUID, slug string) (domain.Team, error) {
	return one(ctx, r.db, scanTeam, `SELECT `+teamCols+` FROM teams WHERE account_id = $1 AND slug = $2`, accountID, slug)
}

func (r teamRepo) GetByExternalID(ctx context.Context, accountID uuid.UUID, externalID string) (domain.Team, error) {
	return one(ctx, r.db, scanTeam, `SELECT `+teamCols+` FROM teams WHERE account_id = $1 AND external_id = $2`, accountID, externalID)
}

func (r teamRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Team, error) {
	return many(ctx, r.db, scanTeam, `SELECT `+teamCols+` FROM teams WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r teamRepo) Update(ctx context.Context, t *domain.Team) error {
	return mapErr(r.db.QueryRow(ctx, `UPDATE teams SET name = $3, slug = $4, external_id = $5, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		t.AccountID, t.ID, t.Name, t.Slug, opt(t.ExternalID)).Scan(&t.UpdatedAt))
}

func (r teamRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE account_id = $1 AND subject_kind = 'team' AND subject_id = $2`, accountID, id); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE account_id = $1 AND team_id = $2`, accountID, id); err != nil {
			return mapErr(err)
		}
		return mustUpdate(ctx, tx, `DELETE FROM teams WHERE account_id = $1 AND id = $2`, accountID, id)
	})
}

func (r teamRepo) AddMember(ctx context.Context, m *domain.TeamMember) error {
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO team_members (team_id, user_id, account_id, source) VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id, user_id) DO UPDATE SET team_id = EXCLUDED.team_id
		RETURNING source, created_at`, m.TeamID, m.UserID, m.AccountID, m.Source).Scan(&m.Source, &m.CreatedAt))
}

func (r teamRepo) RemoveMember(ctx context.Context, accountID, teamID, userID uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM team_members WHERE account_id = $1 AND team_id = $2 AND user_id = $3`, accountID, teamID, userID)
}

func scanTeamMember(row pgx.Row) (m domain.TeamMember, err error) {
	err = row.Scan(&m.TeamID, &m.UserID, &m.AccountID, &m.Source, &m.CreatedAt)
	utc(&m.CreatedAt)
	return m, err
}

func (r teamRepo) Members(ctx context.Context, accountID, teamID uuid.UUID) ([]domain.TeamMember, error) {
	return many(ctx, r.db, scanTeamMember, `SELECT team_id, user_id, account_id, source, created_at FROM team_members
		WHERE account_id = $1 AND team_id = $2 ORDER BY created_at, user_id`, accountID, teamID)
}

func (r teamRepo) TeamsOf(ctx context.Context, accountID, userID uuid.UUID) ([]domain.Team, error) {
	return many(ctx, r.db, scanTeam, `SELECT `+teamCols+` FROM teams WHERE account_id = $1
		AND id IN (SELECT team_id FROM team_members WHERE account_id = $1 AND user_id = $2) ORDER BY created_at, id`, accountID, userID)
}

// ---- service accounts

type serviceAccountRepo struct{ db dbtx }

var serviceAccountCols = `id, account_id, name, description, created_by, ` + ctime("disabled_at") + `, created_at`

func scanServiceAccount(row pgx.Row) (s domain.ServiceAccount, err error) {
	err = row.Scan(&s.ID, &s.AccountID, &s.Name, &s.Description, &s.CreatedBy, &s.DisabledAt, &s.CreatedAt)
	utc(&s.DisabledAt, &s.CreatedAt)
	return s, err
}

func (r serviceAccountRepo) Create(ctx context.Context, s *domain.ServiceAccount) error {
	s.ID = newID(s.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO service_accounts (id, account_id, name, description, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`, s.ID, s.AccountID, s.Name, s.Description, s.CreatedBy).Scan(&s.CreatedAt))
}

func (r serviceAccountRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.ServiceAccount, error) {
	return one(ctx, r.db, scanServiceAccount, `SELECT `+serviceAccountCols+` FROM service_accounts WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r serviceAccountRepo) GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.ServiceAccount, error) {
	return one(ctx, r.db, scanServiceAccount, `SELECT `+serviceAccountCols+` FROM service_accounts WHERE account_id = $1 AND name = $2`, accountID, name)
}

func (r serviceAccountRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.ServiceAccount, error) {
	return many(ctx, r.db, scanServiceAccount, `SELECT `+serviceAccountCols+` FROM service_accounts WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r serviceAccountRepo) Update(ctx context.Context, s *domain.ServiceAccount) error {
	return mustUpdate(ctx, r.db, `UPDATE service_accounts SET name = $3, description = $4 WHERE account_id = $1 AND id = $2`,
		s.AccountID, s.ID, s.Name, s.Description)
}

func (r serviceAccountRepo) SetDisabled(ctx context.Context, accountID, id uuid.UUID, disabled bool) error {
	return mustUpdate(ctx, r.db, `UPDATE service_accounts SET disabled_at = CASE WHEN $3 THEN COALESCE(disabled_at, now()) END
		WHERE account_id = $1 AND id = $2`, accountID, id, disabled)
}

// ---- roles

type roleRepo struct{ db dbtx }

var roleCols = `id, ` + cuuid("account_id") + `, name, description, scope, permissions, built_in, custom, created_at, updated_at`

func scanRole(row pgx.Row) (r domain.Role, err error) {
	var perms []string
	err = row.Scan(&r.ID, &r.AccountID, &r.Name, &r.Description, &r.Scope, &perms, &r.BuiltIn, &r.Custom, &r.CreatedAt, &r.UpdatedAt)
	r.Permissions = fromStrings[domain.Permission](perms)
	utc(&r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (r roleRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Role, error) {
	return one(ctx, r.db, scanRole, `SELECT `+roleCols+` FROM roles WHERE id = $2 AND (account_id = $1 OR built_in)`, accountID, id)
}

func (r roleRepo) GetByName(ctx context.Context, accountID uuid.UUID, scope domain.RoleScope, name string) (domain.Role, error) {
	return one(ctx, r.db, scanRole, `SELECT `+roleCols+` FROM roles WHERE scope = $2 AND name = $3 AND (account_id = $1 OR built_in)
		ORDER BY built_in DESC LIMIT 1`, accountID, scope, name)
}

func (r roleRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Role, error) {
	return many(ctx, r.db, scanRole, `SELECT `+roleCols+` FROM roles WHERE account_id = $1 OR built_in
		ORDER BY built_in DESC, scope, created_at, id`, accountID)
}

func (r roleRepo) Create(ctx context.Context, role *domain.Role) error {
	role.ID = newID(role.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO roles (id, account_id, name, description, scope, permissions, built_in, custom)
		VALUES ($1, $2, $3, $4, $5, $6, false, true) RETURNING created_at, updated_at`,
		role.ID, role.AccountID, role.Name, role.Description, role.Scope, toStrings(role.Permissions)).Scan(&role.CreatedAt, &role.UpdatedAt))
}

func (r roleRepo) Update(ctx context.Context, role *domain.Role) error {
	return mapErr(r.db.QueryRow(ctx, `UPDATE roles SET name = $3, description = $4, permissions = $5, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND custom RETURNING updated_at`,
		role.AccountID, role.ID, role.Name, role.Description, toStrings(role.Permissions)).Scan(&role.UpdatedAt))
}

// Delete implements store.RoleRepo. A binding that uses the role holds it
// by a foreign key, which is a conflict here and not a missing row.
func (r roleRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	var used bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM role_bindings WHERE account_id = $1 AND role_id = $2)`, accountID, id).Scan(&used)
	if err != nil {
		return mapErr(err)
	}
	if used {
		return store.ErrConflict
	}
	return mustUpdate(ctx, r.db, `DELETE FROM roles WHERE account_id = $1 AND id = $2 AND custom`, accountID, id)
}

// ---- role bindings

type roleBindingRepo struct{ db dbtx }

var bindingCols = `id, account_id, subject_kind, subject_id, role_id, ` + cuuid("project_id") + `, created_by, created_at`

func scanBinding(row pgx.Row) (b domain.RoleBinding, err error) {
	err = row.Scan(&b.ID, &b.AccountID, &b.SubjectKind, &b.SubjectID, &b.RoleID, &b.ProjectID, &b.CreatedBy, &b.CreatedAt)
	utc(&b.CreatedAt)
	return b, err
}

func (r roleBindingRepo) Create(ctx context.Context, b *domain.RoleBinding) error {
	b.ID = newID(b.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO role_bindings (id, account_id, subject_kind, subject_id, role_id, project_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		b.ID, b.AccountID, b.SubjectKind, b.SubjectID, b.RoleID, opt(b.ProjectID), b.CreatedBy).Scan(&b.CreatedAt))
}

func (r roleBindingRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.RoleBinding, error) {
	return one(ctx, r.db, scanBinding, `SELECT `+bindingCols+` FROM role_bindings WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r roleBindingRepo) List(ctx context.Context, accountID uuid.UUID, f store.RoleBindingFilter) ([]domain.RoleBinding, error) {
	w := newWhere(accountID)
	andNot(w, "subject_kind = ?", string(f.SubjectKind))
	andNot(w, "subject_id = ?", f.SubjectID)
	andNot(w, "role_id = ?", f.RoleID)
	andNot(w, "project_id = ?", f.ProjectID)
	if f.OrganizationOnly {
		w.conds = append(w.conds, "project_id IS NULL")
	}
	return many(ctx, r.db, scanBinding, `SELECT `+bindingCols+` FROM role_bindings`+w.sql("created_at, id", 0), w.args...)
}

func (r roleBindingRepo) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `DELETE FROM role_bindings WHERE account_id = $1 AND id = $2`, accountID, id)
}

func scanBoundRole(row pgx.Row) (b store.BoundRole, err error) {
	var perms []string
	err = row.Scan(&b.Binding.ID, &b.Binding.AccountID, &b.Binding.SubjectKind, &b.Binding.SubjectID, &b.Binding.RoleID, &b.Binding.ProjectID,
		&b.Binding.CreatedBy, &b.Binding.CreatedAt,
		&b.Role.ID, &b.Role.AccountID, &b.Role.Name, &b.Role.Description, &b.Role.Scope, &perms, &b.Role.BuiltIn, &b.Role.Custom, &b.Role.CreatedAt, &b.Role.UpdatedAt)
	b.Role.Permissions = fromStrings[domain.Permission](perms)
	utc(&b.Binding.CreatedAt, &b.Role.CreatedAt, &b.Role.UpdatedAt)
	return b, err
}

// Effective implements store.RoleBindingRepo.
func (r roleBindingRepo) Effective(ctx context.Context, accountID uuid.UUID, kind domain.SubjectKind, id uuid.UUID) ([]store.BoundRole, error) {
	cols := `b.id, b.account_id, b.subject_kind, b.subject_id, b.role_id, ` + cuuid("b.project_id") + `, b.created_by, b.created_at,
		r.id, ` + cuuid("r.account_id") + `, r.name, r.description, r.scope, r.permissions, r.built_in, r.custom, r.created_at, r.updated_at`
	var where string
	switch kind {
	case domain.SubjectUser:
		where = `EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
				WHERE m.account_id = $1 AND m.user_id = $2 AND m.status = 'active' AND u.disabled_at IS NULL)
			AND ((b.subject_kind = 'user' AND b.subject_id = $2)
				OR (b.subject_kind = 'team' AND b.subject_id IN (SELECT team_id FROM team_members WHERE account_id = $1 AND user_id = $2)))`
	case domain.SubjectServiceAccount:
		where = `EXISTS (SELECT 1 FROM service_accounts s WHERE s.account_id = $1 AND s.id = $2 AND s.disabled_at IS NULL)
			AND b.subject_kind = 'service_account' AND b.subject_id = $2`
	default:
		return []store.BoundRole{}, nil
	}
	return many(ctx, r.db, scanBoundRole, `SELECT `+cols+` FROM role_bindings b JOIN roles r ON r.id = b.role_id
		WHERE b.account_id = $1 AND `+where+` ORDER BY b.created_at, b.id`, accountID, id)
}

// ---- directory

type directoryRepo struct{ db dbtx }

var userCols = `id, name, ` + ctext("email") + `, email_verified, instance_admin, ` + ctime("disabled_at") + `, ` + ctime("last_seen_at") + `, created_at, updated_at`

func scanUser(row pgx.Row) (u domain.User, err error) {
	err = row.Scan(&u.ID, &u.Name, &u.Email, &u.EmailVerified, &u.InstanceAdmin, &u.DisabledAt, &u.LastSeenAt, &u.CreatedAt, &u.UpdatedAt)
	utc(&u.DisabledAt, &u.LastSeenAt, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (r directoryRepo) User(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return one(ctx, r.db, scanUser, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
}

func (r directoryRepo) GitHubMembers(ctx context.Context, accountID uuid.UUID, id int64, login string) ([]uuid.UUID, error) {
	match, arg := `lower(i.login) = lower($2)`, login
	if id != 0 {
		match, arg = `i.subject = $2`, strconv.FormatInt(id, 10)
	}
	return many(ctx, r.db, func(row pgx.Row) (u uuid.UUID, err error) { return u, row.Scan(&u) },
		`SELECT DISTINCT i.user_id FROM identities i
			JOIN connections c ON c.id = i.connection_id
			JOIN memberships m ON m.user_id = i.user_id AND m.account_id = $1
		WHERE c.kind = 'github' AND (c.account_id IS NULL OR c.account_id = $1) AND `+match, accountID, arg)
}

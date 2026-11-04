package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func newID(id uuid.UUID) uuid.UUID {
	if id == uuid.Nil() {
		return domain.NewID()
	}
	return id
}

// ---- accounts

type accountRepo struct{ db dbtx }

var accountCols = `id, name, slug, ` + ctext("created_by") + `, COALESCE(max_token_ttl, '0'), created_at`

func scanAccount(row pgx.Row) (a domain.Account, err error) {
	var ttl time.Duration
	err = row.Scan(&a.ID, &a.Name, &a.Slug, &a.CreatedBy, &ttl, &a.CreatedAt)
	a.MaxTokenTTL = domain.Duration(ttl)
	utc(&a.CreatedAt)
	return a, err
}

const insertAccount = `INSERT INTO accounts (id, name, slug, created_by, max_token_ttl) VALUES ($1, $2, $3, $4, $5)`

// Create stores the account. ON CONFLICT rather than catching the unique
// violation, which would abort a surrounding transaction, and so that
// concurrent creates of one name each get their own slug.
func (r accountRepo) Create(ctx context.Context, a *domain.Account) error {
	a.ID = newID(a.ID)
	insert := func(sql, slug string) error {
		return r.db.QueryRow(ctx, sql, a.ID, a.Name, slug, opt(a.CreatedBy), opt(time.Duration(a.MaxTokenTTL))).Scan(&a.CreatedAt)
	}
	if a.Slug != "" {
		return mapErr(insert(insertAccount+` RETURNING created_at`, a.Slug))
	}
	base := domain.Slugify(a.Name)
	for n := 1; ; n++ {
		slug := domain.SlugCandidate(base, n)
		err := insert(insertAccount+` ON CONFLICT (slug) DO NOTHING RETURNING created_at`, slug)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err == nil {
			a.Slug = slug
		}
		return mapErr(err)
	}
}

func (r accountRepo) SetMaxTokenTTL(ctx context.Context, id uuid.UUID, ttl time.Duration) error {
	return mustUpdate(ctx, r.db, `UPDATE accounts SET max_token_ttl = $2 WHERE id = $1`, id, opt(ttl))
}

func (r accountRepo) Get(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return one(ctx, r.db, scanAccount, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id)
}

func (r accountRepo) List(ctx context.Context) ([]domain.Account, error) {
	return many(ctx, r.db, scanAccount, `SELECT `+accountCols+` FROM accounts ORDER BY created_at, id`)
}

// ---- projects

type projectRepo struct{ db dbtx }

const projectCols = `id, account_id, name, repo, default_branch, status, profile, created_at, updated_at`

func scanProject(row pgx.Row) (p domain.Project, err error) {
	var profile []byte
	err = row.Scan(&p.ID, &p.AccountID, &p.Name, &p.Repo, &p.DefaultBranch, &p.Status, &profile, &p.CreatedAt, &p.UpdatedAt)
	p.Profile = profile
	utc(&p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (r projectRepo) Create(ctx context.Context, p *domain.Project) error {
	p.ID = newID(p.ID)
	if p.Status == "" {
		p.Status = domain.ProjectRegistered
	}
	if p.DefaultBranch == "" {
		p.DefaultBranch = "main"
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO projects (id, account_id, name, repo, default_branch, status, profile)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at, updated_at`,
		p.ID, p.AccountID, p.Name, p.Repo, p.DefaultBranch, string(p.Status), optJSON(p.Profile)).Scan(&p.CreatedAt, &p.UpdatedAt))
}

func (r projectRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Project, error) {
	return one(ctx, r.db, scanProject, `SELECT `+projectCols+` FROM projects WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r projectRepo) GetByName(ctx context.Context, accountID uuid.UUID, nameOrRepo string) (domain.Project, error) {
	return one(ctx, r.db, scanProject, `SELECT `+projectCols+` FROM projects
		WHERE account_id = $1 AND (lower(name) = lower($2) OR lower(repo) = lower($2))
		ORDER BY (lower(name) = lower($2)) DESC, created_at LIMIT 1`, accountID, nameOrRepo)
}

func (r projectRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Project, error) {
	return many(ctx, r.db, scanProject, `SELECT `+projectCols+` FROM projects WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r projectRepo) ListByRepo(ctx context.Context, repo string) ([]domain.Project, error) {
	return many(ctx, r.db, scanProject, `SELECT `+projectCols+` FROM projects WHERE lower(repo) = lower($1) ORDER BY created_at, id`, repo)
}

func (r projectRepo) UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.ProjectStatus) error {
	return updateStatus(ctx, r.db, "projects", accountID, id, from, to, transition[domain.ProjectStatus])
}

func (r projectRepo) SetProfile(ctx context.Context, accountID, id uuid.UUID, profile json.RawMessage) error {
	return mustUpdate(ctx, r.db, `UPDATE projects SET profile = $3, updated_at = now() WHERE account_id = $1 AND id = $2`,
		accountID, id, optJSON(profile))
}

// mustUpdate runs an UPDATE that must touch exactly one row.
func mustUpdate(ctx context.Context, db dbtx, q string, args ...any) error {
	tag, err := db.Exec(ctx, q, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- goals

type goalRepo struct{ db dbtx }

var goalCols = `id, account_id, project_id, number, title, body, platforms, status, ` + cint("issue_number") + `, size, created_by, created_at, updated_at`

func scanGoal(row pgx.Row) (g domain.Goal, err error) {
	var size []byte
	err = row.Scan(&g.ID, &g.AccountID, &g.ProjectID, &g.Number, &g.Title, &g.Body, &g.Platforms, &g.Status, &g.IssueNumber, &size, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	utc(&g.CreatedAt, &g.UpdatedAt)
	if err != nil || size == nil {
		return g, err
	}
	g.Size = new(domain.GoalSize)
	if err := json.Unmarshal(size, g.Size); err != nil {
		return domain.Goal{}, fmt.Errorf("decode goal size: %w", err)
	}
	return g, nil
}

// goalSize encodes the size of a goal for goals.size; NULL without one.
func goalSize(s *domain.GoalSize) (any, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode goal size: %w", err)
	}
	return b, nil
}

func (r goalRepo) Create(ctx context.Context, g *domain.Goal) error {
	g.ID = newID(g.ID)
	if g.Status == "" {
		g.Status = domain.GoalDraft
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, g.AccountID, ref{"projects", g.ProjectID}); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, g.AccountID, string(domain.RefGoal))
		if err != nil {
			return mapErr(err)
		}
		g.Number = n
		g.Platforms = domain.NormalizeLabels(g.Platforms)
		size, err := goalSize(g.Size)
		if err != nil {
			return err
		}
		return mapErr(tx.QueryRow(ctx, `INSERT INTO goals (id, account_id, project_id, number, title, body, platforms, status, issue_number, size, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING created_at, updated_at`,
			g.ID, g.AccountID, g.ProjectID, g.Number, g.Title, g.Body, g.Platforms, string(g.Status), opt(g.IssueNumber), size, g.CreatedBy).Scan(&g.CreatedAt, &g.UpdatedAt))
	})
}

func (r goalRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Goal, error) {
	return one(ctx, r.db, scanGoal, `SELECT `+goalCols+` FROM goals WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r goalRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Goal, error) {
	return one(ctx, r.db, scanGoal, `SELECT `+goalCols+` FROM goals WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r goalRepo) List(ctx context.Context, accountID uuid.UUID, f store.GoalFilter) ([]domain.Goal, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanGoal, `SELECT `+goalCols+` FROM goals`+w.sql("number", f.Limit), w.args...)
}

func (r goalRepo) Update(ctx context.Context, g *domain.Goal) error {
	err := r.db.QueryRow(ctx, `UPDATE goals SET title = $3, body = $4, issue_number = $5, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		g.AccountID, g.ID, g.Title, g.Body, opt(g.IssueNumber)).Scan(&g.UpdatedAt)
	utc(&g.UpdatedAt)
	return mapErr(err)
}

func (r goalRepo) SetSize(ctx context.Context, accountID, id uuid.UUID, size domain.GoalSize) error {
	b, err := goalSize(&size)
	if err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE goals SET size = $3, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND size IS NULL`, accountID, id, b)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing was written: the goal is missing, or it has a size already.
	_, err = r.Get(ctx, accountID, id)
	return err
}

func (r goalRepo) UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.GoalStatus) error {
	return updateStatus(ctx, r.db, "goals", accountID, id, from, to, transition[domain.GoalStatus])
}

// ---- specs

type specRepo struct{ db dbtx }

var specCols = `id, account_id, project_id, goal_id, number, revision, body, ` + cuuid("job_id") + `, created_at`

func scanSpec(row pgx.Row) (s domain.Spec, err error) {
	err = row.Scan(&s.ID, &s.AccountID, &s.ProjectID, &s.GoalID, &s.Number, &s.Revision, &s.Body, &s.JobID, &s.CreatedAt)
	utc(&s.CreatedAt)
	return s, err
}

func (r specRepo) Create(ctx context.Context, s *domain.Spec) error {
	s.ID = newID(s.ID)
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, s.AccountID, ref{"projects", s.ProjectID}, ref{"goals", s.GoalID}, ref{"jobs", s.JobID}); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, s.AccountID, string(domain.RefSpec))
		if err != nil {
			return mapErr(err)
		}
		s.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO specs (id, account_id, project_id, goal_id, number, revision, body, job_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
			s.ID, s.AccountID, s.ProjectID, s.GoalID, s.Number, s.Revision, s.Body, opt(s.JobID)).Scan(&s.CreatedAt))
	})
}

func (r specRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Spec, error) {
	return one(ctx, r.db, scanSpec, `SELECT `+specCols+` FROM specs WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r specRepo) ListByGoal(ctx context.Context, accountID, goalID uuid.UUID) ([]domain.Spec, error) {
	return many(ctx, r.db, scanSpec, `SELECT `+specCols+` FROM specs WHERE account_id = $1 AND goal_id = $2 ORDER BY revision`, accountID, goalID)
}

func (r specRepo) Latest(ctx context.Context, accountID, goalID uuid.UUID) (domain.Spec, error) {
	return one(ctx, r.db, scanSpec, `SELECT `+specCols+` FROM specs WHERE account_id = $1 AND goal_id = $2 ORDER BY revision DESC LIMIT 1`, accountID, goalID)
}

// ---- tasks

type taskRepo struct{ db dbtx }

var taskCols = `id, account_id, project_id, goal_id, spec_id, number, seq, title, body, status, ` +
	cint("issue_number") + `, ` + ctext("branch") + `, created_at, updated_at`

func scanTask(row pgx.Row) (t domain.Task, err error) {
	err = row.Scan(&t.ID, &t.AccountID, &t.ProjectID, &t.GoalID, &t.SpecID, &t.Number, &t.Seq, &t.Title, &t.Body, &t.Status,
		&t.IssueNumber, &t.Branch, &t.CreatedAt, &t.UpdatedAt)
	utc(&t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (r taskRepo) Create(ctx context.Context, t *domain.Task) error {
	t.ID = newID(t.ID)
	if t.Status == "" {
		t.Status = domain.TaskOpen
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, t.AccountID, ref{"projects", t.ProjectID}, ref{"goals", t.GoalID}, ref{"specs", t.SpecID}); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, t.AccountID, string(domain.RefTask))
		if err != nil {
			return mapErr(err)
		}
		t.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO tasks (id, account_id, project_id, goal_id, spec_id, number, seq, title, body, status, issue_number, branch)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING created_at, updated_at`,
			t.ID, t.AccountID, t.ProjectID, t.GoalID, t.SpecID, t.Number, t.Seq, t.Title, t.Body, string(t.Status),
			opt(t.IssueNumber), opt(t.Branch)).Scan(&t.CreatedAt, &t.UpdatedAt))
	})
}

func (r taskRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Task, error) {
	return one(ctx, r.db, scanTask, `SELECT `+taskCols+` FROM tasks WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r taskRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Task, error) {
	return one(ctx, r.db, scanTask, `SELECT `+taskCols+` FROM tasks WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r taskRepo) GetByIssue(ctx context.Context, accountID, projectID uuid.UUID, issueNumber int) (domain.Task, error) {
	return one(ctx, r.db, scanTask, `SELECT `+taskCols+` FROM tasks WHERE account_id = $1 AND project_id = $2 AND issue_number = $3`,
		accountID, projectID, issueNumber)
}

func (r taskRepo) List(ctx context.Context, accountID uuid.UUID, f store.TaskFilter) ([]domain.Task, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "spec_id = ?", f.SpecID)
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanTask, `SELECT `+taskCols+` FROM tasks`+w.sql("seq, number", f.Limit), w.args...)
}

func (r taskRepo) Update(ctx context.Context, t *domain.Task) error {
	err := r.db.QueryRow(ctx, `UPDATE tasks SET title = $3, body = $4, seq = $5, issue_number = $6, branch = $7, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		t.AccountID, t.ID, t.Title, t.Body, t.Seq, opt(t.IssueNumber), opt(t.Branch)).Scan(&t.UpdatedAt)
	utc(&t.UpdatedAt)
	return mapErr(err)
}

func (r taskRepo) UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.TaskStatus) error {
	return updateStatus(ctx, r.db, "tasks", accountID, id, from, to, transition[domain.TaskStatus])
}

// ---- changes

type changeRepo struct{ db dbtx }

var changeCols = `id, account_id, project_id, ` + cuuid("task_id") + `, repo, number, branch, base_ref, head_sha, state, url, created_at, updated_at`

func scanChange(row pgx.Row) (c domain.Change, err error) {
	err = row.Scan(&c.ID, &c.AccountID, &c.ProjectID, &c.TaskID, &c.Repo, &c.Number, &c.Branch, &c.BaseRef, &c.HeadSHA, &c.State, &c.URL, &c.CreatedAt, &c.UpdatedAt)
	utc(&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (r changeRepo) Upsert(ctx context.Context, c *domain.Change) error {
	if err := owned(ctx, r.db, c.AccountID, ref{"projects", c.ProjectID}, ref{"tasks", c.TaskID}); err != nil {
		return err
	}
	got, err := one(ctx, r.db, scanChange, `INSERT INTO changes (id, account_id, project_id, task_id, repo, number, branch, base_ref, head_sha, state, url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (account_id, lower(repo), number) DO UPDATE SET
			task_id = COALESCE(EXCLUDED.task_id, changes.task_id), branch = EXCLUDED.branch, base_ref = EXCLUDED.base_ref,
			head_sha = EXCLUDED.head_sha, state = EXCLUDED.state, url = EXCLUDED.url, updated_at = now()
		RETURNING `+changeCols,
		newID(c.ID), c.AccountID, c.ProjectID, opt(c.TaskID), c.Repo, c.Number, c.Branch, c.BaseRef, c.HeadSHA, string(c.State), c.URL)
	if err != nil {
		return err
	}
	*c = got
	return nil
}

func (r changeRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Change, error) {
	return one(ctx, r.db, scanChange, `SELECT `+changeCols+` FROM changes WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r changeRepo) GetByPR(ctx context.Context, accountID uuid.UUID, repo string, number int) (domain.Change, error) {
	return one(ctx, r.db, scanChange, `SELECT `+changeCols+` FROM changes WHERE account_id = $1 AND lower(repo) = lower($2) AND number = $3`,
		accountID, repo, number)
}

func (r changeRepo) List(ctx context.Context, accountID uuid.UUID, f store.ChangeFilter) ([]domain.Change, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "task_id = ?", f.TaskID)
	andNot(w, "state = ?", string(f.State))
	andNot(w, "head_sha = ?", f.HeadSHA)
	return many(ctx, r.db, scanChange, `SELECT `+changeCols+` FROM changes`+w.sql("created_at, id", f.Limit), w.args...)
}

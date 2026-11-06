package postgres

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ---- councils

type councilRepo struct{ db dbtx }

var councilCols = `id, account_id, project_id, number, ` + cuuid("goal_id") + `, kind, question, constraints, status, brief, ` +
	cuuid("gate_id") + `, ` + cuuid("run_id") + `, created_at, updated_at`

func scanCouncil(row pgx.Row) (c domain.Council, err error) {
	err = row.Scan(&c.ID, &c.AccountID, &c.ProjectID, &c.Number, &c.GoalID, &c.Kind, &c.Question, &c.Constraints, &c.Status, &c.Brief,
		&c.GateID, &c.RunID, &c.CreatedAt, &c.UpdatedAt)
	utc(&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// briefJSON is the brief column of c, NULL without a brief.
func briefJSON(c *domain.Council) (any, error) {
	if c.Brief == nil {
		return nil, nil
	}
	return json.Marshal(c.Brief)
}

func (r councilRepo) Create(ctx context.Context, c *domain.Council) error {
	c.ID = newID(c.ID)
	if c.Status == "" {
		c.Status = domain.CouncilRunning
	}
	if c.Constraints == nil {
		c.Constraints = []string{}
	}
	brief, err := briefJSON(c)
	if err != nil {
		return err
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, c.AccountID, ref{"projects", c.ProjectID}, ref{"goals", c.GoalID}, ref{"gates", c.GateID}, ref{"workflow_runs", c.RunID}); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, c.AccountID, string(domain.RefCouncil))
		if err != nil {
			return mapErr(err)
		}
		c.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO councils (id, account_id, project_id, goal_id, number, kind, question, constraints, status, brief, gate_id, run_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING created_at, updated_at`,
			c.ID, c.AccountID, c.ProjectID, opt(c.GoalID), c.Number, string(c.Kind), c.Question, c.Constraints, string(c.Status), brief,
			opt(c.GateID), opt(c.RunID)).Scan(&c.CreatedAt, &c.UpdatedAt))
	})
}

func (r councilRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error) {
	return one(ctx, r.db, scanCouncil, `SELECT `+councilCols+` FROM councils WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r councilRepo) GetForUpdate(ctx context.Context, accountID, id uuid.UUID) (domain.Council, error) {
	return one(ctx, r.db, scanCouncil, `SELECT `+councilCols+` FROM councils WHERE account_id = $1 AND id = $2 FOR UPDATE`, accountID, id)
}

func (r councilRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Council, error) {
	return one(ctx, r.db, scanCouncil, `SELECT `+councilCols+` FROM councils WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r councilRepo) List(ctx context.Context, accountID uuid.UUID, f store.CouncilFilter) ([]domain.Council, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	if f.InProjects != nil {
		w.and("project_id = ANY(?)", f.InProjects)
	}
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "gate_id = ?", f.GateID)
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanCouncil, `SELECT `+councilCols+` FROM councils`+w.sql("created_at DESC, id DESC", f.Limit), w.args...)
}

func (r councilRepo) Update(ctx context.Context, c *domain.Council) error {
	brief, err := briefJSON(c)
	if err != nil {
		return err
	}
	constraints := c.Constraints
	if constraints == nil {
		constraints = []string{}
	}
	if err := owned(ctx, r.db, c.AccountID, ref{"gates", c.GateID}, ref{"workflow_runs", c.RunID}); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `UPDATE councils SET constraints = $3, brief = $4, gate_id = $5, run_id = $6, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		c.AccountID, c.ID, constraints, brief, opt(c.GateID), opt(c.RunID)).Scan(&c.UpdatedAt))
}

func (r councilRepo) UpdateStatus(ctx context.Context, accountID, id uuid.UUID, from, to domain.CouncilStatus) error {
	return updateStatus(ctx, r.db, "councils", accountID, id, from, to, func(a, b domain.CouncilStatus) error {
		_, err := a.Transition(b)
		return err
	})
}

var contributionCols = `id, account_id, council_id, role, runtime, stage, round, ` + cuuid("job_id") + `, content, evidence_check, created_at`

func scanContribution(row pgx.Row) (c domain.CouncilContribution, err error) {
	err = row.Scan(&c.ID, &c.AccountID, &c.CouncilID, &c.Role, &c.Runtime, &c.Stage, &c.Round, &c.JobID, &c.Content, &c.EvidenceCheck, &c.CreatedAt)
	utc(&c.CreatedAt)
	return c, err
}

func (r councilRepo) AddContribution(ctx context.Context, c *domain.CouncilContribution) error {
	c.ID = newID(c.ID)
	if err := owned(ctx, r.db, c.AccountID, ref{"councils", c.CouncilID}, ref{"jobs", c.JobID}); err != nil {
		return err
	}
	var checks any
	if c.EvidenceCheck != nil {
		raw, err := json.Marshal(c.EvidenceCheck)
		if err != nil {
			return err
		}
		checks = raw
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO council_contributions (id, account_id, council_id, role, runtime, stage, round, job_id, content, evidence_check)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at`,
		c.ID, c.AccountID, c.CouncilID, c.Role, c.Runtime, string(c.Stage), c.Round, opt(c.JobID), []byte(c.Content), checks).Scan(&c.CreatedAt))
}

func (r councilRepo) SetEvidenceCheck(ctx context.Context, accountID, contributionID uuid.UUID, checks []domain.SourceCheck) error {
	if checks == nil {
		checks = []domain.SourceCheck{}
	}
	raw, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	return mustUpdate(ctx, r.db, `UPDATE council_contributions SET evidence_check = $3 WHERE account_id = $1 AND id = $2`, accountID, contributionID, raw)
}

func (r councilRepo) Contributions(ctx context.Context, accountID, councilID uuid.UUID, stage domain.CouncilStage) ([]domain.CouncilContribution, error) {
	w := newWhere(accountID).and("council_id = ?", councilID)
	andNot(w, "stage = ?", string(stage))
	return many(ctx, r.db, scanContribution, `SELECT `+contributionCols+` FROM council_contributions`+w.sql("created_at, id", 0), w.args...)
}

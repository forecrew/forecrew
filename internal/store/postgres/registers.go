package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Assumptions implements store.Store.
func (s *Store) Assumptions() store.AssumptionRepo { return assumptionRepo{s.db} }

// Decisions implements store.Store.
func (s *Store) Decisions() store.DecisionRepo { return decisionRepo{s.db} }

// ---- assumptions

type assumptionRepo struct{ db dbtx }

var assumptionCols = `id, account_id, project_id, number, ` + cuuid("goal_id") + `, statement, made_by, ` + cuuid("job_id") + `, ` +
	cuuid("spec_id") + `, ` + cuuid("council_id") + `, confidence, impact, status, verify_by, evidence, dependents, ` +
	ctext("resolved_by") + `, ` + ctime("resolved_at") + `, created_at, updated_at`

func scanAssumption(row pgx.Row) (a domain.Assumption, err error) {
	var deps []byte
	err = row.Scan(&a.ID, &a.AccountID, &a.ProjectID, &a.Number, &a.GoalID, &a.Statement, &a.MadeBy, &a.JobID, &a.SpecID, &a.CouncilID,
		&a.Confidence, &a.Impact, &a.Status, &a.VerifyBy, &a.Evidence, &deps, &a.ResolvedBy, &a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	utc(&a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt)
	if err := json.Unmarshal(deps, &a.Dependents); err != nil {
		return a, fmt.Errorf("assumption %s dependents: %w", a.ID, err)
	}
	if a.Dependents == nil {
		a.Dependents = []domain.Dependent{}
	}
	return a, nil
}

func (r assumptionRepo) Create(ctx context.Context, a *domain.Assumption) error {
	a.ID = newID(a.ID)
	if a.Status == "" {
		a.Status = domain.AssumptionUnverified
	}
	if a.Dependents == nil {
		a.Dependents = []domain.Dependent{}
	}
	deps, err := json.Marshal(a.Dependents)
	if err != nil {
		return err
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		err := owned(ctx, tx, a.AccountID, ref{"projects", a.ProjectID}, ref{"goals", a.GoalID}, ref{"jobs", a.JobID},
			ref{"specs", a.SpecID}, ref{"councils", a.CouncilID})
		if err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, a.AccountID, string(domain.RefAssumption))
		if err != nil {
			return mapErr(err)
		}
		a.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO assumptions (id, account_id, project_id, number, goal_id, statement, made_by, job_id, spec_id,
				council_id, confidence, impact, status, verify_by, evidence, dependents, resolved_by, resolved_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING created_at, updated_at`,
			a.ID, a.AccountID, a.ProjectID, a.Number, opt(a.GoalID), a.Statement, a.MadeBy, opt(a.JobID), opt(a.SpecID), opt(a.CouncilID),
			string(a.Confidence), string(a.Impact), string(a.Status), string(a.VerifyBy), a.Evidence, deps, opt(a.ResolvedBy),
			optTime(a.ResolvedAt)).Scan(&a.CreatedAt, &a.UpdatedAt))
	})
}

func (r assumptionRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Assumption, error) {
	return one(ctx, r.db, scanAssumption, `SELECT `+assumptionCols+` FROM assumptions WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r assumptionRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Assumption, error) {
	return one(ctx, r.db, scanAssumption, `SELECT `+assumptionCols+` FROM assumptions WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r assumptionRepo) List(ctx context.Context, accountID uuid.UUID, f store.AssumptionFilter) ([]domain.Assumption, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "job_id = ?", f.JobID)
	andNot(w, "status = ?", string(f.Status))
	andNot(w, "impact = ?", string(f.Impact))
	andNot(w, "verify_by = ?", string(f.VerifyBy))
	return many(ctx, r.db, scanAssumption, `SELECT `+assumptionCols+` FROM assumptions`+w.sql("number", f.Limit), w.args...)
}

// AddDependents reads and writes the list under a row lock, so two callers
// adding dependents do not lose each other's.
func (r assumptionRepo) AddDependents(ctx context.Context, accountID, id uuid.UUID, deps []domain.Dependent) error {
	return inTx(ctx, r.db, func(tx dbtx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT dependents FROM assumptions WHERE account_id = $1 AND id = $2 FOR UPDATE`, accountID, id).Scan(&raw)
		if err != nil {
			return mapErr(err)
		}
		var cur []domain.Dependent
		if err := json.Unmarshal(raw, &cur); err != nil {
			return fmt.Errorf("assumption %s dependents: %w", id, err)
		}
		merged := store.MergeDependents(cur, deps)
		if len(merged) == len(cur) {
			return nil
		}
		out, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		return mustUpdate(ctx, tx, `UPDATE assumptions SET dependents = $3, updated_at = now() WHERE account_id = $1 AND id = $2`, accountID, id, out)
	})
}

func (r assumptionRepo) Resolve(ctx context.Context, a *domain.Assumption, from domain.AssumptionStatus) error {
	if _, err := from.Transition(a.Status); err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE assumptions SET status = $4, evidence = $5, resolved_by = $6, resolved_at = $7, updated_at = $8
		WHERE account_id = $1 AND id = $2 AND status = $3`,
		a.AccountID, a.ID, string(from), string(a.Status), a.Evidence, opt(a.ResolvedBy), optTime(a.ResolvedAt), a.UpdatedAt.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := r.Get(ctx, a.AccountID, a.ID)
	if err != nil {
		return err
	}
	return fmt.Errorf("assumption %s is %s, not %s: %w", cur.Ref(), cur.Status, from, store.ErrConflict)
}

// RaiseImpact keeps the higher of the stored impact and impact.
func (r assumptionRepo) RaiseImpact(ctx context.Context, accountID, id uuid.UUID, impact domain.Level) error {
	tag, err := r.db.Exec(ctx, `UPDATE assumptions SET impact = $3, updated_at = now() WHERE account_id = $1 AND id = $2
		AND array_position(ARRAY['low','medium','high'], impact) < array_position(ARRAY['low','medium','high'], $3::text)`,
		accountID, id, string(impact))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		_, err = r.Get(ctx, accountID, id) // ErrNotFound, or nothing to raise
	}
	return err
}

// Lock takes a transaction-scoped advisory lock named after the goal.
func (r assumptionRepo) Lock(ctx context.Context, accountID, projectID, goalID uuid.UUID) error {
	key := fmt.Sprintf("assumptions/%s/%s/%s", accountID, projectID, goalID)
	_, err := r.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return mapErr(err)
}

// ---- decisions

type decisionRepo struct{ db dbtx }

var decisionCols = `id, account_id, project_id, number, ` + cuuid("goal_id") + `, question, options, choice, reasons, dissent, decided_by, ` +
	cuuid("council_id") + `, ` + cuuid("gate_id") + `, assumptions, created_at`

func scanDecision(row pgx.Row) (d domain.Decision, err error) {
	var options []byte
	err = row.Scan(&d.ID, &d.AccountID, &d.ProjectID, &d.Number, &d.GoalID, &d.Question, &options, &d.Choice, &d.Reasons, &d.Dissent,
		&d.DecidedBy, &d.CouncilID, &d.GateID, &d.Assumptions, &d.CreatedAt)
	if err != nil {
		return d, err
	}
	utc(&d.CreatedAt)
	if err := json.Unmarshal(options, &d.Options); err != nil {
		return d, fmt.Errorf("decision %s options: %w", d.ID, err)
	}
	if d.Options == nil {
		d.Options = []domain.DecisionOption{}
	}
	if d.Assumptions == nil {
		d.Assumptions = []uuid.UUID{}
	}
	return d, nil
}

func (r decisionRepo) Create(ctx context.Context, d *domain.Decision) error {
	d.ID = newID(d.ID)
	if d.Options == nil {
		d.Options = []domain.DecisionOption{}
	}
	if d.Assumptions == nil {
		d.Assumptions = []uuid.UUID{}
	}
	options, err := json.Marshal(d.Options)
	if err != nil {
		return err
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		refs := []ref{{"projects", d.ProjectID}, {"goals", d.GoalID}, {"councils", d.CouncilID}, {"gates", d.GateID}}
		for _, id := range d.Assumptions {
			refs = append(refs, ref{"assumptions", id})
		}
		if err := owned(ctx, tx, d.AccountID, refs...); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, d.AccountID, string(domain.RefDecision))
		if err != nil {
			return mapErr(err)
		}
		d.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO decisions (id, account_id, project_id, number, goal_id, question, options, choice, reasons, dissent,
				decided_by, council_id, gate_id, assumptions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING created_at`,
			d.ID, d.AccountID, d.ProjectID, d.Number, opt(d.GoalID), d.Question, options, d.Choice, d.Reasons, d.Dissent,
			d.DecidedBy, opt(d.CouncilID), opt(d.GateID), d.Assumptions).Scan(&d.CreatedAt))
	})
}

func (r decisionRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Decision, error) {
	return one(ctx, r.db, scanDecision, `SELECT `+decisionCols+` FROM decisions WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r decisionRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Decision, error) {
	return one(ctx, r.db, scanDecision, `SELECT `+decisionCols+` FROM decisions WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r decisionRepo) List(ctx context.Context, accountID uuid.UUID, f store.DecisionFilter) ([]domain.Decision, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "goal_id = ?", f.GoalID)
	andNot(w, "council_id = ?", f.CouncilID)
	return many(ctx, r.db, scanDecision, `SELECT `+decisionCols+` FROM decisions`+w.sql("number", f.Limit), w.args...)
}

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

type proposalRepo struct{ db dbtx }

var proposalCols = `id, account_id, project_id, number, scope, category, title, rationale, evidence, benefit, effort, risk, action, measure, status,
	dedupe_key, evidence_weight, ` + ctime("snoozed_until") + `, ` + ctext("decided_by") + `, ` + ctext("reason") + `, outcome, ` + cuuid("gate_id") + `, ` + cuuid("goal_id") + `, created_at, updated_at`

func scanProposal(row pgx.Row) (p domain.Proposal, err error) {
	err = row.Scan(&p.ID, &p.AccountID, &p.ProjectID, &p.Number, &p.Scope, &p.Category, &p.Title, &p.Rationale, &p.Evidence, &p.Benefit, &p.Effort, &p.Risk,
		&p.Action, &p.Measure, &p.Status, &p.DedupeKey, &p.EvidenceWeight, &p.SnoozedUntil, &p.DecidedBy, &p.Reason, &p.Outcome, &p.GateID, &p.GoalID, &p.CreatedAt, &p.UpdatedAt)
	utc(&p.SnoozedUntil, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// proposalJSON encodes the JSON columns of p; a nil measure or outcome is
// NULL.
func proposalJSON(p *domain.Proposal) (evidence, action []byte, measure, outcome any, err error) {
	if p.Evidence == nil {
		p.Evidence = []domain.ProposalEvidence{}
	}
	if evidence, err = json.Marshal(p.Evidence); err != nil {
		return nil, nil, nil, nil, err
	}
	if action, err = json.Marshal(p.Action); err != nil {
		return nil, nil, nil, nil, err
	}
	if p.Measure != nil {
		if measure, err = json.Marshal(p.Measure); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	if p.Outcome != nil {
		if outcome, err = json.Marshal(p.Outcome); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return evidence, action, measure, outcome, nil
}

func (r proposalRepo) Create(ctx context.Context, p *domain.Proposal) error {
	p.ID = newID(p.ID)
	evidence, action, measure, outcome, err := proposalJSON(p)
	if err != nil {
		return err
	}
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := owned(ctx, tx, p.AccountID, ref{"projects", p.ProjectID}, ref{"gates", p.GateID}, ref{"goals", p.GoalID}); err != nil {
			return err
		}
		n, err := nextNumber(ctx, tx, p.AccountID, string(domain.RefProposal))
		if err != nil {
			return mapErr(err)
		}
		p.Number = n
		err = tx.QueryRow(ctx, `INSERT INTO proposals (id, account_id, project_id, number, scope, category, title, rationale, evidence, benefit, effort, risk,
				action, measure, status, dedupe_key, evidence_weight, snoozed_until, decided_by, reason, outcome, gate_id, goal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, NULLIF($19, ''), NULLIF($20, ''), $21, $22, $23)
			RETURNING created_at, updated_at`,
			p.ID, p.AccountID, p.ProjectID, p.Number, string(p.Scope), string(p.Category), p.Title, p.Rationale, evidence, p.Benefit, string(p.Effort), string(p.Risk),
			action, measure, string(p.Status), p.DedupeKey, p.EvidenceWeight, optTime(p.SnoozedUntil), p.DecidedBy, p.Reason, outcome, opt(p.GateID), opt(p.GoalID),
		).Scan(&p.CreatedAt, &p.UpdatedAt)
		utc(&p.CreatedAt, &p.UpdatedAt)
		return mapErr(err)
	})
}

func (r proposalRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Proposal, error) {
	return one(ctx, r.db, scanProposal, `SELECT `+proposalCols+` FROM proposals WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r proposalRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Proposal, error) {
	return one(ctx, r.db, scanProposal, `SELECT `+proposalCols+` FROM proposals WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r proposalRepo) GetOpen(ctx context.Context, accountID, projectID uuid.UUID, dedupeKey string) (domain.Proposal, error) {
	return one(ctx, r.db, scanProposal, `SELECT `+proposalCols+` FROM proposals
		WHERE account_id = $1 AND project_id = $2 AND dedupe_key = $3 AND status IN ('proposed', 'accepted', 'snoozed')`, accountID, projectID, dedupeKey)
}

func (r proposalRepo) List(ctx context.Context, accountID uuid.UUID, f store.ProposalFilter) ([]domain.Proposal, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "status = ?", string(f.Status))
	andNot(w, "category = ?", string(f.Category))
	andNot(w, "dedupe_key = ?", f.DedupeKey)
	andNot(w, "gate_id = ?", f.GateID)
	andNot(w, "goal_id = ?", f.GoalID)
	return many(ctx, r.db, scanProposal, `SELECT `+proposalCols+` FROM proposals`+w.sql("number", f.Limit), w.args...)
}

func (r proposalRepo) Update(ctx context.Context, p *domain.Proposal, from domain.ProposalStatus) error {
	if p.Status != from {
		if _, err := from.Transition(p.Status); err != nil {
			return err
		}
	}
	evidence, action, measure, outcome, err := proposalJSON(p)
	if err != nil {
		return err
	}
	if err := owned(ctx, r.db, p.AccountID, ref{"gates", p.GateID}, ref{"goals", p.GoalID}); err != nil {
		return err
	}
	err = r.db.QueryRow(ctx, `UPDATE proposals SET scope = $4, category = $5, title = $6, rationale = $7, evidence = $8, benefit = $9, effort = $10, risk = $11,
			action = $12, measure = $13, status = $14, dedupe_key = $15, evidence_weight = $16, snoozed_until = $17, decided_by = NULLIF($18, ''),
			reason = NULLIF($19, ''), outcome = $20, gate_id = $21, goal_id = $22, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND status = $3 RETURNING updated_at`,
		p.AccountID, p.ID, string(from), string(p.Scope), string(p.Category), p.Title, p.Rationale, evidence, p.Benefit, string(p.Effort), string(p.Risk),
		action, measure, string(p.Status), p.DedupeKey, p.EvidenceWeight, optTime(p.SnoozedUntil), p.DecidedBy, p.Reason, outcome, opt(p.GateID), opt(p.GoalID),
	).Scan(&p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("proposal %s is not %q: %w", p.Ref(), from, store.ErrConflict)
	}
	utc(&p.UpdatedAt)
	return mapErr(err)
}

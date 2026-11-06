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

// ---- change plans

type configPlanRepo struct{ db dbtx }

var configPlanCols = `id, account_id, status, source, client, title, request, applies, changes, steps, needs, affects, cost, undo, reversible,
	lowers_protection, confirm_word, base_digest, untrusted, created_by, ` + cuuid("conversation_id") + `, ` + cuuid("job_id") + `, ` + cuuid("reverts") + `,
	expires_at, ` + ctext("applied_by") + `, ` + ctime("applied_at") + `, ` + ctext("pr_url") + `, created_at, updated_at`

func scanConfigPlan(row pgx.Row) (p domain.ConfigPlan, err error) {
	var changes, steps []byte
	var needs []string
	err = row.Scan(&p.ID, &p.AccountID, &p.Status, &p.Source, &p.Client, &p.Title, &p.Request, &p.Applies, &changes, &steps, &needs, &p.Affects, &p.Cost, &p.Undo, &p.Reversible,
		&p.LowersProtection, &p.ConfirmWord, &p.BaseDigest, &p.Untrusted, &p.CreatedBy, &p.ConversationID, &p.JobID, &p.Reverts,
		&p.ExpiresAt, &p.AppliedBy, &p.AppliedAt, &p.PRURL, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(changes, &p.Changes); err != nil {
		return p, fmt.Errorf("config plan %s: changes: %w", p.ID, err)
	}
	if err := json.Unmarshal(steps, &p.Steps); err != nil {
		return p, fmt.Errorf("config plan %s: steps: %w", p.ID, err)
	}
	p.Needs = fromStrings[domain.Permission](needs)
	utc(&p.ExpiresAt, &p.AppliedAt, &p.CreatedAt, &p.UpdatedAt)
	return p, nil
}

func (r configPlanRepo) Create(ctx context.Context, p *domain.ConfigPlan) error {
	p.ID = newID(p.ID)
	changes, err := json.Marshal(p.Changes)
	if err != nil {
		return fmt.Errorf("encode changes: %w", err)
	}
	steps, err := json.Marshal(p.Steps) //nolint:gosec // Secret is the name of a secret, not a value
	if err != nil {
		return fmt.Errorf("encode steps: %w", err)
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO config_plans (id, account_id, status, source, client, title, request, applies, changes, steps, needs, affects, cost, undo,
			reversible, lowers_protection, confirm_word, base_digest, untrusted, created_by, conversation_id, job_id, reverts, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24) RETURNING created_at, updated_at`,
		p.ID, p.AccountID, p.Status, p.Source, p.Client, p.Title, p.Request, p.Applies, changes, steps, toStrings(p.Needs), p.Affects, p.Cost, p.Undo,
		p.Reversible, toStrings(p.LowersProtection), p.ConfirmWord, p.BaseDigest, p.Untrusted, p.CreatedBy, opt(p.ConversationID), opt(p.JobID), opt(p.Reverts), p.ExpiresAt).Scan(&p.CreatedAt, &p.UpdatedAt))
}

func (r configPlanRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.ConfigPlan, error) {
	return one(ctx, r.db, scanConfigPlan, `SELECT `+configPlanCols+` FROM config_plans WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r configPlanRepo) List(ctx context.Context, accountID uuid.UUID, f store.ConfigPlanFilter) ([]domain.ConfigPlan, error) {
	w := newWhere(accountID)
	andNot(w, "status = ?", f.Status)
	andNot(w, "created_by = ?", f.CreatedBy)
	andNot(w, "conversation_id = ?", f.ConversationID)
	if f.Open {
		w.conds = append(w.conds, "status IN ('pending','awaiting_approval')")
	}
	return many(ctx, r.db, scanConfigPlan, `SELECT `+configPlanCols+` FROM config_plans`+w.sql("created_at DESC, id DESC", f.Limit), w.args...)
}

func (r configPlanRepo) Move(ctx context.Context, p *domain.ConfigPlan, from domain.ConfigPlanStatus) error {
	if _, err := from.Transition(p.Status); err != nil {
		return err
	}
	changes, err := json.Marshal(p.Changes)
	if err != nil {
		return fmt.Errorf("encode changes: %w", err)
	}
	tag, err := r.db.Exec(ctx, `UPDATE config_plans SET status = $4, changes = $5, applied_by = $6, applied_at = $7, pr_url = $8, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND status = $3`,
		p.AccountID, p.ID, from, p.Status, changes, opt(p.AppliedBy), optTime(p.AppliedAt), opt(p.PRURL))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("config plan %s is not %s: %w", p.ID, from, store.ErrConflict)
	}
	return nil
}

// ---- settings

type configSettingRepo struct{ db dbtx }

const configSettingCols = `id, ` + `COALESCE(account_id, ` + zeroUUID + `), kind, name, spec, updated_by, created_at, updated_at`

func scanConfigSetting(row pgx.Row) (s domain.ConfigSetting, err error) {
	var spec []byte
	err = row.Scan(&s.ID, &s.AccountID, &s.Kind, &s.Name, &spec, &s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt)
	s.Spec = spec
	utc(&s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func (r configSettingRepo) List(ctx context.Context, accountID uuid.UUID, kind string) ([]domain.ConfigSetting, error) {
	return many(ctx, r.db, scanConfigSetting, `SELECT `+configSettingCols+` FROM config_settings
		WHERE account_id IS NOT DISTINCT FROM $1 AND kind = $2 ORDER BY name`, opt(accountID), kind)
}

func (r configSettingRepo) ListAll(ctx context.Context, accountID uuid.UUID) ([]domain.ConfigSetting, error) {
	return many(ctx, r.db, scanConfigSetting, `SELECT `+configSettingCols+` FROM config_settings
		WHERE account_id IS NOT DISTINCT FROM $1 ORDER BY kind, name`, opt(accountID))
}

func (r configSettingRepo) Put(ctx context.Context, s *domain.ConfigSetting) error {
	s.ID = newID(s.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO config_settings (id, account_id, kind, name, spec, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, kind, name) DO UPDATE SET spec = EXCLUDED.spec, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING id, created_at, updated_at`,
		s.ID, opt(s.AccountID), s.Kind, s.Name, []byte(s.Spec), s.UpdatedBy).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt))
}

func (r configSettingRepo) Delete(ctx context.Context, accountID uuid.UUID, kind, name string) error {
	return mustUpdate(ctx, r.db, `DELETE FROM config_settings WHERE account_id IS NOT DISTINCT FROM $1 AND kind = $2 AND name = $3`, opt(accountID), kind, name)
}

// ---- secrets

type secretRepo struct{ db dbtx }

const secretCols = `id, COALESCE(account_id, ` + zeroUUID + `), name, nonce, ciphertext, updated_by, created_at, updated_at`

func scanSecret(row pgx.Row) (s domain.StoredSecret, err error) {
	err = row.Scan(&s.ID, &s.AccountID, &s.Name, &s.Nonce, &s.Ciphertext, &s.UpdatedBy, &s.CreatedAt, &s.UpdatedAt)
	utc(&s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func (r secretRepo) Put(ctx context.Context, s *domain.StoredSecret) error {
	s.ID = newID(s.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO secrets (id, account_id, name, nonce, ciphertext, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, name) DO UPDATE SET nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING id, created_at, updated_at`,
		s.ID, opt(s.AccountID), s.Name, s.Nonce, s.Ciphertext, s.UpdatedBy).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt))
}

func (r secretRepo) Get(ctx context.Context, accountID uuid.UUID, name string) (domain.StoredSecret, error) {
	return one(ctx, r.db, scanSecret, `SELECT `+secretCols+` FROM secrets WHERE account_id IS NOT DISTINCT FROM $1 AND name = $2`, opt(accountID), name)
}

func (r secretRepo) Delete(ctx context.Context, accountID uuid.UUID, name string) error {
	return mustUpdate(ctx, r.db, `DELETE FROM secrets WHERE account_id IS NOT DISTINCT FROM $1 AND name = $2`, opt(accountID), name)
}

// ---- conversations

type conversationRepo struct{ db dbtx }

const conversationCols = `id, account_id, project_id, owner, credential, title, created_at, updated_at`

func scanConversation(row pgx.Row) (c domain.AssistantConversation, err error) {
	err = row.Scan(&c.ID, &c.AccountID, &c.ProjectID, &c.Owner, &c.Credential, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	utc(&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func (r conversationRepo) Create(ctx context.Context, c *domain.AssistantConversation) error {
	c.ID = newID(c.ID)
	if err := owned(ctx, r.db, c.AccountID, ref{"projects", c.ProjectID}); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO assistant_conversations (id, account_id, project_id, owner, credential, title)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at, updated_at`,
		c.ID, c.AccountID, c.ProjectID, c.Owner, c.Credential, c.Title).Scan(&c.CreatedAt, &c.UpdatedAt))
}

func (r conversationRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.AssistantConversation, error) {
	return one(ctx, r.db, scanConversation, `SELECT `+conversationCols+` FROM assistant_conversations WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r conversationRepo) GetByJob(ctx context.Context, accountID, jobID uuid.UUID) (domain.AssistantConversation, error) {
	return one(ctx, r.db, scanConversation, `SELECT `+conversationCols+` FROM assistant_conversations
		WHERE account_id = $1 AND id = (SELECT conversation_id FROM assistant_messages WHERE account_id = $1 AND job_id = $2 LIMIT 1)`, accountID, jobID)
}

func (r conversationRepo) List(ctx context.Context, accountID uuid.UUID, owner string, limit int) ([]domain.AssistantConversation, error) {
	w := newWhere(accountID).and("owner = ?", owner)
	return many(ctx, r.db, scanConversation, `SELECT `+conversationCols+` FROM assistant_conversations`+w.sql("created_at DESC, id DESC", limit), w.args...)
}

func (r conversationRepo) AddMessage(ctx context.Context, m *domain.AssistantMessage) error {
	m.ID = newID(m.ID)
	return inTx(ctx, r.db, func(tx dbtx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO assistant_messages (id, account_id, conversation_id, author, body, job_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
			m.ID, m.AccountID, m.ConversationID, m.Author, m.Body, opt(m.JobID)).Scan(&m.CreatedAt); err != nil {
			return mapErr(err)
		}
		return mustUpdate(ctx, tx, `UPDATE assistant_conversations SET updated_at = now() WHERE account_id = $1 AND id = $2`, m.AccountID, m.ConversationID)
	})
}

const messageCols = `id, account_id, conversation_id, author, body, ` + `COALESCE(job_id, ` + zeroUUID + `), created_at`

func (r conversationRepo) Messages(ctx context.Context, accountID, conversationID uuid.UUID) ([]domain.AssistantMessage, error) {
	return many(ctx, r.db, func(row pgx.Row) (m domain.AssistantMessage, err error) {
		err = row.Scan(&m.ID, &m.AccountID, &m.ConversationID, &m.Author, &m.Body, &m.JobID, &m.CreatedAt)
		utc(&m.CreatedAt)
		return m, err
	}, `SELECT `+messageCols+` FROM assistant_messages WHERE account_id = $1 AND conversation_id = $2 ORDER BY created_at, id`, accountID, conversationID)
}

package postgres

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ---- job events

type jobEventRepo struct{ db dbtx }

func scanJobEvent(row pgx.Row) (e domain.JobEvent, err error) {
	var data []byte
	err = row.Scan(&e.ID, &e.AccountID, &e.JobID, &e.Seq, &e.Time, &e.Type, &e.Text, &data)
	e.Data = data
	utc(&e.Time)
	return e, err
}

func (r jobEventRepo) Append(ctx context.Context, accountID, jobID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	return r.append(ctx, accountID, jobID, uuid.Nil(), events)
}

// AppendLeased is Append for a worker: the job row is locked and must be
// running under workerID, the same lock complete, cancel and the lease
// reaper take, so no event is stored once the lease has ended.
func (r jobEventRepo) AppendLeased(ctx context.Context, accountID, jobID, workerID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	return r.append(ctx, accountID, jobID, workerID, events)
}

// append stores events; a non-nil workerID also requires that worker's lease.
func (r jobEventRepo) append(ctx context.Context, accountID, jobID, workerID uuid.UUID, events []domain.JobEvent) ([]domain.JobEvent, error) {
	out := make([]domain.JobEvent, 0, len(events))
	err := inTx(ctx, r.db, func(tx dbtx) error {
		// The row lock serializes concurrent appends to one job.
		if err := lockLeased(ctx, tx, accountID, jobID, workerID); err != nil {
			return err
		}
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM job_events WHERE job_id = $1`, jobID).Scan(&seq); err != nil {
			return err
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		for _, e := range events {
			seq++
			e.ID, e.AccountID, e.JobID, e.Seq = domain.NewID(), accountID, jobID, seq
			if e.Time.IsZero() {
				e.Time = now
			}
			e.Time = e.Time.UTC()
			if _, err := tx.Exec(ctx, `INSERT INTO job_events (id, account_id, job_id, seq, time, type, text, data)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				e.ID, e.AccountID, e.JobID, e.Seq, e.Time, string(e.Type), e.Text, optJSON(e.Data)); err != nil {
				return mapErr(err)
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r jobEventRepo) List(ctx context.Context, accountID, jobID uuid.UUID, afterSeq int64, limit int) ([]domain.JobEvent, error) {
	w := newWhere(accountID).and("job_id = ?", jobID).and("seq > ?", afterSeq)
	return many(ctx, r.db, scanJobEvent, `SELECT id, account_id, job_id, seq, time, type, text, data FROM job_events`+w.sql("seq", limit), w.args...)
}

// ---- gates

type gateRepo struct{ db dbtx }

var gateCols = `id, account_id, project_id, number, kind, origin, status, title, summary, subject_repo, ` + cint("subject_number") + `, ` +
	cuuid("goal_id") + `, ` + cuuid("spec_id") + `, ` + cuuid("task_id") + `, ` + cuuid("job_id") + `, ` +
	ctext("resolved_by") + `, ` + ctext("feedback") + `, ` + ctime("resolved_at") + `, created_at`

func scanGate(row pgx.Row) (g domain.Gate, err error) {
	err = row.Scan(&g.ID, &g.AccountID, &g.ProjectID, &g.Number, &g.Kind, &g.Origin, &g.Status, &g.Title, &g.Summary, &g.SubjectRepo, &g.SubjectNumber,
		&g.GoalID, &g.SpecID, &g.TaskID, &g.JobID, &g.ResolvedBy, &g.Feedback, &g.ResolvedAt, &g.CreatedAt)
	utc(&g.ResolvedAt, &g.CreatedAt)
	return g, err
}

func (r gateRepo) Create(ctx context.Context, g *domain.Gate) error {
	g.ID = newID(g.ID)
	g.Status = domain.GatePending
	g.Origin = g.Origin.OrDefault()
	return inTx(ctx, r.db, func(tx dbtx) error {
		err := owned(ctx, tx, g.AccountID, ref{"projects", g.ProjectID}, ref{"goals", g.GoalID}, ref{"specs", g.SpecID},
			ref{"tasks", g.TaskID}, ref{"jobs", g.JobID})
		if err != nil {
			return err
		}
		// A gate of a job takes the job row before the account's gate
		// counter, the order of Complete and of every job tool: the other
		// way round deadlocks. The key-share lock does not block a writer
		// that does not change the key, and conflicts with Complete's
		// exclusive lock.
		if g.JobID != uuid.Nil() {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM jobs WHERE account_id = $1 AND id = $2 FOR KEY SHARE`, g.AccountID, g.JobID); err != nil {
				return mapErr(err)
			}
		}
		n, err := nextNumber(ctx, tx, g.AccountID, string(domain.RefGate))
		if err != nil {
			return mapErr(err)
		}
		g.Number = n
		return mapErr(tx.QueryRow(ctx, `INSERT INTO gates (id, account_id, project_id, number, kind, status, title, summary, subject_repo, subject_number,
				goal_id, spec_id, task_id, job_id, origin)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING created_at`,
			g.ID, g.AccountID, g.ProjectID, g.Number, string(g.Kind), string(g.Status), g.Title, g.Summary, g.SubjectRepo, opt(g.SubjectNumber),
			opt(g.GoalID), opt(g.SpecID), opt(g.TaskID), opt(g.JobID), string(g.Origin)).Scan(&g.CreatedAt))
	})
}

func (r gateRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Gate, error) {
	return one(ctx, r.db, scanGate, `SELECT `+gateCols+` FROM gates WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r gateRepo) GetByNumber(ctx context.Context, accountID uuid.UUID, number int64) (domain.Gate, error) {
	return one(ctx, r.db, scanGate, `SELECT `+gateCols+` FROM gates WHERE account_id = $1 AND number = $2`, accountID, number)
}

func (r gateRepo) List(ctx context.Context, accountID uuid.UUID, f store.GateFilter) ([]domain.Gate, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "kind = ?", string(f.Kind))
	andNot(w, "status = ?", string(f.Status))
	return many(ctx, r.db, scanGate, `SELECT `+gateCols+` FROM gates`+w.sql("number", f.Limit), w.args...)
}

func (r gateRepo) ListPendingBySubject(ctx context.Context, accountID uuid.UUID, repo string, number int) ([]domain.Gate, error) {
	return many(ctx, r.db, scanGate, `SELECT `+gateCols+` FROM gates
		WHERE account_id = $1 AND status = 'pending' AND lower(subject_repo) = lower($2) AND subject_number = $3 ORDER BY number`,
		accountID, repo, number)
}

func (r gateRepo) Resolve(ctx context.Context, g *domain.Gate) error {
	if _, err := domain.GatePending.Transition(g.Status); err != nil {
		return err
	}
	err := r.db.QueryRow(ctx, `UPDATE gates SET status = $3, resolved_by = $4, feedback = $5, resolved_at = COALESCE($6, now())
		WHERE account_id = $1 AND id = $2 AND status = 'pending' RETURNING resolved_at`,
		g.AccountID, g.ID, string(g.Status), opt(g.ResolvedBy), opt(g.Feedback), optTime(g.ResolvedAt)).Scan(&g.ResolvedAt)
	if err == nil {
		utc(&g.ResolvedAt)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return mapErr(err)
	}
	cur, err := r.Get(ctx, g.AccountID, g.ID)
	if err != nil {
		return err
	}
	return fmt.Errorf("gate %s is %s: %w", cur.Ref(), cur.Status, store.ErrConflict)
}

// ---- signals

type signalRepo struct{ db dbtx }

const signalCols = `id, account_id, project_id, kind, key, dedupe_key, payload, created_at`

func scanSignal(row pgx.Row) (s domain.Signal, err error) {
	var payload []byte
	err = row.Scan(&s.ID, &s.AccountID, &s.ProjectID, &s.Kind, &s.Key, &s.DedupeKey, &payload, &s.CreatedAt)
	s.Payload = payload
	utc(&s.CreatedAt)
	return s, err
}

// Insert stores s unless its dedupe key exists; then s is replaced by the
// stored signal.
func (r signalRepo) Insert(ctx context.Context, s *domain.Signal) (bool, error) {
	s.ID = newID(s.ID)
	if err := owned(ctx, r.db, s.AccountID, ref{"projects", s.ProjectID}); err != nil {
		return false, err
	}
	tag, err := r.db.Exec(ctx, `INSERT INTO signals (id, account_id, project_id, kind, key, dedupe_key, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (account_id, dedupe_key) DO NOTHING`,
		s.ID, s.AccountID, s.ProjectID, string(s.Kind), s.Key, s.DedupeKey, []byte(s.Payload))
	if err != nil {
		return false, mapErr(err)
	}
	stored, err := one(ctx, r.db, scanSignal, `SELECT `+signalCols+` FROM signals WHERE account_id = $1 AND dedupe_key = $2`, s.AccountID, s.DedupeKey)
	if err != nil {
		return false, err
	}
	*s = stored
	return tag.RowsAffected() == 1, nil
}

func (r signalRepo) ListByKey(ctx context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error) {
	return many(ctx, r.db, scanSignal, `SELECT `+signalCols+` FROM signals WHERE account_id = $1 AND key = $2 ORDER BY created_at, id`, accountID, key)
}

// ---- workers

type workerRepo struct{ db dbtx }

var workerCols = `id, account_id, name, pool, ephemeral, token_hash, labels, version, max_jobs, ` + ctime("last_seen_at") + `, ` + ctime("revoked_at") + `, created_at, agents`

func scanWorker(row pgx.Row) (w domain.Worker, err error) {
	err = row.Scan(&w.ID, &w.AccountID, &w.Name, &w.Pool, &w.Ephemeral, &w.TokenHash, &w.Labels, &w.Version, &w.MaxJobs, &w.LastSeenAt, &w.RevokedAt, &w.CreatedAt, &w.Agents)
	if len(w.Agents) == 0 {
		w.Agents = nil
	}
	utc(&w.LastSeenAt, &w.RevokedAt, &w.CreatedAt)
	return w, err
}

func (r workerRepo) Create(ctx context.Context, w *domain.Worker) error {
	w.ID = newID(w.ID)
	w.Labels = domain.NormalizeLabels(w.Labels)
	w.Pool = domain.OrDefaultPool(w.Pool)
	if w.MaxJobs == 0 {
		w.MaxJobs = 1
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO workers (id, account_id, name, token_hash, labels, version, max_jobs, pool, ephemeral)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at`,
		w.ID, w.AccountID, w.Name, w.TokenHash, w.Labels, w.Version, w.MaxJobs, w.Pool, w.Ephemeral).Scan(&w.CreatedAt))
}

func (r workerRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.Worker, error) {
	return one(ctx, r.db, scanWorker, `SELECT `+workerCols+` FROM workers WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r workerRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.Worker, error) {
	return one(ctx, r.db, scanWorker, `SELECT `+workerCols+` FROM workers WHERE token_hash = $1`, hash)
}

func (r workerRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Worker, error) {
	return many(ctx, r.db, scanWorker, `SELECT `+workerCols+` FROM workers WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r workerRepo) Seen(ctx context.Context, accountID, id uuid.UUID, labels []string, version string) error {
	return mustUpdate(ctx, r.db, `UPDATE workers SET last_seen_at = now(), labels = $3, version = $4 WHERE account_id = $1 AND id = $2`,
		accountID, id, domain.NormalizeLabels(labels), version)
}

func (r workerRepo) SetAgents(ctx context.Context, accountID, id uuid.UUID, agents []domain.WorkerAgent) error {
	if agents == nil {
		agents = []domain.WorkerAgent{}
	}
	return mustUpdate(ctx, r.db, `UPDATE workers SET agents = $3 WHERE account_id = $1 AND id = $2`, accountID, id, agents)
}

func (r workerRepo) SetPool(ctx context.Context, accountID, id uuid.UUID, pool string) error {
	return mustUpdate(ctx, r.db, `UPDATE workers SET pool = $3 WHERE account_id = $1 AND id = $2`, accountID, id, domain.OrDefaultPool(pool))
}

func (r workerRepo) Revoke(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE workers SET revoked_at = COALESCE(revoked_at, now()) WHERE account_id = $1 AND id = $2`, accountID, id)
}

// ---- workflow runs

type workflowRunRepo struct{ db dbtx }

var runCols = `id, account_id, project_id, playbook, status, ` + cuuid("goal_id") + `, ` + cuuid("task_id") + `, inputs, state, ` +
	ctext("error") + `, cancel_reason, created_at, updated_at, ` + ctime("finished_at")

func scanRun(row pgx.Row) (r domain.WorkflowRun, err error) {
	var inputs, state []byte
	err = row.Scan(&r.ID, &r.AccountID, &r.ProjectID, &r.Playbook, &r.Status, &r.GoalID, &r.TaskID, &inputs, &state,
		&r.Error, &r.CancelReason, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	r.Inputs, r.State = inputs, state
	utc(&r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	return r, err
}

func jsonOrEmpty(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

func (r workflowRunRepo) Create(ctx context.Context, run *domain.WorkflowRun) error {
	run.ID = newID(run.ID)
	if run.Status == "" {
		run.Status = domain.RunRunning
	}
	err := owned(ctx, r.db, run.AccountID, ref{"projects", run.ProjectID}, ref{"goals", run.GoalID}, ref{"tasks", run.TaskID})
	if err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO workflow_runs (id, account_id, project_id, playbook, status, goal_id, task_id, inputs, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING created_at, updated_at`,
		run.ID, run.AccountID, run.ProjectID, run.Playbook, string(run.Status), opt(run.GoalID), opt(run.TaskID),
		jsonOrEmpty(run.Inputs), jsonOrEmpty(run.State)).Scan(&run.CreatedAt, &run.UpdatedAt))
}

func (r workflowRunRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error) {
	return one(ctx, r.db, scanRun, `SELECT `+runCols+` FROM workflow_runs WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r workflowRunRepo) ListActive(ctx context.Context, accountID uuid.UUID) ([]domain.WorkflowRun, error) {
	return many(ctx, r.db, scanRun, `SELECT `+runCols+` FROM workflow_runs
		WHERE account_id = $1 AND status IN ('running', 'waiting') ORDER BY created_at, id`, accountID)
}

func (r workflowRunRepo) Update(ctx context.Context, run *domain.WorkflowRun) error {
	err := r.db.QueryRow(ctx, `UPDATE workflow_runs SET status = $3, state = $4, error = $5, finished_at = $6, updated_at = now()
		WHERE account_id = $1 AND id = $2 RETURNING updated_at`,
		run.AccountID, run.ID, string(run.Status), jsonOrEmpty(run.State), opt(run.Error), optTime(run.FinishedAt)).Scan(&run.UpdatedAt)
	utc(&run.UpdatedAt)
	return mapErr(err)
}

func (r workflowRunRepo) RequestCancel(ctx context.Context, accountID, id uuid.UUID, reason string) error {
	_, err := r.db.Exec(ctx, `UPDATE workflow_runs SET cancel_reason = $3, updated_at = now()
		WHERE account_id = $1 AND id = $2 AND status IN ('running', 'waiting')`, accountID, id, reason)
	return mapErr(err)
}

// ---- step runs

type stepRunRepo struct{ db dbtx }

var stepCols = `id, account_id, run_id, step_id, attempt, status, ` + ctext("wait_key") + `, ` + cuuid("job_id") + `, ` +
	cuuid("gate_id") + `, ` + cuuid("signal_id") + `, output, ` + ctext("error") + `, created_at, ` + ctime("started_at") + `, ` + ctime("finished_at")

func scanStep(row pgx.Row) (s domain.StepRun, err error) {
	var output []byte
	err = row.Scan(&s.ID, &s.AccountID, &s.RunID, &s.StepID, &s.Attempt, &s.Status, &s.WaitKey, &s.JobID, &s.GateID, &s.SignalID,
		&output, &s.Error, &s.CreatedAt, &s.StartedAt, &s.FinishedAt)
	s.Output = output
	utc(&s.CreatedAt, &s.StartedAt, &s.FinishedAt)
	return s, err
}

func (r stepRunRepo) Create(ctx context.Context, s *domain.StepRun) error {
	s.ID = newID(s.ID)
	if s.Attempt == 0 {
		s.Attempt = 1
	}
	if s.Status == "" {
		s.Status = domain.StepPending
	}
	err := owned(ctx, r.db, s.AccountID, ref{"workflow_runs", s.RunID}, ref{"jobs", s.JobID}, ref{"gates", s.GateID}, ref{"signals", s.SignalID})
	if err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO step_runs (id, account_id, run_id, step_id, attempt, status, wait_key, job_id, gate_id, signal_id,
			output, error, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING created_at`,
		s.ID, s.AccountID, s.RunID, s.StepID, s.Attempt, string(s.Status), opt(s.WaitKey), opt(s.JobID), opt(s.GateID), opt(s.SignalID),
		optJSON(s.Output), opt(s.Error), optTime(s.StartedAt), optTime(s.FinishedAt)).Scan(&s.CreatedAt))
}

func (r stepRunRepo) Get(ctx context.Context, accountID, id uuid.UUID) (domain.StepRun, error) {
	return one(ctx, r.db, scanStep, `SELECT `+stepCols+` FROM step_runs WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r stepRunRepo) ListByRun(ctx context.Context, accountID, runID uuid.UUID) ([]domain.StepRun, error) {
	return many(ctx, r.db, scanStep, `SELECT `+stepCols+` FROM step_runs WHERE account_id = $1 AND run_id = $2 ORDER BY created_at, id`, accountID, runID)
}

func (r stepRunRepo) ListWaiting(ctx context.Context, accountID uuid.UUID, key string) ([]domain.StepRun, error) {
	return many(ctx, r.db, scanStep, `SELECT `+stepCols+` FROM step_runs
		WHERE account_id = $1 AND status = 'waiting' AND wait_key = $2 ORDER BY created_at, id`, accountID, key)
}

func (r stepRunRepo) Update(ctx context.Context, s *domain.StepRun) error {
	if err := owned(ctx, r.db, s.AccountID, ref{"jobs", s.JobID}, ref{"gates", s.GateID}, ref{"signals", s.SignalID}); err != nil {
		return err
	}
	return mustUpdate(ctx, r.db, `UPDATE step_runs SET status = $3, wait_key = $4, job_id = $5, gate_id = $6, signal_id = $7,
			output = $8, error = $9, started_at = $10, finished_at = $11
		WHERE account_id = $1 AND id = $2`,
		s.AccountID, s.ID, string(s.Status), opt(s.WaitKey), opt(s.JobID), opt(s.GateID), opt(s.SignalID),
		optJSON(s.Output), opt(s.Error), optTime(s.StartedAt), optTime(s.FinishedAt))
}

// ---- webhook deliveries

type webhookRepo struct{ db dbtx }

func (r webhookRepo) Record(ctx context.Context, d domain.WebhookDelivery) (bool, error) {
	tag, err := r.db.Exec(ctx, `INSERT INTO webhook_deliveries (delivery_id, event) VALUES ($1, $2) ON CONFLICT (delivery_id) DO NOTHING`,
		d.DeliveryID, d.Event)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---- MCP tokens

type mcpTokenRepo struct{ db dbtx }

var tokenCols = `id, account_id, name, token_hash, ` + ctext("owner_kind") + `, ` + cuuid("owner_id") + `, scopes, ` +
	ctime("expires_at") + `, ` + ctime("last_used_at") + `, ` + ctime("revoked_at") + `, created_at`

func scanToken(row pgx.Row) (t domain.MCPToken, err error) {
	var scopes []string
	err = row.Scan(&t.ID, &t.AccountID, &t.Name, &t.TokenHash, &t.OwnerKind, &t.OwnerID, &scopes,
		&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt, &t.CreatedAt)
	t.Scopes = fromStrings[domain.Scope](scopes)
	utc(&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt, &t.CreatedAt)
	return t, err
}

func (r mcpTokenRepo) Create(ctx context.Context, t *domain.MCPToken) error {
	t.ID = newID(t.ID)
	t.Scopes = t.ScopesOrLegacy()
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO mcp_tokens (id, account_id, name, token_hash, owner_kind, owner_id, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		t.ID, t.AccountID, t.Name, t.TokenHash, opt(t.OwnerKind), opt(t.OwnerID), toStrings(t.Scopes), opt(t.ExpiresAt)).Scan(&t.CreatedAt))
}

func (r mcpTokenRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.MCPToken, error) {
	return one(ctx, r.db, scanToken, `SELECT `+tokenCols+` FROM mcp_tokens WHERE token_hash = $1`, hash)
}

func (r mcpTokenRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.MCPToken, error) {
	return many(ctx, r.db, scanToken, `SELECT `+tokenCols+` FROM mcp_tokens WHERE account_id = $1 ORDER BY created_at, id`, accountID)
}

func (r mcpTokenRepo) Touch(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE mcp_tokens SET last_used_at = now() WHERE account_id = $1 AND id = $2`, accountID, id)
}

func (r mcpTokenRepo) Revoke(ctx context.Context, accountID, id uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE mcp_tokens SET revoked_at = COALESCE(revoked_at, now()) WHERE account_id = $1 AND id = $2`, accountID, id)
}

// ---- audit log

type auditRepo struct{ db dbtx }

var auditCols = `id, account_id, ` + cuuid("project_id") + `, actor, ` + ctext("credential") + `, action, subject_type, ` + cuuid("subject_id") + `, data, created_at`

func scanAudit(row pgx.Row) (e domain.AuditEntry, err error) {
	var data []byte
	err = row.Scan(&e.ID, &e.AccountID, &e.ProjectID, &e.Actor, &e.Credential, &e.Action, &e.SubjectType, &e.SubjectID, &data, &e.CreatedAt)
	e.Data = data
	utc(&e.CreatedAt)
	return e, err
}

// Append stamps the entry with the time of the write, not of the start of the
// transaction (clock_timestamp, not now): entries written one after the other
// under a row lock, as a connection's test and its clearing are, then sort in
// that order however early each transaction began.
func (r auditRepo) Append(ctx context.Context, e *domain.AuditEntry) error {
	e.ID = newID(e.ID)
	if err := owned(ctx, r.db, e.AccountID, ref{"projects", e.ProjectID}); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO audit_log (id, account_id, project_id, actor, credential, action, subject_type, subject_id, data, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp()) RETURNING created_at`,
		e.ID, e.AccountID, opt(e.ProjectID), e.Actor, opt(e.Credential), e.Action, e.SubjectType, opt(e.SubjectID), optJSON(e.Data)).Scan(&e.CreatedAt))
}

func (r auditRepo) List(ctx context.Context, accountID uuid.UUID, f store.AuditFilter) ([]domain.AuditEntry, error) {
	w := newWhere(accountID)
	andNot(w, "project_id = ?", f.ProjectID)
	andNot(w, "subject_id = ?", f.SubjectID)
	andNot(w, "actor = ?", f.Actor)
	andNot(w, "action = ?", f.Action)
	andNot(w, "subject_type = ?", f.SubjectType)
	if !f.Since.IsZero() {
		w.and("created_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		w.and("created_at < ?", f.Until)
	}
	return many(ctx, r.db, scanAudit, `SELECT `+auditCols+` FROM audit_log`+w.sql("created_at DESC, id DESC", f.Limit), w.args...)
}

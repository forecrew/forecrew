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

// CapacityProviders implements store.Store.
func (s *Store) CapacityProviders() store.CapacityProviderRepo { return capacityProviderRepo{s.db} }

// Shapes implements store.Store.
func (s *Store) Shapes() store.ShapeRepo { return shapeRepo{s.db} }

// Machines implements store.Store.
func (s *Store) Machines() store.MachineRepo { return machineRepo{s.db} }

// BootstrapTokens implements store.Store.
func (s *Store) BootstrapTokens() store.BootstrapTokenRepo { return bootstrapTokenRepo{s.db} }

// FleetPolicies implements store.Store.
func (s *Store) FleetPolicies() store.FleetPolicyRepo { return fleetPolicyRepo{s.db} }

// ProviderJoinCodes implements store.Store.
func (s *Store) ProviderJoinCodes() store.ProviderJoinCodeRepo { return joinCodeRepo{s.db} }

// ImageBuilds implements store.Store.
func (s *Store) ImageBuilds() store.ImageBuildRepo { return imageBuildRepo{s.db} }

// sameOrg is the condition that finds the rows of an organization, or the
// instance-wide ones for the zero ID; $1 is the ID, or NULL through opt.
const sameOrg = `account_id IS NOT DISTINCT FROM $1`

// ---- capacity providers

type capacityProviderRepo struct{ db dbtx }

var providerCols = `id, ` + cuuid("account_id") + `, name, kind, mode, status, token_hash, ` + ctime("last_seen_at") +
	`, capabilities, limits, network_allow, tags, created_at, updated_at`

func scanProvider(row pgx.Row) (p domain.CapacityProvider, err error) {
	var capabilities, limits, tags []byte
	err = row.Scan(&p.ID, &p.AccountID, &p.Name, &p.Kind, &p.Mode, &p.Status, &p.TokenHash, &p.LastSeenAt,
		&capabilities, &limits, &p.NetworkAllow, &tags, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	utc(&p.LastSeenAt, &p.CreatedAt, &p.UpdatedAt)
	for what, v := range map[string]struct {
		raw []byte
		to  any
	}{"capabilities": {capabilities, &p.Capabilities}, "limits": {limits, &p.Limits}, "tags": {tags, &p.Tags}} {
		if err := json.Unmarshal(v.raw, v.to); err != nil {
			return p, fmt.Errorf("decode provider %s: %w", what, err)
		}
	}
	return p, nil
}

func (r capacityProviderRepo) Create(ctx context.Context, p *domain.CapacityProvider) error {
	p.ID = newID(p.ID)
	if p.Mode == "" {
		p.Mode = domain.ProviderModeAgent
	}
	if p.Status == "" {
		p.Status = domain.ProviderPending
	}
	if err := p.Validate(); err != nil {
		return err
	}
	capabilities, limits, tags, err := providerJSON(p)
	if err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO capacity_providers (id, account_id, name, kind, mode, status, token_hash, capabilities, limits, network_allow, tags)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING created_at, updated_at`,
		p.ID, opt(p.AccountID), p.Name, string(p.Kind), string(p.Mode), string(p.Status), optBytes(p.TokenHash), capabilities, limits, strs(p.NetworkAllow), tags).
		Scan(&p.CreatedAt, &p.UpdatedAt))
}

func providerJSON(p *domain.CapacityProvider) (capabilities, limits, tags []byte, err error) {
	if capabilities, err = json.Marshal(p.Capabilities); err != nil {
		return nil, nil, nil, fmt.Errorf("encode capabilities: %w", err)
	}
	if limits, err = json.Marshal(p.Limits); err != nil {
		return nil, nil, nil, fmt.Errorf("encode limits: %w", err)
	}
	if p.Tags == nil {
		return capabilities, limits, []byte(`{}`), nil
	}
	if tags, err = json.Marshal(p.Tags); err != nil {
		return nil, nil, nil, fmt.Errorf("encode tags: %w", err)
	}
	return capabilities, limits, tags, nil
}

func optBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// strs keeps a nil list out of NOT NULL text[] columns.
func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (r capacityProviderRepo) Get(ctx context.Context, id uuid.UUID) (domain.CapacityProvider, error) {
	return one(ctx, r.db, scanProvider, `SELECT `+providerCols+` FROM capacity_providers WHERE id = $1`, id)
}

func (r capacityProviderRepo) GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.CapacityProvider, error) {
	return one(ctx, r.db, scanProvider, `SELECT `+providerCols+` FROM capacity_providers WHERE `+sameOrg+` AND name = $2`, opt(accountID), name)
}

func (r capacityProviderRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.CapacityProvider, error) {
	return one(ctx, r.db, scanProvider, `SELECT `+providerCols+` FROM capacity_providers WHERE token_hash = $1`, hash)
}

func (r capacityProviderRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.CapacityProvider, error) {
	return many(ctx, r.db, scanProvider, `SELECT `+providerCols+` FROM capacity_providers WHERE `+sameOrg+` ORDER BY name`, opt(accountID))
}

func (r capacityProviderRepo) ListAll(ctx context.Context) ([]domain.CapacityProvider, error) {
	return many(ctx, r.db, scanProvider, `SELECT `+providerCols+` FROM capacity_providers ORDER BY name, id`)
}

func (r capacityProviderRepo) Update(ctx context.Context, p *domain.CapacityProvider) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, limits, tags, err := providerJSON(p)
	if err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `UPDATE capacity_providers SET limits = $2, network_allow = $3, tags = $4, updated_at = now()
		WHERE id = $1 RETURNING updated_at`, p.ID, limits, strs(p.NetworkAllow), tags).Scan(&p.UpdatedAt))
}

func (r capacityProviderRepo) SetStatus(ctx context.Context, id uuid.UUID, status domain.ProviderStatus) error {
	tag, err := r.db.Exec(ctx, `UPDATE capacity_providers SET status = $2, updated_at = now()
		WHERE id = $1 AND status <> 'revoked' AND (mode = 'direct' OR $2 <> 'active' OR token_hash IS NOT NULL)`, id, string(status))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("provider %s cannot become %s: %w", id, status, store.ErrConflict)
}

func (r capacityProviderRepo) SetToken(ctx context.Context, id uuid.UUID, hash []byte) error {
	return mustUpdate(ctx, r.db, `UPDATE capacity_providers SET token_hash = $2, status = 'active', updated_at = now() WHERE id = $1 AND status <> 'revoked'`, id, hash)
}

func (r capacityProviderRepo) Seen(ctx context.Context, id uuid.UUID, capabilities *domain.ProviderCapabilities) error {
	var raw any
	if capabilities != nil {
		b, err := json.Marshal(capabilities)
		if err != nil {
			return fmt.Errorf("encode capabilities: %w", err)
		}
		raw = b
	}
	return mustUpdate(ctx, r.db, `UPDATE capacity_providers SET last_seen_at = now(), capabilities = COALESCE($2, capabilities) WHERE id = $1`, id, raw)
}

// ---- shapes

type shapeRepo struct{ db dbtx }

var shapeCols = `id, ` + cuuid("account_id") + `, provider_id, name, os, arch, cpu, memory_mb, disk_gb, gpu, pool, labels, provider_config, image,
	price_per_minute::float8, monthly_budget::float8, warm_min, max, idle_seconds, boot_timeout_seconds, max_jobs, note, enabled, created_at, updated_at`

func scanShape(row pgx.Row) (s domain.Shape, err error) {
	var config []byte
	err = row.Scan(&s.ID, &s.AccountID, &s.ProviderID, &s.Name, &s.OS, &s.Arch, &s.CPU, &s.MemoryMB, &s.DiskGB, &s.GPU, &s.Pool, &s.Labels, &config, &s.Image,
		&s.PricePerMinute, &s.MonthlyBudget, &s.WarmMin, &s.Max, &s.IdleSeconds, &s.BootTimeoutSeconds, &s.MaxJobs, &s.Note, &s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	utc(&s.CreatedAt, &s.UpdatedAt)
	if err := json.Unmarshal(config, &s.ProviderConfig); err != nil {
		return s, fmt.Errorf("decode provider_config: %w", err)
	}
	return s, nil
}

// checked normalizes and validates s and encodes its config.
func checkedShape(s *domain.Shape) ([]byte, error) {
	*s = s.WithDefaults()
	s.Labels = domain.NormalizeLabels(s.Labels)
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.ProviderConfig == nil {
		return []byte(`{}`), nil
	}
	config, err := json.Marshal(s.ProviderConfig)
	if err != nil {
		return nil, fmt.Errorf("encode provider_config: %w", err)
	}
	return config, nil
}

func (r shapeRepo) Create(ctx context.Context, s *domain.Shape) error {
	s.ID = newID(s.ID)
	config, err := checkedShape(s)
	if err != nil {
		return err
	}
	var found bool
	err = r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM capacity_providers WHERE `+sameOrg+` AND id = $2)`, opt(s.AccountID), s.ProviderID).Scan(&found)
	if err != nil {
		return mapErr(err)
	}
	if !found {
		return fmt.Errorf("provider %s: %w", s.ProviderID, store.ErrNotFound)
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO shapes (id, account_id, provider_id, name, os, arch, cpu, memory_mb, disk_gb, gpu, pool, labels, provider_config, image,
			price_per_minute, monthly_budget, warm_min, max, idle_seconds, boot_timeout_seconds, max_jobs, note, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23) RETURNING created_at, updated_at`,
		s.ID, opt(s.AccountID), s.ProviderID, s.Name, string(s.OS), string(s.Arch), s.CPU, s.MemoryMB, s.DiskGB, s.GPU, s.Pool, s.Labels, config, s.Image,
		s.PricePerMinute, s.MonthlyBudget, s.WarmMin, s.Max, s.IdleSeconds, s.BootTimeoutSeconds, s.MaxJobs, s.Note, s.Enabled).Scan(&s.CreatedAt, &s.UpdatedAt))
}

func (r shapeRepo) Get(ctx context.Context, id uuid.UUID) (domain.Shape, error) {
	return one(ctx, r.db, scanShape, `SELECT `+shapeCols+` FROM shapes WHERE id = $1`, id)
}

func (r shapeRepo) GetByName(ctx context.Context, accountID uuid.UUID, name string) (domain.Shape, error) {
	return one(ctx, r.db, scanShape, `SELECT `+shapeCols+` FROM shapes WHERE `+sameOrg+` AND name = $2`, opt(accountID), name)
}

func (r shapeRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.Shape, error) {
	return many(ctx, r.db, scanShape, `SELECT `+shapeCols+` FROM shapes WHERE `+sameOrg+` ORDER BY name`, opt(accountID))
}

func (r shapeRepo) ListAvailable(ctx context.Context, accountID uuid.UUID) ([]domain.Shape, error) {
	return many(ctx, r.db, scanShape, `SELECT `+shapeCols+` FROM shapes WHERE account_id = $1 OR account_id IS NULL ORDER BY name, id`, accountID)
}

func (r shapeRepo) Update(ctx context.Context, s *domain.Shape) error {
	config, err := checkedShape(s)
	if err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `UPDATE shapes SET name = $2, os = $3, arch = $4, cpu = $5, memory_mb = $6, disk_gb = $7, gpu = $8, pool = $9, labels = $10,
			provider_config = $11, image = $12, price_per_minute = $13, monthly_budget = $14, warm_min = $15, max = $16, idle_seconds = $17,
			boot_timeout_seconds = $18, max_jobs = $19, note = $20, enabled = $21, updated_at = now()
		WHERE id = $1 RETURNING updated_at`,
		s.ID, s.Name, string(s.OS), string(s.Arch), s.CPU, s.MemoryMB, s.DiskGB, s.GPU, s.Pool, s.Labels, config, s.Image, s.PricePerMinute, s.MonthlyBudget,
		s.WarmMin, s.Max, s.IdleSeconds, s.BootTimeoutSeconds, s.MaxJobs, s.Note, s.Enabled).Scan(&s.UpdatedAt))
}

// ---- machines

type machineRepo struct{ db dbtx }

var machineCols = `id, ` + cuuid("account_id") + `, provider_id, shape_id, ` + ctext("external_id") + `, state, ` + cuuid("worker_id") + `, ` + cuuid("job_id") +
	`, jobs_taken, error, ` + ctime("assigned_at") + `, ` + ctime("booting_at") + `, ` + ctime("ready_at") + `, ` + ctime("deadline_at") + `, ` + ctime("destroyed_at") +
	`, ` + ctime("failed_at") + `, created_at, updated_at`

func scanMachine(row pgx.Row) (m domain.Machine, err error) {
	err = row.Scan(&m.ID, &m.AccountID, &m.ProviderID, &m.ShapeID, &m.ExternalID, &m.State, &m.WorkerID, &m.JobID, &m.JobsTaken, &m.Error,
		&m.AssignedAt, &m.BootingAt, &m.ReadyAt, &m.DeadlineAt, &m.DestroyedAt, &m.FailedAt, &m.CreatedAt, &m.UpdatedAt)
	utc(&m.AssignedAt, &m.BootingAt, &m.ReadyAt, &m.DeadlineAt, &m.DestroyedAt, &m.FailedAt, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

func (r machineRepo) Create(ctx context.Context, m *domain.Machine) error {
	m.ID = newID(m.ID)
	if m.State == "" {
		m.State = domain.MachineRequested
	}
	if err := m.Validate(); err != nil {
		return err
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO machines (id, account_id, provider_id, shape_id, state, assigned_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $2::uuid IS NOT NULL THEN now() END)
		RETURNING `+ctime("assigned_at")+`, created_at, updated_at`,
		m.ID, opt(m.AccountID), m.ProviderID, m.ShapeID, string(m.State)).Scan(&m.AssignedAt, &m.CreatedAt, &m.UpdatedAt))
}

func (r machineRepo) Get(ctx context.Context, id uuid.UUID) (domain.Machine, error) {
	return one(ctx, r.db, scanMachine, `SELECT `+machineCols+` FROM machines WHERE id = $1`, id)
}

func (r machineRepo) GetByWorker(ctx context.Context, accountID, workerID uuid.UUID) (domain.Machine, error) {
	return one(ctx, r.db, scanMachine, `SELECT `+machineCols+` FROM machines WHERE account_id = $1 AND worker_id = $2`, accountID, workerID)
}

func (r machineRepo) GetByExternalID(ctx context.Context, providerID uuid.UUID, externalID string) (domain.Machine, error) {
	return one(ctx, r.db, scanMachine, `SELECT `+machineCols+` FROM machines WHERE provider_id = $1 AND external_id = $2`, providerID, externalID)
}

func (r machineRepo) List(ctx context.Context, f store.MachineFilter) ([]domain.Machine, error) {
	w := &where{conds: []string{"true"}}
	andNot(w, "account_id = ?", f.AccountID)
	andNot(w, "provider_id = ?", f.ProviderID)
	andNot(w, "shape_id = ?", f.ShapeID)
	andNot(w, "state = ?", string(f.State))
	if f.Active {
		w.conds = append(w.conds, "state NOT IN ('destroyed','failed')")
	}
	if !f.Since.IsZero() {
		w.and("(destroyed_at IS NULL OR destroyed_at >= ?)", f.Since)
	}
	return many(ctx, r.db, scanMachine, `SELECT `+machineCols+` FROM machines`+w.sql("created_at DESC, id DESC", f.Limit), w.args...)
}

func (r machineRepo) UpdateState(ctx context.Context, id uuid.UUID, from, to domain.MachineState, errMsg string) error {
	if _, err := from.Transition(to); err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE machines SET state = $3, updated_at = now(),
			error = CASE WHEN $4 <> '' THEN $4 ELSE error END,
			booting_at = CASE WHEN $3 = 'booting' THEN now() ELSE booting_at END,
			ready_at = CASE WHEN $3 = 'ready' THEN COALESCE(ready_at, now()) ELSE ready_at END,
			destroyed_at = CASE WHEN $3 = 'destroyed' THEN now() ELSE destroyed_at END,
			failed_at = CASE WHEN $3 = 'failed' THEN COALESCE(failed_at, now()) ELSE failed_at END
		WHERE id = $1 AND state = $2`, id, string(from), string(to), errMsg)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	m, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("machine %s is %s, not %s: %w", id, m.State, from, store.ErrConflict)
}

func (r machineRepo) Retire(ctx context.Context, id uuid.UUID, from domain.MachineState, jobsTaken int) error {
	if _, err := from.Transition(domain.MachineDraining); err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE machines SET state = 'draining', updated_at = now()
		WHERE id = $1 AND state = $2 AND jobs_taken = $3
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.worker_id = machines.worker_id AND j.status = 'running')`, id, string(from), jobsTaken)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("machine %s is in use: %w", id, store.ErrConflict)
}

func (r machineRepo) SetExternalID(ctx context.Context, id uuid.UUID, externalID string) error {
	return mustUpdate(ctx, r.db, `UPDATE machines SET external_id = $2, updated_at = now() WHERE id = $1`, id, externalID)
}

func (r machineRepo) Assign(ctx context.Context, id, accountID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE machines SET account_id = $2, assigned_at = now(), updated_at = now() WHERE id = $1 AND account_id IS NULL`, id, accountID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("machine %s already has an organization: %w", id, store.ErrConflict)
}

func (r machineRepo) SetWorker(ctx context.Context, id, workerID uuid.UUID) error {
	return mustUpdate(ctx, r.db, `UPDATE machines SET worker_id = $2, updated_at = now() WHERE id = $1`, id, workerID)
}

func (r machineRepo) MarkUp(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE machines SET ready_at = now(), updated_at = now() WHERE id = $1 AND state = 'booting' AND ready_at IS NULL`, id)
	return mapErr(err)
}

func (r machineRepo) TakeJob(ctx context.Context, id, jobID uuid.UUID, deadline time.Time) (domain.Machine, error) {
	m, err := one(ctx, r.db, scanMachine, `UPDATE machines SET job_id = $2, deadline_at = $3, jobs_taken = jobs_taken + 1, updated_at = now()
		WHERE id = $1 AND jobs_taken < (SELECT max_jobs FROM shapes WHERE id = machines.shape_id)
		RETURNING `+machineCols, id, jobID, optTime(deadline))
	if !errors.Is(err, store.ErrNotFound) {
		return m, err
	}
	if _, err := r.Get(ctx, id); err != nil {
		return m, err
	}
	return m, fmt.Errorf("machine %s took its last job: %w", id, store.ErrConflict)
}

// ---- bootstrap tokens

type bootstrapTokenRepo struct{ db dbtx }

var bootstrapCols = `machine_id, ` + cuuid("account_id") + `, token_hash, ` + ctime("expires_at") + `, ` + ctime("used_at") + `, created_at`

func scanBootstrapToken(row pgx.Row) (t domain.BootstrapToken, err error) {
	err = row.Scan(&t.MachineID, &t.AccountID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	utc(&t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	return t, err
}

func (r bootstrapTokenRepo) Issue(ctx context.Context, t *domain.BootstrapToken) error {
	if err := t.Validate(); err != nil {
		return err
	}
	err := r.db.QueryRow(ctx, `INSERT INTO bootstrap_tokens AS b (machine_id, account_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (machine_id) DO UPDATE SET account_id = $2, token_hash = $3, expires_at = $4, created_at = now()
		WHERE b.used_at IS NULL RETURNING created_at`,
		t.MachineID, opt(t.AccountID), t.TokenHash, optTime(t.ExpiresAt)).Scan(&t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("bootstrap token of machine %s was used: %w", t.MachineID, store.ErrConflict)
	}
	return mapErr(err)
}

func (r bootstrapTokenRepo) GetByTokenHash(ctx context.Context, hash []byte) (domain.BootstrapToken, error) {
	return one(ctx, r.db, scanBootstrapToken, `SELECT `+bootstrapCols+` FROM bootstrap_tokens WHERE token_hash = $1`, hash)
}

func (r bootstrapTokenRepo) Assign(ctx context.Context, machineID, accountID uuid.UUID, expiresAt time.Time) error {
	return mustUpdate(ctx, r.db, `UPDATE bootstrap_tokens SET account_id = $2, expires_at = $3 WHERE machine_id = $1 AND account_id IS NULL`, machineID, accountID, expiresAt.UTC())
}

func (r bootstrapTokenRepo) Consume(ctx context.Context, hash []byte) (domain.BootstrapToken, error) {
	t, err := one(ctx, r.db, scanBootstrapToken, `UPDATE bootstrap_tokens SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND account_id IS NOT NULL AND expires_at > now()
		  AND EXISTS (SELECT 1 FROM machines m WHERE m.id = machine_id AND m.state = 'booting')
		RETURNING `+bootstrapCols, hash)
	if !errors.Is(err, store.ErrNotFound) {
		return t, err
	}
	if _, err := r.GetByTokenHash(ctx, hash); err != nil {
		return t, err
	}
	return t, fmt.Errorf("bootstrap token cannot be used: %w", store.ErrConflict)
}

// ---- fleet policies

type fleetPolicyRepo struct{ db dbtx }

const policyCols = `account_id, routing, allowed_shapes, quotas, egress, plan, created_at, updated_at`

func scanPolicy(row pgx.Row) (p domain.FleetPolicy, err error) {
	var quotas []byte
	err = row.Scan(&p.AccountID, &p.Routing, &p.AllowedShapes, &quotas, &p.Egress, &p.Plan, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	utc(&p.CreatedAt, &p.UpdatedAt)
	if err := json.Unmarshal(quotas, &p.Quotas); err != nil {
		return p, fmt.Errorf("decode quotas: %w", err)
	}
	return p, nil
}

func (r fleetPolicyRepo) Get(ctx context.Context, accountID uuid.UUID) (domain.FleetPolicy, error) {
	p, err := one(ctx, r.db, scanPolicy, `SELECT `+policyCols+` FROM fleet_policy WHERE account_id = $1`, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.DefaultFleetPolicy(accountID), nil
	}
	return p, err
}

func (r fleetPolicyRepo) Put(ctx context.Context, p *domain.FleetPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	quotas, err := json.Marshal(p.Quotas)
	if err != nil {
		return fmt.Errorf("encode quotas: %w", err)
	}
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO fleet_policy (account_id, routing, allowed_shapes, quotas, egress, plan) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id) DO UPDATE SET routing = $2, allowed_shapes = $3, quotas = $4, egress = $5, plan = $6, updated_at = now()
		RETURNING created_at, updated_at`,
		p.AccountID, string(p.Routing), strs(p.AllowedShapes), quotas, strs(p.Egress), string(p.Plan)).Scan(&p.CreatedAt, &p.UpdatedAt))
}

// ---- join codes

type joinCodeRepo struct{ db dbtx }

var joinCodeCols = `id, ` + cuuid("account_id") + `, code_hash, claim_hash, COALESCE(kind, ''), facts, ` + cuuid("provider_id") + `, expires_at, ` + ctime("used_at") + `, created_at`

func scanJoinCode(row pgx.Row) (c domain.ProviderJoinCode, err error) {
	var facts []byte
	err = row.Scan(&c.ID, &c.AccountID, &c.CodeHash, &c.ClaimHash, &c.Kind, &facts, &c.ProviderID, &c.ExpiresAt, &c.UsedAt, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	utc(&c.ExpiresAt, &c.UsedAt, &c.CreatedAt)
	if err := json.Unmarshal(facts, &c.Facts); err != nil {
		return c, fmt.Errorf("decode join code facts: %w", err)
	}
	return c, nil
}

func (r joinCodeRepo) Create(ctx context.Context, c *domain.ProviderJoinCode) error {
	c.ID = newID(c.ID)
	return mapErr(r.db.QueryRow(ctx, `INSERT INTO provider_join_codes (id, account_id, code_hash, expires_at) VALUES ($1, $2, $3, $4) RETURNING created_at`,
		c.ID, opt(c.AccountID), c.CodeHash, c.ExpiresAt.UTC()).Scan(&c.CreatedAt))
}

func (r joinCodeRepo) Get(ctx context.Context, id uuid.UUID) (domain.ProviderJoinCode, error) {
	return one(ctx, r.db, scanJoinCode, `SELECT `+joinCodeCols+` FROM provider_join_codes WHERE id = $1`, id)
}

func (r joinCodeRepo) List(ctx context.Context, accountID uuid.UUID) ([]domain.ProviderJoinCode, error) {
	return many(ctx, r.db, scanJoinCode, `SELECT `+joinCodeCols+` FROM provider_join_codes WHERE `+sameOrg+` ORDER BY created_at DESC, id DESC`, opt(accountID))
}

func (r joinCodeRepo) GetByCodeHash(ctx context.Context, hash []byte) (domain.ProviderJoinCode, error) {
	return one(ctx, r.db, scanJoinCode, `SELECT `+joinCodeCols+` FROM provider_join_codes WHERE code_hash = $1`, hash)
}

// changed answers ErrConflict when an update of code id found no row it may
// change, and ErrNotFound when there is no such code.
func (r joinCodeRepo) changed(ctx context.Context, id uuid.UUID, q string, args ...any) error {
	tag, err := r.db.Exec(ctx, q, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("join code %s: %w", id, store.ErrConflict)
}

func (r joinCodeRepo) Claim(ctx context.Context, id uuid.UUID, claimHash []byte, kind domain.ProviderKind, facts domain.ProviderCapabilities) error {
	raw, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("encode join code facts: %w", err)
	}
	return r.changed(ctx, id, `UPDATE provider_join_codes SET claim_hash = $2, kind = $3, facts = $4
		WHERE id = $1 AND used_at IS NULL AND provider_id IS NULL AND expires_at > now() AND (claim_hash IS NULL OR claim_hash = $2)`,
		id, claimHash, string(kind), raw)
}

func (r joinCodeRepo) Confirm(ctx context.Context, id, providerID uuid.UUID) error {
	return r.changed(ctx, id, `UPDATE provider_join_codes SET provider_id = $2
		WHERE id = $1 AND used_at IS NULL AND provider_id IS NULL AND claim_hash IS NOT NULL AND expires_at > now()`, id, providerID)
}

func (r joinCodeRepo) MarkUsed(ctx context.Context, id uuid.UUID) error {
	return r.changed(ctx, id, `UPDATE provider_join_codes SET used_at = now() WHERE id = $1 AND used_at IS NULL AND provider_id IS NOT NULL`, id)
}

// ---- image builds

type imageBuildRepo struct{ db dbtx }

var imageBuildCols = `id, ` + cuuid("account_id") + `, provider_id, state, version, iso, iso_checksum, chocolatey_sha256, windows_image_name,
	license_confirmed_by, step, percent, error, manifest, digest, created_at, updated_at, ` + ctime("started_at") + `, ` + ctime("finished_at")

func scanImageBuild(row pgx.Row) (b domain.ImageBuild, err error) {
	err = row.Scan(&b.ID, &b.AccountID, &b.ProviderID, &b.State, &b.Version, &b.ISO, &b.ISOChecksum, &b.ChocolateySHA256, &b.WindowsImageName,
		&b.LicenseConfirmedBy, &b.Step, &b.Percent, &b.Error, &b.Manifest, &b.Digest, &b.CreatedAt, &b.UpdatedAt, &b.StartedAt, &b.FinishedAt)
	utc(&b.CreatedAt, &b.UpdatedAt, &b.StartedAt, &b.FinishedAt)
	return b, err
}

func (r imageBuildRepo) Create(ctx context.Context, b *domain.ImageBuild) error {
	b.ID = newID(b.ID)
	b.State = domain.ImageBuildRequested
	if err := b.Validate(); err != nil {
		return err
	}
	err := r.db.QueryRow(ctx, `INSERT INTO image_builds (id, account_id, provider_id, state, version, iso, iso_checksum, chocolatey_sha256, windows_image_name, license_confirmed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at, updated_at`,
		b.ID, opt(b.AccountID), b.ProviderID, b.State, b.Version, b.ISO, b.ISOChecksum, b.ChocolateySHA256, b.WindowsImageName, b.LicenseConfirmedBy).Scan(&b.CreatedAt, &b.UpdatedAt)
	utc(&b.CreatedAt, &b.UpdatedAt)
	return mapErr(err)
}

func (r imageBuildRepo) Get(ctx context.Context, id uuid.UUID) (domain.ImageBuild, error) {
	return one(ctx, r.db, scanImageBuild, `SELECT `+imageBuildCols+` FROM image_builds WHERE id = $1`, id)
}

func (r imageBuildRepo) List(ctx context.Context, providerID uuid.UUID, limit int) ([]domain.ImageBuild, error) {
	if limit <= 0 {
		limit = 20
	}
	return many(ctx, r.db, scanImageBuild, `SELECT `+imageBuildCols+` FROM image_builds WHERE provider_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, providerID, limit)
}

func (r imageBuildRepo) Open(ctx context.Context, providerID uuid.UUID) ([]domain.ImageBuild, error) {
	return many(ctx, r.db, scanImageBuild, `SELECT `+imageBuildCols+` FROM image_builds WHERE provider_id = $1 AND state IN ('requested','running') ORDER BY created_at, id`, providerID)
}

func (r imageBuildRepo) Save(ctx context.Context, b *domain.ImageBuild) error {
	if err := b.Validate(); err != nil {
		return err
	}
	tag, err := r.db.Exec(ctx, `UPDATE image_builds SET state = $2, step = $3, percent = $4, error = $5, manifest = $6, digest = $7,
		started_at = $8, finished_at = $9, updated_at = now()
		WHERE id = $1 AND state IN ('requested','running')`,
		b.ID, b.State, b.Step, b.Percent, b.Error, b.Manifest, b.Digest, optTime(b.StartedAt), optTime(b.FinishedAt))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := r.Get(ctx, b.ID); err != nil {
		return err
	}
	return fmt.Errorf("image build %s has ended: %w", b.ID, store.ErrConflict)
}

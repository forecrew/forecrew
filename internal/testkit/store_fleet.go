package testkit

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func providersT(d *storeData) *table[domain.CapacityProvider] { return &d.providers }
func shapesT(d *storeData) *table[domain.Shape]               { return &d.shapes }
func machinesT(d *storeData) *table[domain.Machine]           { return &d.machines }
func bootstrapT(d *storeData) *table[domain.BootstrapToken]   { return &d.bootstrap }
func joinCodesT(d *storeData) *table[domain.ProviderJoinCode] { return &d.joinCodes }

// CapacityProviders implements store.Store.
func (s *Store) CapacityProviders() store.CapacityProviderRepo { return providerRepo{s} }

// Shapes implements store.Store.
func (s *Store) Shapes() store.ShapeRepo { return shapeRepo{s} }

// Machines implements store.Store.
func (s *Store) Machines() store.MachineRepo { return machineRepo{s} }

// BootstrapTokens implements store.Store.
func (s *Store) BootstrapTokens() store.BootstrapTokenRepo { return bootstrapRepo{s} }

// FleetPolicies implements store.Store.
func (s *Store) FleetPolicies() store.FleetPolicyRepo { return policyRepo{s} }

// ProviderJoinCodes implements store.Store.
func (s *Store) ProviderJoinCodes() store.ProviderJoinCodeRepo { return joinCodeRepo{s} }

// byID edits the row with id, whatever its organization; ErrNotFound when
// there is none.
func byID[T any](s *Store, t func(*storeData) *table[T], match func(T) bool, fn func(r *T, now time.Time) error) error {
	return s.do(func(d *storeData, now time.Time) error {
		r, err := t(d).find(match)
		if err != nil {
			return err
		}
		return fn(r, now)
	})
}

type providerRepo struct{ s *Store }

func providerID(id uuid.UUID) func(domain.CapacityProvider) bool {
	return func(p domain.CapacityProvider) bool { return p.ID == id }
}

func (r providerRepo) Create(_ context.Context, p *domain.CapacityProvider) error {
	if p.Mode == "" {
		p.Mode = domain.ProviderModeAgent
	}
	if p.Status == "" {
		p.Status = domain.ProviderPending
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.providers.exists(func(x domain.CapacityProvider) bool { return x.AccountID == p.AccountID && x.Name == p.Name }) {
			return fmt.Errorf("provider %s: %w", p.Name, store.ErrConflict)
		}
		p.ID = newID(p.ID)
		p.CreatedAt, p.UpdatedAt = now, now
		d.providers.insert(*p)
		return nil
	})
}

func (r providerRepo) Get(_ context.Context, id uuid.UUID) (domain.CapacityProvider, error) {
	return findRow(r.s, providersT, providerID(id))
}

func (r providerRepo) GetByName(_ context.Context, accountID uuid.UUID, name string) (domain.CapacityProvider, error) {
	return findRow(r.s, providersT, func(p domain.CapacityProvider) bool { return p.AccountID == accountID && p.Name == name })
}

func (r providerRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.CapacityProvider, error) {
	return findRow(r.s, providersT, func(p domain.CapacityProvider) bool { return bytes.Equal(p.TokenHash, hash) })
}

func (r providerRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.CapacityProvider, error) {
	out, err := listRows(r.s, providersT, 0, func(p domain.CapacityProvider) bool { return p.AccountID == accountID })
	sortByName(out, func(p domain.CapacityProvider) string { return p.Name })
	return out, err
}

func (r providerRepo) ListAll(_ context.Context) ([]domain.CapacityProvider, error) {
	out, err := listRows(r.s, providersT, 0, func(domain.CapacityProvider) bool { return true })
	sortByName(out, func(p domain.CapacityProvider) string { return p.Name })
	return out, err
}

func sortByName[T any](rows []T, name func(T) string) {
	slices.SortStableFunc(rows, func(a, b T) int {
		switch x, y := name(a), name(b); {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
}

func (r providerRepo) Update(_ context.Context, p *domain.CapacityProvider) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return byID(r.s, providersT, providerID(p.ID), func(cur *domain.CapacityProvider, now time.Time) error {
		cur.Limits, cur.NetworkAllow, cur.Tags, cur.UpdatedAt = p.Limits, p.NetworkAllow, p.Tags, now
		p.UpdatedAt = now
		return nil
	})
}

func (r providerRepo) SetStatus(_ context.Context, id uuid.UUID, status domain.ProviderStatus) error {
	return byID(r.s, providersT, providerID(id), func(p *domain.CapacityProvider, now time.Time) error {
		if p.Status == domain.ProviderRevoked || (status == domain.ProviderActive && p.Mode == domain.ProviderModeAgent && len(p.TokenHash) == 0) {
			return fmt.Errorf("provider %s cannot become %s: %w", p.Name, status, store.ErrConflict)
		}
		p.Status, p.UpdatedAt = status, now
		return nil
	})
}

func (r providerRepo) SetToken(_ context.Context, id uuid.UUID, hash []byte) error {
	return byID(r.s, providersT, providerID(id), func(p *domain.CapacityProvider, now time.Time) error {
		if p.Status == domain.ProviderRevoked {
			return store.ErrNotFound
		}
		p.TokenHash, p.Status, p.UpdatedAt = hash, domain.ProviderActive, now
		return nil
	})
}

func (r providerRepo) Seen(_ context.Context, id uuid.UUID, capabilities *domain.ProviderCapabilities) error {
	return byID(r.s, providersT, providerID(id), func(p *domain.CapacityProvider, now time.Time) error {
		p.LastSeenAt = now
		if capabilities != nil {
			p.Capabilities = *capabilities
		}
		return nil
	})
}

type shapeRepo struct{ s *Store }

func shapeID(id uuid.UUID) func(domain.Shape) bool {
	return func(s domain.Shape) bool { return s.ID == id }
}

func (r shapeRepo) Create(_ context.Context, s *domain.Shape) error {
	*s = s.WithDefaults()
	s.Labels = domain.NormalizeLabels(s.Labels)
	if err := s.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		if !d.providers.exists(func(p domain.CapacityProvider) bool { return p.ID == s.ProviderID && p.AccountID == s.AccountID }) {
			return fmt.Errorf("provider %s: %w", s.ProviderID, store.ErrNotFound)
		}
		if d.shapes.exists(func(x domain.Shape) bool { return x.AccountID == s.AccountID && x.Name == s.Name }) {
			return fmt.Errorf("shape %s: %w", s.Name, store.ErrConflict)
		}
		s.ID = newID(s.ID)
		s.CreatedAt, s.UpdatedAt = now, now
		d.shapes.insert(*s)
		return nil
	})
}

func (r shapeRepo) Get(_ context.Context, id uuid.UUID) (domain.Shape, error) {
	return findRow(r.s, shapesT, shapeID(id))
}

func (r shapeRepo) GetByName(_ context.Context, accountID uuid.UUID, name string) (domain.Shape, error) {
	return findRow(r.s, shapesT, func(s domain.Shape) bool { return s.AccountID == accountID && s.Name == name })
}

func (r shapeRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.Shape, error) {
	out, err := listRows(r.s, shapesT, 0, func(s domain.Shape) bool { return s.AccountID == accountID })
	sortByName(out, func(s domain.Shape) string { return s.Name })
	return out, err
}

func (r shapeRepo) ListAvailable(_ context.Context, accountID uuid.UUID) ([]domain.Shape, error) {
	out, err := listRows(r.s, shapesT, 0, func(s domain.Shape) bool { return s.AccountID == accountID || s.AccountID == uuid.Nil() })
	sortByName(out, func(s domain.Shape) string { return s.Name })
	return out, err
}

func (r shapeRepo) Update(_ context.Context, s *domain.Shape) error {
	*s = s.WithDefaults()
	s.Labels = domain.NormalizeLabels(s.Labels)
	if err := s.Validate(); err != nil {
		return err
	}
	return byID(r.s, shapesT, shapeID(s.ID), func(cur *domain.Shape, now time.Time) error {
		s.AccountID, s.ProviderID, s.CreatedAt, s.UpdatedAt = cur.AccountID, cur.ProviderID, cur.CreatedAt, now
		*cur = *s
		return nil
	})
}

type machineRepo struct{ s *Store }

func machineID(id uuid.UUID) func(domain.Machine) bool {
	return func(m domain.Machine) bool { return m.ID == id }
}

func (r machineRepo) Create(_ context.Context, m *domain.Machine) error {
	if m.State == "" {
		m.State = domain.MachineRequested
	}
	if err := m.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		s, err := d.shapes.find(shapeID(m.ShapeID))
		if err != nil {
			return fmt.Errorf("shape %s: %w", m.ShapeID, err)
		}
		if s.ProviderID != m.ProviderID || (s.AccountID != uuid.Nil() && s.AccountID != m.AccountID) {
			return fmt.Errorf("machine does not fit shape %s: %w", s.Name, store.ErrConflict)
		}
		m.ID = newID(m.ID)
		m.CreatedAt, m.UpdatedAt = now, now
		if m.AccountID != uuid.Nil() {
			m.AssignedAt = now
		}
		d.machines.insert(*m)
		return nil
	})
}

func (r machineRepo) Get(_ context.Context, id uuid.UUID) (domain.Machine, error) {
	return findRow(r.s, machinesT, machineID(id))
}

func (r machineRepo) GetByWorker(_ context.Context, accountID, workerID uuid.UUID) (domain.Machine, error) {
	return findRow(r.s, machinesT, func(m domain.Machine) bool { return m.AccountID == accountID && m.WorkerID == workerID })
}

func (r machineRepo) GetByExternalID(_ context.Context, providerID uuid.UUID, externalID string) (domain.Machine, error) {
	return findRow(r.s, machinesT, func(m domain.Machine) bool { return m.ProviderID == providerID && m.ExternalID == externalID })
}

func (r machineRepo) List(_ context.Context, f store.MachineFilter) ([]domain.Machine, error) {
	return read(r.s, func(d *storeData) ([]domain.Machine, error) {
		return d.machines.newest(f.Limit, func(m domain.Machine) bool {
			return zeroOr(f.AccountID, m.AccountID) && zeroOr(f.ProviderID, m.ProviderID) && zeroOr(f.ShapeID, m.ShapeID) &&
				zeroOr(f.State, m.State) && (!f.Active || m.State.Counts()) && (f.Since.IsZero() || m.DestroyedAt.IsZero() || !m.DestroyedAt.Before(f.Since))
		}), nil
	})
}

func (r machineRepo) UpdateState(_ context.Context, id uuid.UUID, from, to domain.MachineState, errMsg string) error {
	if _, err := from.Transition(to); err != nil {
		return err
	}
	return byID(r.s, machinesT, machineID(id), func(m *domain.Machine, now time.Time) error {
		if m.State != from {
			return fmt.Errorf("machine %s is %s, not %s: %w", id, m.State, from, store.ErrConflict)
		}
		m.State, m.UpdatedAt = to, now
		if errMsg != "" {
			m.Error = errMsg
		}
		switch to {
		case domain.MachineBooting:
			m.BootingAt = now
		case domain.MachineReady:
			if m.ReadyAt.IsZero() {
				m.ReadyAt = now
			}
		case domain.MachineDestroyed:
			m.DestroyedAt = now
		case domain.MachineFailed:
			if m.FailedAt.IsZero() {
				m.FailedAt = now
			}
		}
		return nil
	})
}

func (r machineRepo) Retire(_ context.Context, id uuid.UUID, from domain.MachineState, jobsTaken int) error {
	if _, err := from.Transition(domain.MachineDraining); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		m, err := d.machines.find(machineID(id))
		if err != nil {
			return err
		}
		running := m.WorkerID != uuid.Nil() && d.jobs.exists(func(j domain.Job) bool { return j.Status == domain.JobRunning && j.WorkerID == m.WorkerID })
		if m.State != from || m.JobsTaken != jobsTaken || running {
			return fmt.Errorf("machine %s is in use: %w", id, store.ErrConflict)
		}
		m.State, m.UpdatedAt = domain.MachineDraining, now
		return nil
	})
}

func (r machineRepo) SetExternalID(_ context.Context, id uuid.UUID, externalID string) error {
	return byID(r.s, machinesT, machineID(id), func(m *domain.Machine, now time.Time) error {
		m.ExternalID, m.UpdatedAt = externalID, now
		return nil
	})
}

func (r machineRepo) Assign(_ context.Context, id, accountID uuid.UUID) error {
	return byID(r.s, machinesT, machineID(id), func(m *domain.Machine, now time.Time) error {
		if m.AccountID != uuid.Nil() {
			return fmt.Errorf("machine %s already has an organization: %w", id, store.ErrConflict)
		}
		m.AccountID, m.AssignedAt, m.UpdatedAt = accountID, now, now
		return nil
	})
}

func (r machineRepo) SetWorker(_ context.Context, id, workerID uuid.UUID) error {
	return byID(r.s, machinesT, machineID(id), func(m *domain.Machine, now time.Time) error {
		m.WorkerID, m.UpdatedAt = workerID, now
		return nil
	})
}

func (r machineRepo) MarkUp(_ context.Context, id uuid.UUID) error {
	return byID(r.s, machinesT, machineID(id), func(m *domain.Machine, now time.Time) error {
		if m.State == domain.MachineBooting && m.ReadyAt.IsZero() {
			m.ReadyAt, m.UpdatedAt = now, now
		}
		return nil
	})
}

func (r machineRepo) TakeJob(_ context.Context, id, jobID uuid.UUID, deadline time.Time) (domain.Machine, error) {
	var out domain.Machine
	err := r.s.do(func(d *storeData, now time.Time) error {
		m, err := d.machines.find(machineID(id))
		if err != nil {
			return err
		}
		s, err := d.shapes.find(shapeID(m.ShapeID))
		if err != nil {
			return err
		}
		if m.JobsTaken >= s.MaxJobs {
			return fmt.Errorf("machine %s took its last job: %w", id, store.ErrConflict)
		}
		m.JobID, m.DeadlineAt, m.UpdatedAt = jobID, deadline, now
		m.JobsTaken++
		out = *m
		return nil
	})
	return out, err
}

// machineFits returns what an ephemeral worker may lease: jobs its machine's
// shape fits, while the machine is ready or busy and below max_jobs. Without
// such a machine it may lease nothing.
func (d *storeData) machineFits(w domain.Worker) func(domain.Job) bool {
	m, err := d.machines.find(func(m domain.Machine) bool { return m.AccountID == w.AccountID && m.WorkerID == w.ID })
	if err != nil || (m.State != domain.MachineReady && m.State != domain.MachineBusy) {
		return func(domain.Job) bool { return false }
	}
	s, err := d.shapes.find(shapeID(m.ShapeID))
	if err != nil || m.JobsTaken >= s.MaxJobs {
		return func(domain.Job) bool { return false }
	}
	shape := *s
	return func(j domain.Job) bool { return shape.Fits(j.Resources) }
}

type bootstrapRepo struct{ s *Store }

func tokenOf(machine uuid.UUID) func(domain.BootstrapToken) bool {
	return func(t domain.BootstrapToken) bool { return t.MachineID == machine }
}

func (r bootstrapRepo) Issue(_ context.Context, t *domain.BootstrapToken) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		m, err := d.machines.find(machineID(t.MachineID))
		if err != nil {
			return err
		}
		if m.AccountID != t.AccountID {
			return fmt.Errorf("a bootstrap token has its machine's organization: %w", store.ErrConflict)
		}
		t.CreatedAt = now
		cur, err := d.bootstrap.find(tokenOf(t.MachineID))
		if err != nil {
			d.bootstrap.insert(*t)
			return nil
		}
		if !cur.UsedAt.IsZero() {
			return fmt.Errorf("bootstrap token of machine %s was used: %w", t.MachineID, store.ErrConflict)
		}
		*cur = *t
		return nil
	})
}

func (r bootstrapRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.BootstrapToken, error) {
	return findRow(r.s, bootstrapT, func(t domain.BootstrapToken) bool { return bytes.Equal(t.TokenHash, hash) })
}

func (r bootstrapRepo) Assign(_ context.Context, machine, accountID uuid.UUID, expiresAt time.Time) error {
	return byID(r.s, bootstrapT, func(t domain.BootstrapToken) bool { return t.MachineID == machine && !t.Assigned() }, func(t *domain.BootstrapToken, _ time.Time) error {
		t.AccountID, t.ExpiresAt = accountID, expiresAt
		return nil
	})
}

func (r bootstrapRepo) Consume(_ context.Context, hash []byte) (domain.BootstrapToken, error) {
	var out domain.BootstrapToken
	err := r.s.do(func(d *storeData, now time.Time) error {
		t, err := d.bootstrap.find(func(t domain.BootstrapToken) bool { return bytes.Equal(t.TokenHash, hash) })
		if err != nil {
			return err
		}
		m, err := d.machines.find(machineID(t.MachineID))
		if err != nil {
			return err
		}
		if !t.Usable(now) || m.State != domain.MachineBooting {
			return fmt.Errorf("bootstrap token cannot be used: %w", store.ErrConflict)
		}
		t.UsedAt = now
		out = *t
		return nil
	})
	return out, err
}

type policyRepo struct{ s *Store }

func (r policyRepo) Get(_ context.Context, accountID uuid.UUID) (domain.FleetPolicy, error) {
	return read(r.s, func(d *storeData) (domain.FleetPolicy, error) {
		p, err := d.policies.get(accountID, accountID)
		if err != nil {
			return domain.DefaultFleetPolicy(accountID), nil //nolint:nilerr // no row means the default
		}
		return *p, nil
	})
}

func (r policyRepo) Put(_ context.Context, p *domain.FleetPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return r.s.do(func(d *storeData, now time.Time) error {
		p.UpdatedAt = now
		if cur, err := d.policies.get(p.AccountID, p.AccountID); err == nil {
			p.CreatedAt = cur.CreatedAt
			*cur = *p
			return nil
		}
		p.CreatedAt = now
		d.policies.insert(*p)
		return nil
	})
}

type joinCodeRepo struct{ s *Store }

func joinCodeID(id uuid.UUID) func(domain.ProviderJoinCode) bool {
	return func(c domain.ProviderJoinCode) bool { return c.ID == id }
}

func (r joinCodeRepo) Create(_ context.Context, c *domain.ProviderJoinCode) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.joinCodes.exists(func(x domain.ProviderJoinCode) bool { return bytes.Equal(x.CodeHash, c.CodeHash) }) {
			return fmt.Errorf("join code exists: %w", store.ErrConflict)
		}
		if c.ID == uuid.Nil() {
			c.ID = uuid.New()
		}
		c.CreatedAt = now
		d.joinCodes.insert(*c)
		return nil
	})
}

func (r joinCodeRepo) Get(_ context.Context, id uuid.UUID) (domain.ProviderJoinCode, error) {
	return findRow(r.s, joinCodesT, joinCodeID(id))
}

func (r joinCodeRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.ProviderJoinCode, error) {
	rows, err := listRows(r.s, joinCodesT, 0, func(c domain.ProviderJoinCode) bool { return c.AccountID == accountID })
	slices.Reverse(rows) // inserted oldest first
	return rows, err
}

func (r joinCodeRepo) GetByCodeHash(_ context.Context, hash []byte) (domain.ProviderJoinCode, error) {
	return findRow(r.s, joinCodesT, func(c domain.ProviderJoinCode) bool { return bytes.Equal(c.CodeHash, hash) })
}

func (r joinCodeRepo) Claim(_ context.Context, id uuid.UUID, claimHash []byte, kind domain.ProviderKind, facts domain.ProviderCapabilities) error {
	return byID(r.s, joinCodesT, joinCodeID(id), func(c *domain.ProviderJoinCode, now time.Time) error {
		if !c.Usable(now) || c.ProviderID != uuid.Nil() || (len(c.ClaimHash) != 0 && !bytes.Equal(c.ClaimHash, claimHash)) {
			return fmt.Errorf("join code %s: %w", id, store.ErrConflict)
		}
		c.ClaimHash, c.Kind, c.Facts = claimHash, kind, facts
		return nil
	})
}

func (r joinCodeRepo) Confirm(_ context.Context, id, providerID uuid.UUID) error {
	return byID(r.s, joinCodesT, joinCodeID(id), func(c *domain.ProviderJoinCode, now time.Time) error {
		if !c.Usable(now) || c.ProviderID != uuid.Nil() || len(c.ClaimHash) == 0 {
			return fmt.Errorf("join code %s: %w", id, store.ErrConflict)
		}
		c.ProviderID = providerID
		return nil
	})
}

func (r joinCodeRepo) MarkUsed(_ context.Context, id uuid.UUID) error {
	return byID(r.s, joinCodesT, joinCodeID(id), func(c *domain.ProviderJoinCode, now time.Time) error {
		if !c.UsedAt.IsZero() || c.ProviderID == uuid.Nil() {
			return fmt.Errorf("join code %s: %w", id, store.ErrConflict)
		}
		c.UsedAt = now
		return nil
	})
}

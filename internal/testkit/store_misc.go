package testkit

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func gatesT(d *storeData) *table[domain.Gate]       { return &d.gates }
func signalsT(d *storeData) *table[domain.Signal]   { return &d.signals }
func runsT(d *storeData) *table[domain.WorkflowRun] { return &d.runs }
func stepsT(d *storeData) *table[domain.StepRun]    { return &d.steps }
func tokensT(d *storeData) *table[domain.MCPToken]  { return &d.tokens }

// Gates implements store.Store.
func (s *Store) Gates() store.GateRepo { return gateRepo{s} }

type gateRepo struct{ s *Store }

func (r gateRepo) Create(_ context.Context, g *domain.Gate) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, g.AccountID, g.ProjectID), foreign(&d.goals, g.AccountID, g.GoalID), foreign(&d.specs, g.AccountID, g.SpecID), foreign(&d.tasks, g.AccountID, g.TaskID), foreign(&d.jobs, g.AccountID, g.JobID)); err != nil {
			return err
		}
		g.ID = newID(g.ID)
		g.Number = d.nextNumber(g.AccountID, domain.RefGate)
		g.Status = domain.GatePending
		g.CreatedAt = now
		d.gates.insert(*g)
		return nil
	})
}

func (r gateRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.Gate, error) {
	return getRow(r.s, gatesT, accountID, id)
}

func (r gateRepo) GetByNumber(_ context.Context, accountID uuid.UUID, number int64) (domain.Gate, error) {
	return findRow(r.s, gatesT, func(g domain.Gate) bool { return g.AccountID == accountID && g.Number == number })
}

func (r gateRepo) List(_ context.Context, accountID uuid.UUID, f store.GateFilter) ([]domain.Gate, error) {
	return listRows(r.s, gatesT, f.Limit, func(g domain.Gate) bool {
		return g.AccountID == accountID && zeroOr(f.ProjectID, g.ProjectID) && zeroOr(f.Kind, g.Kind) && zeroOr(f.Status, g.Status)
	})
}

func (r gateRepo) ListPendingBySubject(_ context.Context, accountID uuid.UUID, repo string, number int) ([]domain.Gate, error) {
	return listRows(r.s, gatesT, 0, func(g domain.Gate) bool {
		return g.AccountID == accountID && g.Status == domain.GatePending && strings.EqualFold(g.SubjectRepo, repo) && g.SubjectNumber == number
	})
}

func (r gateRepo) Resolve(_ context.Context, g *domain.Gate) error {
	return update(r.s, gatesT, g.AccountID, g.ID, func(cur *domain.Gate, _ time.Time) error {
		if err := casStatus(&cur.Status, domain.GatePending, g.Status); err != nil {
			return err
		}
		cur.ResolvedBy, cur.Feedback, cur.ResolvedAt = g.ResolvedBy, g.Feedback, g.ResolvedAt
		return nil
	})
}

// Signals implements store.Store.
func (s *Store) Signals() store.SignalRepo { return signalRepo{s} }

type signalRepo struct{ s *Store }

func (r signalRepo) Insert(_ context.Context, sig *domain.Signal) (bool, error) {
	inserted := false
	err := r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.projects, sig.AccountID, sig.ProjectID); err != nil {
			return err
		}
		if d.signals.exists(func(x domain.Signal) bool { return x.AccountID == sig.AccountID && x.DedupeKey == sig.DedupeKey }) {
			return nil
		}
		sig.ID = newID(sig.ID)
		if sig.CreatedAt.IsZero() {
			sig.CreatedAt = now
		}
		d.signals.insert(*sig)
		inserted = true
		return nil
	})
	return inserted, err
}

func (r signalRepo) ListByKey(_ context.Context, accountID uuid.UUID, key string) ([]domain.Signal, error) {
	return listRows(r.s, signalsT, 0, func(sig domain.Signal) bool { return sig.AccountID == accountID && sig.Key == key })
}

// WorkflowRuns implements store.Store.
func (s *Store) WorkflowRuns() store.WorkflowRunRepo { return runRepo{s} }

type runRepo struct{ s *Store }

func (r runRepo) Create(_ context.Context, run *domain.WorkflowRun) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.projects, run.AccountID, run.ProjectID), foreign(&d.goals, run.AccountID, run.GoalID), foreign(&d.tasks, run.AccountID, run.TaskID)); err != nil {
			return err
		}
		run.ID = newID(run.ID)
		run.CreatedAt, run.UpdatedAt = now, now
		d.runs.insert(*run)
		return nil
	})
}

func (r runRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.WorkflowRun, error) {
	return getRow(r.s, runsT, accountID, id)
}

func (r runRepo) ListActive(_ context.Context, accountID uuid.UUID) ([]domain.WorkflowRun, error) {
	return listRows(r.s, runsT, 0, func(run domain.WorkflowRun) bool {
		return run.AccountID == accountID && (run.Status == domain.RunRunning || run.Status == domain.RunWaiting)
	})
}

func (r runRepo) Update(_ context.Context, run *domain.WorkflowRun) error {
	return update(r.s, runsT, run.AccountID, run.ID, func(cur *domain.WorkflowRun, now time.Time) error {
		cur.Status, cur.State, cur.Error, cur.FinishedAt, cur.UpdatedAt = run.Status, run.State, run.Error, run.FinishedAt, now
		*run = *cur
		return nil
	})
}

// StepRuns implements store.Store.
func (s *Store) StepRuns() store.StepRunRepo { return stepRepo{s} }

type stepRepo struct{ s *Store }

func (r stepRepo) Create(_ context.Context, st *domain.StepRun) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := firstErr(foreign(&d.runs, st.AccountID, st.RunID), foreign(&d.jobs, st.AccountID, st.JobID), foreign(&d.gates, st.AccountID, st.GateID), foreign(&d.signals, st.AccountID, st.SignalID)); err != nil {
			return err
		}
		st.ID = newID(st.ID)
		st.CreatedAt = now
		d.steps.insert(*st)
		return nil
	})
}

func (r stepRepo) Get(_ context.Context, accountID, id uuid.UUID) (domain.StepRun, error) {
	return getRow(r.s, stepsT, accountID, id)
}

func (r stepRepo) ListByRun(_ context.Context, accountID, runID uuid.UUID) ([]domain.StepRun, error) {
	return listRows(r.s, stepsT, 0, func(st domain.StepRun) bool { return st.AccountID == accountID && st.RunID == runID })
}

func (r stepRepo) ListWaiting(_ context.Context, accountID uuid.UUID, key string) ([]domain.StepRun, error) {
	return listRows(r.s, stepsT, 0, func(st domain.StepRun) bool {
		return st.AccountID == accountID && st.Status == domain.StepWaiting && st.WaitKey == key
	})
}

func (r stepRepo) Update(_ context.Context, st *domain.StepRun) error {
	return update(r.s, stepsT, st.AccountID, st.ID, func(cur *domain.StepRun, _ time.Time) error {
		id, account, run, step, attempt, created := cur.ID, cur.AccountID, cur.RunID, cur.StepID, cur.Attempt, cur.CreatedAt
		*cur = *st
		cur.ID, cur.AccountID, cur.RunID, cur.StepID, cur.Attempt, cur.CreatedAt = id, account, run, step, attempt, created
		*st = *cur
		return nil
	})
}

// WebhookDeliveries implements store.Store.
func (s *Store) WebhookDeliveries() store.WebhookDeliveryRepo { return deliveryRepo{s} }

type deliveryRepo struct{ s *Store }

func (r deliveryRepo) Record(_ context.Context, dl domain.WebhookDelivery) (bool, error) {
	isNew := false
	err := r.s.do(func(d *storeData, now time.Time) error {
		if _, ok := d.deliveries[dl.DeliveryID]; ok {
			return nil
		}
		if dl.ReceivedAt.IsZero() {
			dl.ReceivedAt = now
		}
		d.deliveries[dl.DeliveryID] = dl
		isNew = true
		return nil
	})
	return isNew, err
}

// MCPTokens implements store.Store.
func (s *Store) MCPTokens() store.MCPTokenRepo { return tokenRepo{s} }

type tokenRepo struct{ s *Store }

func (r tokenRepo) Create(_ context.Context, t *domain.MCPToken) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if d.tokens.exists(func(x domain.MCPToken) bool { return x.AccountID == t.AccountID && x.Name == t.Name }) {
			return fmt.Errorf("token %s: %w", t.Name, store.ErrConflict)
		}
		t.ID = newID(t.ID)
		t.CreatedAt = now
		d.tokens.insert(*t)
		return nil
	})
}

func (r tokenRepo) GetByTokenHash(_ context.Context, hash []byte) (domain.MCPToken, error) {
	return findRow(r.s, tokensT, func(t domain.MCPToken) bool { return bytes.Equal(t.TokenHash, hash) })
}

func (r tokenRepo) List(_ context.Context, accountID uuid.UUID) ([]domain.MCPToken, error) {
	return listRows(r.s, tokensT, 0, func(t domain.MCPToken) bool { return t.AccountID == accountID })
}

func (r tokenRepo) Touch(_ context.Context, accountID, id uuid.UUID) error {
	return update(r.s, tokensT, accountID, id, func(t *domain.MCPToken, now time.Time) error {
		t.LastUsedAt = now
		return nil
	})
}

func (r tokenRepo) Revoke(_ context.Context, accountID, id uuid.UUID) error {
	return update(r.s, tokensT, accountID, id, func(t *domain.MCPToken, now time.Time) error {
		if t.RevokedAt.IsZero() {
			t.RevokedAt = now
		}
		return nil
	})
}

// Audit implements store.Store.
func (s *Store) Audit() store.AuditRepo { return auditRepo{s} }

type auditRepo struct{ s *Store }

func (r auditRepo) Append(_ context.Context, e *domain.AuditEntry) error {
	return r.s.do(func(d *storeData, now time.Time) error {
		if err := foreign(&d.projects, e.AccountID, e.ProjectID); err != nil {
			return err
		}
		e.ID = newID(e.ID)
		e.CreatedAt = now
		d.audit.insert(*e)
		return nil
	})
}

func (r auditRepo) List(_ context.Context, accountID uuid.UUID, f store.AuditFilter) ([]domain.AuditEntry, error) {
	return read(r.s, func(d *storeData) ([]domain.AuditEntry, error) {
		return d.audit.newest(f.Limit, func(e domain.AuditEntry) bool {
			return e.AccountID == accountID && zeroOr(f.ProjectID, e.ProjectID) && zeroOr(f.SubjectID, e.SubjectID)
		}), nil
	})
}

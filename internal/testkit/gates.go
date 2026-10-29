package testkit

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ gates.Service = (*Gates)(nil)

// Gates is a gates.Service on top of a store and a signal bus.
type Gates struct {
	st  store.Store
	bus workflow.SignalBus
	now func() time.Time
}

// NewGates returns a gate service. now defaults to the real UTC time.
func NewGates(st store.Store, bus workflow.SignalBus, now func() time.Time) *Gates {
	return &Gates{st: st, bus: bus, now: orNow(now)}
}

// Open implements gates.Service.
func (s *Gates) Open(ctx context.Context, req gates.OpenRequest) (domain.Gate, error) {
	g := domain.Gate{
		AccountID: req.AccountID, ProjectID: req.ProjectID, Kind: req.Kind, Title: req.Title, Summary: req.Summary,
		SubjectRepo: req.SubjectRepo, SubjectNumber: req.SubjectNumber,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID, JobID: req.JobID,
	}
	if err := s.st.Gates().Create(ctx, &g); err != nil {
		return domain.Gate{}, err
	}
	err := s.st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: g.AccountID, ProjectID: g.ProjectID, Actor: domain.Actor{Kind: domain.ActorSystem, Name: "forecrewd"}.String(),
		Action: "gate.opened", SubjectType: "gate", SubjectID: g.ID,
	})
	return g, err
}

// Resolve implements gates.Service.
func (s *Gates) Resolve(ctx context.Context, by domain.Actor, gateID uuid.UUID, decision domain.GateStatus, feedback string) (domain.Gate, error) {
	g, err := s.Get(ctx, by.AccountID, gateID)
	if err != nil {
		return domain.Gate{}, err
	}
	now := s.now()
	if err := g.Resolve(by, decision, feedback, now); err != nil {
		return domain.Gate{}, err
	}
	if err := s.st.Gates().Resolve(ctx, &g); err != nil {
		return domain.Gate{}, err
	}
	err = s.st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: g.AccountID, ProjectID: g.ProjectID, Actor: by.String(),
		Action: "gate.resolved", SubjectType: "gate", SubjectID: g.ID,
	})
	if err != nil {
		return domain.Gate{}, err
	}
	sig, err := domain.NewSignal(g.AccountID, g.ProjectID, &domain.GateResolved{GateID: g.ID, Decision: decision, Feedback: feedback}, now)
	if err != nil {
		return domain.Gate{}, err
	}
	_, err = s.bus.Publish(ctx, sig)
	return g, err
}

// Get implements gates.Service.
func (s *Gates) Get(ctx context.Context, accountID, gateID uuid.UUID) (domain.Gate, error) {
	g, err := s.st.Gates().Get(ctx, accountID, gateID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Gate{}, fmt.Errorf("gate %s: %w", gateID, gates.ErrNotFound)
	}
	return g, err
}

// ListPending implements gates.Service.
func (s *Gates) ListPending(ctx context.Context, accountID, projectID uuid.UUID) ([]domain.Gate, error) {
	return s.st.Gates().List(ctx, accountID, store.GateFilter{ProjectID: projectID, Status: domain.GatePending})
}

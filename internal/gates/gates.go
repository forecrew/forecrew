// Package gates defines the service that opens gates and records human
// decisions. internal/testkit has a fake.
package gates

import (
	"context"
	"errors"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// ErrNotFound means the gate does not exist in the account.
var ErrNotFound = errors.New("gate not found")

// OpenRequest opens a gate.
type OpenRequest struct {
	AccountID     uuid.UUID
	ProjectID     uuid.UUID
	Kind          domain.GateKind
	Title         string
	Summary       string
	SubjectRepo   string
	SubjectNumber int
	GoalID        uuid.UUID
	SpecID        uuid.UUID
	TaskID        uuid.UUID
	JobID         uuid.UUID
}

// Service manages gates.
type Service interface {
	// Open stores a pending gate and writes an audit entry.
	Open(ctx context.Context, req OpenRequest) (domain.Gate, error)
	// Resolve records the decision of by (domain.Gate.Resolve), writes an
	// audit entry and publishes a gate.resolved signal. Errors wrap
	// domain.ErrInvalidDecision or domain.ErrInvalidTransition.
	Resolve(ctx context.Context, by domain.Actor, gateID uuid.UUID, decision domain.GateStatus, feedback string) (domain.Gate, error)
	Get(ctx context.Context, accountID, gateID uuid.UUID) (domain.Gate, error)
	// ListPending returns the pending gates of a project, or of the whole
	// account when projectID is nil.
	ListPending(ctx context.Context, accountID, projectID uuid.UUID) ([]domain.Gate, error)
}

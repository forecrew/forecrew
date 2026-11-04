package councils

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Playbook is the playbook a council runs: packs/playbooks/council.yaml. It
// takes the inputs council_id and roles.
const Playbook = "council"

// Starter starts playbook runs; *workflow.Engine implements it.
type Starter interface {
	Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error)
}

// Request starts a council.
type Request struct {
	// ID, when set, makes Start idempotent: starting it again finds the
	// council and its run.
	ID          uuid.UUID
	AccountID   uuid.UUID
	ProjectID   uuid.UUID
	GoalID      uuid.UUID // nil for a council of its own
	Kind        domain.CouncilKind
	Question    string
	Constraints []string
	// Roles are the members instead of the kind's (Set.Seated).
	Roles []string
}

// Start stores a council and starts its playbook, as a run of its own with
// the council's ID. A request a council cannot run wraps ErrInvalidTurn.
func (s *Set) Start(ctx context.Context, st store.Store, runs Starter, r Request) (domain.Council, error) {
	r.Kind = cmp.Or(r.Kind, domain.CouncilAdhoc)
	if strings.TrimSpace(r.Question) == "" {
		return domain.Council{}, fmt.Errorf("%w: question is required", ErrInvalidTurn)
	}
	if _, err := s.Seated(r.Kind, r.Roles); err != nil {
		return domain.Council{}, err
	}
	c, err := s.record(ctx, st, r)
	if err != nil {
		return domain.Council{}, err
	}
	run, err := runs.Start(ctx, Playbook, workflow.StartRequest{
		ID: c.ID, AccountID: c.AccountID, ProjectID: c.ProjectID, GoalID: c.GoalID,
		Inputs: map[string]any{"council_id": c.ID.String(), "roles": slices.Concat([]string{}, r.Roles)},
	})
	if err != nil {
		return domain.Council{}, fmt.Errorf("start %s: %w", c.Ref(), errors.Join(err, st.Councils().UpdateStatus(ctx, c.AccountID, c.ID, domain.CouncilRunning, domain.CouncilFailed)))
	}
	if c.RunID == run.ID {
		return c, nil
	}
	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		cur, err := tx.Councils().GetForUpdate(ctx, c.AccountID, c.ID)
		if err != nil {
			return err
		}
		cur.RunID = run.ID
		c = cur
		return tx.Councils().Update(ctx, &cur)
	})
	return c, err
}

// record returns the council of r, created unless its ID has one.
func (s *Set) record(ctx context.Context, st store.Store, r Request) (domain.Council, error) {
	if r.ID != uuid.Nil() {
		c, err := st.Councils().Get(ctx, r.AccountID, r.ID)
		if !errors.Is(err, store.ErrNotFound) {
			return c, err
		}
	}
	c := domain.Council{
		ID: r.ID, AccountID: r.AccountID, ProjectID: r.ProjectID, GoalID: r.GoalID, Kind: r.Kind, Question: r.Question,
		Constraints: slices.Concat([]string{}, r.Constraints), Status: domain.CouncilRunning,
	}
	if err := c.Validate(); err != nil {
		return domain.Council{}, fmt.Errorf("%w: %w", ErrInvalidTurn, err)
	}
	if err := st.Councils().Create(ctx, &c); err != nil {
		return domain.Council{}, fmt.Errorf("store the council: %w", err)
	}
	return c, nil
}

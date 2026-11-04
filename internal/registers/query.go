package registers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// RestsOn reports whether the task rests on a: it is a dependent of a, listed
// by ref or ID, or a is an assumption of the goal as a whole (it lists no
// task: the architect's or the spec writer's, a council's) and the task is one
// of the goal's. Dependent work merges only after the assumptions it rests
// on are resolved, whoever stated them.
func RestsOn(a domain.Assumption, t domain.Task) bool {
	isTask := func(d domain.Dependent) bool { return d.Kind == domain.DependentTask }
	if slices.ContainsFunc(a.Dependents, func(d domain.Dependent) bool {
		return isTask(d) && (d.ID == t.Ref() || d.ID == t.ID.String())
	}) {
		return true
	}
	return a.GoalID != uuid.Nil() && a.GoalID == t.GoalID && !slices.ContainsFunc(a.Dependents, isTask)
}

// OfTask returns the assumptions of the task's goal that the task rests on
// (see RestsOn), whatever their status, by number. The verify_by test ones are what the
// test engineer is pointed at.
func (s *Service) OfTask(ctx context.Context, accountID uuid.UUID, t domain.Task) ([]domain.Assumption, error) {
	return ofTask(ctx, s.st, accountID, t)
}

func ofTask(ctx context.Context, st store.Store, accountID uuid.UUID, t domain.Task) ([]domain.Assumption, error) {
	all, err := st.Assumptions().List(ctx, accountID, store.AssumptionFilter{ProjectID: t.ProjectID, GoalID: t.GoalID})
	if err != nil {
		return nil, fmt.Errorf("list the assumptions of %s: %w", t.Ref(), err)
	}
	return slices.DeleteFunc(all, func(a domain.Assumption) bool { return !RestsOn(a, t) }), nil
}

// Blocking returns the assumptions that keep the merge gate closed: the
// high-impact ones that are still unverified.
func Blocking(as []domain.Assumption) []domain.Assumption {
	return slices.DeleteFunc(slices.Clone(as), func(a domain.Assumption) bool {
		return a.Impact != domain.LevelHigh || !a.Status.Open()
	})
}

// ReviewGates returns the pending assumption_review gates of as.
func (s *Service) ReviewGates(ctx context.Context, accountID, projectID uuid.UUID, as []domain.Assumption) ([]domain.Gate, error) {
	if len(as) == 0 {
		return []domain.Gate{}, nil
	}
	pending, err := s.st.Gates().List(ctx, accountID, store.GateFilter{ProjectID: projectID, Kind: domain.GateAssumptionReview, Status: domain.GatePending})
	if err != nil {
		return nil, fmt.Errorf("list assumption reviews: %w", err)
	}
	return slices.DeleteFunc(pending, func(g domain.Gate) bool {
		n, ok := ReviewedRef(g)
		return !ok || !slices.ContainsFunc(as, func(a domain.Assumption) bool { return a.Number == n })
	}), nil
}

// AskedFor returns what a human asked for when they sent the review of an
// assumption of as back for changes, by assumption number: the feedback of
// the latest such review. Assumptions never sent back by a human are not in
// the map.
func (s *Service) AskedFor(ctx context.Context, accountID, projectID uuid.UUID, as []domain.Assumption) (map[int64]string, error) {
	asked := map[int64]string{}
	if len(as) == 0 {
		return asked, nil
	}
	sent, err := s.st.Gates().List(ctx, accountID, store.GateFilter{ProjectID: projectID, Kind: domain.GateAssumptionReview, Status: domain.GateChangesRequested})
	if err != nil {
		return nil, fmt.Errorf("list assumption reviews: %w", err)
	}
	for _, g := range sent {
		n, ok := ReviewedRef(g)
		// forecrewd's own requests (a PR merged meanwhile) are not a human's.
		human := !strings.HasPrefix(g.ResolvedBy, "system:")
		if ok && human && slices.ContainsFunc(as, func(a domain.Assumption) bool { return a.Number == n }) && strings.TrimSpace(g.Feedback) != "" {
			asked[n] = g.Feedback
		}
	}
	return asked, nil
}

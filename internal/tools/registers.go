package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

func (s *Service) registers() (*registers.Service, error) {
	if s.cfg.Registers == nil {
		return nil, fmt.Errorf("the assumption register is not configured: %w", mcp.ErrUnavailable)
	}
	return s.cfg.Registers, nil
}

func (s *Service) assumption(ctx context.Context, accountID uuid.UUID, ref string) (domain.Assumption, error) {
	return lookup(ref, domain.RefAssumption,
		func(n int64) (domain.Assumption, error) {
			return s.cfg.Store.Assumptions().GetByNumber(ctx, accountID, n)
		},
		func(id uuid.UUID) (domain.Assumption, error) {
			return s.cfg.Store.Assumptions().Get(ctx, accountID, id)
		})
}

// assumptionInfos adds ref and source to assumptions.
func (s *Service) assumptionInfos(ctx context.Context, regs *registers.Service, accountID uuid.UUID, as []domain.Assumption) ([]mcp.AssumptionInfo, error) {
	out := make([]mcp.AssumptionInfo, len(as))
	for i, a := range as {
		src, err := regs.SourceRef(ctx, accountID, a)
		if err != nil {
			return nil, err
		}
		out[i] = mcp.AssumptionInfo{Assumption: a, Ref: a.Ref(), Source: src}
	}
	return out, nil
}

// goalAssumptions returns the register of a goal for get_goal; without a
// register it is empty.
func (s *Service) goalAssumptions(ctx context.Context, g domain.Goal) ([]mcp.AssumptionInfo, error) {
	if s.cfg.Registers == nil {
		return []mcp.AssumptionInfo{}, nil
	}
	as, err := s.cfg.Store.Assumptions().List(ctx, g.AccountID, store.AssumptionFilter{ProjectID: g.ProjectID, GoalID: g.ID})
	if err != nil {
		return nil, fmt.Errorf("assumptions of %s: %w", g.Ref(), err)
	}
	return s.assumptionInfos(ctx, s.cfg.Registers, g.AccountID, as)
}

// ListAssumptions implements mcp.ToolService.
func (s *Service) ListAssumptions(ctx context.Context, by domain.Actor, in mcp.ListAssumptionsInput) ([]mcp.AssumptionInfo, error) {
	regs, err := s.registers()
	if err != nil {
		return nil, err
	}
	if err := checkAssumptionFilter(in); err != nil {
		return nil, err
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	f := store.AssumptionFilter{ProjectID: p.ID, Status: in.Status, Impact: in.Impact, VerifyBy: in.VerifyBy}
	task, err := s.narrowToTask(ctx, by.AccountID, p, in, &f)
	if err != nil {
		return nil, err
	}
	as, err := s.cfg.Store.Assumptions().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	if task != nil {
		as = slices.DeleteFunc(as, func(a domain.Assumption) bool { return !registers.RestsOn(a, *task) })
	}
	return s.assumptionInfos(ctx, regs, by.AccountID, as)
}

// ResolveAssumption implements mcp.ToolService.
func (s *Service) ResolveAssumption(ctx context.Context, by domain.Actor, in mcp.ResolveAssumptionInput) (mcp.AssumptionInfo, error) {
	regs, err := s.registers()
	if err != nil {
		return mcp.AssumptionInfo{}, err
	}
	if in.Status == domain.AssumptionUnverified || !slices.Contains(domain.AssumptionStatuses(), in.Status) {
		return mcp.AssumptionInfo{}, fmt.Errorf("status must be verified, invalidated or accepted, not %q: %w", in.Status, mcp.ErrInvalidInput)
	}
	a, err := s.assumption(ctx, by.AccountID, in.Assumption)
	if err != nil {
		return mcp.AssumptionInfo{}, err
	}
	got, err := regs.Resolve(ctx, by, a.ID, in.Status, in.Evidence)
	if errors.Is(err, domain.ErrInvalidRecord) {
		err = fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.AssumptionInfo{}, err
	}
	infos, err := s.assumptionInfos(ctx, regs, by.AccountID, []domain.Assumption{got})
	if err != nil {
		return mcp.AssumptionInfo{}, err
	}
	return infos[0], nil
}

// ListDecisions implements mcp.ToolService.
func (s *Service) ListDecisions(ctx context.Context, by domain.Actor, in mcp.ListDecisionsInput) ([]mcp.DecisionInfo, error) {
	if _, err := s.registers(); err != nil {
		return nil, err
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	f := store.DecisionFilter{ProjectID: p.ID}
	if in.Goal != "" {
		g, err := s.goal(ctx, by.AccountID, in.Goal)
		if err != nil {
			return nil, err
		}
		f.GoalID = g.ID
	}
	if in.Council != "" {
		c, err := s.council(ctx, by.AccountID, in.Council)
		if err != nil {
			return nil, err
		}
		f.CouncilID = c.ID
	}
	ds, err := s.cfg.Store.Decisions().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	refs := map[uuid.UUID]string{}
	out := make([]mcp.DecisionInfo, len(ds))
	for i, d := range ds {
		info := mcp.DecisionInfo{Decision: d, Ref: d.Ref(), Assumptions: []string{}}
		for _, id := range d.Assumptions {
			ref, err := memo(refs, id, func() (string, error) {
				a, err := s.cfg.Store.Assumptions().Get(ctx, by.AccountID, id)
				return a.Ref(), err
			})
			if err != nil {
				return nil, err
			}
			info.Assumptions = append(info.Assumptions, ref)
		}
		out[i] = info
	}
	return out, nil
}

func checkAssumptionFilter(in mcp.ListAssumptionsInput) error {
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"status " + string(in.Status), in.Status == "" || slices.Contains(domain.AssumptionStatuses(), in.Status)},
		{"impact " + string(in.Impact), in.Impact == "" || slices.Contains(domain.Levels(), in.Impact)},
		{"verify_by " + string(in.VerifyBy), in.VerifyBy == "" || slices.Contains(domain.VerifyBys(), in.VerifyBy)},
	} {
		if !c.ok {
			return fmt.Errorf("unknown %q: %w", c.name, mcp.ErrInvalidInput)
		}
	}
	return nil
}

// narrowToTask applies the goal and task of a list to f. It returns the task
// when the list is of what it rests on.
func (s *Service) narrowToTask(ctx context.Context, accountID uuid.UUID, p domain.Project, in mcp.ListAssumptionsInput, f *store.AssumptionFilter) (*domain.Task, error) {
	var task *domain.Task
	if in.Task != "" {
		t, err := s.task(ctx, accountID, in.Task)
		if err != nil {
			return nil, err
		}
		if t.ProjectID != p.ID {
			return nil, fmt.Errorf("task %s is not in project %s: %w", in.Task, p.Name, mcp.ErrInvalidInput)
		}
		task, f.GoalID = &t, t.GoalID
	}
	if in.Goal != "" {
		g, err := s.goal(ctx, accountID, in.Goal)
		if err != nil {
			return nil, err
		}
		if f.GoalID != uuid.Nil() && f.GoalID != g.ID {
			return nil, fmt.Errorf("task %s is not in goal %s: %w", in.Task, in.Goal, mcp.ErrInvalidInput)
		}
		f.GoalID = g.ID
	}
	return task, nil
}

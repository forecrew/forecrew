package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// MaxConstraint is the longest constraint in characters: every job of the
// council quotes it.
const MaxConstraint = 2000

func (s *Service) council(ctx context.Context, accountID uuid.UUID, ref string) (domain.Council, error) {
	return lookup(ref, domain.RefCouncil,
		func(n int64) (domain.Council, error) { return s.cfg.Store.Councils().GetByNumber(ctx, accountID, n) },
		func(id uuid.UUID) (domain.Council, error) { return s.cfg.Store.Councils().Get(ctx, accountID, id) })
}

func constraintOf(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("constraint is empty: %w", mcp.ErrInvalidInput)
	}
	if utf8.RuneCountInString(text) > MaxConstraint {
		return "", fmt.Errorf("constraint is over %d characters: %w", MaxConstraint, mcp.ErrInvalidInput)
	}
	return text, nil
}

// StartCouncil implements mcp.ToolService.
func (s *Service) StartCouncil(ctx context.Context, by domain.Actor, in mcp.StartCouncilInput) (mcp.CouncilInfo, error) {
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermCouncilStart, p.ID); err != nil {
		return mcp.CouncilInfo{}, err
	}
	constraints := make([]string, len(in.Constraints))
	for i, text := range in.Constraints {
		if constraints[i], err = constraintOf(text); err != nil {
			return mcp.CouncilInfo{}, err
		}
	}
	c, err := s.cfg.Councils.Start(ctx, s.cfg.Store, s.cfg.Playbooks, councils.Request{
		AccountID: by.AccountID, ProjectID: p.ID, Kind: domain.CouncilKind(in.Kind), Question: strings.TrimSpace(in.Question),
		Constraints: constraints, Roles: in.Roles,
	})
	if errors.Is(err, councils.ErrInvalidTurn) {
		return mcp.CouncilInfo{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	err = s.audit(ctx, s.cfg.Store, by, p.ID, "council.started", "council", c.ID, map[string]any{"kind": c.Kind, "question": c.Question})
	return mcp.CouncilInfo{Council: c, Ref: c.Ref()}, err
}

// MaxListedCouncils is how many councils list_councils returns.
const MaxListedCouncils = 200

// ListCouncils implements mcp.ToolService.
func (s *Service) ListCouncils(ctx context.Context, by domain.Actor, in mcp.ListCouncilsInput) ([]mcp.CouncilInfo, error) {
	if in.Status != "" && !slices.Contains(domain.CouncilStatuses(), in.Status) {
		return nil, fmt.Errorf("unknown status %q: %w", in.Status, mcp.ErrInvalidInput)
	}
	f := store.CouncilFilter{Status: in.Status, Limit: MaxListedCouncils}
	var err error
	if f.ProjectID, err = s.optionalProject(ctx, by.AccountID, in.Project); err != nil {
		return nil, err
	}
	visible, err := s.scope(ctx, by, domain.PermProjectRead, f.ProjectID)
	if err != nil {
		return nil, err
	}
	if !visible.All {
		f.InProjects = visible.IDs
	}
	cs, err := s.cfg.Store.Councils().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.CouncilInfo, len(cs))
	for i, c := range cs {
		c.Brief = nil // agent text: it is read with get_council, which marks it untrusted
		readsGates, err := s.readsGates(ctx, by, c.ProjectID)
		if err != nil {
			return nil, err
		}
		if !readsGates {
			c.GateID = uuid.Nil()
		}
		out[i] = mcp.CouncilInfo{Council: c, Ref: c.Ref()}
	}
	return out, nil
}

// GetCouncil implements mcp.ToolService.
func (s *Service) GetCouncil(ctx context.Context, by domain.Actor, in mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
	c, err := s.council(ctx, by.AccountID, in.Council)
	if err != nil {
		return mcp.CouncilDetail{}, err
	}
	if err := s.allow(ctx, by, domain.PermProjectRead, c.ProjectID); err != nil {
		return mcp.CouncilDetail{}, err
	}
	all, err := s.cfg.Store.Councils().Contributions(ctx, by.AccountID, c.ID, "")
	if err != nil {
		return mcp.CouncilDetail{}, err
	}
	stages := domain.CouncilStages()
	slices.SortStableFunc(all, func(a, b domain.CouncilContribution) int {
		return cmp.Or(cmp.Compare(slices.Index(stages, a.Stage), slices.Index(stages, b.Stage)), cmp.Compare(a.Round, b.Round))
	})
	d := mcp.CouncilDetail{Council: c, Ref: c.Ref(), Contributions: make([]mcp.ContributionInfo, len(all))}
	for i, k := range all {
		d.Contributions[i] = mcp.ContributionInfo{CouncilContribution: k, Member: councils.MemberName(k.Role, k.Runtime)}
	}
	readsGates, err := s.readsGates(ctx, by, c.ProjectID)
	if err != nil {
		return mcp.CouncilDetail{}, err
	}
	if c.GateID != uuid.Nil() && readsGates {
		g, err := s.cfg.Store.Gates().Get(ctx, by.AccountID, c.GateID)
		if err != nil {
			return mcp.CouncilDetail{}, err
		}
		infos, err := s.gateInfos(ctx, by, []domain.Gate{g})
		if err != nil {
			return mcp.CouncilDetail{}, err
		}
		d.Gate = &infos[0]
	}
	if d.Untrusted, err = untrustedText(d); err != nil {
		return mcp.CouncilDetail{}, err
	}
	if d.Decision, err = s.councilDecision(ctx, c); err != nil {
		return d, err
	}
	if !readsGates {
		d.GateID = uuid.Nil()
		if d.Decision != nil {
			d.Decision.GateID = uuid.Nil()
		}
	}
	return d, nil
}

// untrustedText is what agents wrote in a council, each piece between
// untrusted-data markers.
func untrustedText(d mcp.CouncilDetail) (string, error) {
	var text strings.Builder
	for _, k := range d.Contributions {
		raw, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		text.WriteString(contextpack.WrapUntrusted(fmt.Sprintf("%s, %s round %d", k.Member, k.Stage, k.Round), string(raw)))
	}
	if d.Brief != nil {
		raw, err := json.Marshal(d.Brief)
		if err != nil {
			return "", err
		}
		text.WriteString(contextpack.WrapUntrusted("the brief of "+d.Ref, string(raw)))
	}
	return text.String(), nil
}

// councilDecision returns the decision council.decided announced, if any.
func (s *Service) councilDecision(ctx context.Context, c domain.Council) (*domain.Decision, error) {
	signals, err := s.cfg.Store.Signals().ListByKey(ctx, c.AccountID, domain.CouncilKey(c.ID))
	if err != nil || len(signals) == 0 {
		return nil, err
	}
	p, err := signals[0].Decode()
	if err != nil {
		return nil, err
	}
	id := p.(*domain.CouncilDecidedSignal).DecisionID
	if id == uuid.Nil() {
		return nil, nil
	}
	d, err := s.cfg.Store.Decisions().Get(ctx, c.AccountID, id)
	return &d, err
}

// AddCouncilConstraint implements mcp.ToolService.
func (s *Service) AddCouncilConstraint(ctx context.Context, by domain.Actor, in mcp.AddCouncilConstraintInput) (mcp.CouncilInfo, error) {
	text, err := constraintOf(in.Constraint)
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	c, err := s.council(ctx, by.AccountID, in.Council)
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermCouncilStart, c.ProjectID); err != nil {
		return mcp.CouncilInfo{}, err
	}
	err = s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		cur, err := tx.Councils().GetForUpdate(ctx, by.AccountID, c.ID)
		if err != nil {
			return err
		}
		c = cur
		if c.Status.Terminal() {
			return fmt.Errorf("%s is %s: %w", c.Ref(), c.Status, domain.ErrInvalidTransition)
		}
		if slices.Contains(c.Constraints, text) {
			return nil
		}
		c.Constraints = append(c.Constraints, text)
		if err := tx.Councils().Update(ctx, &c); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, c.ProjectID, "council.constraint_added", "council", c.ID, map[string]any{"constraint": text})
	})
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	readsGates, err := s.readsGates(ctx, by, c.ProjectID)
	if err != nil {
		return mcp.CouncilInfo{}, err
	}
	if !readsGates {
		c.GateID = uuid.Nil()
	}
	return mcp.CouncilInfo{Council: c, Ref: c.Ref()}, nil
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

func (s *Service) improvement(ctx context.Context, accountID uuid.UUID, ref string) (domain.Proposal, error) {
	return lookup(ref, domain.RefProposal,
		func(n int64) (domain.Proposal, error) { return s.cfg.Store.Proposals().GetByNumber(ctx, accountID, n) },
		func(id uuid.UUID) (domain.Proposal, error) { return s.cfg.Store.Proposals().Get(ctx, accountID, id) })
}

// snoozed reports whether p is hidden now.
func (s *Service) snoozed(p domain.Proposal) bool {
	return p.Status == domain.ProposalSnoozed && p.SnoozedUntil.After(s.cfg.Now())
}

// improvementInfo adds the refs of p and whether by may decide it. A proposal
// that is final has nothing left to decide.
func (s *Service) improvementInfo(ctx context.Context, by domain.Actor, p domain.Proposal) (mcp.ProposalInfo, error) {
	info := mcp.ProposalInfo{Proposal: p, Ref: p.Ref()}
	if p.GateID != uuid.Nil() {
		g, err := s.cfg.Store.Gates().Get(ctx, p.AccountID, p.GateID)
		if err != nil {
			return info, fmt.Errorf("gate of %s: %w", p.Ref(), err)
		}
		info.GateRef = g.Ref()
		if !p.Status.Final() {
			ok, err := s.authz.CanResolve(ctx, by, g)
			if err != nil {
				return info, err
			}
			info.CanDecide = ok && s.allow(ctx, by, domain.PermProposalDecide, p.ProjectID) == nil
		}
	}
	if p.GoalID != uuid.Nil() {
		g, err := s.cfg.Store.Goals().Get(ctx, p.AccountID, p.GoalID)
		if err != nil {
			return info, fmt.Errorf("goal of %s: %w", p.Ref(), err)
		}
		info.GoalRef = g.Ref()
	}
	return info, nil
}

// gateProposal returns the proposal of g for its card, nil when it has none.
func (s *Service) gateProposal(ctx context.Context, by domain.Actor, g domain.Gate) (*mcp.ProposalInfo, error) {
	found, err := s.cfg.Store.Proposals().List(ctx, g.AccountID, store.ProposalFilter{GateID: g.ID})
	if err != nil || len(found) == 0 {
		return nil, err
	}
	info, err := s.improvementInfo(ctx, by, found[0])
	return &info, err
}

// ListProposals implements mcp.ToolService.
func (s *Service) ListProposals(ctx context.Context, by domain.Actor, in mcp.ListProposalsInput) ([]mcp.ProposalInfo, error) {
	if in.Status != "" && !slices.Contains(domain.ProposalStatuses(), in.Status) {
		return nil, fmt.Errorf("status %q is not a proposal status: %w", in.Status, mcp.ErrInvalidInput)
	}
	if in.Category != "" && !slices.Contains(domain.ProposalCategories(), in.Category) {
		return nil, fmt.Errorf("category %q is not a proposal category: %w", in.Category, mcp.ErrInvalidInput)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	if err := s.allow(ctx, by, domain.PermProjectRead, p.ID); err != nil {
		return nil, err
	}
	found, err := s.cfg.Store.Proposals().List(ctx, by.AccountID, store.ProposalFilter{ProjectID: p.ID, Status: in.Status, Category: in.Category})
	if err != nil {
		return nil, err
	}
	if in.Status == "" {
		found = slices.DeleteFunc(found, s.snoozed)
	}
	out := make([]mcp.ProposalInfo, len(found))
	for i, x := range found {
		if out[i], err = s.improvementInfo(ctx, by, x); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DecideProposal implements mcp.ToolService. Accepting and rejecting resolve
// the proposal's gate, which needs what any resolution of it needs; the gate's
// recorder (advisor.Service) then moves the proposal, and its run goes on.
func (s *Service) DecideProposal(ctx context.Context, by domain.Actor, in mcp.DecideProposalInput) (mcp.ProposalInfo, error) {
	p, err := s.improvement(ctx, by.AccountID, in.Proposal)
	if err != nil {
		return mcp.ProposalInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermProposalDecide, p.ProjectID); err != nil {
		return mcp.ProposalInfo{}, err
	}
	switch in.Decision {
	case domain.ProposalAccept:
		err = s.resolveProposal(ctx, by, p, mcp.ToolApprove, in.Reason)
	case domain.ProposalReject:
		err = s.resolveProposal(ctx, by, p, mcp.ToolReject, in.Reason)
	case domain.ProposalSnooze:
		err = s.snoozeProposal(ctx, by, p, in)
	case mcp.ProposalDone:
		err = s.finishSetup(ctx, by, p)
	default:
		err = fmt.Errorf("decision %q must be accept, reject, snooze or done: %w", in.Decision, mcp.ErrInvalidInput)
	}
	if err != nil {
		return mcp.ProposalInfo{}, err
	}
	if p, err = s.cfg.Store.Proposals().Get(ctx, by.AccountID, p.ID); err != nil {
		return mcp.ProposalInfo{}, err
	}
	return s.improvementInfo(ctx, by, p)
}

func (s *Service) resolveProposal(ctx context.Context, by domain.Actor, p domain.Proposal, tool, reason string) error {
	if p.Status != domain.ProposalProposed && p.Status != domain.ProposalSnoozed {
		return fmt.Errorf("%s is %s: %w", p.Ref(), p.Status, mcp.ErrConflict)
	}
	g, err := s.cfg.Store.Gates().Get(ctx, p.AccountID, p.GateID)
	if err != nil {
		return fmt.Errorf("gate of %s: %w", p.Ref(), err)
	}
	if g.Status != domain.GatePending {
		return fmt.Errorf("%s is being revised: %w", p.Ref(), mcp.ErrConflict)
	}
	_, err = s.decide(ctx, by, tool, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: reason})
	return err
}

// moved stores p, moved from from by a decision, with its audit entry.
func (s *Service) moved(ctx context.Context, by domain.Actor, p domain.Proposal, from domain.ProposalStatus, action string) error {
	return s.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Proposals().Update(ctx, &p, from); err != nil {
			return conflict(err, p.Ref()+" changed meanwhile")
		}
		return s.audit(ctx, tx, by, p.ProjectID, action, "proposal", p.ID, map[string]any{"ref": p.Ref()})
	})
}

func (s *Service) snoozeProposal(ctx context.Context, by domain.Actor, p domain.Proposal, in mcp.DecideProposalInput) error {
	until, err := time.Parse(time.RFC3339, in.Until)
	if err != nil {
		return fmt.Errorf("until must be an RFC 3339 time: %w", mcp.ErrInvalidInput)
	}
	from := p.Status
	if err := p.Decide(by, domain.ProposalSnooze, in.Reason, until.UTC(), s.cfg.Now()); err != nil {
		return decisionError(err)
	}
	return s.moved(ctx, by, p, from, "proposal.snoozed")
}

// finishSetup marks the setup step of an accepted proposal done.
func (s *Service) finishSetup(ctx context.Context, by domain.Actor, p domain.Proposal) error {
	if p.Status != domain.ProposalAccepted || p.Action.Setup == nil {
		return fmt.Errorf("%s has no setup step waiting: %w", p.Ref(), mcp.ErrConflict)
	}
	p.Status = domain.ProposalDone
	return s.moved(ctx, by, p, domain.ProposalAccepted, "proposal.done")
}

// decisionError says what is wrong with a decision the domain refused.
func decisionError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalidTransition):
		return fmt.Errorf("%w: %w", err, mcp.ErrConflict)
	case errors.Is(err, domain.ErrInvalidRecord):
		return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	}
	return err
}

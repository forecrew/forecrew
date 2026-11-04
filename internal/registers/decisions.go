package registers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
)

var _ gates.Recorder = (*Service)(nil)

// decided are the gate kinds whose resolution by a human is a decision.
var decided = []domain.GateKind{domain.GateSpecApproval, domain.GateMergeApproval, domain.GateCouncilDecision, domain.GateAssumptionReview}

func human(by domain.Actor) bool {
	return by.Kind == domain.ActorUser || by.Kind == domain.ActorGitHub || by.Kind == domain.ActorMCP
}

// GateResolved implements gates.Recorder. A gate of kind spec_approval,
// merge_approval, council_decision or assumption_review that a human
// resolved becomes a decision: the question is the gate's title, the choice
// its decision and the reasons the feedback. The decision on the gate of a
// council names the council, and an approval is that of its recommendation
// (accepted), with the feedback first among the reasons; that is the one
// record of a human's decision, so the council runner records none. A
// resolved assumption_review
// gate also resolves its assumption: approved accepts it, rejected
// invalidates it (the feedback is the evidence), changes requested leaves it
// unverified and opens a new review. The returned function, to run after the commit, publishes the
// merge gates an invalidation sent back.
func (s *Service) GateResolved(ctx context.Context, tx store.Store, by domain.Actor, g domain.Gate) (func(context.Context) error, error) {
	if !slices.Contains(decided, g.Kind) {
		return func(context.Context) error { return nil }, nil
	}
	rests, then, err := s.restsOn(ctx, tx, by, g)
	if err != nil {
		return then, err
	}
	if !human(by) {
		return then, nil
	}
	d := domain.Decision{
		AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.GoalID, Question: g.Title, Choice: string(g.Status),
		Reasons: g.Feedback, DecidedBy: by.String(), GateID: g.ID, Assumptions: rests,
	}
	if d, err = s.ofCouncil(ctx, tx, g, d); err != nil {
		return then, err
	}
	_, err = s.createDecision(ctx, tx, by, d)
	return then, err
}

// ofCouncil returns d, the decision on g, as the decision on a council when
// g is the gate of one.
func (s *Service) ofCouncil(ctx context.Context, tx store.Store, g domain.Gate, d domain.Decision) (domain.Decision, error) {
	if g.Kind != domain.GateCouncilDecision {
		return d, nil
	}
	found, err := tx.Councils().List(ctx, g.AccountID, store.CouncilFilter{GateID: g.ID})
	if err != nil {
		return d, fmt.Errorf("find the council of %s: %w", g.Ref(), err)
	}
	if len(found) == 0 {
		return d, nil
	}
	c := found[0]
	d.CouncilID = c.ID
	if g.Status != domain.GateApproved || c.Brief == nil {
		return d, nil
	}
	a, err := s.accepted(ctx, tx, c)
	if err != nil {
		return d, err
	}
	a.DecidedBy, a.GateID = d.DecidedBy, g.ID
	a.Reasons = strings.TrimSpace(d.Reasons + "\n" + a.Reasons)
	return a, nil
}

// restsOn returns the assumptions the decision on g rests on: the reviewed
// one of an assumption_review gate (resolved from the decision, the returned
// function following up), the task's for a merge gate.
func (s *Service) restsOn(ctx context.Context, tx store.Store, by domain.Actor, g domain.Gate) ([]uuid.UUID, func(context.Context) error, error) {
	then := after(func(context.Context) error { return nil })
	var rests []uuid.UUID
	switch g.Kind {
	case domain.GateAssumptionReview:
		a, ok, err := reviewedAssumption(ctx, tx, g)
		if err != nil || !ok {
			return nil, then, err
		}
		rests = []uuid.UUID{a.ID}
		then, err = s.applyReview(ctx, tx, by, g, a)
		return rests, then, err
	case domain.GateMergeApproval:
		if g.TaskID == uuid.Nil() {
			return nil, then, nil
		}
		t, err := tx.Tasks().Get(ctx, g.AccountID, g.TaskID)
		if err != nil {
			return nil, then, fmt.Errorf("get the task of %s: %w", g.Ref(), err)
		}
		as, err := ofTask(ctx, tx, g.AccountID, t)
		for _, a := range as {
			rests = append(rests, a.ID)
		}
		return rests, then, err
	}
	return nil, then, nil
}

// reviewedAssumption returns the assumption an assumption_review gate is
// about; ok is false when the gate's title names none that exists.
func reviewedAssumption(ctx context.Context, tx store.Store, g domain.Gate) (domain.Assumption, bool, error) {
	n, ok := ReviewedRef(g)
	if !ok {
		return domain.Assumption{}, false, nil
	}
	a, err := tx.Assumptions().GetByNumber(ctx, g.AccountID, n)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return a, false, fmt.Errorf("get the assumption of %s: %w", g.Ref(), err)
	}
	return a, err == nil, nil
}

// applyReview resolves the assumption a from the decision on its gate g,
// unless someone resolved it already.
func (s *Service) applyReview(ctx context.Context, tx store.Store, by domain.Actor, g domain.Gate, a domain.Assumption) (after, error) {
	none := after(func(context.Context) error { return nil })
	if !a.Status.Open() {
		return none, nil
	}
	var to domain.AssumptionStatus
	evidence := g.Feedback
	switch g.Status {
	case domain.GateApproved:
		to = domain.AssumptionAccepted
	case domain.GateRejected:
		to = domain.AssumptionInvalidated
		if strings.TrimSpace(evidence) == "" {
			evidence = "rejected at " + g.Ref() + " without a reason"
		}
	case domain.GateChangesRequested:
		// It stays unverified. A human's request opens a new review, so that
		// the run waits for a human and not for agents; the feedback is
		// there, and in what the developer is told (Feedback).
		if !human(by) {
			return none, nil
		}
		return none, s.reopenReview(ctx, tx, g, a)
	default:
		return none, nil
	}
	_, then, err := s.resolve(ctx, tx, by, a.ID, to, evidence)
	return then, err
}

// createDecision stores d, audits it and returns it.
func (s *Service) createDecision(ctx context.Context, tx store.Store, by domain.Actor, d domain.Decision) (domain.Decision, error) {
	if err := d.Validate(); err != nil {
		return d, err
	}
	if err := tx.Decisions().Create(ctx, &d); err != nil {
		return d, fmt.Errorf("record decision: %w", err)
	}
	raw, err := json.Marshal(map[string]any{"ref": d.Ref(), "question": d.Question, "choice": d.Choice})
	if err != nil {
		return d, err
	}
	err = tx.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: d.AccountID, ProjectID: d.ProjectID, Actor: by.String(), Action: "decision.recorded",
		SubjectType: "decision", SubjectID: d.ID, Data: raw,
	})
	if err != nil {
		return d, fmt.Errorf("audit decision: %w", err)
	}
	return d, nil
}

// AcceptRecommendation records the decision of the autonomy policy to accept
// the recommendation of a council (T33 calls it, before the council moves to
// decided): the brief's question and options, the recommended option as the
// choice, the votes for it as reasons, the dissent, and the brief's
// assumptions, registered (or found, when the facilitator's job registered
// them already) with the council as source. A council has one such decision:
// a repeated call, after a crash for example, returns it.
func (s *Service) AcceptRecommendation(ctx context.Context, c domain.Council) (domain.Decision, error) {
	if c.Brief == nil {
		return domain.Decision{}, fmt.Errorf("%s has no brief: %w", c.Ref(), domain.ErrInvalidRecord)
	}
	var out domain.Decision
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		all, err := tx.Decisions().List(ctx, c.AccountID, store.DecisionFilter{CouncilID: c.ID})
		if err != nil {
			return fmt.Errorf("list the decisions of %s: %w", c.Ref(), err)
		}
		if i := slices.IndexFunc(all, func(d domain.Decision) bool { return d.DecidedBy == domain.DecidedByPolicy }); i >= 0 {
			out = all[i]
			return nil
		}
		d, err := s.accepted(ctx, tx, c)
		if err != nil {
			return err
		}
		out, err = s.createDecision(ctx, tx, domain.SystemActor(c.AccountID, domain.ForecrewdName), d)
		return err
	})
	return out, err
}

// accepted returns the decision to accept the recommendation of c, which has
// a brief, by the policy; it registers the assumptions of the brief.
func (s *Service) accepted(ctx context.Context, tx store.Store, c domain.Council) (domain.Decision, error) {
	b := c.Brief
	as, err := s.record(ctx, tx, c.AccountID, c.ProjectID, Source{MadeBy: "council " + c.Ref(), GoalID: c.GoalID, CouncilID: c.ID}, b.Assumptions)
	if err != nil {
		return domain.Decision{}, err
	}
	d := domain.Decision{
		AccountID: c.AccountID, ProjectID: c.ProjectID, GoalID: c.GoalID, Question: b.Question, Choice: b.Recommendation,
		DecidedBy: domain.DecidedByPolicy, CouncilID: c.ID, GateID: c.GateID, Assumptions: []uuid.UUID{},
	}
	for _, o := range b.Options {
		d.Options = append(d.Options, domain.DecisionOption{Name: contextpack.OneLine(o.Name), Summary: contextpack.OneLine(o.Summary)})
	}
	var reasons, dissent []string
	for _, v := range b.Votes {
		if v.Option == b.Recommendation {
			reasons = append(reasons, contextpack.OneLine(v.Member+": "+v.Reason))
		}
	}
	for _, x := range b.Dissent {
		dissent = append(dissent, contextpack.OneLine(x.Member+": "+x.Position))
	}
	d.Reasons, d.Dissent = strings.Join(reasons, "\n"), strings.Join(dissent, "\n")
	for _, a := range as {
		d.Assumptions = append(d.Assumptions, a.ID)
	}
	return d, nil
}

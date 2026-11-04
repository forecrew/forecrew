package registers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// after is work to do once the transaction has committed: resolving the
// gates of the work an invalidation sent back publishes signals.
type after func(ctx context.Context) error

// Resolve moves an assumption to verified, invalidated or accepted by by,
// with evidence (required for verified and invalidated), and audits it. Its
// pending review gate is decided with it (approved, or rejected for an
// invalidation), so that a run waiting for the review goes on. An
// invalidation flags every dependent: a task not merged yet is moved back
// to in progress and its pending merge gate is sent back for changes with
// the evidence as feedback; merged work (a merged task, a merged change)
// gets one re-evaluation goal in draft, not started. Errors wrap
// domain.ErrInvalidRecord, domain.ErrInvalidTransition or store.ErrNotFound.
func (s *Service) Resolve(ctx context.Context, by domain.Actor, id uuid.UUID, to domain.AssumptionStatus, evidence string) (domain.Assumption, error) {
	var a domain.Assumption
	var then after
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) (err error) {
		a, then, err = s.resolve(ctx, tx, by, id, to, evidence)
		return err
	})
	if err != nil {
		return domain.Assumption{}, err
	}
	if err := then(ctx); err != nil {
		return a, err
	}
	return a, s.closeReviews(ctx, by, a)
}

// closeReviews decides the pending review gates of the resolved assumption a
// as by: approved, rejected when a was invalidated. A gate someone decided
// meanwhile is left as it is.
func (s *Service) closeReviews(ctx context.Context, by domain.Actor, a domain.Assumption) error {
	gs, err := s.ReviewGates(ctx, a.AccountID, a.ProjectID, []domain.Assumption{a})
	if err != nil {
		return err
	}
	decision := domain.GateApproved
	if a.Status == domain.AssumptionInvalidated {
		decision = domain.GateRejected
	}
	feedback := a.Evidence
	if feedback == "" {
		feedback = a.Ref() + " was " + string(a.Status) + " without a review."
	}
	var errs []error
	for _, g := range gs {
		_, err := s.keeper(s.st).Resolve(ctx, by, g.ID, decision, feedback)
		if err != nil && !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, store.ErrConflict) {
			errs = append(errs, fmt.Errorf("close %s: %w", g.Ref(), err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) resolve(ctx context.Context, tx store.Store, by domain.Actor, id uuid.UUID, to domain.AssumptionStatus, evidence string) (domain.Assumption, after, error) {
	none := func(context.Context) error { return nil }
	a, err := tx.Assumptions().Get(ctx, by.AccountID, id)
	if err != nil {
		return a, none, fmt.Errorf("get assumption %s: %w", id, err)
	}
	from := a.Status
	if err := a.Resolve(by, to, strings.TrimSpace(evidence), s.now()); err != nil {
		return a, none, err
	}
	if err := tx.Assumptions().Resolve(ctx, &a, from); err != nil {
		return a, none, err
	}
	data := map[string]any{"ref": a.Ref(), "from": from, "status": to, "evidence": a.Evidence}
	if to != domain.AssumptionInvalidated {
		return a, none, s.audit(ctx, tx, by, a, "assumption.resolved", data)
	}
	flagged, then, err := s.flag(ctx, tx, a)
	if err != nil {
		return a, none, err
	}
	data["flagged"] = flagged.dependents
	if flagged.reevaluation != "" {
		data["reevaluation"] = flagged.reevaluation
	}
	return a, then, s.audit(ctx, tx, by, a, "assumption.resolved", data)
}

// flagged is what an invalidation did.
type flagged struct {
	dependents   []domain.Dependent
	reevaluation string // ref of the re-evaluation goal, "" when no merged work rests on the assumption
}

// flagger collects what an invalidation does to the dependents of a.
type flagger struct {
	tx     store.Store
	a      domain.Assumption
	seen   map[uuid.UUID]bool
	merged []string      // merged work that rests on a
	gates  []domain.Gate // pending merge gates to send back
}

// flag handles the dependents of the invalidated assumption a.
func (s *Service) flag(ctx context.Context, tx store.Store, a domain.Assumption) (flagged, after, error) {
	f := flagged{dependents: slices.Clone(a.Dependents)}
	fl := &flagger{tx: tx, a: a, seen: map[uuid.UUID]bool{}}
	for _, d := range a.Dependents {
		if err := fl.dependent(ctx, d); err != nil {
			return f, nil, err
		}
	}
	if len(fl.merged) > 0 {
		g, err := s.reevaluation(ctx, tx, a, fl.merged)
		if err != nil {
			return f, nil, err
		}
		f.reevaluation = g.Ref()
	}
	feedback := fmt.Sprintf("%s was invalidated.\nStatement:\n%sEvidence:\n%sRework the change without relying on it.", a.Ref(),
		contextpack.WrapUntrusted("assumption "+a.Ref(), a.Statement), contextpack.WrapUntrusted("evidence "+a.Ref(), a.Evidence))
	then := func(ctx context.Context) error {
		var errs []error
		keeper := s.keeper(s.st)
		for _, g := range fl.gates {
			_, err := keeper.Resolve(ctx, domain.SystemActor(a.AccountID, domain.ForecrewdName), g.ID, domain.GateChangesRequested, feedback)
			if err != nil && !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, store.ErrConflict) {
				errs = append(errs, fmt.Errorf("send %s back: %w", g.Ref(), err))
			}
		}
		return errors.Join(errs...)
	}
	return f, then, nil
}

// dependent flags one dependent: tasks and changes matter, the rest is
// flagged by the audit entry.
func (fl *flagger) dependent(ctx context.Context, d domain.Dependent) error {
	switch d.Kind {
	case domain.DependentTask:
		t, err := findTask(ctx, fl.tx, fl.a.AccountID, d.ID)
		if err != nil || t == nil {
			return err
		}
		return fl.task(ctx, *t)
	case domain.DependentChange:
		c, err := findChange(ctx, fl.tx, fl.a.AccountID, d.ID)
		if err != nil || c == nil {
			return err
		}
		switch {
		case c.State == domain.ChangeMerged:
			fl.merged = append(fl.merged, d.ID)
		case c.State == domain.ChangeOpen && c.TaskID != uuid.Nil():
			t, err := fl.tx.Tasks().Get(ctx, fl.a.AccountID, c.TaskID)
			if err != nil {
				return fmt.Errorf("get the task of %s: %w", d.ID, err)
			}
			return fl.task(ctx, t)
		}
	}
	return nil
}

// task sends an unmerged task back to in progress and collects its pending
// merge gates; a merged task is merged work.
func (fl *flagger) task(ctx context.Context, t domain.Task) error {
	if fl.seen[t.ID] {
		return nil
	}
	fl.seen[t.ID] = true
	switch t.Status {
	case domain.TaskMerged:
		fl.merged = append(fl.merged, t.Ref())
		return nil
	case domain.TaskClosed:
		return nil
	case domain.TaskInReview, domain.TaskMergeReady:
		if err := fl.tx.Tasks().UpdateStatus(ctx, t.AccountID, t.ID, t.Status, domain.TaskInProgress); err != nil {
			return fmt.Errorf("send %s back: %w", t.Ref(), err)
		}
	}
	gs, err := fl.tx.Gates().List(ctx, t.AccountID, store.GateFilter{ProjectID: t.ProjectID, Kind: domain.GateMergeApproval, Status: domain.GatePending})
	if err != nil {
		return fmt.Errorf("list the merge gates of %s: %w", t.Ref(), err)
	}
	fl.gates = append(fl.gates, slices.DeleteFunc(gs, func(g domain.Gate) bool { return g.TaskID != t.ID })...)
	return nil
}

// reevaluation creates the draft goal that asks what to do about merged
// work resting on the invalidated assumption a, and lists it among the
// dependents of a. The statement and the evidence are text an agent wrote:
// the goal is made by forecrewd, so they are wrapped.
func (s *Service) reevaluation(ctx context.Context, tx store.Store, a domain.Assumption, merged []string) (domain.Goal, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s was invalidated.\n\nStatement:\n%s\nEvidence:\n%s\nMerged work that rests on it:\n", a.Ref(),
		contextpack.WrapUntrusted("assumption "+a.Ref(), a.Statement), contextpack.WrapUntrusted("evidence "+a.Ref(), a.Evidence))
	for _, m := range merged {
		fmt.Fprintf(&b, "- %s\n", contextpack.OneLine(m))
	}
	b.WriteString("\nDecide what to change, then start this goal.")
	g := domain.Goal{
		AccountID: a.AccountID, ProjectID: a.ProjectID, Title: "Re-evaluate " + a.Ref(),
		Body: b.String(), Status: domain.GoalDraft, CreatedBy: domain.SystemActor(a.AccountID, domain.ForecrewdName).String(),
	}
	if err := tx.Goals().Create(ctx, &g); err != nil {
		return g, fmt.Errorf("create the re-evaluation goal of %s: %w", a.Ref(), err)
	}
	dep := []domain.Dependent{{Kind: domain.DependentGoal, ID: g.Ref()}}
	if err := tx.Assumptions().AddDependents(ctx, a.AccountID, a.ID, dep); err != nil {
		return g, fmt.Errorf("add %s to the dependents of %s: %w", g.Ref(), a.Ref(), err)
	}
	return g, nil
}

// findTask returns the task a dependent ID (a ref or UUID) names, nil when
// it no longer exists.
func findTask(ctx context.Context, tx store.Store, accountID uuid.UUID, ref string) (*domain.Task, error) {
	number, id, err := domain.ParseRefOrID(domain.RefTask, ref)
	if err != nil {
		return nil, nil //nolint:nilerr // not a task ref: nothing to flag
	}
	var t domain.Task
	if number != 0 {
		t, err = tx.Tasks().GetByNumber(ctx, accountID, number)
	} else {
		t, err = tx.Tasks().Get(ctx, accountID, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}
	return &t, nil
}

// findChange returns the change a dependent ID (owner/name#number) names,
// nil when it is not one or is not known.
func findChange(ctx context.Context, tx store.Store, accountID uuid.UUID, id string) (*domain.Change, error) {
	repo, num, ok := strings.Cut(id, "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil {
		return nil, nil //nolint:nilerr // not a change: nothing to flag
	}
	c, err := tx.Changes().GetByPR(ctx, accountID, repo, n)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get change %s: %w", id, err)
	}
	return &c, nil
}

// ChangeDependent is the dependent that names a pull request.
func ChangeDependent(repo string, number int) domain.Dependent {
	return domain.Dependent{Kind: domain.DependentChange, ID: repo + "#" + strconv.Itoa(number)}
}

// LinkChange adds the pull request as a dependent of every assumption the
// task rests on, so that invalidating one flags the change.
func (s *Service) LinkChange(ctx context.Context, accountID uuid.UUID, t domain.Task, repo string, number int) error {
	as, err := s.OfTask(ctx, accountID, t)
	if err != nil {
		return err
	}
	dep := []domain.Dependent{ChangeDependent(repo, number)}
	for _, a := range as {
		if err := s.st.Assumptions().AddDependents(ctx, accountID, a.ID, dep); err != nil {
			return fmt.Errorf("link %s#%d to %s: %w", repo, number, a.Ref(), err)
		}
	}
	return nil
}

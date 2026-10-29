package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Deps are what the handler and the poller work with.
type Deps struct {
	Store   store.Store
	Signals workflow.SignalBus
	Gates   gates.Service
	Tracker github.Tracker
	// Allowed reports whether a GitHub login may command @forecrew.
	Allowed func(login string) bool
	Log     *slog.Logger
	// Now is the clock; it defaults to the real UTC time.
	Now func() time.Time
}

type engine struct {
	Deps
}

func newEngine(d Deps) *engine {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &engine{d}
}

// handle maps one parsed event to signals and gate decisions.
func (e *engine) handle(ctx context.Context, ev github.Event) error {
	switch ev.Kind {
	case "pull_request":
		return e.pullRequest(ctx, ev)
	case "issue_comment":
		return e.comment(ctx, ev)
	case "check_suite", "check_run", "status":
		return e.checkEvent(ctx, ev)
	}
	return nil
}

// eachProject runs fn for every project that tracks repo and joins the errors.
func (e *engine) eachProject(ctx context.Context, repo string, fn func(domain.Project) error) error {
	ps, err := e.Store.Projects().ListByRepo(ctx, repo)
	if err != nil {
		return fmt.Errorf("list projects of %s: %w", repo, err)
	}
	var errs []error
	for _, p := range ps {
		errs = append(errs, fn(p))
	}
	return errors.Join(errs...)
}

func (e *engine) pullRequest(ctx context.Context, ev github.Event) error {
	switch ev.Action {
	case "closed":
		return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
			return e.prClosed(ctx, p, ev.Repo, ev.Number, ev.Merged, ev.SHA)
		})
	case "reopened", "synchronize":
		return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
			return e.refreshChange(ctx, p.AccountID, ev.Repo, ev.Number, ev.SHA)
		})
	}
	return nil
}

// refreshChange records that a known PR is open with head sha. Upserting
// moves its updated_at, which starts the wait for the checks of a new head.
func (e *engine) refreshChange(ctx context.Context, accountID uuid.UUID, repo string, number int, sha string) error {
	c, err := e.Store.Changes().GetByPR(ctx, accountID, repo, number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	c.State, c.HeadSHA = domain.ChangeOpen, sha
	if err := e.Store.Changes().Upsert(ctx, &c); err != nil {
		return fmt.Errorf("update change %s#%d: %w", repo, number, err)
	}
	return nil
}

// prClosed publishes pr.merged or pr.closed, decides the gates the PR is the
// subject of and records the new state of the change. The change is updated
// last, so a failure leaves it open for the poller to retry. sha is the head
// of the PR: GitHub's merge commit is not part of the parsed event.
func (e *engine) prClosed(ctx context.Context, p domain.Project, repo string, number int, merged bool, sha string) error {
	var payload domain.SignalPayload = domain.PRClosed{Repo: repo, Number: number}
	state := domain.ChangeClosed
	if merged {
		payload = domain.PRMerged{Repo: repo, Number: number, SHA: sha}
		state = domain.ChangeMerged
	}
	if _, err := e.publish(ctx, p.AccountID, p.ID, payload); err != nil {
		return err
	}
	if err := e.resolvePRGates(ctx, p.AccountID, repo, number, merged); err != nil {
		return err
	}
	return e.setChangeState(ctx, p.AccountID, repo, number, state)
}

// publish stores the signal; it reports whether it was new.
func (e *engine) publish(ctx context.Context, accountID, projectID uuid.UUID, payload domain.SignalPayload) (bool, error) {
	s, err := domain.NewSignal(accountID, projectID, payload, e.Now())
	if err != nil {
		return false, err
	}
	isNew, err := e.Signals.Publish(ctx, s)
	if err != nil {
		return false, fmt.Errorf("publish %s: %w", s.DedupeKey, err)
	}
	if isNew {
		e.Log.Info("signal published", "account_id", accountID, "project_id", projectID, "kind", s.Kind, "key", s.Key)
	}
	return isNew, nil
}

// resolvePRGates decides the profile and merge approval gates whose subject
// is the PR: merged approves them, closed rejects them.
func (e *engine) resolvePRGates(ctx context.Context, accountID uuid.UUID, repo string, number int, merged bool) error {
	pending, err := e.Store.Gates().ListPendingBySubject(ctx, accountID, repo, number)
	if err != nil {
		return fmt.Errorf("list gates of %s#%d: %w", repo, number, err)
	}
	decision := domain.GateRejected
	if merged {
		decision = domain.GateApproved
	}
	by := domain.Actor{AccountID: accountID, Kind: domain.ActorGitHub, Name: "github"}
	var errs []error
	for _, g := range pending {
		if g.Kind != domain.GateProfileApproval && g.Kind != domain.GateMergeApproval {
			continue
		}
		errs = append(errs, e.resolve(ctx, by, g, decision, ""))
	}
	return errors.Join(errs...)
}

// resolve decides a gate. A gate someone else decided in the meantime is not
// an error.
func (e *engine) resolve(ctx context.Context, by domain.Actor, g domain.Gate, decision domain.GateStatus, feedback string) error {
	_, err := e.Gates.Resolve(ctx, by, g.ID, decision, feedback)
	switch {
	case err == nil:
		e.Log.Info("gate resolved", "account_id", g.AccountID, "gate_id", g.ID, "ref", g.Ref(), "decision", decision, "by", by.String())
	case errors.Is(err, domain.ErrInvalidTransition):
		e.Log.Debug("gate already resolved", "account_id", g.AccountID, "gate_id", g.ID, "ref", g.Ref())
	default:
		return fmt.Errorf("resolve %s: %w", g.Ref(), err)
	}
	return nil
}

// setChangeState records the state of a known PR; unknown PRs are ignored.
func (e *engine) setChangeState(ctx context.Context, accountID uuid.UUID, repo string, number int, state domain.ChangeState) error {
	c, err := e.Store.Changes().GetByPR(ctx, accountID, repo, number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	if c.State == state {
		return nil
	}
	c.State = state
	if err := e.Store.Changes().Upsert(ctx, &c); err != nil {
		return fmt.Errorf("update change %s#%d: %w", repo, number, err)
	}
	return nil
}

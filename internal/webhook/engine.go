package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Deps are what the handler and the poller work with.
type Deps struct {
	Store   store.Store
	Signals workflow.SignalBus
	Gates   gates.Service
	// Authz checks the permission of GitHub commands.
	Authz   *authz.Engine
	Tracker github.Tracker
	// Knowledge keeps the knowledge index in step with the default branch;
	// nil leaves push events and the poller's knowledge pass out.
	Knowledge KnowledgeSyncer
	// Proposals settles the knowledge proposals of closed PRs; nil leaves
	// them to their runs.
	Proposals ProposalFollower
	// Tickets is told of issue events, for the GitHub Issues tracker; nil
	// leaves tickets to polling.
	Tickets TicketDeliverer
	// Bugs starts the bug-fix playbook for @forecrew bug; nil answers that
	// the command is not available.
	Bugs BugReporter
	// Allowed reports whether a GitHub login may command @forecrew in an
	// organization nobody has a role in yet; see commander.
	Allowed func(login string) bool
	Log     *slog.Logger
	// RunChanged, when set, is told of every workflow_run event, so the
	// GitHub Actions provider reads the run at once.
	RunChanged func(runID int64)
	// Now is the clock; it defaults to the real UTC time.
	Now func() time.Time
}

// KnowledgeSyncer queues the reindexing of a project's default branch;
// *knowledge.Scheduler implements it. Request must not wait for GitHub: it
// runs on the goroutine that handles webhooks.
type KnowledgeSyncer interface {
	Request(p domain.Project, before, head string)
}

// TicketDeliverer hands a delivery to the trackers of a kind; *tracker.Service
// implements it.
type TicketDeliverer interface {
	Deliver(ctx context.Context, kind string, r tracker.Request) error
}

// BugReporter makes an issue the goal issue of a bug goal and starts the
// bug-fix playbook; *tools.Service implements it. It does not check
// permissions: the command has.
type BugReporter interface {
	ReportIssueAsBug(ctx context.Context, by domain.Actor, p domain.Project, issue github.Issue) (domain.Goal, error)
}

// ProposalFollower settles the knowledge proposals of a closed PR;
// *proposals.Service implements it.
type ProposalFollower interface {
	PRClosed(ctx context.Context, accountID uuid.UUID, repo string, number int, merged bool) error
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
	case "workflow_run":
		if e.RunChanged != nil {
			e.RunChanged(ev.RunID)
		}
	case "push":
		return e.push(ctx, ev)
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
			return e.prClosed(ctx, p.AccountID, p.ID, ev.Repo, ev.Number, ev.Merged, ev.SHA)
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
func (e *engine) prClosed(ctx context.Context, accountID, projectID uuid.UUID, repo string, number int, merged bool, sha string) error {
	var payload domain.SignalPayload = domain.PRClosed{Repo: repo, Number: number}
	state := domain.ChangeClosed
	if merged {
		payload = domain.PRMerged{Repo: repo, Number: number, SHA: sha}
		state = domain.ChangeMerged
	}
	if _, err := e.publish(ctx, accountID, projectID, payload); err != nil {
		return err
	}
	if err := e.resolvePRGates(ctx, accountID, repo, number, merged); err != nil {
		return err
	}
	if err := e.releaseReviews(ctx, accountID, repo, number, merged); err != nil {
		return err
	}
	if e.Proposals != nil {
		if err := e.Proposals.PRClosed(ctx, accountID, repo, number, merged); err != nil {
			return err
		}
	}
	return e.setChangeState(ctx, accountID, repo, number, state)
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

// prGate reports whether a gate of this kind is decided by its PR being
// merged or closed.
func prGate(k domain.GateKind) bool {
	return k == domain.GateProfileApproval || k == domain.GateMergeApproval || k == domain.GateKnowledgeProposal || k == domain.GateTestPlanApproval
}

// resolvePRGates decides the profile, test plan, merge and knowledge approval
// gates whose subject is the PR: merged approves them, closed rejects them.
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
		if !prGate(g.Kind) {
			continue
		}
		errs = append(errs, e.resolve(ctx, by, g, decision, ""))
	}
	return errors.Join(errs...)
}

// releaseReviews sends back the pending assumption reviews of the task of a
// PR that was merged or closed: a run waiting for them has nothing left to
// wait for. forecrewd asks for changes, which leaves the assumptions
// unverified and records no decision.
func (e *engine) releaseReviews(ctx context.Context, accountID uuid.UUID, repo string, number int, merged bool) error {
	c, err := e.Store.Changes().GetByPR(ctx, accountID, repo, number)
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.TaskID == uuid.Nil()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	pending, err := e.Store.Gates().List(ctx, accountID, store.GateFilter{ProjectID: c.ProjectID, Kind: domain.GateAssumptionReview, Status: domain.GatePending})
	if err != nil {
		return fmt.Errorf("list assumption reviews: %w", err)
	}
	how := "closed"
	if merged {
		how = "merged"
	}
	feedback := fmt.Sprintf("%s#%d was %s on GitHub: this review is not waited for any more. The assumption stays unverified.", repo, number, how)
	by := domain.SystemActor(accountID, domain.ForecrewdName)
	var errs []error
	for _, g := range pending {
		if g.TaskID == c.TaskID {
			errs = append(errs, e.resolve(ctx, by, g, domain.GateChangesRequested, feedback))
		}
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

// push queues the reindexing of the knowledge of the projects whose default branch was
// pushed to (docs/knowledge.md): changed entries, drift and the generated
// section. Pushes to other branches and projects that are not onboarded are
// ignored.
func (e *engine) push(ctx context.Context, ev github.Event) error {
	if e.Knowledge == nil || ev.Action != "pushed" {
		return nil
	}
	return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
		if p.Status != domain.ProjectOnboarded || ev.Ref != "refs/heads/"+p.DefaultBranch {
			return nil
		}
		e.Knowledge.Request(p, ev.Before, ev.SHA)
		return nil
	})
}

package webhook

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// pollEvery is how often the poller looks at the open changes.
const pollEvery = 5 * time.Minute

// Poller recovers events the webhook missed: it compares every open change
// with GitHub and publishes the signals the webhook would have.
type Poller struct {
	e        *engine
	interval time.Duration
}

// NewPoller returns a poller that runs every five minutes.
func NewPoller(d Deps) *Poller {
	return &Poller{e: newEngine(d), interval: pollEvery}
}

// Run reconciles at once and then every interval until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		if err := p.Reconcile(ctx); err != nil {
			p.e.Log.Error("reconcile changes", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Reconcile looks at every open change of every account, and at the PRs that
// are the subject of a pending profile or merge approval without being a
// stored change, so a missed merge cannot leave such a gate pending forever.
// A PR that fails does not stop the others; the errors are joined.
func (p *Poller) Reconcile(ctx context.Context) error {
	accounts, err := p.e.Store.Accounts().List(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	var errs []error
	for _, a := range accounts {
		changes, err := p.e.Store.Changes().List(ctx, a.ID, store.ChangeFilter{State: domain.ChangeOpen})
		if err != nil {
			errs = append(errs, fmt.Errorf("list open changes: %w", err))
			continue
		}
		seen := map[string]bool{}
		for _, c := range changes {
			seen[domain.PRKey(c.Repo, c.Number)] = true
			if err := p.reconcileChange(ctx, c); err != nil {
				errs = append(errs, fmt.Errorf("%s#%d: %w", c.Repo, c.Number, err))
			}
		}
		errs = append(errs, p.reconcileGates(ctx, a.ID, seen))
	}
	return errors.Join(errs...)
}

// reconcileGates closes the pending approval gates of PRs that are not in seen
// and have been closed on GitHub.
func (p *Poller) reconcileGates(ctx context.Context, accountID uuid.UUID, seen map[string]bool) error {
	pending, err := p.e.Store.Gates().List(ctx, accountID, store.GateFilter{Status: domain.GatePending})
	if err != nil {
		return fmt.Errorf("list pending gates: %w", err)
	}
	var errs []error
	for _, g := range pending {
		if (g.Kind != domain.GateProfileApproval && g.Kind != domain.GateMergeApproval) || g.SubjectRepo == "" || g.SubjectNumber < 1 {
			continue
		}
		key := domain.PRKey(g.SubjectRepo, g.SubjectNumber)
		if seen[key] {
			continue
		}
		seen[key] = true
		pr, err := p.e.Tracker.GetPR(ctx, g.SubjectRepo, g.SubjectNumber)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: get pull request: %w", key, err))
			continue
		}
		if pr.State == "closed" {
			errs = append(errs, p.e.prClosed(ctx, accountID, g.ProjectID, g.SubjectRepo, g.SubjectNumber, pr.Merged, pr.HeadSHA))
		}
	}
	return errors.Join(errs...)
}

func (p *Poller) reconcileChange(ctx context.Context, c domain.Change) error {
	e := p.e
	pr, err := e.Tracker.GetPR(ctx, c.Repo, c.Number)
	if err != nil {
		return fmt.Errorf("get pull request: %w", err)
	}
	if pr.State == "closed" {
		return e.prClosed(ctx, c.AccountID, c.ProjectID, c.Repo, c.Number, pr.Merged, pr.HeadSHA)
	}
	if pr.HeadSHA != c.HeadSHA {
		c.HeadSHA = pr.HeadSHA
		if err := e.Store.Changes().Upsert(ctx, &c); err != nil {
			return fmt.Errorf("update change: %w", err)
		}
	}
	return e.checkChange(ctx, c)
}

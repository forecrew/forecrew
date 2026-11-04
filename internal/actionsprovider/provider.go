package actionsprovider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/queue"
	"github.com/forecrew/forecrew/internal/store"
)

// WorkerName is the name of the worker row forecrewd keeps, in pool
// actions, to hold the leases of the jobs it runs itself.
const WorkerName = "forecrewd-actions"

// Defaults of Config.
const (
	DefaultPollInterval = 15 * time.Second
	DefaultFindDelay    = 5 * time.Second
	// DefaultMaxRunTime bounds a job that sets no timeout; it is GitHub's
	// own limit for a job.
	DefaultMaxRunTime = 6 * time.Hour
	// findTries is how often the run of a dispatch is looked for when
	// GitHub did not return its id.
	findTries = 6
	// maxPollErrors is how many reads of a run may fail in a row before the
	// attempt is given up as an infrastructure problem.
	maxPollErrors = 5
)

// Config holds the dependencies of a Provider.
type Config struct {
	Store  store.Store
	Jobs   *jobs.Service
	GitHub github.Actions
	// Evidence stores the artifacts of a run; nil: they are not stored and
	// the job log says so.
	Evidence evidence.Store
	// MaxInFlight is the most runs followed at once (actions.max_in_flight);
	// it defaults to config.DefaultActionsMaxInFlight.
	MaxInFlight int
	// MaxArtifactSize is the largest artifact stored (evidence.max_artifact_size);
	// it defaults to config.DefaultMaxArtifactSize.
	MaxArtifactSize int64
	// PollInterval is how often a run, the queue and the lease are looked at
	// (a workflow_run webhook looks sooner); FindDelay is the wait before
	// looking for the run of a dispatch GitHub gave no id for. They default
	// to the constants.
	PollInterval time.Duration
	FindDelay    time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

// Provider runs the command jobs of pool actions on GitHub-hosted runners.
type Provider struct {
	cfg      Config
	inflight atomic.Int64
	freed    chan struct{}
	wg       sync.WaitGroup

	mu     sync.Mutex
	wakers map[int64]chan struct{}
}

// New returns a Provider.
func New(cfg Config) *Provider {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = config.DefaultActionsMaxInFlight
	}
	if cfg.MaxArtifactSize <= 0 {
		cfg.MaxArtifactSize = config.DefaultMaxArtifactSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.FindDelay <= 0 {
		cfg.FindDelay = DefaultFindDelay
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Provider{cfg: cfg, freed: make(chan struct{}, 1), wakers: map[int64]chan struct{}{}}
}

// Run leases and follows jobs until ctx ends, then cancels the runs it still
// follows (their jobs go back to the queue when their lease expires) and
// returns.
func (p *Provider) Run(ctx context.Context) {
	for {
		wake := p.cfg.Jobs.Wait() // before leasing, so no new job is missed
		p.Poll(ctx)
		t := time.NewTimer(p.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			p.wg.Wait()
			return
		case <-wake:
		case <-p.freed:
		case <-t.C:
		}
		t.Stop()
	}
}

// Poll leases queued jobs of pool actions, in every account, while fewer
// than MaxInFlight runs are followed, and starts following them. Accounts
// take turns, one job each, so one account cannot fill every slot while
// another waits.
func (p *Provider) Poll(ctx context.Context) {
	accounts, err := p.cfg.Store.Accounts().List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.cfg.Logger.ErrorContext(ctx, "list accounts", "error", err)
		}
		return
	}
	var ready []domain.Worker
	for _, a := range accounts {
		w, err := p.worker(ctx, a.ID)
		if err != nil {
			if ctx.Err() == nil {
				p.cfg.Logger.ErrorContext(ctx, "actions worker", "account_id", a.ID, "error", err)
			}
			continue
		}
		ready = append(ready, w)
	}
	for progress := true; progress && p.room(); {
		progress = false
		for _, w := range ready {
			if p.room() && p.leaseOne(ctx, w) {
				progress = true
			}
		}
	}
}

// room reports whether another run may be followed.
func (p *Provider) room() bool { return p.inflight.Load() < int64(p.cfg.MaxInFlight) }

// leaseOne leases the oldest queued job of w's account and starts following
// it. It reports whether it did.
func (p *Provider) leaseOne(ctx context.Context, w domain.Worker) bool {
	j, err := queue.New(p.cfg.Store.Jobs()).Lease(ctx, w.AccountID, w.ID, nil)
	if errors.Is(err, store.ErrNoJob) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			p.cfg.Logger.ErrorContext(ctx, "lease job", "account_id", w.AccountID, "error", err)
		}
		return false
	}
	if j.Pool != domain.PoolActions || j.Kind.OrDefault() != domain.JobKindCommand {
		// The store leases by the worker's pool as it is in the database;
		// never run what is not ours.
		p.cfg.Logger.ErrorContext(ctx, "leased a job of another pool, giving it back", "job_id", j.ID, "pool", j.Pool, "kind", j.Kind)
		if _, err := p.cfg.Jobs.Release(context.WithoutCancel(ctx), w, j.ID, "leased by the Actions provider by mistake"); err != nil {
			p.cfg.Logger.ErrorContext(ctx, "release job", "job_id", j.ID, "error", err)
		}
		return false
	}
	p.inflight.Add(1)
	p.wg.Go(func() {
		defer func() {
			p.inflight.Add(-1)
			select {
			case p.freed <- struct{}{}:
			default:
			}
		}()
		p.runJob(ctx, w, j)
	})
	return true
}

// Wait blocks until every run being followed has ended; Poll starts no new
// ones meanwhile only if its context is done.
func (p *Provider) Wait() { p.wg.Wait() }

// providerVersion marks the worker row the provider created; no worker API
// client reports it.
const providerVersion = "forecrewd-actions-provider"

// worker returns the account's worker row of pool actions, creating it on
// first use, and records it as seen so that queued jobs of the pool do not
// look like they wait for a worker. It reads the row every time: LeaseNext
// follows the pool stored in the database. Its token hash is random and its
// token is never issued, so it cannot call the worker API. A row of that
// name the provider did not create, or one that left the pool, is an error.
func (p *Provider) worker(ctx context.Context, account uuid.UUID) (domain.Worker, error) {
	list, err := p.cfg.Store.Workers().List(ctx, account)
	if err != nil {
		return domain.Worker{}, fmt.Errorf("list workers: %w", err)
	}
	i := slices.IndexFunc(list, func(w domain.Worker) bool { return w.Name == WorkerName })
	if i < 0 {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return domain.Worker{}, fmt.Errorf("make worker secret: %w", err)
		}
		w := domain.Worker{
			AccountID: account, Name: WorkerName, Pool: domain.PoolActions, TokenHash: mcp.HashToken(string(secret[:])),
			Version: providerVersion, MaxJobs: p.cfg.MaxInFlight,
		}
		if err := p.cfg.Store.Workers().Create(ctx, &w); err != nil {
			return domain.Worker{}, fmt.Errorf("create worker %s: %w", WorkerName, err)
		}
		list = append(list, w)
		i = len(list) - 1
	}
	w := list[i]
	switch {
	case w.Version != providerVersion:
		return domain.Worker{}, fmt.Errorf("worker %s exists but the provider did not create it: revoke or rename it", WorkerName)
	case w.Revoked():
		return domain.Worker{}, fmt.Errorf("worker %s is revoked", WorkerName)
	case w.Pool != domain.PoolActions:
		return domain.Worker{}, fmt.Errorf("worker %s is in pool %s, not %s", WorkerName, w.Pool, domain.PoolActions)
	}
	if err := p.cfg.Store.Workers().Seen(ctx, account, w.ID, nil, providerVersion); err != nil {
		return domain.Worker{}, fmt.Errorf("record worker %s as seen: %w", WorkerName, err)
	}
	return w, nil
}

// Notify tells the provider that GitHub reported a change of run id (a
// workflow_run webhook), so it reads the run now instead of at the next poll.
func (p *Provider) Notify(runID int64) {
	p.mu.Lock()
	ch := p.wakers[runID]
	p.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// watch registers a wake-up channel for a run; the returned func removes it.
func (p *Provider) watch(runID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.wakers[runID] = ch
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.wakers, runID)
		p.mu.Unlock()
	}
}

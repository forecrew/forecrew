package fleetagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/retry"
)

// Timings of the agent.
const (
	// ReconcileEvery is how often the agent lists what its provider holds.
	ReconcileEvery = 5 * time.Minute
	backoffMin     = time.Second
	backoffMax     = 30 * time.Second
	// noRoomWait spaces polls while the provider has no room: forecrewd
	// answers at once for a machine the agent does not have, so polling
	// right away would spin.
	noRoomWait = 5 * time.Second
	// AbuseEvery is how often a provider that watches its machines for abuse
	// (fleet.AbuseChecker) looks.
	AbuseEvery = 30 * time.Second
)

// Config holds what an Agent needs.
type Config struct {
	Client   *Client
	Provider fleet.Provider
	Version  string
	Logger   *slog.Logger
	// Detect reports the capabilities; it defaults to Detect.
	Detect func(ctx context.Context) domain.ProviderCapabilities
	Now    func() time.Time
	// Images does the Windows image builds forecrewd asks for. Nil refuses them:
	// the agent is not set up to build (agent.yaml, image_build).
	Images ImageBuilder
	// The timings default to the constants above.
	ReconcileEvery         time.Duration
	BackoffMin, BackoffMax time.Duration
	NoRoomWait             time.Duration
	AbuseEvery             time.Duration
}

// machine is one machine the agent has or is starting.
type machine struct {
	id         uuid.UUID
	externalID string // empty until Create returns
	// busy means Create, Update or Destroy is running on it, so nothing else
	// may start.
	busy bool
	// applied is what Update or Create last gave it; empty after a restart,
	// which applies the values once.
	applied  string
	deadline time.Time
}

// Agent keeps a provider in line with forecrewd. Everything it knows can be
// rebuilt from the provider and forecrewd, so nothing is written to disk.
type Agent struct {
	cfg Config

	// ops keeps a reconcile from seeing a provider in the middle of a change:
	// a machine Create just started would be missing from a list taken before
	// it and reported after it, and forecrewd would fail it as gone. Create and
	// Destroy hold it shared, reconcile alone.
	ops sync.RWMutex

	mu       sync.Mutex
	machines map[uuid.UUID]*machine
	// pending are reports forecrewd did not get yet, sent again until it does.
	pending map[uuid.UUID]StateReport
	noRoom  bool
	// builds are the image builds the agent took or answered, until forecrewd
	// stops listing them.
	builds map[uuid.UUID]struct{}

	generation string
	registered domain.ProviderCapabilities
	wg         sync.WaitGroup
}

// New returns an agent.
func New(cfg Config) *Agent {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Detect == nil {
		cfg.Detect = func(ctx context.Context) domain.ProviderCapabilities { return Detect(ctx, cfg.Provider, cfg.Version) }
	}
	if cfg.ReconcileEvery == 0 {
		cfg.ReconcileEvery = ReconcileEvery
	}
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = backoffMin
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = backoffMax
	}
	if cfg.NoRoomWait == 0 {
		cfg.NoRoomWait = noRoomWait
	}
	if cfg.AbuseEvery == 0 {
		cfg.AbuseEvery = AbuseEvery
	}
	return &Agent{cfg: cfg, machines: map[uuid.UUID]*machine{}, pending: map[uuid.UUID]StateReport{}, builds: map[uuid.UUID]struct{}{}}
}

// Run works until ctx ends, and returns nil then. While forecrewd cannot be
// reached the machines keep running, nothing new starts, and deadlines already
// known still apply. A 401 ends it with ErrRejected: a revoked provider gets no
// more demand, and retrying would not change that.
func (a *Agent) Run(ctx context.Context) error {
	defer a.wg.Wait()
	failures := 0
	for {
		err := a.session(ctx, func() {
			if failures > 0 {
				a.cfg.Logger.InfoContext(ctx, "connected to forecrewd again")
			}
			failures = 0
		})
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		failures++
		if failures == 1 {
			a.cfg.Logger.WarnContext(ctx, "no contact with forecrewd: machines keep running and nothing new starts", "error", clean(err))
		}
		a.expire(ctx)
		if retry.Sleep(ctx, retry.Backoff(failures, a.cfg.BackoffMin, a.cfg.BackoffMax)) != nil {
			return nil
		}
	}
}

func clean(err error) string { return fleet.CleanError(err.Error()) }

// session is one connection: register, reconcile, then poll until something
// fails. connected is called each time forecrewd answered a poll.
func (a *Agent) session(ctx context.Context, connected func()) error {
	if err := a.register(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	last := a.cfg.Now()
	var checked time.Time
	for {
		if err := a.flush(ctx); err != nil {
			return err
		}
		if a.cfg.Now().Sub(checked) >= a.cfg.AbuseEvery {
			a.checkAbuse(ctx)
			checked = a.cfg.Now()
		}
		// A slow Create must not stop the polls: with one running the
		// reconcile waits for a later round.
		if a.cfg.Now().Sub(last) >= a.cfg.ReconcileEvery && a.ops.TryLock() {
			if err := a.refresh(ctx); err != nil {
				return err
			}
			last = a.cfg.Now()
		}
		if a.takeNoRoom() {
			if err := retry.Sleep(ctx, a.cfg.NoRoomWait); err != nil {
				return err
			}
		}
		answer, err := a.cfg.Client.Poll(ctx, a.generation, a.have())
		if err != nil {
			return err
		}
		connected()
		a.generation = answer.Generation
		a.apply(ctx, answer.Machines)
		a.applyBuilds(ctx, answer.ImageBuilds)
	}
}

func (a *Agent) register(ctx context.Context) error {
	capabilities := a.cfg.Detect(ctx)
	r, err := a.cfg.Client.Register(ctx, capabilities)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	a.registered = capabilities
	a.cfg.Logger.InfoContext(ctx, "registered", "provider", r.Name, "kind", r.Kind, "status", r.Status)
	return nil
}

// refresh is the periodic work: new capabilities and a reconcile. The caller
// holds ops, which it gives up.
func (a *Agent) refresh(ctx context.Context) error {
	if !reflect.DeepEqual(a.cfg.Detect(ctx), a.registered) {
		if err := a.register(ctx); err != nil {
			a.ops.Unlock()
			return err
		}
	}
	return a.reconcile(ctx)
}

// reconcile tells forecrewd what the provider holds, learns what the agent
// lost in a restart, and destroys what forecrewd disowns. A failure of the
// provider says nothing about its machines, so it changes nothing here. The
// caller holds ops, which it gives up.
func (a *Agent) reconcile(ctx context.Context) error {
	instances, destroy, err := a.compare(ctx)
	a.ops.Unlock()
	if err != nil {
		return err
	}
	for _, external := range destroy {
		// forecrewd says what to destroy, but only among what the agent listed:
		// it can never name something else this provider's credentials reach.
		i := slices.IndexFunc(instances, func(x fleet.Instance) bool { return x.ExternalID == external })
		if i < 0 {
			a.cfg.Logger.WarnContext(ctx, "forecrewd named a machine the provider did not list; leaving it alone", "external_id", external)
			continue
		}
		a.destroy(ctx, instances[i].MachineID, external)
	}
	return nil
}

func (a *Agent) compare(ctx context.Context) (instances []fleet.Instance, destroy []string, err error) {
	if instances, err = a.cfg.Provider.List(ctx); err != nil {
		return nil, nil, fmt.Errorf("list machines: %w", err)
	}
	if destroy, err = a.cfg.Client.Reconcile(ctx, instances); err != nil {
		return nil, nil, fmt.Errorf("reconcile: %w", err)
	}
	a.adopt(instances)
	return instances, destroy, nil
}

// adopt makes the list of machines what the provider says: one the agent lost
// in a restart is its own again, and one that is gone is gone.
func (a *Agent) adopt(instances []fleet.Instance) {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := map[uuid.UUID]bool{}
	for _, inst := range instances {
		if inst.MachineID == uuid.Nil() {
			continue
		}
		held[inst.MachineID] = true
		if m, ok := a.machines[inst.MachineID]; ok {
			m.externalID = inst.ExternalID
			continue
		}
		a.machines[inst.MachineID] = &machine{id: inst.MachineID, externalID: inst.ExternalID}
	}
	for id, m := range a.machines {
		if !held[id] && !m.busy {
			delete(a.machines, id)
		}
	}
}

// have lists every machine the agent has or is starting.
func (a *Agent) have() []uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uuid.UUID{}, slices.Collect(maps.Keys(a.machines))...)
}

func (a *Agent) takeNoRoom() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	was := a.noRoom
	a.noRoom = false
	return was
}

// apply acts on the whole desired list. A machine in the middle of something
// waits for the next poll, which comes within the poll's wait. A machine in the
// list that the agent neither has nor gets a token for is forecrewd's to sort
// out: the next reconcile fails it as gone.
func (a *Agent) apply(ctx context.Context, desired []fleet.CreateRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	wanted := map[uuid.UUID]bool{}
	for _, req := range desired {
		wanted[req.MachineID] = true
		m, ok := a.machines[req.MachineID]
		switch {
		case !ok && req.Bootstrap != nil:
			a.create(ctx, req)
		case ok && !m.busy:
			m.deadline = req.Deadline
			a.update(ctx, m, fleet.UpdateRequest{Tags: req.Tags, NetworkAllow: req.NetworkAllow, Deadline: req.Deadline})
		}
	}
	for id, m := range a.machines {
		if !wanted[id] && !m.busy {
			m.busy = true
			a.wg.Go(func() { a.destroyMachine(ctx, m) })
		}
	}
}

// create starts req in the background: a slow Create must not stop the polls
// that keep this provider online. It lists the machine at once, which is what
// keeps forecrewd from replacing its token. The caller holds mu.
func (a *Agent) create(ctx context.Context, req fleet.CreateRequest) {
	key := fleet.UpdateRequest{Tags: req.Tags, NetworkAllow: req.NetworkAllow, Deadline: req.Deadline}.Key()
	m := &machine{id: req.MachineID, busy: true, deadline: req.Deadline}
	a.machines[m.id] = m
	delete(a.pending, m.id) // a report of an earlier try is out of date
	a.wg.Go(func() {
		a.ops.RLock()
		inst, err := a.cfg.Provider.Create(ctx, req)
		a.ops.RUnlock()
		a.created(ctx, m, inst, key, err)
	})
}

// created records how Create went. Dropping a machine from have is what brings
// it back with a new token.
func (a *Agent) created(ctx context.Context, m *machine, inst fleet.Instance, key string, err error) {
	a.mu.Lock()
	if err != nil {
		delete(a.machines, m.id)
		a.noRoom = a.noRoom || errors.Is(err, fleet.ErrCapacity)
	} else {
		m.externalID, m.applied, m.busy = inst.ExternalID, key, false
	}
	a.mu.Unlock()
	switch {
	case err == nil:
		a.cfg.Logger.InfoContext(ctx, "machine created", "machine_id", m.id, "external_id", inst.ExternalID)
		a.report(ctx, m.id, StateReport{State: domain.MachineBooting, ExternalID: inst.ExternalID})
	case errors.Is(err, fleet.ErrCapacity):
		a.cfg.Logger.InfoContext(ctx, "no room for a machine", "machine_id", m.id)
	case ctx.Err() != nil: // stopping: the next start looks at what exists
	default:
		why := "create: " + clean(err)
		a.cfg.Logger.WarnContext(ctx, "create machine", "machine_id", m.id, "error", why)
		a.report(ctx, m.id, StateReport{State: domain.MachineFailed, Error: why})
	}
}

// update applies new values in the background; a failure is tried again on the
// next poll and does not fail the machine. The caller holds mu.
func (a *Agent) update(ctx context.Context, m *machine, u fleet.UpdateRequest) {
	key := u.Key()
	if key == m.applied {
		return
	}
	m.busy = true
	external := m.externalID
	a.wg.Go(func() {
		err := a.cfg.Provider.Update(ctx, external, u)
		a.mu.Lock()
		defer a.mu.Unlock()
		m.busy = false
		if err != nil {
			a.cfg.Logger.WarnContext(ctx, "update machine", "machine_id", m.id, "error", clean(err))
			return
		}
		m.applied = key
	})
}

// destroy removes what forecrewd disowned, known to the agent or not.
func (a *Agent) destroy(ctx context.Context, id uuid.UUID, external string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.machines[id]
	if ok && m.busy {
		return // the next reconcile looks again
	}
	if !ok {
		m = &machine{id: id, externalID: external}
	}
	m.busy = true
	a.wg.Go(func() { a.destroyMachine(ctx, m) })
}

// destroyMachine destroys m and reports it, or leaves it to be tried again.
func (a *Agent) destroyMachine(ctx context.Context, m *machine) {
	a.ops.RLock()
	external := m.externalID
	err := a.cfg.Provider.Destroy(ctx, external)
	a.ops.RUnlock()
	a.mu.Lock()
	m.busy = false
	if err == nil {
		delete(a.machines, m.id)
	}
	a.mu.Unlock()
	if err != nil {
		a.cfg.Logger.WarnContext(ctx, "destroy machine", "machine_id", m.id, "external_id", external, "error", clean(err))
		return
	}
	a.cfg.Logger.InfoContext(ctx, "machine destroyed", "machine_id", m.id, "external_id", external)
	if m.id != uuid.Nil() {
		a.report(ctx, m.id, StateReport{State: domain.MachineDestroyed})
	}
}

// expire applies the deadlines the agent knows, for when forecrewd cannot say
// so itself. It is the only thing destroyed while offline.
func (a *Agent) expire(ctx context.Context) {
	now := a.cfg.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.machines {
		if !m.busy && !m.deadline.IsZero() && !now.Before(m.deadline) {
			a.cfg.Logger.InfoContext(ctx, "machine past its deadline", "machine_id", m.id)
			m.busy = true
			a.wg.Go(func() { a.destroyMachine(ctx, m) })
		}
	}
}

// report sends r now, or keeps it for flush. A report forecrewd refuses is
// dropped: the machine moved on, and the next poll or reconcile says how.
func (a *Agent) report(ctx context.Context, id uuid.UUID, r StateReport) {
	err := a.cfg.Client.State(ctx, id, r)
	if err == nil {
		return
	}
	if !retryable(err) {
		a.cfg.Logger.DebugContext(ctx, "report refused", "machine_id", id, "state", r.State, "error", clean(err))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// A failed Create delivered again since has a new life this report is
	// not about.
	if _, again := a.machines[id]; r.State != domain.MachineFailed || !again {
		a.pending[id] = r
	}
}

// flush sends the reports forecrewd has not got. The first failure ends it:
// forecrewd is away, and the rest would fail the same way.
func (a *Agent) flush(ctx context.Context) error {
	a.mu.Lock()
	pending := maps.Clone(a.pending)
	a.mu.Unlock()
	for id, r := range pending {
		err := a.cfg.Client.State(ctx, id, r)
		if err != nil && retryable(err) {
			return fmt.Errorf("report machine: %w", err)
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		a.mu.Lock()
		if a.pending[id] == r {
			delete(a.pending, id)
		}
		a.mu.Unlock()
	}
	return nil
}

// checkAbuse asks a provider that watches its machines which it stopped for
// abuse, and reports each as failed with its kind: forecrewd cancels its job
// and tells the operator. The machine leaves the desired list and is destroyed
// as any other.
func (a *Agent) checkAbuse(ctx context.Context) {
	w, ok := a.cfg.Provider.(fleet.AbuseChecker)
	if !ok {
		return
	}
	found, err := w.CheckAbuse(ctx)
	if err != nil {
		a.cfg.Logger.WarnContext(ctx, "check machines for abuse", "error", clean(err))
	}
	for _, r := range found {
		a.cfg.Logger.WarnContext(ctx, "machine stopped for abuse", "machine_id", r.MachineID, "kind", r.Kind)
		if r.MachineID != uuid.Nil() {
			a.report(ctx, r.MachineID, StateReport{State: domain.MachineFailed, Error: fleet.CleanError(r.Detail), Abuse: r.Kind})
		}
	}
}

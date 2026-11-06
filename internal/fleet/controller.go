package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Timings of the controller.
const (
	// ReconcileInterval is how often Run reconciles without a wake-up. It
	// is well below domain.OnlineWindow, which direct providers must meet.
	ReconcileInterval = 10 * time.Second
	// maxErrorLength bounds a machine's error (fleet-agent-api.md).
	maxErrorLength = 2000
	// settle lets a burst of wake-ups end in one reconcile.
	settle = time.Second
)

// Audit actions of the controller.
const (
	ActionCreated   = "machine.created"
	ActionAssigned  = "machine.assigned"
	ActionFailed    = "machine.failed"
	ActionDestroyed = "machine.destroyed"
	ActionOrphan    = "machine.orphan_destroyed"
)

// Config holds what a Controller needs.
type Config struct {
	Store  store.Store
	Logger *slog.Logger
	Now    func() time.Time
	// Direct returns the provider of a direct-mode row, nil when forecrewd
	// has none for it. Nil serves no direct provider.
	Direct func(domain.CapacityProvider) Provider
	// Image resolves a shape's image for an organization. Nil sends only
	// what the reference says, until images are stored (T60).
	Image func(ctx context.Context, p domain.CapacityProvider, s domain.Shape, accountID uuid.UUID) (domain.Image, error)
	// ServerURL is what workers on machines dial.
	ServerURL string
	// Currency is config.md fleet.currency.
	Currency string
}

// Controller makes machines follow queue demand (fleet.md). Everything it
// knows is in the store, so a new Controller after a crash goes on where the
// last one stopped: a machine's row is written before its provider hears of
// it, and Provider.Create is idempotent for the row's ID.
type Controller struct {
	cfg Config

	mu sync.Mutex // one Reconcile at a time
	// applied holds what each direct machine last got from Update. After a
	// restart it is empty, and Update allows applying the same values again.
	applied map[uuid.UUID]string
}

// New returns a controller.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Direct == nil {
		cfg.Direct = func(domain.CapacityProvider) Provider { return nil }
	}
	return &Controller{cfg: cfg, applied: map[uuid.UUID]string{}}
}

// Run reconciles every interval and when wake's channel closes, until ctx
// ends. Errors are logged.
func (c *Controller) Run(ctx context.Context, every time.Duration, wake func() <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		woken := wake() // before reconciling, so no job is missed
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			c.cfg.Logger.ErrorContext(ctx, "reconcile fleet", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-woken:
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-time.After(min(settle, every)):
			}
		}
	}
}

// Reconcile compares what should exist with what does and acts once. An
// error of one provider or machine does not stop the others; they are logged
// and returned joined.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	providers, err := c.cfg.Store.CapacityProviders().ListAll(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	if len(providers) == 0 {
		return nil
	}
	var errs []error
	reachable := map[uuid.UUID]Provider{}
	for _, p := range providers {
		if d := c.direct(p); d != nil {
			if err := c.sync(ctx, p, d); err != nil {
				errs = append(errs, fmt.Errorf("provider %s: %w", p.Name, err))
				continue
			}
			reachable[p.ID] = d
		}
	}
	s, err := c.load(ctx, uuid.Nil())
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	errs = append(errs, c.apply(ctx, s, s.plan())...)
	for _, p := range providers {
		if d := reachable[p.ID]; d != nil {
			errs = append(errs, c.actuate(ctx, s.providers[p.ID], d)...)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) direct(p domain.CapacityProvider) Provider {
	if p.Mode != domain.ProviderModeDirect || p.Status == domain.ProviderRevoked {
		return nil
	}
	return c.cfg.Direct(p)
}

// sync does for a direct provider what a fleet agent's register and
// reconcile do: records the contact and destroys orphans.
func (c *Controller) sync(ctx context.Context, p domain.CapacityProvider, d Provider) error {
	capabilities, err := d.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("capabilities: %w", err)
	}
	if err := c.cfg.Store.CapacityProviders().Seen(ctx, p.ID, &capabilities); err != nil {
		return fmt.Errorf("record contact: %w", err)
	}
	instances, err := d.List(ctx)
	if err != nil {
		return fmt.Errorf("list machines: %w", err)
	}
	orphans, err := c.ReconcileInstances(ctx, p, instances)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, o := range orphans {
		if err := d.Destroy(ctx, o.ExternalID); err != nil {
			errs = append(errs, fmt.Errorf("destroy orphan %s: %w", o.ExternalID, err))
			continue
		}
		if err := c.auditOrphan(ctx, p, o); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Orphan is something a provider holds that should not exist. Unknown means
// no machine row ever knew it; the row of a known one is finished the usual
// way once it is gone. AccountID is its organization when that is known.
type Orphan struct {
	ExternalID string
	AccountID  uuid.UUID
	MachineID  uuid.UUID
	Unknown    bool
}

// ReconcileInstances compares what provider p lists with its machines and
// returns what it must destroy (fleet-agent-api.md, reconcile): instances with
// no machine, or with one destroyed, failed or past its deadline. A machine
// the provider started before a crash moves to booting; one that was up and is
// gone from the list fails.
func (c *Controller) ReconcileInstances(ctx context.Context, p domain.CapacityProvider, instances []Instance) ([]Orphan, error) {
	active, err := c.cfg.Store.Machines().List(ctx, store.MachineFilter{ProviderID: p.ID, Active: true})
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	now := c.cfg.Now()
	var orphans []Orphan
	var errs []error
	seen := map[uuid.UUID]bool{}
	for _, inst := range instances {
		m, err := c.machineOf(ctx, p, inst)
		switch {
		case errors.Is(err, store.ErrNotFound):
			orphans = append(orphans, Orphan{ExternalID: inst.ExternalID, AccountID: tagOrg(p, inst), MachineID: inst.MachineID, Unknown: true})
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		seen[m.ID] = true
		switch {
		case !m.State.Counts(), !m.DeadlineAt.IsZero() && !now.Before(m.DeadlineAt):
			orphans = append(orphans, Orphan{ExternalID: inst.ExternalID, AccountID: m.AccountID, MachineID: m.ID})
		case m.State == domain.MachineRequested:
			if err := c.started(ctx, m, inst.ExternalID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, m := range active {
		if !seen[m.ID] && m.State != domain.MachineRequested {
			if err := c.failMachine(ctx, m, "gone from the provider"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return orphans, errors.Join(errs...)
}

// machineOf finds the machine of an instance by its tag or external ID. A
// machine of another provider is not one of this provider's.
func (c *Controller) machineOf(ctx context.Context, p domain.CapacityProvider, inst Instance) (domain.Machine, error) {
	if inst.MachineID == uuid.Nil() {
		return c.cfg.Store.Machines().GetByExternalID(ctx, p.ID, inst.ExternalID)
	}
	m, err := c.cfg.Store.Machines().Get(ctx, inst.MachineID)
	if err == nil && m.ProviderID != p.ID {
		return m, store.ErrNotFound
	}
	return m, err
}

// tagOrg returns the organization an unknown instance belongs to: its
// provider's, or what its tag says on an instance-wide provider.
func tagOrg(p domain.CapacityProvider, inst Instance) uuid.UUID {
	if p.AccountID != uuid.Nil() {
		return p.AccountID
	}
	id, err := uuid.Parse(inst.Tags[TagOrg])
	if err != nil {
		return uuid.Nil()
	}
	return id
}

// started records that the provider started machine m.
func (c *Controller) started(ctx context.Context, m domain.Machine, externalID string) error {
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().SetExternalID(ctx, m.ID, externalID); err != nil {
			return fmt.Errorf("machine %s: %w", m.ID, err)
		}
		if err := tx.Machines().UpdateState(ctx, m.ID, domain.MachineRequested, domain.MachineBooting, ""); err != nil {
			return fmt.Errorf("machine %s: %w", m.ID, err)
		}
		return nil
	})
}

// load reads a snapshot of the organization only, or of all of them. Budgets
// and warm machines cross organizations, so machines are always all read.
func (c *Controller) load(ctx context.Context, only uuid.UUID) (*snapshot, error) {
	st := c.cfg.Store
	s := &snapshot{now: c.cfg.Now(), currency: c.cfg.Currency, providers: map[uuid.UUID]domain.CapacityProvider{}, shapes: map[uuid.UUID]domain.Shape{}, jobEnds: map[uuid.UUID]time.Time{}}
	if err := c.loadFleet(ctx, s); err != nil {
		return nil, err
	}
	accounts, err := st.Accounts().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	for _, a := range accounts {
		if only != uuid.Nil() && a.ID != only {
			continue
		}
		o, err := c.loadOrg(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("organization %s: %w", a.ID, err)
		}
		for _, sh := range o.shapes {
			s.shapes[sh.ID] = sh
		}
		s.orgs = append(s.orgs, o)
	}
	return s, c.loadMachines(ctx, s)
}

// loadFleet reads the providers and the instance-wide shapes.
func (c *Controller) loadFleet(ctx context.Context, s *snapshot) error {
	providers, err := c.cfg.Store.CapacityProviders().ListAll(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	for _, p := range providers {
		s.providers[p.ID] = p
	}
	hosted, err := c.cfg.Store.Shapes().List(ctx, uuid.Nil())
	if err != nil {
		return fmt.Errorf("list shapes: %w", err)
	}
	for _, sh := range hosted {
		s.shapes[sh.ID] = sh
	}
	return nil
}

// loadMachines reads this month's machines and when busy ones last finished.
func (c *Controller) loadMachines(ctx context.Context, s *snapshot) error {
	var err error
	if s.machines, err = c.cfg.Store.Machines().List(ctx, store.MachineFilter{Since: monthStart(s.now)}); err != nil {
		return fmt.Errorf("list machines: %w", err)
	}
	slices.Reverse(s.machines) // oldest first
	for _, m := range s.machines {
		if m.State != domain.MachineBusy || m.JobID == uuid.Nil() {
			continue
		}
		j, err := c.cfg.Store.Jobs().Get(ctx, m.AccountID, m.JobID)
		if err != nil {
			return fmt.Errorf("job of machine %s: %w", m.ID, err)
		}
		s.jobEnds[m.ID] = j.FinishedAt
		if j.FinishedAt.IsZero() {
			s.jobEnds[m.ID] = m.UpdatedAt // given back or requeued
		}
	}
	return nil
}

func (c *Controller) loadOrg(ctx context.Context, id uuid.UUID) (organization, error) {
	st := c.cfg.Store
	o := organization{id: id}
	var err error
	if o.policy, err = st.FleetPolicies().Get(ctx, id); err != nil {
		return o, fmt.Errorf("policy: %w", err)
	}
	if o.shapes, err = st.Shapes().ListAvailable(ctx, id); err != nil {
		return o, fmt.Errorf("shapes: %w", err)
	}
	if o.queued, err = st.Jobs().List(ctx, id, store.JobFilter{Status: domain.JobQueued}); err != nil {
		return o, fmt.Errorf("queued jobs: %w", err)
	}
	slices.Reverse(o.queued) // List is newest first
	if o.running, err = st.Jobs().List(ctx, id, store.JobFilter{Status: domain.JobRunning}); err != nil {
		return o, fmt.Errorf("running jobs: %w", err)
	}
	if o.workers, err = st.Workers().List(ctx, id); err != nil {
		return o, fmt.Errorf("workers: %w", err)
	}
	return o, nil
}

// apply writes a plan to the store. A machine that changed since the
// snapshot lost its plan: the next pass looks again.
func (c *Controller) apply(ctx context.Context, s *snapshot, p *plan) []error {
	var errs []error
	keep := func(err error) {
		if err != nil && !errors.Is(err, store.ErrConflict) {
			errs = append(errs, err)
		}
	}
	for _, t := range p.fail {
		keep(c.failMachine(ctx, t.machine, t.why))
	}
	for _, t := range p.drain {
		keep(c.drainMachine(ctx, t.machine, t.why))
	}
	for _, m := range p.withdraw {
		keep(c.finish(ctx, m, domain.MachineRequested, "no longer needed"))
	}
	for _, a := range p.assign {
		keep(c.assignMachine(ctx, a))
	}
	for i := range p.create {
		keep(c.createMachine(ctx, s, &p.create[i]))
	}
	return errs
}

func (c *Controller) createMachine(ctx context.Context, s *snapshot, m *domain.Machine) error {
	shape := s.shapes[m.ShapeID]
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().Create(ctx, m); err != nil {
			return fmt.Errorf("create machine of %s: %w", shape.Name, err)
		}
		return c.audit(ctx, tx, *m, ActionCreated, map[string]any{"shape": shape.Name, "provider": s.providers[m.ProviderID].Name})
	})
}

func (c *Controller) assignMachine(ctx context.Context, a assignment) error {
	m := a.machine
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().Assign(ctx, m.ID, a.org); err != nil {
			return fmt.Errorf("assign machine %s: %w", m.ID, err)
		}
		// A machine not delivered yet has no token; it gets one with the
		// organization when it is.
		err := tx.BootstrapTokens().Assign(ctx, m.ID, a.org, a.expires)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("assign token of machine %s: %w", m.ID, err)
		}
		m.AccountID = a.org
		return c.audit(ctx, tx, m, ActionAssigned, nil)
	})
}

// failMachine fails m and revokes its worker. Its job is requeued by its
// lease, as for any worker.
func (c *Controller) failMachine(ctx context.Context, m domain.Machine, why string) error {
	if len(why) > maxErrorLength {
		why = why[:maxErrorLength]
	}
	c.cfg.Logger.WarnContext(ctx, "machine failed", "machine_id", m.ID, "error", why)
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().UpdateState(ctx, m.ID, m.State, domain.MachineFailed, why); err != nil {
			return fmt.Errorf("fail machine %s: %w", m.ID, err)
		}
		if err := revokeWorker(ctx, tx, m); err != nil {
			return err
		}
		return c.audit(ctx, tx, m, ActionFailed, map[string]any{"error": why})
	})
}

// drainMachine stops m taking jobs and revokes its worker.
func (c *Controller) drainMachine(ctx context.Context, m domain.Machine, why string) error {
	c.cfg.Logger.InfoContext(ctx, "machine draining", "machine_id", m.ID, "why", why)
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().UpdateState(ctx, m.ID, m.State, domain.MachineDraining, ""); err != nil {
			return fmt.Errorf("drain machine %s: %w", m.ID, err)
		}
		return revokeWorker(ctx, tx, m)
	})
}

func revokeWorker(ctx context.Context, tx store.Store, m domain.Machine) error {
	if m.WorkerID == uuid.Nil() {
		return nil
	}
	if err := tx.Workers().Revoke(ctx, m.AccountID, m.WorkerID); err != nil {
		return fmt.Errorf("revoke worker of machine %s: %w", m.ID, err)
	}
	return nil
}

// finish moves m, gone from its provider, to destroyed.
func (c *Controller) finish(ctx context.Context, m domain.Machine, from domain.MachineState, why string) error {
	delete(c.applied, m.ID)
	return c.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Machines().UpdateState(ctx, m.ID, from, domain.MachineDestroyed, ""); err != nil {
			return fmt.Errorf("finish machine %s: %w", m.ID, err)
		}
		return c.audit(ctx, tx, m, ActionDestroyed, map[string]any{"why": why, "jobs_taken": m.JobsTaken})
	})
}

// audit records what happened to m in its organization's log. An unassigned
// machine has no organization, and the log has no place without one, so it
// goes to the server log.
func (c *Controller) audit(ctx context.Context, tx store.Store, m domain.Machine, action string, data map[string]any) error {
	if m.AccountID == uuid.Nil() {
		c.cfg.Logger.InfoContext(ctx, action, "machine_id", m.ID, "provider_id", m.ProviderID, "shape_id", m.ShapeID)
		return nil
	}
	return appendAudit(ctx, tx, m.AccountID, m.ID, action, data)
}

func (c *Controller) auditOrphan(ctx context.Context, p domain.CapacityProvider, o Orphan) error {
	if !o.Unknown {
		return nil // its row records the end
	}
	if o.AccountID == uuid.Nil() {
		c.cfg.Logger.InfoContext(ctx, ActionOrphan, "provider_id", p.ID, "external_id", o.ExternalID)
		return nil
	}
	return appendAudit(ctx, c.cfg.Store, o.AccountID, o.MachineID, ActionOrphan, map[string]any{"provider": p.Name, "external_id": o.ExternalID})
}

func appendAudit(ctx context.Context, st store.Store, account, machine uuid.UUID, action string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode audit data: %w", err)
	}
	by := domain.SystemActor(account, domain.ForecrewdName)
	err = st.Audit().Append(ctx, &domain.AuditEntry{AccountID: account, Actor: by.String(), Action: action, SubjectType: "machine", SubjectID: machine, Data: raw})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// actuate makes direct provider d do what the machines' states ask.
func (c *Controller) actuate(ctx context.Context, p domain.CapacityProvider, d Provider) []error {
	machines, err := c.cfg.Store.Machines().List(ctx, store.MachineFilter{ProviderID: p.ID, Since: c.cfg.Now()})
	if err != nil {
		return []error{fmt.Errorf("provider %s: list machines: %w", p.Name, err)}
	}
	var errs []error
	for _, m := range slices.Backward(machines) {
		var err error
		switch m.State {
		case domain.MachineRequested:
			if p.CanStart(c.cfg.Now()) {
				err = c.start(ctx, p, d, m)
			}
		case domain.MachineDraining, domain.MachineFailed:
			err = c.destroy(ctx, d, m)
		case domain.MachineBooting, domain.MachineReady, domain.MachineBusy:
			err = c.update(ctx, p, d, m)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("provider %s: %w", p.Name, err))
		}
	}
	return errs
}

// start asks the provider for machine m with a new token. Only the hash is
// stored, so a retry always carries a new one. No room leaves m requested;
// any other error fails it, and demand replaces it.
func (c *Controller) start(ctx context.Context, p domain.CapacityProvider, d Provider, m domain.Machine) error {
	shape, err := c.cfg.Store.Shapes().Get(ctx, m.ShapeID)
	if err != nil {
		return fmt.Errorf("machine %s: shape: %w", m.ID, err)
	}
	image, err := c.image(ctx, p, shape, m.AccountID)
	if err != nil {
		return c.failMachine(ctx, m, "image: "+err.Error())
	}
	token, hash := newToken(prefixBootstrapToken)
	t := domain.BootstrapToken{MachineID: m.ID, AccountID: m.AccountID, TokenHash: hash}
	if m.AccountID != uuid.Nil() {
		t.ExpiresAt = c.cfg.Now().Add(time.Duration(shape.BootTimeoutSeconds) * time.Second)
	}
	if err := c.cfg.Store.BootstrapTokens().Issue(ctx, &t); err != nil {
		return fmt.Errorf("machine %s: issue token: %w", m.ID, err)
	}
	u, err := c.desired(ctx, p, m)
	if err != nil {
		return err
	}
	inst, err := d.Create(ctx, CreateRequest{
		MachineID: m.ID, Shape: shape, Image: image, Bootstrap: &Bootstrap{ServerURL: c.cfg.ServerURL, Token: token},
		Tags: u.Tags, NetworkAllow: u.NetworkAllow,
	})
	if errors.Is(err, ErrCapacity) {
		c.cfg.Logger.InfoContext(ctx, "provider has no room", "provider", p.Name, "machine_id", m.ID)
		return nil
	}
	if err != nil {
		return c.failMachine(ctx, m, "create: "+err.Error())
	}
	c.applied[m.ID] = appliedKey(u)
	return c.started(ctx, m, inst.ExternalID)
}

func (c *Controller) image(ctx context.Context, p domain.CapacityProvider, s domain.Shape, account uuid.UUID) (domain.Image, error) {
	if c.cfg.Image != nil {
		return c.cfg.Image(ctx, p, s, account)
	}
	name, version, _ := domain.ParseImageRef(s.Image)
	return domain.Image{Name: name, Version: version, OS: s.OS, Arch: s.Arch}, nil
}

// destroy removes m from its provider and finishes its row. A failed machine
// that never got an external ID has nothing to remove: sync destroyed what
// the provider listed for it.
func (c *Controller) destroy(ctx context.Context, d Provider, m domain.Machine) error {
	if m.ExternalID != "" {
		if err := d.Destroy(ctx, m.ExternalID); err != nil {
			return fmt.Errorf("destroy machine %s: %w", m.ID, err)
		}
	}
	return c.finish(ctx, m, m.State, string(m.State))
}

// update applies new tags, network and deadline to a running machine.
func (c *Controller) update(ctx context.Context, p domain.CapacityProvider, d Provider, m domain.Machine) error {
	if m.ExternalID == "" {
		return nil
	}
	u, err := c.desired(ctx, p, m)
	if err != nil {
		return err
	}
	key := appliedKey(u)
	if c.applied[m.ID] == key {
		return nil
	}
	if err := d.Update(ctx, m.ExternalID, u); err != nil {
		c.cfg.Logger.WarnContext(ctx, "update machine", "machine_id", m.ID, "error", err)
		return nil // tried again on the next pass; it does not fail the machine
	}
	c.applied[m.ID] = key
	return nil
}

// desired returns the tags, network and deadline machine m should have.
func (c *Controller) desired(ctx context.Context, p domain.CapacityProvider, m domain.Machine) (UpdateRequest, error) {
	tags := map[string]string{TagMachine: m.ID.String()}
	maps.Copy(tags, p.Tags)
	tags[TagMachine] = m.ID.String()
	network := slices.Clone(p.NetworkAllow)
	if m.AccountID != uuid.Nil() {
		tags[TagOrg] = m.AccountID.String()
		if p.Ownership() == domain.OwnershipHosted {
			policy, err := c.cfg.Store.FleetPolicies().Get(ctx, m.AccountID)
			if err != nil {
				return UpdateRequest{}, fmt.Errorf("machine %s: policy: %w", m.ID, err)
			}
			network = append(network, policy.Egress...)
		}
	}
	if m.JobID != uuid.Nil() {
		j, err := c.cfg.Store.Jobs().Get(ctx, m.AccountID, m.JobID)
		if err != nil {
			return UpdateRequest{}, fmt.Errorf("machine %s: job: %w", m.ID, err)
		}
		tags[TagJob] = j.Ref()
	}
	return UpdateRequest{Tags: tags, NetworkAllow: network, Deadline: m.DeadlineAt}, nil
}

// appliedKey is a value to compare two updates by.
func appliedKey(u UpdateRequest) string {
	keys := slices.Sorted(maps.Keys(u.Tags))
	var s strings.Builder
	s.WriteString(strconv.FormatInt(u.Deadline.UnixMilli(), 10))
	for _, k := range keys {
		s.WriteString("\x00" + k + "=" + u.Tags[k])
	}
	for _, n := range u.NetworkAllow {
		s.WriteString("\x00" + n)
	}
	return s.String()
}

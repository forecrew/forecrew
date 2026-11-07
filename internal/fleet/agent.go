package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrBadReport means an agent reported what it may not (fleet-agent-api.md,
// POST /fleet/v1/machines/{machine_id}/state).
var ErrBadReport = errors.New("bad machine report")

// Desired is what a fleet agent should hold: the answer of a poll
// (fleet-agent-api.md). Machines lack their shape, image and token until
// Deliver.
type Desired struct {
	// Generation changes when the list changes. It is a hash of the list, so
	// it needs no state and a restart of forecrewd costs nothing.
	Generation string
	Machines   []CreateRequest
	// missing holds the requested machines the agent does not have.
	missing map[uuid.UUID]bool
	rows    map[uuid.UUID]domain.Machine
}

// Undelivered reports a requested machine the agent does not have, which a
// poll answers at once.
func (d Desired) Undelivered() bool { return len(d.missing) > 0 }

// Desired reads what provider p should hold, given what its agent has. It
// changes nothing, so a waiting poll can call it as often as it likes.
func (c *Controller) Desired(ctx context.Context, p domain.CapacityProvider, have []uuid.UUID) (Desired, error) {
	machines, err := c.cfg.Store.Machines().List(ctx, store.MachineFilter{ProviderID: p.ID, Active: true})
	if err != nil {
		return Desired{}, fmt.Errorf("list machines: %w", err)
	}
	d := Desired{Machines: []CreateRequest{}, missing: map[uuid.UUID]bool{}, rows: map[uuid.UUID]domain.Machine{}}
	sum := sha256.New()
	for _, m := range slices.Backward(machines) { // oldest first
		if m.State == domain.MachineDraining || (m.State == domain.MachineRequested && p.Status != domain.ProviderActive) {
			continue
		}
		u, err := c.desired(ctx, p, m)
		if err != nil {
			return Desired{}, err
		}
		if u.NetworkAllow == nil {
			u.NetworkAllow = []string{}
		}
		d.Machines = append(d.Machines, CreateRequest{MachineID: m.ID, Tags: u.Tags, NetworkAllow: u.NetworkAllow, Deadline: u.Deadline})
		d.rows[m.ID] = m
		fmt.Fprintf(sum, "%s\x00%s\x01", m.ID, appliedKey(u))
		if m.State == domain.MachineRequested && !slices.Contains(have, m.ID) {
			d.missing[m.ID] = true
		}
	}
	d.Generation = hex.EncodeToString(sum.Sum(nil)[:8])
	return d, nil
}

// Deliver completes d for the agent of p: every machine gets its shape and
// image, and a machine the agent does not have gets a new token, which
// replaces its old one. Nothing else mints one, so a slow Create keeps its
// token. A machine that may not start after all leaves the list.
func (c *Controller) Deliver(ctx context.Context, p domain.CapacityProvider, d *Desired) error {
	kept := d.Machines[:0]
	for _, req := range d.Machines {
		shape, err := c.cfg.Store.Shapes().Get(ctx, d.rows[req.MachineID].ShapeID)
		if err != nil {
			return fmt.Errorf("machine %s: shape: %w", req.MachineID, err)
		}
		req.Shape = shape
		if d.missing[req.MachineID] {
			ok, err := c.deliver(ctx, p, &req)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
		} else if req.Image, err = c.image(ctx, p, shape, d.rows[req.MachineID].AccountID); err != nil {
			// The agent has it, and looks at nothing but tags, network and
			// deadline on a machine it has.
			name, version, _ := domain.ParseImageRef(shape.Image)
			req.Image = domain.Image{Name: name, Version: version, OS: shape.OS, Arch: shape.Arch}
		}
		kept = append(kept, req)
	}
	d.Machines = kept
	return nil
}

// deliver gives req its image and token, from the machine as it is now. The
// lock keeps an assignment out from between that look and the token, which
// would leave the token without the machine's organization.
func (c *Controller) deliver(ctx context.Context, p domain.CapacityProvider, req *CreateRequest) (bool, error) {
	c.issuing.Lock()
	defer c.issuing.Unlock()
	m, err := c.cfg.Store.Machines().Get(ctx, req.MachineID)
	if err != nil {
		return false, fmt.Errorf("machine %s: %w", req.MachineID, err)
	}
	if m.State != domain.MachineRequested {
		return false, nil
	}
	if err := RequireHostedKind(c.cfg.Hosted, p); err != nil {
		if err := c.failMachine(ctx, m, err.Error()); err != nil && !errors.Is(err, store.ErrConflict) {
			return false, err
		}
		return false, nil
	}
	image, err := c.image(ctx, p, req.Shape, m.AccountID)
	if err != nil {
		if err := c.failMachine(ctx, m, "image: "+err.Error()); err != nil && !errors.Is(err, store.ErrConflict) {
			return false, err
		}
		return false, nil
	}
	token, err := c.issue(ctx, m, req.Shape)
	if err != nil {
		return false, err
	}
	u, err := c.desired(ctx, p, m)
	if err != nil {
		return false, err
	}
	req.Image, req.Tags, req.Bootstrap = image, u.Tags, &Bootstrap{ServerURL: c.cfg.ServerURL, Token: token}
	return true, nil
}

// ReportState records what an agent says happened to machine id of provider
// p (fleet-agent-api.md). A report that repeats the current state, or that
// events overtook, is accepted, so the agent can retry. ErrNotFound means the
// machine is not p's, store.ErrConflict that it cannot make that change. The
// machines of an agent are never in applied, so this needs no controller lock.
func (c *Controller) ReportState(ctx context.Context, p domain.CapacityProvider, id uuid.UUID, state domain.MachineState, externalID, why string) error {
	m, err := c.cfg.Store.Machines().Get(ctx, id)
	if err != nil {
		return err
	}
	if m.ProviderID != p.ID {
		return store.ErrNotFound
	}
	switch state {
	case domain.MachineBooting:
		return c.reportBooting(ctx, m, externalID)
	case domain.MachineDestroyed:
		return c.reportDestroyed(ctx, m)
	case domain.MachineFailed:
		switch m.State {
		case domain.MachineFailed:
			return nil
		case domain.MachineDestroyed:
			return fmt.Errorf("machine %s is destroyed: %w", id, store.ErrConflict)
		}
		return c.failMachine(ctx, m, CleanError(why))
	}
	return fmt.Errorf("%w: state %q belongs to forecrewd", ErrBadReport, state)
}

func (c *Controller) reportBooting(ctx context.Context, m domain.Machine, externalID string) error {
	if externalID == "" {
		return fmt.Errorf("%w: booting needs external_id", ErrBadReport)
	}
	switch m.State {
	case domain.MachineRequested:
		return c.started(ctx, m, externalID)
	case domain.MachineDestroyed, domain.MachineFailed:
		return fmt.Errorf("machine %s is %s: %w", m.ID, m.State, store.ErrConflict)
	}
	return nil // said before, or the worker called first
}

// reportDestroyed finishes m. A machine still in use that the provider found
// gone fails first, which ends its worker and requeues its job.
func (c *Controller) reportDestroyed(ctx context.Context, m domain.Machine) error {
	if m.State == domain.MachineDestroyed {
		return nil
	}
	if !m.State.CanTransition(domain.MachineDestroyed) {
		if err := c.failMachine(ctx, m, errGone); err != nil {
			return err
		}
		m.State = domain.MachineFailed
	}
	return c.finishRow(ctx, m, m.State, "reported by its provider")
}

// CleanError makes an error of a provider fit to store and show: no secrets,
// valid text, and at most maxErrorLength bytes.
func CleanError(s string) string {
	s = domain.SecretPatterns.ReplaceAllString(s, "[redacted]")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > maxErrorLength {
		s = s[:maxErrorLength]
	}
	return strings.ToValidUTF8(s, "")
}

// ReconcileAgent answers an agent's list of what its provider holds
// (fleet-agent-api.md, POST /fleet/v1/reconcile): it returns the external IDs
// the agent must destroy. A failed machine the provider no longer lists has
// reached its end, since no agent is left to say so.
func (c *Controller) ReconcileAgent(ctx context.Context, p domain.CapacityProvider, instances []Instance) ([]string, error) {
	orphans, err := c.ReconcileInstances(ctx, p, instances)
	errs := []error{err}
	destroy := make([]string, 0, len(orphans))
	for _, o := range orphans {
		destroy = append(destroy, o.ExternalID)
		errs = append(errs, c.auditOrphan(ctx, p, o))
	}
	errs = append(errs, c.finishGone(ctx, p, instances))
	return destroy, errors.Join(errs...)
}

func (c *Controller) finishGone(ctx context.Context, p domain.CapacityProvider, instances []Instance) error {
	failed, err := c.cfg.Store.Machines().List(ctx, store.MachineFilter{ProviderID: p.ID, State: domain.MachineFailed})
	if err != nil {
		return fmt.Errorf("list failed machines: %w", err)
	}
	var errs []error
	for _, m := range failed {
		held := slices.ContainsFunc(instances, func(i Instance) bool {
			return i.MachineID == m.ID || (m.ExternalID != "" && i.ExternalID == m.ExternalID)
		})
		if !held {
			errs = append(errs, c.finishRow(ctx, m, domain.MachineFailed, "failed"))
		}
	}
	return errors.Join(errs...)
}

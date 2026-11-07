package fleet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// ActionAbuse records a machine its location stopped for abuse.
const ActionAbuse = "machine.abuse"

// JobStopper cancels a job as an actor; *jobs.Service is one.
type JobStopper interface {
	Cancel(ctx context.Context, by domain.Actor, accountID, jobID uuid.UUID) (domain.Job, error)
}

// StopJobsWith lets the controller stop the job of a machine stopped for
// abuse; without it the job is requeued as for any failed machine.
func (c *Controller) StopJobsWith(j JobStopper) { c.jobs = j }

// ReportAbuse records that provider p stopped machine id for abuse (kind, in
// the agent's words detail, untrusted): the machine fails, its job is
// canceled rather than run again, the organization's audit log says so and a
// notice opens for the instance's operator. A report sent again changes
// nothing, so the agent can retry. ErrNotFound means the machine is not p's.
func (c *Controller) ReportAbuse(ctx context.Context, p domain.CapacityProvider, id uuid.UUID, kind, detail string) error {
	if !slices.Contains(domain.AbuseKinds(), kind) {
		return fmt.Errorf("%w: abuse %q", ErrBadReport, kind)
	}
	detail = CleanError(detail)
	m, err := c.cfg.Store.Machines().Get(ctx, id)
	if err != nil {
		return err
	}
	if m.ProviderID != p.ID {
		return store.ErrNotFound
	}
	c.cfg.Logger.WarnContext(ctx, "machine stopped for abuse", "machine_id", m.ID, "account_id", m.AccountID, "kind", kind)
	if m.State.Counts() {
		err := c.failMachine(ctx, m, "stopped for abuse ("+kind+"): "+detail)
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	job, err := c.stopJob(ctx, m)
	if err != nil {
		return err
	}
	n := domain.FleetNotice{AccountID: m.AccountID, MachineID: m.ID, ProviderID: p.ID, JobID: job, Kind: kind, Detail: detail}
	switch err := c.cfg.Store.FleetNotices().Create(ctx, &n); {
	case errors.Is(err, store.ErrConflict):
		return nil // reported before
	case err != nil:
		return fmt.Errorf("open notice: %w", err)
	}
	c.cfg.Logger.ErrorContext(ctx, "notice for the operator: a machine was stopped for abuse", "notice_id", n.ID, "machine_id", m.ID, "kind", kind)
	data := map[string]any{"kind": kind, "notice": n.ID.String()}
	if job != uuid.Nil() {
		data["job_id"] = job.String()
	}
	return c.audit(ctx, c.cfg.Store, m, ActionAbuse, data)
}

// stopJob cancels the job machine m runs, if it still runs there, and returns
// its ID; without a JobStopper the job is left to its lease, which requeues it.
func (c *Controller) stopJob(ctx context.Context, m domain.Machine) (uuid.UUID, error) {
	if m.JobID == uuid.Nil() || m.AccountID == uuid.Nil() {
		return uuid.Nil(), nil
	}
	j, err := c.cfg.Store.Jobs().Get(ctx, m.AccountID, m.JobID)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("job of machine %s: %w", m.ID, err)
	}
	if c.jobs == nil || j.Status != domain.JobRunning || j.WorkerID != m.WorkerID {
		return j.ID, nil
	}
	by := domain.SystemActor(m.AccountID, domain.ForecrewdName)
	if _, err := c.jobs.Cancel(ctx, by, m.AccountID, j.ID); err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, domain.ErrInvalidTransition) {
		return j.ID, fmt.Errorf("stop job %s: %w", j.Ref(), err)
	}
	return j.ID, nil
}

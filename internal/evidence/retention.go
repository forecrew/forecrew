package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// SweepInterval is how often Run sweeps.
const SweepInterval = 24 * time.Hour

// sweepBatch is how many expired artifacts a sweep reads at a time.
const sweepBatch = 100

// Audit entry of a deleted artifact.
const (
	ActionArtifactDeleted = "artifact.deleted"
	SubjectArtifact       = "artifact"
)

// Retention deletes artifacts older than Days from storage and from the
// database. A verification report that points at a deleted artifact keeps
// its data and loses only the link to the rendered copy (store.ArtifactRepo
// Delete). Every deletion is recorded in the audit log.
type Retention struct {
	Store    store.Store
	Evidence Store
	// Days is the age in days from which artifacts are deleted; 0 keeps
	// them forever, so Sweep and Run do nothing.
	Days   int
	Logger *slog.Logger
	// Now defaults to the real UTC time.
	Now func() time.Time
}

// Sweep deletes the expired artifacts of every account and returns how many
// it deleted. The object goes first, the row and the audit entry follow in
// one transaction: after a failure between the two the next sweep finds the
// row again and deleting a missing object is not an error. One artifact or
// one account failing does not stop the others; the errors are logged and
// returned joined.
func (r *Retention) Sweep(ctx context.Context) (int, error) {
	if r.Days <= 0 {
		return 0, nil
	}
	accounts, err := r.Store.Accounts().List(ctx)
	if err != nil {
		return 0, fmt.Errorf("retention: list accounts: %w", err)
	}
	now := time.Now().UTC
	if r.Now != nil {
		now = r.Now
	}
	cutoff := now().AddDate(0, 0, -r.Days)
	var deleted int
	var errs []error
	for _, acc := range accounts {
		n, err := r.sweepAccount(ctx, acc.ID, cutoff)
		deleted += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return deleted, errors.Join(errs...)
}

func (r *Retention) sweepAccount(ctx context.Context, accountID uuid.UUID, cutoff time.Time) (int, error) {
	var deleted int
	var errs []error
	failed := map[uuid.UUID]bool{}
	for {
		batch, err := r.Store.Artifacts().ListOlderThan(ctx, accountID, cutoff, sweepBatch+len(failed))
		if err != nil {
			return deleted, errors.Join(append(errs, fmt.Errorf("retention: list artifacts: %w", err))...)
		}
		progress := false
		for _, a := range batch {
			if failed[a.ID] {
				continue
			}
			progress = true
			if err := r.delete(ctx, a); err != nil {
				failed[a.ID] = true
				r.logger().ErrorContext(ctx, "delete expired artifact", "account_id", a.AccountID, "artifact_id", a.ID, "error", err)
				errs = append(errs, err)
				continue
			}
			deleted++
		}
		if !progress {
			return deleted, errors.Join(errs...)
		}
	}
}

func (r *Retention) delete(ctx context.Context, a domain.Artifact) error {
	if err := r.Evidence.Delete(ctx, a.StorageKey); err != nil {
		return fmt.Errorf("artifact %s: %w", a.ID, err)
	}
	data, err := json.Marshal(map[string]any{
		"name": a.Name, "job_id": a.JobID, "size": a.Size, "sha256": a.SHA256, "created_at": a.CreatedAt, "retention_days": r.Days,
	})
	if err != nil {
		return err
	}
	entry := domain.AuditEntry{
		AccountID: a.AccountID, ProjectID: a.ProjectID, Actor: domain.SystemActor(a.AccountID, domain.ForecrewdName).String(),
		Action: ActionArtifactDeleted, SubjectType: SubjectArtifact, SubjectID: a.ID, Data: data,
	}
	err = r.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Artifacts().Delete(ctx, a.AccountID, a.ID); err != nil {
			return err
		}
		return tx.Audit().Append(ctx, &entry)
	})
	if err != nil {
		return fmt.Errorf("artifact %s: %w", a.ID, err)
	}
	return nil
}

func (r *Retention) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// Run sweeps now and then every interval until ctx ends. Errors
// are logged. It returns at once when Days is 0.
func (r *Retention) Run(ctx context.Context, interval time.Duration) {
	if r.Days <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := r.Sweep(ctx)
		if err != nil {
			r.logger().ErrorContext(ctx, "artifact retention sweep failed", "deleted", n, "error", err)
		} else if n > 0 {
			r.logger().InfoContext(ctx, "expired artifacts deleted", "deleted", n, "retention_days", r.Days)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

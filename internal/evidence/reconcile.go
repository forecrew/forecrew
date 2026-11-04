package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Reconciliation of the bucket with the artifacts table.
const (
	// OrphanGrace is how old an object must be before it is judged an
	// orphan, far longer than any upload takes.
	OrphanGrace = 24 * time.Hour
	// reconcilePage is how many objects one listing and one row lookup cover.
	reconcilePage = 500
	// ReconcileMaxObjects bounds the objects one pass examines; the next
	// pass continues after the last one.
	ReconcileMaxObjects = 20000

	ActionOrphanDeleted = "artifact.orphan_deleted"
)

// Reconcile deletes the objects under jobs/ that no artifact row points at
// and that are older than OrphanGrace, and returns how many it deleted. Such
// objects are left by a crash between the upload and the row, or by a failed
// delete of a replaced object. Each is recorded in the audit log under the
// account named by its key. A pass examines at most ReconcileMaxObjects
// objects and the next one continues where it stopped, wrapping to the start
// when the bucket ends (the position is kept in memory, so a restart begins
// again). One failure does not stop the others; errors are returned joined.
func (r *Retention) Reconcile(ctx context.Context) (int, error) {
	cutoff := r.now().Add(-OrphanGrace)
	var deleted int
	var errs []error
	after := r.cursor
	for examined := 0; examined < ReconcileMaxObjects; {
		page, err := r.Evidence.List(ctx, ObjectsPrefix, after, reconcilePage)
		if err != nil {
			return deleted, errors.Join(append(errs, fmt.Errorf("reconcile: list objects: %w", err))...)
		}
		if len(page) == 0 {
			after = "" // the end: start over next time
			break
		}
		examined += len(page)
		after = page[len(page)-1].Key
		n, err := r.reconcilePage(ctx, page, cutoff)
		deleted += n
		if err != nil {
			errs = append(errs, err)
		}
		if len(page) < reconcilePage {
			after = ""
			break
		}
	}
	r.cursor = after
	return deleted, errors.Join(errs...)
}

func (r *Retention) reconcilePage(ctx context.Context, page []Object, cutoff time.Time) (int, error) {
	keys := make([]string, len(page))
	for i, o := range page {
		keys[i] = o.Key
	}
	have, err := r.Store.Artifacts().ExistingKeys(ctx, keys)
	if err != nil {
		return 0, fmt.Errorf("reconcile: look up keys: %w", err)
	}
	referenced := make(map[string]bool, len(have))
	for _, k := range have {
		referenced[k] = true
	}
	var deleted int
	var errs []error
	for _, o := range page {
		if referenced[o.Key] || !strings.HasPrefix(o.Key, ObjectsPrefix) || o.LastModified.IsZero() || !o.LastModified.Before(cutoff) {
			continue
		}
		account, ok := keyAccount(o.Key)
		if !ok {
			r.logger().WarnContext(ctx, "orphan object with an unexpected key kept", "key", o.Key)
			continue
		}
		if _, err := r.Store.Accounts().Get(ctx, account); errors.Is(err, store.ErrNotFound) {
			r.logger().WarnContext(ctx, "orphan object of an unknown account kept", "key", o.Key, "account_id", account)
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("orphan %s: look up account: %w", o.Key, err))
			continue
		}
		if err := r.deleteOrphan(ctx, account, o); err != nil {
			r.logger().ErrorContext(ctx, "delete orphan object", "key", o.Key, "error", err)
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

func (r *Retention) deleteOrphan(ctx context.Context, account uuid.UUID, o Object) error {
	if err := r.Evidence.Delete(ctx, o.Key); err != nil {
		return fmt.Errorf("orphan %s: %w", o.Key, err)
	}
	data, err := json.Marshal(map[string]any{"key": o.Key, "size": o.Size, "last_modified": o.LastModified})
	if err != nil {
		return err
	}
	entry := domain.AuditEntry{
		AccountID: account, Actor: domain.SystemActor(account, domain.ForecrewdName).String(),
		Action: ActionOrphanDeleted, SubjectType: SubjectArtifact, Data: data,
	}
	if err := r.Store.Audit().Append(ctx, &entry); err != nil {
		return fmt.Errorf("orphan %s deleted, audit failed: %w", o.Key, err)
	}
	return nil
}

// keyAccount is the account of a key jobs/<account>/<job>/<name>.
func keyAccount(key string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(key, ObjectsPrefix)
	if !ok {
		return uuid.UUID{}, false
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok {
		return uuid.UUID{}, false
	}
	account, err := uuid.Parse(id)
	return account, err == nil
}

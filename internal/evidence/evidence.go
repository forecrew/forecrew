// Package evidence stores artifact content (test reports, logs, screenshots,
// verification reports) in S3-compatible object storage. Metadata lives in
// the artifacts table (store.ArtifactRepo); this package only moves bytes.
//
// minio-go is the S3 client: one small dependency with streaming uploads and
// path-style addressing, where aws-sdk-go-v2 pulls in a much larger tree.
package evidence

import (
	"context"
	"errors"
	"io"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// ErrNotFound means no object has the key.
var ErrNotFound = errors.New("evidence not found")

// Store is object storage for artifact content.
type Store interface {
	// Put stores r under key, replacing any object there. size is the exact
	// number of bytes r yields; the S3 store refuses -1 (unknown), because
	// its client would buffer a whole part up front. A failed Put stores
	// nothing under key.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens the object at key; the caller closes it. A missing object
	// is ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object at key; a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List returns up to limit objects whose key starts with prefix and
	// sorts after the key after ("" starts at the beginning), in key order.
	// Fewer than limit objects mean the listing is complete.
	List(ctx context.Context, prefix, after string, limit int) ([]Object, error)
}

// Object is the listing entry of a stored object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ObjectsPrefix is the key prefix of every object forecrewd stores;
// reconciliation never looks at, or deletes, anything outside it.
const ObjectsPrefix = "jobs/"

// JobKey is the object key prefix of the artifact name of a job. Names are
// validated (domain.ValidArtifactName), so the key cannot escape its job's
// prefix.
func JobKey(accountID, jobID uuid.UUID, name string) string {
	return ObjectsPrefix + accountID.String() + "/" + jobID.String() + "/" + name
}

// NewKey is a fresh object key for one upload of the artifact name of a job.
// Every upload gets its own object (the "~" cannot occur in a name), so a
// failed or racing upload can only ever remove its own object, never the one
// an existing row points at.
func NewKey(accountID, jobID uuid.UUID, name string) string {
	return JobKey(accountID, jobID, name) + "~" + domain.NewID().String()
}

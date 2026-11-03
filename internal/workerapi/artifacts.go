package workerapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
)

// DefaultContentType is the content type of an upload that names none.
const DefaultContentType = "application/octet-stream"

// maxContentTypeLen bounds the stored content type.
const maxContentTypeLen = 255

type artifactResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// uploadArtifact streams the body to evidence storage and records the
// artifact. It never buffers the body and never stores more than
// MaxArtifactSize bytes.
func (s *Server) uploadArtifact(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w, logger := s.worker(r)
	id, ok := jobID(rw, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !domain.ValidArtifactName(name) {
		writeError(rw, http.StatusBadRequest, "bad_request", "invalid artifact name")
		return
	}
	j, err := s.leasedJob(ctx, w, id)
	if err != nil {
		fail(ctx, rw, logger, "check job for artifact", err)
		return
	}
	if s.cfg.Evidence == nil {
		writeError(rw, http.StatusServiceUnavailable, "unavailable", "no evidence storage is configured")
		return
	}
	logger = logger.With("job_id", j.ID, "ref", j.Ref(), "artifact", name)
	// The size must be known up front: the S3 client buffers a part for a
	// body of unknown length, and the limit is checked before reading.
	if r.ContentLength < 0 {
		writeError(rw, http.StatusLengthRequired, "length_required", "Content-Length is required")
		return
	}
	if r.ContentLength > s.cfg.MaxArtifactSize {
		tooLarge(rw, s.cfg.MaxArtifactSize)
		return
	}

	contentType := cleanContentType(r.Header.Get("Content-Type"))
	body := &limitedHash{r: r.Body, max: s.cfg.MaxArtifactSize, h: sha256.New()}
	key := evidence.NewKey(w.AccountID, j.ID, name) // this upload's own object
	if err := s.cfg.Evidence.Put(ctx, key, body, r.ContentLength, contentType); err != nil {
		if body.exceeded {
			tooLarge(rw, s.cfg.MaxArtifactSize)
			return
		}
		fail(ctx, rw, logger, "store artifact", err)
		return
	}
	// The upload took time and the lease may have ended meanwhile: the store
	// checks it and writes the row in one transaction.
	a := domain.Artifact{
		AccountID: w.AccountID, ProjectID: j.ProjectID, JobID: j.ID, Name: name, ContentType: contentType,
		Size: body.n, SHA256: hex.EncodeToString(body.h.Sum(nil)), StorageKey: key,
	}
	replaced, err := s.cfg.Store.Artifacts().PutLeased(ctx, &a, w.ID)
	if err != nil {
		s.removeObject(ctx, logger, key) // only ours: the row, if any, points elsewhere
		fail(ctx, rw, logger, "record artifact", err)
		return
	}
	if replaced != "" {
		s.removeObject(ctx, logger, replaced)
	}
	logger.InfoContext(ctx, "artifact stored", "artifact_id", a.ID, "size", a.Size)
	writeJSON(rw, http.StatusCreated, artifactResponse{ID: a.ID.String(), Name: a.Name, Size: a.Size, SHA256: a.SHA256})
}

// removeObject deletes a stored object nothing points at; a failure is only
// logged, it leaves garbage but never a broken artifact.
func (s *Server) removeObject(ctx context.Context, logger *slog.Logger, key string) {
	if err := s.cfg.Evidence.Delete(context.WithoutCancel(ctx), key); err != nil {
		logger.ErrorContext(ctx, "delete unused artifact object", "error", err)
	}
}

func tooLarge(rw http.ResponseWriter, max int64) {
	writeError(rw, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("artifact over %d bytes", max))
}

// cleanContentType returns v when it is a media type, else the default.
func cleanContentType(v string) string {
	if _, _, err := mime.ParseMediaType(v); err != nil || len(v) > maxContentTypeLen {
		return DefaultContentType
	}
	return v
}

// errTooLarge is returned by limitedHash once the body passes its limit.
var errTooLarge = errors.New("artifact too large")

// limitedHash passes the body through, counting and hashing it, and fails
// with errTooLarge instead of yielding more than max bytes.
type limitedHash struct {
	r        io.Reader
	max      int64
	h        hash.Hash
	n        int64
	exceeded bool
}

func (l *limitedHash) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if l.n+int64(n) > l.max {
		l.exceeded = true
		return 0, errTooLarge
	}
	l.n += int64(n)
	_, _ = l.h.Write(p[:n])
	return n, err
}

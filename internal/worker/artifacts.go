package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// uploadAttempts is how many times UploadArtifact tries before giving up on
// network errors and 5xx answers.
const uploadAttempts = 4

// uploadTimeout bounds one upload attempt; the client's request timeout is
// too short for a large file on a slow link.
const uploadTimeout = 10 * time.Minute

// Artifact is a stored artifact as forecrewd reports it.
type Artifact struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
}

// UploadArtifact uploads the file at path as the artifact name of a running
// job and returns what forecrewd stored. name is the path relative to the
// repo root, with slashes (domain.ValidArtifactName). The file is streamed,
// never read into memory. Network errors and 5xx answers are retried with
// backoff, each attempt from the start of the file; uploading a name again
// replaces the artifact, so a retry is safe. ErrLeaseLost, ErrTooLarge and
// ErrEvidenceUnavailable are not retried.
func (c *Client) UploadArtifact(ctx context.Context, jobID uuid.UUID, name, path string) (Artifact, error) {
	if !domain.ValidArtifactName(name) {
		return Artifact{}, fmt.Errorf("invalid artifact name %q", name)
	}
	// Problems with the file are permanent: check them before retrying.
	info, err := os.Stat(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("artifact %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return Artifact{}, fmt.Errorf("artifact %s is not a regular file", name)
	}
	var a Artifact
	err = c.attempt(ctx, uploadAttempts, func() (err error) {
		a, err = c.uploadOnce(ctx, jobID, name, path)
		return err
	})
	if err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (c *Client) uploadOnce(ctx context.Context, jobID uuid.UUID, name, path string) (Artifact, error) {
	f, err := os.Open(path) // #nosec G304 -- the worker uploads files of its own workspace
	if err != nil {
		return Artifact{}, fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Artifact{}, fmt.Errorf("stat artifact: %w", err)
	}
	contentType, err := contentTypeOf(f, path)
	if err != nil {
		return Artifact{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	// forecrewd requires Content-Length. An empty body must be http.NoBody:
	// with any other body, a ContentLength of 0 means "unknown" and the
	// request would be sent chunked.
	var body io.Reader = http.NoBody
	if info.Size() > 0 {
		body = io.LimitReader(f, info.Size())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+jobPath(jobID, "artifacts")+"/"+name, body)
	if err != nil {
		return Artifact{}, fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.token)
	hc := *c.http
	hc.Timeout = 0 // bounded by uploadTimeout instead
	resp, err := hc.Do(req)
	if err != nil {
		return Artifact{}, fmt.Errorf("put artifact: %w", redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Artifact{}, fmt.Errorf("read artifact answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		if resp.StatusCode/100 == 2 {
			return Artifact{}, fmt.Errorf("artifact upload answered %d, want 201", resp.StatusCode)
		}
		return Artifact{}, answerError(resp.StatusCode, data)
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return Artifact{}, fmt.Errorf("decode artifact answer: %w", err)
	}
	return a, nil
}

// contentTypeOf names the type of a file by its extension, else by its first
// bytes; the file is rewound afterwards.
func contentTypeOf(f *os.File, path string) (string, error) {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t, nil
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read artifact: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind artifact: %w", err)
	}
	return http.DetectContentType(head[:n]), nil
}

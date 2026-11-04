package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// uploadEvidence uploads the files the agent marked with the job MCP tool
// evidence_attach, after the runtime exited: one more heartbeat collects the
// last marks, then each file is checked and uploaded. A file that cannot be
// uploaded is reported in the job log and skipped. It returns why the job
// must stop (canceled, lease gone), or nil.
func (r *jobRun) uploadEvidence(ctx context.Context) error {
	if r.spec.JobMCP == nil {
		return nil
	}
	if cause := r.beat(ctx); cause != nil {
		return cause
	}
	for _, a := range r.attachments() {
		file, err := evidenceFile(r.dir, a.Path)
		if err == nil {
			_, err = r.w.cfg.Client.UploadArtifact(ctx, r.spec.JobID, a.Name, file)
		}
		switch {
		case stopsJob(err):
			return err
		case err != nil:
			r.ev.text(domain.EventError, r.red.String(fmt.Sprintf("evidence %s not uploaded: %v", a.Name, err)))
		default:
			r.ev.text(domain.EventLog, "evidence uploaded: "+a.Name)
		}
	}
	return nil
}

// errNotEvidence means a marked path is not a regular file inside the work
// tree.
var errNotEvidence = errors.New("not a regular file inside the work tree")

// evidenceFile returns the file of the work tree dir at rel, a slash path
// relative to its root: no absolute path, no "..", nothing under .git, and
// no symlink anywhere on the way.
func evidenceFile(dir, rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) || strings.Contains(rel, "\\") || path.Clean(rel) != rel ||
		rel == ".." || strings.HasPrefix(rel, "../") || rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", fmt.Errorf("%q: %w", rel, errNotEvidence)
	}
	cur := dir
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("%q does not exist", rel)
			}
			return "", fmt.Errorf("%q: %w", rel, err)
		}
		last := i == len(parts)-1
		if info.Mode()&fs.ModeSymlink != 0 || (last && !info.Mode().IsRegular()) || (!last && !info.IsDir()) {
			return "", fmt.Errorf("%q: %w", rel, errNotEvidence)
		}
	}
	return cur, nil
}

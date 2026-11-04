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
	"github.com/forecrew/forecrew/internal/runtime"
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
		file, err := evidenceFile(r.dir, r.scratch, a.Path)
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
// tree or the scratch directory.
var errNotEvidence = errors.New("not a regular file inside the work tree or scratch directory")

// evidenceFile returns the file a marked path names: a slash path relative
// to the work tree dir, or "evidence:" and a slash path relative to the
// scratch directory. Either way: no absolute path, no "..", and no symlink
// anywhere on the way; nothing under .git in the work tree.
func evidenceFile(dir, scratch, rel string) (string, error) {
	root, p := dir, rel
	if q, ok := strings.CutPrefix(rel, runtime.EvidencePrefix); ok {
		root, p = scratch, q
	}
	if root == "" || p == "" || path.IsAbs(p) || strings.Contains(p, "\\") || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") || p == ".git" || strings.HasPrefix(p, ".git/") {
		return "", fmt.Errorf("%q: %w", rel, errNotEvidence)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%q: %w", rel, errNotEvidence)
	}
	cur := root
	parts := strings.Split(p, "/")
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

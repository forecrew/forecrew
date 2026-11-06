package images

import (
	"embed"
	"fmt"
	"os"
	"strings"
)

// The guest files are the one copy of what runs inside a Forecrew machine:
// the microVM init, the script that starts the worker from the settings the
// provider delivered, and the systemd unit of images that have systemd. The
// Packer templates of deploy/images install the same files.
//
//go:embed guest
var guestFS embed.FS

// Paths inside an image. /opt/forecrew is new in any image, so adding it
// never goes through a symlink of the base image.
const (
	GuestDir    = "opt/forecrew"
	GuestWorker = GuestDir + "/forecrew-worker"
	GuestInit   = GuestDir + "/forecrew-init"
	GuestStart  = GuestDir + "/forecrew-start"
	GuestUnit   = "etc/systemd/system/forecrew-worker.service"
	guestLabels = "etc/forecrew/labels"
)

// maxWorkerBytes bounds the worker binary read into an overlay.
const maxWorkerBytes = 512 << 20

// File is a file added to an image.
type File struct {
	Path string // relative to the root, no leading slash
	Mode int64
	Data []byte
}

// Guest returns the guest files of an image: the init, the start script,
// the systemd unit, the labels the image offers (a shape lists them) and the
// worker binary read from workerPath, which must be a static binary for the
// image's architecture.
func Guest(workerPath string, labels []string) ([]File, error) {
	st, err := os.Stat(workerPath)
	if err != nil {
		return nil, fmt.Errorf("worker binary: %w", err)
	}
	if !st.Mode().IsRegular() || st.Size() > maxWorkerBytes {
		return nil, fmt.Errorf("worker binary %s is not a regular file of at most %d MB", workerPath, maxWorkerBytes>>20)
	}
	worker, err := os.ReadFile(workerPath) // #nosec G304 -- the caller's path
	if err != nil {
		return nil, fmt.Errorf("worker binary: %w", err)
	}
	files := []File{{Path: GuestWorker, Mode: 0o755, Data: worker}}
	for _, f := range []struct {
		name, path string
		mode       int64
	}{
		{"forecrew-init", GuestInit, 0o755},
		{"forecrew-start", GuestStart, 0o755},
		{"forecrew-worker.service", GuestUnit, 0o644},
	} {
		data, err := guestFS.ReadFile("guest/" + f.name)
		if err != nil {
			return nil, fmt.Errorf("guest file %s: %w", f.name, err)
		}
		files = append(files, File{Path: f.path, Mode: f.mode, Data: data})
	}
	if len(labels) > 0 {
		files = append(files, File{Path: guestLabels, Mode: 0o644, Data: []byte(strings.Join(labels, "\n") + "\n")})
	}
	return files, nil
}

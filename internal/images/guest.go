package images

import (
	"bytes"
	"debug/elf"
	"embed"
	"fmt"
	"os"
	"slices"
	"strings"
)

// The guest files are the one copy of what runs inside a Forecrew machine: the
// script that starts the worker from the settings the provider delivered, and
// the systemd unit of images that have systemd. The Packer templates of
// deploy/images install the same files. A microVM uses neither: its PID 1 is the
// worker binary (GuestInitArgs), because everything else on its disk may be a
// repo's code.
//
//go:embed guest
var guestFS embed.FS

// Paths inside an image. /opt/forecrew is Forecrew's in any image: the
// converter drops what the base image has there and refuses an /opt that is not
// a directory, so what the kernel starts is the file added here.
const (
	GuestDir    = "opt/forecrew"
	GuestWorker = GuestDir + "/forecrew-worker"
	GuestStart  = GuestDir + "/forecrew-start"
	GuestUnit   = "etc/systemd/system/forecrew-worker.service"
	guestLabels = "etc/forecrew/labels"
	// GuestEnv holds the Docker image's environment, NAME=value per line.
	GuestEnv = "etc/forecrew/env"
)

// GuestInitArgs goes at the end of a microVM's kernel command line: the kernel
// starts the worker binary as PID 1, with what follows -- as its arguments. It
// is a static program, so no file of the image runs before it is done with the
// bootstrap token.
const GuestInitArgs = "init=/" + GuestWorker + " -- guest-init"

// maxWorkerBytes bounds the worker binary read into an overlay.
const maxWorkerBytes = 512 << 20

// File is a file added to an image.
type File struct {
	Path string // relative to the root, no leading slash
	Mode int64
	Data []byte
}

// Guest returns the guest files of an image: the start script, the systemd
// unit, the labels the image offers (a shape lists them) and the worker binary
// read from workerPath, which must be a static binary for the image's
// architecture: a dynamic one would start through the image's own linker.
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
	if dynamicELF(worker) {
		return nil, fmt.Errorf("worker binary %s is dynamically linked: build it with CGO_ENABLED=0", workerPath)
	}
	files := []File{{Path: GuestWorker, Mode: 0o755, Data: worker}}
	for _, f := range []struct {
		name, path string
		mode       int64
	}{
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

// dynamicELF reports an ELF file that names a program interpreter.
func dynamicELF(data []byte) bool {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return slices.ContainsFunc(f.Progs, func(p *elf.Prog) bool { return p.Type == elf.PT_INTERP })
}

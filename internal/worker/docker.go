package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/runtime"
)

// dockerLabel is the worker label of a machine whose Docker engine answers.
const dockerLabel = "docker"

// Names the worker gives to what it creates in Docker. Containers and
// networks carry both labels so they can be found and removed: the job's
// own, and the worker's, so that a worker starting up removes only the
// leftovers of its own crashed run.
const (
	jobLabelKey    = "forecrew.job"
	workerLabelKey = "forecrew.worker"
	// envImageRepo is the repository of the images built for environments.
	envImageRepo = "forecrew-env"
	// dockerQuick bounds the calls that only ask or remove something.
	dockerQuick = 2 * time.Minute
)

// dockerCLI runs the docker command line client. The CLI, not the Engine
// API client, because the worker needs a dozen calls only, the CLI is what
// the operator already has and configures (contexts, DOCKER_HOST, registry
// logins), and it adds no dependency.
type dockerCLI struct {
	bin    string
	env    []string // environment of the client, never of a container
	worker string   // identifies this worker's containers, see workerLabelKey
}

// newDockerCLI returns the client of the binary bin. workDir identifies the
// worker; environ is the worker's own environment, of which the client gets
// the allowlisted variables and DOCKER_* ones.
func newDockerCLI(bin, workDir string, environ []string) *dockerCLI {
	sum := sha256.Sum256([]byte(workDir))
	return &dockerCLI{
		bin: bin, worker: hex.EncodeToString(sum[:6]),
		env: filterEnv(environ, func(name string) bool {
			n := strings.ToUpper(name)
			return commandEnvNames[n] || strings.HasPrefix(n, "DOCKER_") || n == "XDG_RUNTIME_DIR"
		}),
	}
}

// exec runs docker with args, streaming output lines to onOut and onErr.
func (d *dockerCLI) exec(ctx context.Context, timeout time.Duration, onOut, onErr func([]byte), args ...string) (runtime.Exit, error) {
	p := d.process(args...)
	p.Timeout = timeout
	return runtime.Exec(ctx, p, onOut, onErr)
}

// output runs docker with args and returns its standard output, trimmed. An
// error carries the end of standard error.
func (d *dockerCLI) output(ctx context.Context, args ...string) (string, error) {
	var out []string
	exit, err := d.exec(ctx, dockerQuick, func(b []byte) { out = append(out, string(b)) }, nil, args...)
	if err != nil {
		msg := strings.TrimSpace(exit.Stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(strings.Join(out, "\n")), nil
}

// version returns the version of the Docker engine, or an error when it does
// not answer.
func (d *dockerCLI) version(ctx context.Context) (string, error) {
	return d.output(ctx, "version", "--format", "{{.Server.Version}}")
}

// labelFilter is the filter argument selecting what carries label k=v (just
// k when v is empty).
func labelFilter(k, v string) string {
	if v == "" {
		return "label=" + k
	}
	return "label=" + k + "=" + v
}

// removeJob removes the containers and networks of one job.
func (d *dockerCLI) removeJob(ctx context.Context, jobID string) error {
	return d.removeLabeled(ctx, labelFilter(jobLabelKey, jobID))
}

// removeLeftovers removes the containers and networks a crashed run of this
// worker left behind.
func (d *dockerCLI) removeLeftovers(ctx context.Context) error {
	return d.removeLabeled(ctx, labelFilter(workerLabelKey, d.worker))
}

// removeLabeled removes the containers, then the networks, matching filter.
func (d *dockerCLI) removeLabeled(ctx context.Context, filter string) error {
	var errs []error
	for _, kind := range []struct{ list, rm []string }{
		{[]string{"ps", "-aq", "--filter", filter}, []string{"rm", "-f", "-v"}},
		{[]string{"network", "ls", "-q", "--filter", filter}, []string{"network", "rm"}},
	} {
		out, err := d.output(ctx, kind.list...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ids := strings.Fields(out); len(ids) > 0 {
			if _, err := d.output(ctx, append(append([]string(nil), kind.rm...), ids...)...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// imageMarkers remembers when each environment image was last used, as the
// modification time of an empty file per image hash. It lives next to the
// job workspaces and survives restarts.
type imageMarkers string

func (m imageMarkers) touch(hash string, now time.Time) {
	p := filepath.Join(string(m), hash)
	if err := os.MkdirAll(string(m), 0o750); err != nil {
		return
	}
	if err := os.WriteFile(p, nil, 0o600); err == nil {
		_ = os.Chtimes(p, now, now)
	}
}

// pruneImages removes the environment images that were not used for maxAge.
// An image without a marker (built before markers, or by hand) starts its
// clock now. Images in use by a container are kept by Docker itself.
func (d *dockerCLI) pruneImages(ctx context.Context, m imageMarkers, maxAge time.Duration, now time.Time) (removed []string, err error) {
	out, err := d.output(ctx, "image", "ls", "--filter", "reference="+envImageRepo, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	var errs []error
	for ref := range strings.FieldsSeq(out) {
		tag := strings.TrimPrefix(ref, envImageRepo+":")
		hash := strings.TrimSuffix(tag, "-base")
		info, serr := os.Stat(filepath.Join(string(m), hash))
		switch {
		case serr != nil:
			m.touch(hash, now)
		case now.Sub(info.ModTime()) > maxAge:
			if _, err := d.output(ctx, "image", "rm", ref); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, ref)
		}
	}
	return removed, errors.Join(errs...)
}

// process is the docker client run with args.
func (d *dockerCLI) process(args ...string) runtime.Process {
	return runtime.Process{Path: d.bin, Args: args, Env: d.env}
}

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// isolate makes the client use an empty Docker config at configDir instead of
// the user's: image names come from repos, and a pull or build must not send
// the operator's registry credentials. The user's DOCKER_CONFIG,
// DOCKER_AUTH_CONFIG and DOCKER_CONTEXT are dropped; host, the daemon the
// user's current context names, is kept as DOCKER_HOST unless the
// environment sets one.
func (d *dockerCLI) isolate(configDir, host string) {
	_ = os.MkdirAll(configDir, 0o700)
	hasHost := false
	d.env = slices.DeleteFunc(d.env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "DOCKER_HOST":
			hasHost = true
			return false
		case "DOCKER_CONFIG", "DOCKER_AUTH_CONFIG", "DOCKER_CONTEXT":
			return true
		}
		return false
	})
	d.env = append(d.env, "DOCKER_CONFIG="+configDir)
	if host != "" && !hasHost {
		d.env = append(d.env, "DOCKER_HOST="+host)
	}
}

// host returns the daemon endpoint of the user's current Docker context.
func (d *dockerCLI) host(ctx context.Context) string {
	h, _ := d.output(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	return h
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

// imageMarkers remembers the images the worker pulled or built and when a job
// last used them, as one small file per image reference (named by the hash of
// the reference, holding the reference) whose modification time is the last
// use. It lives next to the job workspaces and survives restarts.
type imageMarkers string

func (m imageMarkers) path(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(string(m), hex.EncodeToString(sum[:16]))
}

// tracked reports whether ref has a marker.
func (m imageMarkers) tracked(ref string) bool {
	_, err := os.Stat(m.path(ref))
	return err == nil
}

// touch records a use of ref at now.
func (m imageMarkers) touch(ref string, now time.Time) {
	if err := os.MkdirAll(string(m), 0o750); err != nil {
		return
	}
	p := m.path(ref)
	if err := os.WriteFile(p, []byte(ref), 0o600); err == nil {
		_ = os.Chtimes(p, now, now)
	}
}

// pruneImages removes the images no job used for maxAge: the environment
// images (forecrew-env:*) and the images the worker pulled for them, for
// services or as the base of a build. An environment image without a marker
// (built by hand, or before markers) starts its clock now. An image still
// used by a container, or that Docker refuses to remove, keeps its marker and
// is tried again; layers shared with an image still in use stay.
func (d *dockerCLI) pruneImages(ctx context.Context, m imageMarkers, maxAge time.Duration, now time.Time) (removed []string, err error) {
	out, err := d.output(ctx, "image", "ls", "--filter", "reference="+envImageRepo, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	for ref := range strings.FieldsSeq(out) {
		if !m.tracked(ref) {
			m.touch(ref, now)
		}
	}
	entries, _ := os.ReadDir(string(m))
	var errs []error
	for _, e := range entries {
		p := filepath.Join(string(m), e.Name())
		info, serr := e.Info()
		ref, rerr := os.ReadFile(p) // #nosec G304 -- the worker's own marker directory
		if serr != nil || rerr != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		switch _, err := d.output(ctx, "image", "rm", "--", string(ref)); {
		case err == nil:
			removed = append(removed, string(ref))
			_ = os.Remove(p)
		case strings.Contains(err.Error(), "No such image"):
			_ = os.Remove(p)
		default:
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// process is the docker client run with args.
func (d *dockerCLI) process(args ...string) runtime.Process {
	return runtime.Process{Path: d.bin, Args: args, Env: d.env}
}

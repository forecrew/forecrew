package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// Defaults of a service's health check.
const (
	defaultHealthInterval = 2 * time.Second
	defaultHealthRetries  = 30
	healthTryTimeout      = 30 * time.Second
	// workMount is where the work tree is mounted in the job container.
	workMount = "/work"
	// containerPidsLimit bounds the processes of a container, so that a fork
	// bomb does not exhaust the host's.
	containerPidsLimit = "4096"
)

// hostIDs returns the uid and gid the job runs as: the worker's own. A
// variable for the tests.
var hostIDs = func() (uid, gid int) { return os.Getuid(), os.Getgid() }

// dockerJob is what a command job of provider docker runs in: the image, the
// private network and the service containers. Every container and network
// carries the job's label so that cleanup finds them in all outcomes.
type dockerJob struct {
	r       *jobRun
	d       *dockerCLI
	id      string
	tag     string // the environment image
	network string
	env     map[string]string // devcontainer and service variables of the job container
	user    string            // uid:gid of the worker user
	mark    string
	seq     atomic.Int32

	cleaned sync.Once
}

// dockerUser returns the uid:gid containers run as: the worker's own. A
// worker running as root is refused.
func dockerUser() (string, error) {
	uid, gid := hostIDs()
	if uid == 0 {
		return "", errors.New("the worker runs as root; Docker environments run jobs as the worker's user and refuse uid 0, run the worker as a dedicated user")
	}
	if uid < 0 { // Windows
		uid, gid = 1000, 1000
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
}

// labels are the labels every container and network of the job gets.
func (dj *dockerJob) labels() []string {
	return []string{"--label", jobLabelKey + "=" + dj.id, "--label", workerLabelKey + "=" + dj.d.worker}
}

// startDocker prepares the docker environment of the job: the image, the
// private network and the services, within the job's timeout. The returned
// dockerJob, when not nil, must be cleaned up. done is the completion that
// ends the job, or nil.
func (r *jobRun) startDocker(parent context.Context, mark string) (dj *dockerJob, done *Completion) {
	ctx := parent
	if !r.deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(parent, r.deadline)
		defer cancel()
	}
	fail := func(format string, args ...any) *Completion {
		switch {
		case parent.Err() != nil: // canceled, or the lease is gone
			return &Completion{}
		case ctx.Err() != nil:
			c := r.failed("the timeout of the job ran out while the environment was prepared (%s)", fmt.Sprintf(format, args...))
			return &c
		}
		c := r.failed(format, args...)
		return &c
	}
	if r.w.docker == nil {
		return nil, fail("this worker has no Docker engine (no docker label)")
	}
	src, err := resolveSource(r.dir, r.spec.Environment)
	if err != nil {
		return nil, fail("%v", err)
	}
	user, err := dockerUser()
	if err != nil {
		return nil, fail("%v", err)
	}
	dj = &dockerJob{r: r, d: r.w.docker, id: r.spec.JobID.String(), env: src.Env, mark: mark, user: user}
	if dj.env == nil {
		dj.env = map[string]string{}
	}
	if dj.tag, err = dj.image(ctx, src); err != nil {
		return dj, fail("%v", err)
	}
	if err := dj.startNetwork(ctx); err != nil {
		if parent.Err() != nil {
			return dj, fail("%v", err)
		}
		c := r.retry("create the job network: %v", err) // the daemon's problem, not the repo's
		return dj, &c
	}
	if err := dj.startServices(ctx); err != nil {
		return dj, fail("%v", err)
	}
	return dj, nil
}

// cleanup stops and removes the job's containers and network. It is safe to
// call more than once, and it runs even when the job was canceled.
func (dj *dockerJob) cleanup(ctx context.Context) {
	dj.cleaned.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerQuick)
		defer cancel()
		if err := dj.d.removeJob(ctx, dj.id); err != nil {
			dj.r.log.Warn("remove the job's containers and network", "error", err)
		}
	})
}

// startNetwork creates the job's private network, so that concurrent jobs do
// not see each other on the default bridge. It is removed with the job's
// containers, as soon as the command ends.
func (dj *dockerJob) startNetwork(ctx context.Context) error {
	dj.network = "forecrew-" + dj.id
	_, err := dj.d.output(ctx, append(append([]string{"network", "create"}, dj.labels()...), "--", dj.network)...)
	return err
}

// startServices starts each service on the network, waiting for its health
// check. The job gets <NAME>_HOST and <NAME>_PORT (the first
// port) of each.
func (dj *dockerJob) startServices(ctx context.Context) error {
	services := dj.r.spec.Environment.Services
	if len(services) == 0 {
		return nil
	}
	dj.r.ev.text(domain.EventStatus, "starting services")
	names := slices.Sorted(maps.Keys(services))
	for _, name := range names {
		s := services[name]
		ctr := "forecrew-" + dj.id + "-" + name
		if err := dj.pull(ctx, s.Image); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		args := append([]string{"run", "-d", "--name", ctr, "--network", dj.network, "--network-alias", name,
			"--security-opt", "no-new-privileges", "--pids-limit", containerPidsLimit}, dj.labels()...)
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			args = append(args, "-e", k+"="+s.Env[k])
		}
		if _, err := dj.d.output(ctx, append(args, "--", s.Image)...); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		if err := dj.waitHealthy(ctx, ctr, s); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		prefix := domain.ServiceVar(name)
		dj.env[prefix+"_HOST"] = name
		if len(s.Ports) > 0 {
			dj.env[prefix+"_PORT"] = strconv.Itoa(s.Ports[0])
		}
	}
	return nil
}

// portProbe is a shell command succeeding once something listens on every
// port in the container: nc, or bash's /dev/tcp when nc is missing.
func portProbe(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprintf("(nc -z localhost %[1]d || bash -c ': </dev/tcp/localhost/%[1]d')", p)
	}
	return strings.Join(parts, " && ") + " >/dev/null 2>&1"
}

// healthCheck returns the shell command that tells a service is ready (empty:
// a running container is), the time between tries and the number of tries.
func healthCheck(s domain.Service) (check string, interval time.Duration, tries int) {
	interval, tries = defaultHealthInterval, defaultHealthRetries
	if h := s.Health; h != nil {
		if h.Interval > 0 {
			interval = time.Duration(h.Interval)
		}
		if h.Retries > 0 {
			tries = h.Retries
		}
		return h.Command, interval, tries
	}
	if len(s.Ports) > 0 {
		check = portProbe(s.Ports)
	}
	return check, interval, tries
}

// waitHealthy runs the service's health command (or probes its ports) until
// it succeeds, as often as its retries say, within the job's timeout. It
// stops early when the container exited.
func (dj *dockerJob) waitHealthy(ctx context.Context, ctr string, s domain.Service) error {
	check, interval, tries := healthCheck(s)
	var last error
	for i := range tries {
		if i > 0 {
			if err := dj.pause(ctx, interval); err != nil {
				return err
			}
		}
		if running, err := dj.d.output(ctx, "inspect", "--format", "{{.State.Running}}", ctr); err != nil || running != "true" {
			return fmt.Errorf("the container exited before it was ready%s", dj.logTail(ctx, ctr))
		}
		if check == "" {
			return nil
		}
		// The output is not read: a health command may print without end.
		_, last = dj.d.exec(ctx, healthTryTimeout, nil, nil, "exec", ctr, "sh", "-c", check)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("not healthy after %d tries: %v%s", tries, last, dj.logTail(ctx, ctr))
}

// pause sleeps for d, or less when the job's timeout ends sooner, in which
// case it returns runtime.ErrTimeout.
func (dj *dockerJob) pause(ctx context.Context, d time.Duration) error {
	left, ok := dj.r.left()
	if !ok {
		return fmt.Errorf("%w: the timeout of the job is used up", runtime.ErrTimeout)
	}
	if left > 0 && left < d {
		d = left
	}
	return sleep(ctx, d)
}

// logTail returns the end of a container's log for an error message, and
// writes it to the job log.
func (dj *dockerJob) logTail(ctx context.Context, ctr string) string {
	out, err := dj.d.output(ctx, "logs", "--tail", "20", ctr)
	if err != nil || out == "" {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		dj.r.logLine("service log: " + line[:min(len(line), maxLogLine)])
	}
	return " (see the service log in the job log)"
}

// runArgs are the arguments of every `docker run` of the job: the job's
// labels, the worker's user, no capabilities, no privilege gain, a process
// limit, no entrypoint and the job's private network.
func (dj *dockerJob) runArgs() []string {
	args := []string{"run", "--rm", "--init", "--name", fmt.Sprintf("forecrew-%s-run%d", dj.id, dj.seq.Add(1))}
	args = append(args, dj.labels()...)
	return append(args, "--user", dj.user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", containerPidsLimit, "--entrypoint", "", "--network", dj.network)
}

// contain turns a process meant for the host into the docker run of the same
// process in the job container: the work tree at /work, the worker user's
// uid, no capabilities, no privilege gain, no Docker socket, no published
// ports, no host network. Its environment is exactly p.Env.
func (dj *dockerJob) contain(p runtime.Process) runtime.Process {
	args := append(dj.runArgs(), "-v", dj.r.dir+":"+workMount)
	wd := workMount
	if rel, err := filepath.Rel(dj.r.dir, p.Dir); err == nil && rel != "." && p.Dir != "" {
		wd += "/" + filepath.ToSlash(rel)
	}
	args = append(args, "-w", wd)
	for _, kv := range p.Env {
		args = append(args, "-e", kv)
	}
	args = append(args, "--", dj.tag, p.Path)
	args = append(args, p.Args...)
	out := dj.d.process(args...)
	out.Dir = dj.r.dir
	out.Env = append(slices.Clone(out.Env), markEnv+"="+dj.mark)
	out.Timeout = p.Timeout
	return out
}

// facts returns what the verification report says of the environment: the
// image digest and the operating system of the container.
func (dj *dockerJob) facts(ctx context.Context, env []string) map[string]string {
	out := map[string]string{}
	if digest, err := dj.d.imageDigest(ctx, dj.tag); err == nil {
		out["image"] = digest
	} else {
		out["image"] = "unavailable: " + err.Error()
	}
	p := dj.r.shell(`sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"'`, dj.r.dir, env, versionTimeout)
	if osName := dj.r.firstLine(ctx, p); osName != "" {
		out["os-release"] = osName
	} else {
		out["os-release"] = "unavailable"
	}
	return out
}

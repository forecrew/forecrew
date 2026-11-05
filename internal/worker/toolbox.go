package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// Toolbox images and the limits of their build.
const (
	// toolboxHashVersion changes the hash of every toolbox image when the way
	// they are built changes.
	toolboxHashVersion = "1"
	// toolboxBaseDir is the directory of toolbox files holding the base image
	// (the agent CLIs) that toolbox Dockerfiles start from.
	toolboxBaseDir = "base"
	// toolboxBaseArg is the build argument naming the base image. A toolbox
	// Dockerfile that mentions it gets the base built first:
	//   ARG FORECREW_BASE
	//   FROM ${FORECREW_BASE}
	toolboxBaseArg = "FORECREW_BASE"
	// toolboxBuildTimeout bounds pulling or building a toolbox image.
	toolboxBuildTimeout = 30 * time.Minute
	// toolboxVersionTimeout bounds the version commands of a toolbox's CLIs.
	toolboxVersionTimeout = time.Minute
	// maxToolboxFiles and maxToolboxBytes bound the files hashed and
	// extracted for a build.
	// hostGateway is the name a container reaches the host's gateway by, when
	// forecrewd listens on the worker's loopback.
	hostGateway = "host.docker.internal"
	// containerShm is the shared memory of the container: Chromium needs more
	// than Docker's 64 MiB.
	containerShm = "512m"
	// containerTmpSize bounds each in-memory directory of the container.
	containerTmpSize = "1g"
)

// toolboxImageRepo is the repository of the images built from toolbox files;
// the tag is the toolbox name and a hash of its build context. A variable for
// the tests, which use their own so that they never build or remove the
// images of a worker on the same engine.
var toolboxImageRepo = "forecrew-toolbox"

// Variables for the tests: the most files and bytes a toolbox build context
// may hold.
var (
	maxToolboxFiles       = 2000
	maxToolboxBytes int64 = 64 << 20
)

// toolboxRun is the toolbox container setup of one agent job: the runtime
// adapter's processes run in it (runtime.Container). The container sees the
// work tree, the evidence directory and the repo cache (read only, so that
// git works) at the same paths as the host, an in-memory home and /tmp, and
// the job's private network. It never sees the Docker socket, a repo token
// or the worker's environment.
type toolboxRun struct {
	dj      *dockerJob
	toolbox domain.Toolbox
	mounts  []string
	// secrets are the variables handed to the container by name: their values
	// stay out of the argument list (and so out of the process list) and
	// travel in the environment of the docker client.
	secrets map[string]bool
	// gateway is true when the container needs host.docker.internal.
	gateway bool
	// literal are the variables set to fixed values.
	literal []string
}

var _ runtime.Container = (*toolboxRun)(nil)

// startToolbox prepares the toolbox container of the job: it pulls or builds
// the image and creates the job's network, and logs the versions of the
// toolbox's CLIs. The returned toolboxRun, when not nil, must be cleaned up.
// done is the completion that ends the job, or nil.
func (r *jobRun) startToolbox(parent context.Context) (box *toolboxRun, done *Completion) {
	t := *r.spec.Toolbox
	fail := func(format string, args ...any) *Completion {
		if parent.Err() != nil { // canceled, or the lease is gone
			return &Completion{}
		}
		c := r.failed(format, args...)
		return &c
	}
	if r.w.docker == nil {
		c := r.retry("this worker has no Docker engine (no docker label), needed by toolbox %s", t.Name)
		return nil, &c
	}
	user, err := dockerUser()
	if err != nil {
		return nil, fail("%v", err)
	}
	dj := &dockerJob{r: r, d: r.w.docker, id: r.spec.JobID.String(), env: map[string]string{}, mark: r.spec.JobID.String(), user: user}
	box = &toolboxRun{dj: dj, toolbox: t, secrets: map[string]bool{}}
	if err := box.configure(); err != nil {
		return nil, fail("%v", err)
	}
	r.deadline = time.Now().Add(toolboxBuildTimeout) // the image build, not the agent run
	defer func() { r.deadline = time.Time{} }()
	r.ev.text(domain.EventStatus, "preparing toolbox "+t.Name)
	ctx, cancel := context.WithTimeout(parent, toolboxBuildTimeout)
	defer cancel()
	if dj.tag, err = dj.toolboxImage(ctx, t); err != nil {
		return box, fail("toolbox %s: %v", t.Name, err)
	}
	if err := dj.startNetwork(ctx); err != nil {
		if parent.Err() != nil {
			return box, fail("%v", err)
		}
		c := r.retry("create the job network: %v", err) // the daemon's problem, not the repo's
		return box, &c
	}
	box.logVersions(ctx)
	return box, nil
}

// configure decides what the container gets: its mounts, the variables it
// receives by name and the address of forecrewd.
func (b *toolboxRun) configure() error {
	r := b.dj.r
	b.mounts = []string{r.dir + ":" + r.dir, r.scratch + ":" + r.scratch}
	if r.cache != "" {
		// The worktree's .git file names its admin directory in the cache.
		b.mounts = append(b.mounts, r.cache+":"+r.cache+":ro")
	}
	for _, m := range b.mounts {
		if strings.Count(strings.TrimSuffix(m, ":ro"), ":") != 1 {
			return fmt.Errorf("the path %q cannot be mounted in a container", strings.TrimSuffix(m, ":ro"))
		}
	}
	for k := range r.spec.Env {
		b.secrets[k] = true
	}
	if r.spec.JobMCP != nil {
		b.secrets[runtime.JobMCPTokenEnv] = true
	}
	if k := runtime.KeyEnv(r.spec.Runtime); k != "" {
		b.secrets[k] = true // the worker's own key, when the spec has none
	}
	for _, k := range slices.Sorted(maps.Keys(b.secrets)) {
		if reservedCredential(k) {
			return fmt.Errorf("the credential variable %s is one the Docker client or the loader reads (DOCKER_*, LD_*, proxy and system variables); rename it in forecrew.yaml", k)
		}
	}
	b.literal = append(filterEnv(r.w.cfg.Environ, func(name string) bool {
		n := strings.ToUpper(name)
		return containerEnvNames[n] || strings.HasPrefix(n, "LC_")
	}), "HOME="+runtime.ContainerHome, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	_, b.gateway = containerURL(r.w.cfg.Client.JobMCPURL())
	return nil
}

// reservedCredential reports whether a credential may not be named name: the
// value travels in the environment of the docker client, where these
// variables would change how it connects, loads or runs (DOCKER_*, the
// variables of the worker's own allowlist, LD_*, proxies).
func reservedCredential(name string) bool {
	n := strings.ToUpper(name)
	switch {
	case commandEnvNames[n], n == "XDG_RUNTIME_DIR", strings.HasPrefix(n, "DOCKER_"), strings.HasPrefix(n, "LD_"), strings.HasPrefix(n, "LC_"):
		return true
	}
	return strings.HasSuffix(n, "_PROXY") || n == "NO_PROXY"
}

// JobMCPURL returns the address of the job MCP gateway as the container
// reaches it.
func (b *toolboxRun) JobMCPURL() string {
	u, _ := containerURL(b.dj.r.w.cfg.Client.JobMCPURL())
	return u
}

// containerURL returns raw as a container reaches it. A forecrewd on the
// worker's own loopback is not the container's loopback: the container gets
// host.docker.internal instead, which only works when forecrewd listens on an
// address the Docker bridge can reach (docs/worker.md).
func containerURL(raw string) (rewritten string, viaHost bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return raw, false
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(hostGateway, port)
	} else {
		u.Host = hostGateway
	}
	return u.String(), true
}

// Wrap implements runtime.Container: it turns p into the docker run of p in
// the toolbox container. p.Dir is the work tree; the values of the variables
// in secrets only travel in the environment of the docker client.
func (b *toolboxRun) Wrap(p runtime.Process) runtime.Process {
	dj := b.dj
	uid, gid, _ := strings.Cut(dj.user, ":")
	home := fmt.Sprintf("%s:rw,exec,nosuid,nodev,mode=0700,uid=%s,gid=%s,size=%s", runtime.ContainerHome, uid, gid, containerTmpSize)
	args := append(dj.runArgs(), "-i", "--shm-size", containerShm,
		"--tmpfs", home, "--tmpfs", "/tmp:rw,exec,nosuid,nodev,mode=1777,size="+containerTmpSize)
	for _, m := range b.mounts {
		args = append(args, "-v", m)
	}
	if b.gateway {
		args = append(args, "--add-host", hostGateway+":host-gateway")
	}
	if p.Dir != "" {
		args = append(args, "-w", p.Dir)
	}
	out := dj.d.process()
	out.Env = slices.Clone(out.Env)
	for _, kv := range append(slices.Clone(b.literal), p.Env...) {
		name, _, _ := strings.Cut(kv, "=")
		if b.secrets[name] {
			args = append(args, "-e", name)
			out.Env = append(out.Env, kv)
		} else {
			args = append(args, "-e", kv)
		}
	}
	args = append(args, "--", dj.tag, p.Path)
	out.Args = append(args, p.Args...)
	out.Dir = dj.r.dir
	out.Env = append(out.Env, markEnv+"="+dj.mark)
	out.Timeout = p.Timeout
	return out
}

// logVersions records the version of each CLI the toolbox says it provides in
// the job log; one that does not answer is logged as such.
func (b *toolboxRun) logVersions(ctx context.Context) {
	if len(b.toolbox.CLIs) == 0 {
		return
	}
	var script strings.Builder
	for _, c := range b.toolbox.CLIs {
		// The name is a validated name; the command is the toolbox's own.
		fmt.Fprintf(&script, "printf '%%s: ' %s; ( %s ) 2>&1 | head -n 1\n", c.Name, c.Version)
	}
	r := b.dj.r
	p := b.Wrap(runtime.Process{Path: "sh", Args: []string{"-c", script.String()}, Dir: r.dir, Env: nil, Timeout: toolboxVersionTimeout})
	_, err := runtime.Exec(ctx, p, func(line []byte) {
		r.ev.text(domain.EventLog, "toolbox "+b.toolbox.Name+" cli "+cleanText(string(line)))
	}, nil)
	if err != nil && ctx.Err() == nil {
		r.ev.text(domain.EventLog, "toolbox "+b.toolbox.Name+": reading the CLI versions failed: "+cleanText(r.red.String(err.Error())))
	}
}

// cleanup stops and removes the toolbox container and the job's network.
func (b *toolboxRun) cleanup(ctx context.Context) { b.dj.cleanup(ctx) }

// toolboxImage makes the image of toolbox t available and returns its tag. A
// toolbox with an image is pulled (recorded for pruning). One with a
// Dockerfile is built from the toolbox files the worker ships, reusing an
// image built before for the same files; a Dockerfile that starts from the
// Forecrew base gets the base, with the agent CLIs, built first.
func (dj *dockerJob) toolboxImage(ctx context.Context, t domain.Toolbox) (string, error) {
	if t.Image != "" {
		return t.Image, dj.pull(ctx, t.Image)
	}
	files := dj.r.w.cfg.Toolboxes
	if files == nil {
		return "", errors.New("this worker has no toolbox files")
	}
	text, err := fs.ReadFile(files, t.Dockerfile)
	if err != nil {
		return "", fmt.Errorf("dockerfile %s: %w (the worker's toolbox files may be older than forecrewd's)", t.Dockerfile, err)
	}
	var extra []string
	hash := sha256.New()
	fmt.Fprintf(hash, "forecrew-toolbox %s\nname %q\ndockerfile %q\n", toolboxHashVersion, t.Name, t.Dockerfile)
	if strings.Contains(string(text), toolboxBaseArg) {
		base, err := dj.toolboxBase(ctx, files)
		if err != nil {
			return "", err
		}
		extra = []string{"--build-arg", toolboxBaseArg + "=" + base}
		fmt.Fprintf(hash, "base %q\n", base)
	}
	if err := hashFS(hash, files); err != nil {
		return "", err
	}
	tag := toolboxImageRepo + ":" + t.Name + "-" + hex.EncodeToString(hash.Sum(nil))[:24]
	return tag, dj.ensureBuilt(ctx, files, t.Dockerfile, tag, extra...)
}

// toolboxBase builds the base image from the files of toolboxBaseDir and
// returns its tag.
func (dj *dockerJob) toolboxBase(ctx context.Context, files fs.FS) (string, error) {
	sub, err := fs.Sub(files, toolboxBaseDir)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "forecrew-toolbox-base %s\n", toolboxHashVersion)
	if err := hashFS(hash, sub); err != nil {
		return "", fmt.Errorf("base image: %w", err)
	}
	tag := toolboxImageRepo + ":base-" + hex.EncodeToString(hash.Sum(nil))[:24]
	if err := dj.ensureBuilt(ctx, sub, "Dockerfile", tag); err != nil {
		return "", fmt.Errorf("base image: %w", err)
	}
	return tag, nil
}

// ensureBuilt builds the Dockerfile of the files (which are the build
// context) as the image tag unless the daemon has it. Every use is recorded
// for pruning.
func (dj *dockerJob) ensureBuilt(ctx context.Context, files fs.FS, dockerfile, tag string, extra ...string) error {
	markers := dj.r.w.imageMarkers()
	markers.touch(tag, dj.r.w.cfg.Now())
	if _, err := dj.d.output(ctx, "image", "inspect", "--format", "{{.Id}}", "--", tag); err == nil {
		dj.r.ev.text(domain.EventStatus, "toolbox image cached")
		return nil
	}
	dj.r.ev.text(domain.EventStatus, "building toolbox image")
	dir, err := os.MkdirTemp(dj.r.w.cfg.WorkDir, "toolbox-build-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := extractFS(files, dir); err != nil {
		return err
	}
	text, err := fs.ReadFile(files, dockerfile)
	if err != nil {
		return err
	}
	return dj.buildFile(ctx, string(text), filepath.Join(dir, filepath.FromSlash(dockerfile)), dir, tag, extra...)
}

// walkFiles calls fn for every regular file of files in path order. Anything
// else (a symlink, a device) is refused, and so is a tree beyond the limits.
func walkFiles(files fs.FS, fn func(name string, info fs.FileInfo) error) error {
	n, total := 0, int64(0)
	return fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("toolbox file %s is not a regular file", name)
		}
		if n++; n > maxToolboxFiles {
			return fmt.Errorf("more than %d toolbox files", maxToolboxFiles)
		}
		if total += info.Size(); total > maxToolboxBytes {
			return fmt.Errorf("toolbox files larger than %d MiB", maxToolboxBytes>>20)
		}
		return fn(name, info)
	})
}

// hashFS writes every file of files (path, mode and contents) to h.
func hashFS(h io.Writer, files fs.FS) error {
	return walkFiles(files, func(name string, info fs.FileInfo) error {
		fmt.Fprintf(h, "f %q %d %o\n", name, info.Size(), info.Mode().Perm())
		f, err := files.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(h, f)
		return err
	})
}

// extractFS writes the files to the directory dir.
func extractFS(files fs.FS, dir string) error {
	return walkFiles(files, func(name string, info fs.FileInfo) error {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600|info.Mode().Perm()&0o100) // #nosec G306 -- a build context, removed after the build
	})
}

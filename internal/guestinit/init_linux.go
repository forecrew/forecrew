package guestinit

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/worker"
)

// Where things are in the guest. A test puts them under a directory.
const (
	workDir      = "/var/lib/forecrew-worker/work"
	runDir       = "/run/forecrew"
	labelsFile   = "/etc/forecrew/labels"
	imageEnvFile = "/etc/forecrew/env"
	workerBin    = "/opt/forecrew/forecrew-worker"
	cacheDir     = "/var/cache/forecrew"
	// orgCacheDir is the organization's own cache, which its jobs write.
	orgCacheDir   = "/var/cache/forecrew-org"
	dockerSocket  = "/var/run/docker.sock"
	defaultConfig = "/dev/vdb"
	defaultRoot   = "/dev/vda"
	// basePath is the worker's PATH until the image's environment says more.
	basePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// dockerWait bounds how long a machine waits for its Docker daemon.
	dockerWait = 30 * time.Second
	// maxImageFile bounds what is read of the image's files.
	maxImageFile = 1 << 20
	// dialTimeout bounds one address of the server.
	dialTimeout = 5 * time.Second
	// exchangeTimeout bounds one call to the server.
	exchangeTimeout = 30 * time.Second
)

// system is what PID 1 does to the machine that only a real one can do.
type system interface {
	mountProc() error
	mountAll() error
	setClock(t time.Time) error
	loadModules(paths []string) error
	network(ip, gateway string) error
	growRoot(device string) error
	mountCache(device, target string, writable bool) error
	// wait returns the exit status of pid, reaping what else ends meanwhile.
	wait(pid int) int
	halt(how string) error
}

// guest is one boot.
type guest struct {
	sys     system
	root    string // prefix of every path; empty on a machine
	version string
	say     func(format string, args ...any)
	// dockerWait defaults to the constant above.
	dockerWait time.Duration
}

func (g *guest) path(p string) string { return g.root + p }

// boot runs the machine until its worker ends, then ends the machine.
func (g *guest) boot(ctx context.Context) {
	args := map[string]string{}
	if err := g.run(ctx, args); err != nil {
		g.say("%v", err)
	}
	if err := g.sys.halt(args["halt"]); err != nil {
		g.say("halt: %v", err)
	}
}

func (g *guest) run(ctx context.Context, args map[string]string) error {
	if err := g.sys.mountProc(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	// The kernel would run the image's modprobe, as root, to load a module
	// anything below asks for: no image program runs before the exchange.
	restore, err := g.blockModprobe()
	if err != nil {
		return err
	}
	if err := g.sys.mountAll(); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	line, err := os.ReadFile(g.path("/proc/cmdline"))
	if err != nil {
		return fmt.Errorf("kernel command line: %w", err)
	}
	maps.Copy(args, parseCmdline(string(line)))
	if err := g.prepare(args); err != nil {
		return err
	}
	s, err := g.takeSettings(cmp.Or(args["config"], defaultConfig))
	if err != nil {
		return err
	}
	// The one call that uses the token. Until it returns nothing of the image
	// has run, and after it the token is worth nothing.
	if s.Time.After(time.Now()) {
		// The guest's clock is behind the moment the host made it.
		if err := g.sys.setClock(s.Time); err != nil {
			g.say("set the clock: %v", err)
		}
	}
	grant, err := exchange(ctx, s, g.version)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := restore(); err != nil {
		g.say("%v", err)
	}
	g.pinServer(s)
	cfg, err := g.workerConfig(s.ServerURL, grant)
	if err != nil {
		return err
	}
	env := g.environment()
	g.startDocker(ctx, env)
	return g.runWorker(ctx, cfg, env, grant.WorkerToken)
}

// modprobeOff is what the kernel is told to run to load a module: nothing.
const modprobeOff = "/nonexistent/forecrew-modprobe-off"

// blockModprobe turns off the kernel's module autoloader, and returns what turns
// it back on.
func (g *guest) blockModprobe() (func() error, error) {
	path := g.path("/proc/sys/kernel/modprobe")
	old, err := os.ReadFile(path) // #nosec G304 -- a fixed path
	if errors.Is(err, fs.ErrNotExist) {
		// A kernel without modules has no loader: nothing to turn off.
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, fmt.Errorf("module loader: %w", err)
	}
	if err := os.WriteFile(path, []byte(modprobeOff+"\n"), 0o644); err != nil { // #nosec G306 -- a file of /proc
		return nil, fmt.Errorf("turn off the module loader: %w", err)
	}
	return func() error {
		if err := os.WriteFile(path, old, 0o644); err != nil { // #nosec G306 G703 -- a file of /proc
			return fmt.Errorf("turn the module loader back on: %w", err)
		}
		return nil
	}, nil
}

// prepare makes the machine ready. A disk that does not grow or a missing cache
// is no reason to give up; a machine with no network is.
func (g *guest) prepare(args map[string]string) error {
	if m := args["modules"]; m != "" {
		if err := g.sys.loadModules(strings.Split(m, ",")); err != nil {
			g.say("%v", err)
		}
	}
	if err := g.sys.network(args["ip"], args["gw"]); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if dns := args["dns"]; dns != "" {
		resolv := g.path("/etc/resolv.conf")
		_ = os.Remove(resolv)                                                               // a link of the image's goes, not what it points at
		if err := os.WriteFile(resolv, []byte("nameserver "+dns+"\n"), 0o644); err != nil { // #nosec G306 -- every user of the guest resolves names
			g.say("write resolv.conf: %v", err)
		}
	}
	if err := g.sys.growRoot(cmp.Or(args["root"], defaultRoot)); err != nil {
		g.say("grow the root disk: %v", err)
	}
	if c := args["cache"]; c != "" {
		if err := g.sys.mountCache(c, g.path(cacheDir), false); err != nil {
			g.say("mount the cache disk: %v", err)
		}
	}
	if c := args["orgcache"]; c != "" {
		if err := g.sys.mountCache(c, g.path(orgCacheDir), true); err != nil {
			g.say("mount the organization's cache disk: %v", err)
		}
	}
	return nil
}

// takeSettings reads the config disk and wipes it, so that what runs later
// finds no token there. A disk that cannot be wiped stops the machine.
func (g *guest) takeSettings(device string) (Settings, error) {
	f, err := os.OpenFile(g.path(device), os.O_RDWR, 0) // #nosec G304 -- a device the provider named
	if err != nil {
		return Settings{}, fmt.Errorf("config disk: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes))
	if err != nil {
		return Settings{}, fmt.Errorf("config disk: %w", err)
	}
	s, err := Decode(strings.NewReader(string(raw)))
	if err != nil {
		return Settings{}, err
	}
	if _, err := f.WriteAt(make([]byte, len(raw)), 0); err != nil {
		return Settings{}, fmt.Errorf("wipe the config disk: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Settings{}, fmt.Errorf("wipe the config disk: %w", err)
	}
	return s, nil
}

// exchange trades the token for a worker token. It trusts only the settings: no
// proxy, no name lookup and no CA of the image.
func exchange(ctx context.Context, s Settings, version string) (worker.Grant, error) {
	client, err := pinnedClient(s)
	if err != nil {
		return worker.Grant{}, err
	}
	return worker.NewClient(s.ServerURL, s.Token, client).Bootstrap(ctx, version)
}

// pinnedClient returns a client that dials only s.ServerAddrs and, over https,
// trusts only s.CAPEM.
func pinnedClient(s Settings) (*http.Client, error) {
	u, err := url.Parse(s.ServerURL)
	if err != nil {
		return nil, errors.New("config disk: bad server URL")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, ip := range s.ServerAddrs {
				d := net.Dialer{Timeout: dialTimeout}
				c, err := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
	}
	if u.Scheme == "https" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s.CAPEM)) {
			return nil, errors.New("config disk: an https server needs a CA")
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: t, Timeout: exchangeTimeout}, nil
}

// pinServer gives the worker the address the host resolved: the name may be one
// only the host's network knows, like forecrew.lan.
func (g *guest) pinServer(s Settings) {
	u, err := url.Parse(s.ServerURL)
	if err != nil || net.ParseIP(u.Hostname()) != nil {
		return
	}
	f, err := os.OpenFile(g.path("/etc/hosts"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644) // #nosec G302,G304 -- every user of the guest resolves names
	if err == nil {
		_, err = fmt.Fprintf(f, "%s %s\n", s.ServerAddrs[0], u.Hostname())
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		g.say("write hosts: %v", err)
	}
}

// workerConfig writes worker.yaml, in RAM. The labels of the image are the
// image's word for what it offers, as they are for any worker.
func (g *guest) workerConfig(serverURL string, grant worker.Grant) (string, error) {
	labels := slices.Concat(grant.Labels, g.imageLines(labelsFile))
	cfg := struct {
		ServerURL string   `yaml:"server_url"`
		Name      string   `yaml:"name,omitempty"`
		WorkDir   string   `yaml:"work_dir"`
		MaxJobs   int      `yaml:"max_jobs"`
		Labels    []string `yaml:"labels,omitempty"`
	}{serverURL, grant.Name, g.path(workDir), max(grant.MaxJobs, 1), domain.NormalizeLabels(labels)}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("worker config: %w", err)
	}
	dir := g.path(runDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("worker config: %w", err)
	}
	if err := os.MkdirAll(g.path(workDir), 0o755); err != nil { // #nosec G301 -- the jobs' own directory
		return "", fmt.Errorf("work directory: %w", err)
	}
	path := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("worker config: %w", err)
	}
	return path, nil
}

// imageLines reads a file of the image as data: lines, without blanks and
// comments. A missing file has none.
func (g *guest) imageLines(path string) []string {
	f, err := os.Open(g.path(path)) // #nosec G304 -- a fixed path
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(io.LimitReader(f, maxImageFile))
	sc.Buffer(nil, maxImageFile)
	for sc.Scan() {
		if l := sc.Text(); strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// environment is what the worker and the daemons it needs start with: the
// image's own, read as data, without anything of Forecrew's.
func (g *guest) environment() []string {
	env := []string{"PATH=" + basePath, "HOME=/root"}
	for _, l := range g.imageLines(imageEnvFile) {
		name, _, ok := strings.Cut(l, "=")
		if !ok || name == "" || strings.HasPrefix(name, "FORECREW_") || strings.ContainsFunc(name, notEnvName) || steersTraffic(name) {
			continue
		}
		if i := slices.IndexFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") }); i >= 0 {
			env[i] = l
			continue
		}
		env = append(env, l)
	}
	return env
}

// steersTraffic reports a variable that would send the worker's calls, and its
// token, through the image's choice of proxy or trust, or change how Go checks
// them.
func steersTraffic(name string) bool {
	n := strings.ToUpper(name)
	return strings.HasSuffix(n, "_PROXY") || strings.HasPrefix(n, "SSL_CERT_") || n == "GODEBUG"
}

func notEnvName(r rune) bool {
	return r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
}

// startDocker starts the image's Docker daemon when it has one, in the guest's
// own kernel, and waits for it: the worker looks for Docker once, at its start.
func (g *guest) startDocker(ctx context.Context, env []string) {
	bin := g.findCommand("dockerd", env)
	if bin == "" {
		return
	}
	cmd := exec.CommandContext(ctx, bin) // #nosec G204 -- the image's daemon, after the token is spent
	cmd.Env = env
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil {
		g.say("start dockerd: %v", err)
		return
	}
	wait := g.dockerWait
	if wait == 0 {
		wait = dockerWait
	}
	for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(g.path(dockerSocket)); err == nil {
			return
		}
	}
	g.say("dockerd is not up after %s; the machine goes on without it", wait)
}

// findCommand looks in the PATH of env, in the guest's root.
func (g *guest) findCommand(name string, env []string) string {
	for _, e := range env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			for dir := range strings.SplitSeq(p, ":") {
				path := g.path(filepath.Join(dir, name))
				if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
					return path
				}
			}
		}
	}
	return ""
}

// runWorker runs the worker and waits for it: a machine lives for its jobs.
func (g *guest) runWorker(ctx context.Context, configPath string, env []string, token string) error {
	cmd := exec.CommandContext(ctx, g.path(workerBin), "run", "--config", configPath) // #nosec G204 -- fixed path
	cmd.Env = append(slices.Clone(env), config.EnvWorkerToken+"="+token)
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the worker: %w", err)
	}
	g.say("forecrew-worker ended with status %d", g.sys.wait(cmd.Process.Pid))
	return nil
}

// Main is PID 1. It does not return.
func Main(version string) {
	console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		console = os.Stderr
	}
	g := &guest{sys: machine{}, version: version, say: func(format string, args ...any) {
		fmt.Fprintf(console, "forecrew-init: "+format+"\n", args...)
	}}
	g.boot(context.Background())
	select {} // PID 1 must not end: the kernel would panic
}

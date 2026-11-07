package worker

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// AgentManifestFile is the manifest of the agent CLIs a worker may install,
// in the files the binaries ship with.
const AgentManifestFile = "packs/agents/manifest.yaml"

// Limits of an install.
const (
	// maxAgentDownload bounds a download: the largest CLI is under 300 MB.
	maxAgentDownload = 1 << 30
	// installRetry is how long a failed install is not tried again for the
	// same version.
	installRetry = time.Hour
	// downloadTimeout bounds one download.
	downloadTimeout = 15 * time.Minute
)

// AgentManifest lists, per agent, the pinned version and the official
// address and SHA-256 of its standalone binary for each OS and architecture.
type AgentManifest struct {
	Version int                     `yaml:"version"`
	Agents  map[string]AgentRelease `yaml:"agents"`
}

// AgentRelease is one agent of the manifest.
type AgentRelease struct {
	Version string `yaml:"version"`
	// Binary is the name of the CLI on PATH; it must be the adapter's own.
	Binary string `yaml:"binary"`
	// Platforms holds a download per "<goos>/<goarch>".
	Platforms map[string]AgentDownload `yaml:"platforms"`
}

// AgentDownload is where one build of an agent comes from.
type AgentDownload struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"` // of the file at URL
	// Archive is empty for a bare binary, tar.gz or zip.
	Archive string `yaml:"archive"`
	// Path is the name of the binary inside the archive.
	Path string `yaml:"path"`
}

var (
	exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	platformKey  = regexp.MustCompile(`^(linux|darwin|windows)/(amd64|arm64)$`)
	// adapterName is what an install request may name: nothing else, so a
	// request cannot carry a version or an address.
	adapterName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// ParseAgentManifest decodes and checks a manifest: only the adapters the
// worker knows, each with its own binary name, an exact version, and for
// every platform an https address that names the version and a SHA-256.
func ParseAgentManifest(data []byte) (AgentManifest, error) {
	var m AgentManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return AgentManifest{}, fmt.Errorf("agent manifest: %w", err)
	}
	if m.Version != 1 {
		return AgentManifest{}, fmt.Errorf("agent manifest: version %d is not 1", m.Version)
	}
	for name, a := range m.Agents {
		if err := a.check(name); err != nil {
			return AgentManifest{}, fmt.Errorf("agent manifest: %s: %w", name, err)
		}
	}
	return m, nil
}

func (a AgentRelease) check(adapter string) error {
	i := slices.IndexFunc(runtimeCLIs, func(c struct{ binary, adapter string }) bool { return c.adapter == adapter })
	switch {
	case i < 0:
		return errors.New("not an adapter of this worker")
	case a.Binary != runtimeCLIs[i].binary:
		return fmt.Errorf("binary %q is not %q", a.Binary, runtimeCLIs[i].binary)
	case !exactVersion.MatchString(a.Version):
		return fmt.Errorf("version %q is not an exact version", a.Version)
	case len(a.Platforms) == 0:
		return errors.New("no platforms")
	}
	for p, d := range a.Platforms {
		if !platformKey.MatchString(p) {
			return fmt.Errorf("platform %q is not <os>/<arch>", p)
		}
		u, err := url.Parse(d.URL)
		switch {
		case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil:
			return fmt.Errorf("%s: %q is not an https address", p, d.URL)
		case !strings.Contains(u.Path, a.Version):
			return fmt.Errorf("%s: %q does not name version %s", p, d.URL, a.Version)
		case !sha256Hex.MatchString(d.SHA256):
			return fmt.Errorf("%s: sha256 is not 64 lowercase hex digits", p)
		case d.Archive != "" && d.Archive != "tar.gz" && d.Archive != "zip":
			return fmt.Errorf("%s: archive %q is not tar.gz or zip", p, d.Archive)
		case (d.Archive == "") != (d.Path == ""):
			return fmt.Errorf("%s: path names the binary inside an archive, and only there", p)
		case d.Path != "" && (strings.ContainsAny(d.Path, `/\`) || d.Path == "." || d.Path == ".."):
			return fmt.Errorf("%s: path %q is not a file name", p, d.Path)
		}
	}
	return nil
}

// DefaultAgentManifest is the manifest of this release.
func DefaultAgentManifest() (AgentManifest, error) {
	data, err := fs.ReadFile(forecrew.Files, AgentManifestFile)
	if err != nil {
		return AgentManifest{}, err
	}
	return ParseAgentManifest(data)
}

// agentState is what the worker installed and what failed, kept in its
// agents directory so a restart and `forecrew-worker status` see it.
type agentState struct {
	Installed map[string]installedAgent `json:"installed"`
	Failed    map[string]failedInstall  `json:"failed"`
}

// installedAgent records an install: what, from where, and when.
type installedAgent struct {
	Version     string    `json:"version"`
	URL         string    `json:"url"`
	SHA256      string    `json:"sha256"`
	InstalledAt time.Time `json:"installed_at"`
}

// failedInstall is the last failure of an install of a version.
type failedInstall struct {
	Version string    `json:"version"`
	Code    string    `json:"code"`
	Detail  string    `json:"detail"`
	At      time.Time `json:"at"`
}

// installError is a failed install: a code for forecrewd and the detail for
// the machine's own log and status.
type installError struct {
	code   string
	detail string
}

func (e *installError) Error() string { return e.detail }

// errUnknownAgent means the request named no agent of the manifest; it is
// ignored, not recorded.
var errUnknownAgent = errors.New("not an agent of this release's manifest")

// agentInstaller installs agent CLIs from the manifest into the worker's
// own directory: never system-wide, never with more rights than the worker.
type agentInstaller struct {
	dir      string // <work_dir>/agents; the binaries are in dir/bin
	manifest AgentManifest
	goos     string
	platform string // <goos>/<goarch>
	client   *http.Client
	now      func() time.Time
	mu       sync.Mutex
}

// newAgentInstaller returns the installer of the agents directory dir.
// client nil means one that follows https redirects only.
func newAgentInstaller(dir string, m AgentManifest, goos, goarch string, client *http.Client, now func() time.Time) *agentInstaller {
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}
	c := *client
	// The manifest's address is https; a redirect may not leave it.
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to %s refused: not https", req.URL.Scheme)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &agentInstaller{dir: dir, manifest: m, goos: goos, platform: goos + "/" + goarch, client: &c, now: now}
}

// agentsDir is where a worker keeps the agents it installs.
func agentsDir(workDir string) string { return filepath.Join(workDir, "agents") }

// binDir is the directory put first on PATH.
func (a *agentInstaller) binDir() string { return filepath.Join(a.dir, "bin") }

func (a *agentInstaller) statePath() string { return filepath.Join(a.dir, "state.json") }

// exe is the file name of a binary on the installer's OS.
func exe(goos, name string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// readAgentState reads the state of the agents directory dir; a missing or
// unreadable file is an empty state.
func readAgentState(dir string) agentState {
	s := agentState{Installed: map[string]installedAgent{}, Failed: map[string]failedInstall{}}
	data, err := os.ReadFile(filepath.Join(dir, "state.json")) // #nosec G304 -- the worker's own directory
	if err != nil || json.Unmarshal(data, &s) != nil {
		return agentState{Installed: map[string]installedAgent{}, Failed: map[string]failedInstall{}}
	}
	if s.Installed == nil {
		s.Installed = map[string]installedAgent{}
	}
	if s.Failed == nil {
		s.Failed = map[string]failedInstall{}
	}
	return s
}

func (a *agentInstaller) writeState(s agentState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath())
}

// prepare creates the private directory and drops what no longer holds: an
// install of another version than this release's manifest, or one whose
// binary is gone. Such an agent is installed again when a job needs it.
func (a *agentInstaller) prepare() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(a.binDir(), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(a.dir, 0o700); err != nil { // #nosec G302 -- a directory needs its execute bit; only the worker's user has any
		return err
	}
	// A download a crash cut short is never used.
	for _, pattern := range []string{".download-*", ".binary-*"} {
		left, _ := filepath.Glob(filepath.Join(a.dir, pattern))
		for _, f := range left {
			_ = os.Remove(f)
		}
	}
	s := readAgentState(a.dir)
	changed := false
	for name, in := range s.Installed {
		rel, ok := a.manifest.Agents[name]
		bin := filepath.Join(a.binDir(), exe(a.goos, rel.Binary))
		if _, err := os.Stat(bin); ok && err == nil && in.Version == rel.Version {
			continue
		}
		if ok {
			_ = os.Remove(bin)
		}
		delete(s.Installed, name)
		changed = true
	}
	if !changed {
		return nil
	}
	return a.writeState(s)
}

// due reports whether adapter may be installed now: it is in the manifest,
// and no install of its version failed within installRetry.
func (a *agentInstaller) due(adapter string) bool {
	rel, ok := a.manifest.Agents[adapter]
	if !ok {
		return false
	}
	f, failed := readAgentState(a.dir).Failed[adapter]
	return !failed || f.Version != rel.Version || a.now().Sub(f.At) >= installRetry
}

// Install downloads adapter's binary from the manifest, checks its SHA-256
// and puts it in the private bin directory. A failure is recorded; a wrong
// checksum deletes the download.
func (a *agentInstaller) Install(ctx context.Context, adapter string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rel, ok := a.manifest.Agents[adapter]
	if !ok {
		return fmt.Errorf("%q: %w", adapter, errUnknownAgent)
	}
	err := os.MkdirAll(a.binDir(), 0o700)
	if err == nil {
		err = a.install(ctx, rel, rel.Platforms[a.platform])
	}
	s := readAgentState(a.dir)
	if err == nil {
		d := rel.Platforms[a.platform]
		s.Installed[adapter] = installedAgent{Version: rel.Version, URL: d.URL, SHA256: d.SHA256, InstalledAt: a.now()}
		delete(s.Failed, adapter)
		return a.writeState(s)
	}
	ie, ok := errors.AsType[*installError](err)
	if !ok {
		ie = &installError{code: domain.AgentInstallWrite, detail: err.Error()}
	}
	s.Failed[adapter] = failedInstall{Version: rel.Version, Code: ie.code, Detail: ie.detail, At: a.now()}
	if werr := a.writeState(s); werr != nil {
		return errors.Join(ie, werr)
	}
	return ie
}

func (a *agentInstaller) install(ctx context.Context, rel AgentRelease, d AgentDownload) error {
	if d.URL == "" {
		return &installError{domain.AgentInstallUnsupported, fmt.Sprintf("this release has no standalone binary of version %s for %s", rel.Version, a.platform)}
	}
	tmp, err := a.download(ctx, d)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	bin := tmp
	if d.Archive != "" {
		if bin, err = a.extract(tmp, d); err != nil {
			return err
		}
		defer func() { _ = os.Remove(bin) }()
	}
	if err := os.Chmod(bin, 0o700); err != nil { // #nosec G302 -- the binary must run; only the worker's user may
		return &installError{domain.AgentInstallWrite, err.Error()}
	}
	if err := os.Rename(bin, filepath.Join(a.binDir(), exe(a.goos, rel.Binary))); err != nil {
		return &installError{domain.AgentInstallWrite, err.Error()}
	}
	return nil
}

// download saves d.URL to a file of the agents directory and checks its
// SHA-256 as it reads; a mismatch deletes the file.
func (a *agentInstaller) download(ctx context.Context, d AgentDownload) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return "", &installError{domain.AgentInstallDownload, err.Error()}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", &installError{domain.AgentInstallDownload, "download " + d.URL + ": " + redactURLError(err).Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &installError{domain.AgentInstallDownload, fmt.Sprintf("download %s: %s", d.URL, resp.Status)}
	}
	f, err := os.CreateTemp(a.dir, ".download-*")
	if err != nil {
		return "", &installError{domain.AgentInstallWrite, err.Error()}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAgentDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	sum := hex.EncodeToString(h.Sum(nil))
	switch {
	case err != nil:
		err = &installError{domain.AgentInstallDownload, "download " + d.URL + ": " + err.Error()}
	case n > maxAgentDownload:
		err = &installError{domain.AgentInstallDownload, fmt.Sprintf("download %s: larger than %d bytes", d.URL, maxAgentDownload)}
	case sum != d.SHA256:
		err = &installError{domain.AgentInstallChecksum, fmt.Sprintf("download %s: SHA-256 %s is not %s of the manifest; the file was deleted", d.URL, sum, d.SHA256)}
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extract writes the binary d.Path of the archive file to a file of the
// agents directory.
func (a *agentInstaller) extract(file string, d AgentDownload) (string, error) {
	out, err := os.CreateTemp(a.dir, ".binary-*")
	if err != nil {
		return "", &installError{domain.AgentInstallWrite, err.Error()}
	}
	found, err := copyFromArchive(file, d, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && !found {
		err = &installError{domain.AgentInstallArchive, fmt.Sprintf("%s does not hold %s", d.URL, d.Path)}
	}
	if err != nil {
		_ = os.Remove(out.Name())
		if _, ok := errors.AsType[*installError](err); !ok {
			err = &installError{domain.AgentInstallArchive, fmt.Sprintf("%s: %v", d.URL, err)}
		}
		return "", err
	}
	return out.Name(), nil
}

// entryName is an archive entry's name without a leading ./ or directory.
func entryName(name string) string {
	return path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), "./"))
}

// copyFromArchive copies the regular file d.Path of the archive to out.
func copyFromArchive(file string, d AgentDownload, out io.Writer) (bool, error) {
	if d.Archive == "zip" {
		return copyFromZip(file, d.Path, out)
	}
	return copyFromTarGz(file, d.Path, out)
}

func copyFromZip(file, name string, out io.Writer) (bool, error) {
	z, err := zip.OpenReader(file)
	if err != nil {
		return false, err
	}
	defer func() { _ = z.Close() }()
	for _, e := range z.File {
		if entryName(e.Name) != name || !e.Mode().IsRegular() {
			continue
		}
		r, err := e.Open()
		if err != nil {
			return false, err
		}
		defer func() { _ = r.Close() }()
		return true, copyLimited(out, r)
	}
	return false, nil
}

func copyFromTarGz(file, name string, out io.Writer) (bool, error) {
	f, err := os.Open(file) // #nosec G304 -- the worker's own download
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if h.Typeflag == tar.TypeReg && entryName(h.Name) == name {
			return true, copyLimited(out, tr)
		}
	}
}

func copyLimited(out io.Writer, r io.Reader) error {
	n, err := io.Copy(out, io.LimitReader(r, maxAgentDownload+1))
	if err == nil && n > maxAgentDownload {
		err = fmt.Errorf("larger than %d bytes", maxAgentDownload)
	}
	return err
}

// lookPath finds a CLI in the private bin directory first, then on PATH.
func (a *agentInstaller) lookPath(name string) (string, error) {
	p := filepath.Join(a.binDir(), exe(a.goos, name))
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p, nil
	}
	return exec.LookPath(name)
}

// report lists every adapter's CLI as the worker has it, for forecrewd:
// image on a fleet machine, installed when it came from the manifest,
// system when found on PATH; missing with the code of a failed install.
func (a *agentInstaller) report(lookPath func(string) (string, error), fleet bool) []domain.WorkerAgent {
	s := agentState{}
	if a != nil {
		s = readAgentState(a.dir)
	}
	out := make([]domain.WorkerAgent, 0, len(runtimeCLIs))
	for _, cli := range runtimeCLIs {
		ag := domain.WorkerAgent{Adapter: cli.adapter}
		in, installed := s.Installed[cli.adapter]
		found, err := lookPath(cli.binary)
		switch {
		case err != nil:
			// A failure stops being reported once the install may be tried
			// again, so forecrewd asks again.
			if f, failed := s.Failed[cli.adapter]; failed && (a == nil || a.manifest.Agents == nil || !a.due(cli.adapter)) {
				ag.Error = f.Code
			}
		case installed && a != nil && filepath.Dir(found) == a.binDir():
			ag.Source, ag.Version = domain.AgentSourceInstalled, in.Version
		case fleet:
			ag.Source = domain.AgentSourceImage
		default:
			ag.Source = domain.AgentSourceSystem
		}
		out = append(out, ag)
	}
	return out
}

// prependPath puts dir first on the PATH of environ (added when missing).
func prependPath(environ []string, dir string) []string {
	out := slices.Clone(environ)
	for i, kv := range out {
		// Windows spells it Path.
		if name, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(name, "PATH") {
			out[i] = name + "=" + dir + string(os.PathListSeparator) + v
			return out
		}
	}
	return append(out, "PATH="+dir)
}

// currentLabels are the labels the worker polls with.
func (w *Worker) currentLabels() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.labels
}

// agentReport is what the worker tells forecrewd of its agent CLIs.
func (w *Worker) agentReport() []domain.WorkerAgent {
	look := exec.LookPath
	if w.cfg.agents != nil {
		look = w.cfg.agents.lookPath
	}
	return w.cfg.agents.report(look, w.cfg.fleet)
}

// installAgent installs the agent forecrewd asked for, when the worker may:
// the request is only an adapter name, and everything else comes from the
// worker's own manifest. A request for anything else is ignored, as is one
// for an agent the worker has or failed to install within installRetry.
func (w *Worker) installAgent(ctx context.Context, adapter string) {
	a := w.cfg.agents
	if adapter == "" || a == nil {
		return
	}
	if !adapterName.MatchString(adapter) {
		w.cfg.Logger.Warn("agent install request ignored: not an adapter name")
		return
	}
	label := domain.RuntimeLabel(adapter)
	if slices.Contains(w.currentLabels(), label) {
		return
	}
	if _, ok := a.manifest.Agents[adapter]; !ok {
		w.cfg.Logger.Warn("agent install request ignored: not in this release's manifest", "adapter", adapter)
		return
	}
	if !a.due(adapter) {
		return
	}
	rel := a.manifest.Agents[adapter]
	w.cfg.Logger.Info("installing agent", "adapter", adapter, "version", rel.Version, "url", rel.Platforms[a.platform].URL)
	if err := a.Install(ctx, adapter); err != nil {
		w.cfg.Logger.Warn("agent install failed", "adapter", adapter, "version", rel.Version, "error", err)
		return
	}
	w.mu.Lock()
	w.labels = domain.NormalizeLabels(append(slices.Clone(w.labels), label))
	w.mu.Unlock()
	w.cfg.Logger.Info("agent installed", "adapter", adapter, "version", rel.Version, "dir", a.binDir())
}

// Status prints the agent CLIs of the worker of cfg and where each comes
// from (image, installed, system), with the last failed install of a missing
// one. It reads only the machine: no server is needed.
func Status(cfg *config.Worker, environ []string, out io.Writer) {
	fleet := slices.ContainsFunc(environ, func(kv string) bool {
		v, ok := strings.CutPrefix(kv, config.EnvBootstrapToken+"=")
		return ok && v != ""
	})
	dir := agentsDir(cfg.WorkDir)
	a := &agentInstaller{dir: dir, goos: goruntime.GOOS, platform: goruntime.GOOS + "/" + goruntime.GOARCH}
	look := exec.LookPath
	switch {
	case fleet:
		fmt.Fprintln(out, "auto_install_agents: off (a fleet machine uses the agents of its image)")
	case cfg.InstallsAgents():
		look = a.lookPath
		fmt.Fprintf(out, "auto_install_agents: on, into %s\n", a.binDir())
	default:
		fmt.Fprintln(out, "auto_install_agents: off")
	}
	s := readAgentState(dir)
	for _, ag := range a.report(look, fleet) {
		cli := runtimeCLIs[slices.IndexFunc(runtimeCLIs, func(c struct{ binary, adapter string }) bool { return c.adapter == ag.Adapter })]
		if ag.Source == "" {
			line := ag.Adapter + ": missing"
			if f, ok := s.Failed[ag.Adapter]; ok {
				line += fmt.Sprintf("; install of %s failed at %s: %s", f.Version, f.At.Format(time.RFC3339), f.Detail)
			}
			fmt.Fprintln(out, line)
			continue
		}
		where, _ := look(cli.binary)
		version := ""
		if ag.Version != "" {
			version = " " + ag.Version
		}
		fmt.Fprintf(out, "%s: %s%s (%s)\n", ag.Adapter, ag.Source, version, where)
	}
}

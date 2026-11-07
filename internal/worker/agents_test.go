package worker

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

const fakeCLI = "#!/bin/sh\necho fake codex 1.2.3\n"

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must(t, tw.WriteHeader(&tar.Header{Name: "./README", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}))
	_, _ = tw.Write([]byte("hi"))
	must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, _ = tw.Write(body)
	must(t, tw.Close())
	must(t, gz.Close())
	return buf.Bytes()
}

func zipped(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	must(t, err)
	_, _ = f.Write(body)
	must(t, zw.Close())
	return buf.Bytes()
}

// downloads is a local HTTPS server of agent files, counting requests.
type downloads struct {
	srv   *httptest.Server
	files map[string][]byte
	hits  atomic.Int32
}

func newDownloads(t *testing.T, files map[string][]byte) *downloads {
	d := &downloads{files: files}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			http.Redirect(w, r, "http://"+r.Host+"/plain", http.StatusFound)
			return
		}
		b, ok := d.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// manifest returns a manifest of codex at version 1.2.3 served by d.
func (d *downloads) manifest(dl AgentDownload) AgentManifest {
	dl.URL = d.srv.URL + dl.URL
	return AgentManifest{Version: 1, Agents: map[string]AgentRelease{
		"codex": {Version: "1.2.3", Binary: "codex", Platforms: map[string]AgentDownload{"linux/amd64": dl}},
	}}
}

func (d *downloads) installer(t *testing.T, m AgentManifest, now func() time.Time) *agentInstaller {
	a := newAgentInstaller(filepath.Join(t.TempDir(), "agents"), m, "linux", "amd64", d.srv.Client(), now)
	must(t, a.prepare())
	return a
}

// leftovers lists the files of the agents directory but state.json.
func leftovers(t *testing.T, a *agentInstaller) []string {
	var out []string
	must(t, filepath.WalkDir(a.dir, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && filepath.Base(p) != "state.json" {
			out = append(out, p)
		}
		return err
	}))
	return out
}

func TestDefaultAgentManifest(t *testing.T) {
	m, err := DefaultAgentManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, cli := range runtimeCLIs {
		a, ok := m.Agents[cli.adapter]
		if !ok || len(a.Platforms) < 4 {
			t.Errorf("%s: %+v, want every desktop platform", cli.adapter, a)
		}
	}
}

// TestManifestMatchesTheToolboxBase: an installed agent is the version the
// toolbox base image pins, so a job runs the same CLI either way.
func TestManifestMatchesTheToolboxBase(t *testing.T) {
	m, err := DefaultAgentManifest()
	must(t, err)
	data, err := fs.ReadFile(forecrew.Files, "packs/toolboxes/base/package.json")
	must(t, err)
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	must(t, json.Unmarshal(data, &pkg))
	for adapter, npm := range map[string]string{"claude-code": "@anthropic-ai/claude-code", "codex": "@openai/codex"} {
		if got, want := m.Agents[adapter].Version, pkg.Dependencies[npm]; got != want {
			t.Errorf("%s: manifest %s, toolbox base %s", adapter, got, want)
		}
	}
}

func TestParseAgentManifestRefuses(t *testing.T) {
	ok := `version: 1
agents:
  codex:
    version: 1.2.3
    binary: codex
    platforms:
      linux/amd64: {url: "https://example.com/1.2.3/codex", sha256: ` + strings.Repeat("a", 64) + `}
`
	if _, err := ParseAgentManifest([]byte(ok)); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	for name, bad := range map[string]string{
		"version":         strings.Replace(ok, "version: 1\n", "version: 2\n", 1),
		"moving version":  strings.Replace(ok, "version: 1.2.3", "version: latest", 1),
		"http":            strings.Replace(ok, "https://", "http://", 1),
		"no version":      strings.Replace(ok, "/1.2.3/", "/latest/", 1),
		"short sha":       strings.Replace(ok, strings.Repeat("a", 64), "abc", 1),
		"other binary":    strings.Replace(ok, "binary: codex", "binary: git", 1),
		"unknown adapter": strings.Replace(ok, "  codex:", "  evil:", 1),
		"platform":        strings.Replace(ok, "linux/amd64", "plan9/386", 1),
		"archive":         strings.Replace(ok, "sha256:", "archive: rar, path: x, sha256:", 1),
		"path no archive": strings.Replace(ok, "sha256:", "path: codex, sha256:", 1),
		"path traversal":  strings.Replace(ok, "sha256:", "archive: zip, path: ../x, sha256:", 1),
		"user in url":     strings.Replace(ok, "https://", "https://me@", 1),
		"yaml":            "agents: [",
	} {
		if _, err := ParseAgentManifest([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// checkInstalled checks that a holds codex 1.2.3 as body, private, recorded
// and reported, with nothing else left.
func checkInstalled(t *testing.T, a *agentInstaller, body []byte, sha string) {
	t.Helper()
	bin := filepath.Join(a.binDir(), "codex")
	got, err := os.ReadFile(bin) //nolint:gosec // test path
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("installed %q, %v", got, err)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if fi, _ := os.Stat(a.dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("agents directory mode = %v, want private", fi.Mode())
	}
	if in := readAgentState(a.dir).Installed["codex"]; in.Version != "1.2.3" || in.SHA256 != sha || !strings.HasPrefix(in.URL, "https://") || in.InstalledAt.IsZero() {
		t.Errorf("recorded %+v", in)
	}
	if p, err := a.lookPath("codex"); err != nil || p != bin {
		t.Errorf("lookPath = %q, %v", p, err)
	}
	if r := a.report(a.lookPath, false); !slices.Contains(r, domain.WorkerAgent{Adapter: "codex", Source: domain.AgentSourceInstalled, Version: "1.2.3"}) {
		t.Errorf("report = %+v", r)
	}
	if left := leftovers(t, a); len(left) != 1 {
		t.Errorf("files = %v, want only the binary", left)
	}
}

func TestInstallBinaryAndArchives(t *testing.T) {
	body := []byte(fakeCLI)
	for name, dl := range map[string]struct {
		file []byte
		dl   AgentDownload
	}{
		"binary": {body, AgentDownload{URL: "/1.2.3/codex"}},
		"tar.gz": {tarGz(t, "./codex-x86_64-unknown-linux-musl", body), AgentDownload{URL: "/1.2.3/codex.tar.gz", Archive: "tar.gz", Path: "codex-x86_64-unknown-linux-musl"}},
		"zip":    {zipped(t, "codex", body), AgentDownload{URL: "/1.2.3/codex.zip", Archive: "zip", Path: "codex"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDownloads(t, map[string][]byte{dl.dl.URL: dl.file})
			dl.dl.SHA256 = sum(dl.file)
			a := d.installer(t, d.manifest(dl.dl), nil)
			must(t, a.Install(t.Context(), "codex"))
			checkInstalled(t, a, body, dl.dl.SHA256)
		})
	}
}

func TestInstallWrongChecksumIsRefusedAndDeleted(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	a := d.installer(t, d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum([]byte("something else"))}), func() time.Time { return now })
	err := a.Install(t.Context(), "codex")
	if ie, ok := err.(*installError); !ok || ie.code != domain.AgentInstallChecksum { //nolint:errorlint // the type itself
		t.Fatalf("Install = %v, want a checksum failure", err)
	}
	if left := leftovers(t, a); len(left) != 0 {
		t.Errorf("files left = %v, want the download deleted", left)
	}
	if f := readAgentState(a.dir).Failed["codex"]; f.Code != domain.AgentInstallChecksum || f.Version != "1.2.3" {
		t.Errorf("failure recorded = %+v", f)
	}
	if r := a.report(a.lookPath, false); !slices.Contains(r, domain.WorkerAgent{Adapter: "codex", Error: domain.AgentInstallChecksum}) {
		t.Errorf("report = %+v", r)
	}
	if a.due("codex") {
		t.Error("a failed install is tried again at once")
	}
	now = now.Add(installRetry)
	if !a.due("codex") {
		t.Error("a failed install is never tried again")
	}
	if r := a.report(a.lookPath, false); !slices.Contains(r, domain.WorkerAgent{Adapter: "codex"}) {
		t.Errorf("report after an hour = %+v, want the failure gone so forecrewd asks again", r)
	}
}

func TestInstallFailures(t *testing.T) {
	good := []byte(fakeCLI)
	archive := tarGz(t, "other", good)
	for name, tc := range map[string]struct {
		dl   AgentDownload
		code string
	}{
		"not found":        {AgentDownload{URL: "/1.2.3/missing", SHA256: sum(good)}, domain.AgentInstallDownload},
		"redirect to http": {AgentDownload{URL: "/redirect/1.2.3/codex", SHA256: sum(good)}, domain.AgentInstallDownload},
		"archive":          {AgentDownload{URL: "/1.2.3/codex.tar.gz", SHA256: sum(archive), Archive: "tar.gz", Path: "codex"}, domain.AgentInstallArchive},
		"broken zip":       {AgentDownload{URL: "/1.2.3/codex", SHA256: sum(good), Archive: "zip", Path: "codex"}, domain.AgentInstallArchive},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDownloads(t, map[string][]byte{"/1.2.3/codex": good, "/1.2.3/codex.tar.gz": archive})
			a := d.installer(t, d.manifest(tc.dl), nil)
			err := a.Install(t.Context(), "codex")
			if ie, ok := err.(*installError); !ok || ie.code != tc.code { //nolint:errorlint // the type itself
				t.Fatalf("Install = %v, want %s", err, tc.code)
			}
			if left := leftovers(t, a); len(left) != 0 {
				t.Errorf("files left = %v", left)
			}
		})
	}
	t.Run("unsupported platform", func(t *testing.T) {
		d := newDownloads(t, nil)
		m := d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum(good)})
		a := newAgentInstaller(filepath.Join(t.TempDir(), "agents"), m, "linux", "arm64", d.srv.Client(), nil)
		err := a.Install(t.Context(), "codex")
		if ie, ok := err.(*installError); !ok || ie.code != domain.AgentInstallUnsupported { //nolint:errorlint // the type itself
			t.Fatalf("Install = %v", err)
		}
		if d.hits.Load() != 0 {
			t.Error("downloaded for a platform the manifest lacks")
		}
	})
	t.Run("unknown agent", func(t *testing.T) {
		d := newDownloads(t, nil)
		a := d.installer(t, d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum(good)}), nil)
		if err := a.Install(t.Context(), "opencode"); err == nil || len(readAgentState(a.dir).Failed) != 0 {
			t.Fatalf("Install = %v, state %+v", err, readAgentState(a.dir))
		}
	})
}

func TestPrepareDropsAnInstallOfAnotherRelease(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	m := d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum([]byte(fakeCLI))})
	a := d.installer(t, m, nil)
	must(t, a.Install(t.Context(), "codex"))
	next := m
	next.Agents = map[string]AgentRelease{"codex": {Version: "1.2.4", Binary: "codex", Platforms: m.Agents["codex"].Platforms}}
	b := newAgentInstaller(a.dir, next, "linux", "amd64", d.srv.Client(), nil)
	must(t, b.prepare())
	if _, err := os.Stat(filepath.Join(b.binDir(), "codex")); !os.IsNotExist(err) {
		t.Errorf("the old version is still there: %v", err)
	}
	if len(readAgentState(b.dir).Installed) != 0 {
		t.Error("the old install is still recorded")
	}
}

// installEnv is a worker with the installer of d, polling a fake API that
// asks it to install adapter.
func installEnv(t *testing.T, d *downloads, m AgentManifest, adapter string) *testEnv {
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.cfg.agents = newAgentInstaller(filepath.Join(t.TempDir(), "agents"), m, "linux", "amd64", d.srv.Client(), nil)
	must(t, e.cfg.agents.prepare())
	e.rebuild()
	e.api.install = adapter
	return e
}

func (a *fakeAPI) lastPoll() pollBody {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pollBodies) == 0 {
		return pollBody{}
	}
	return a.pollBodies[len(a.pollBodies)-1]
}

func TestWorkerInstallsWhatTheServerAsksAndAdvertisesIt(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	e := installEnv(t, d, d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum([]byte(fakeCLI))}), "codex")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)
	waitFor(t, "the runtime label", func() bool { return slices.Contains(e.api.lastPoll().Labels, "runtime:codex") })
	cancel()
	_ = waitRun(t, done)
	p := e.api.lastPoll()
	if !slices.Contains(p.Capabilities, domain.AgentInstallCapability) {
		t.Errorf("capabilities = %v", p.Capabilities)
	}
	if !slices.Contains(p.Agents, domain.WorkerAgent{Adapter: "codex", Source: domain.AgentSourceInstalled, Version: "1.2.3"}) {
		t.Errorf("agents = %+v", p.Agents)
	}
	if d.hits.Load() != 1 {
		t.Errorf("downloads = %d, want 1: an installed agent is not installed again", d.hits.Load())
	}
	if !strings.Contains(e.logs.String(), "agent installed") {
		t.Errorf("logs: %s", e.logs.String())
	}
}

func TestWorkerReportsAFailedInstall(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	e := installEnv(t, d, d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum([]byte("other"))}), "codex")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := e.start(ctx)
	waitFor(t, "the failure reported", func() bool {
		return slices.Contains(e.api.lastPoll().Agents, domain.WorkerAgent{Adapter: "codex", Error: domain.AgentInstallChecksum})
	})
	cancel()
	_ = waitRun(t, done)
	if d.hits.Load() != 1 {
		t.Errorf("downloads = %d, want 1: a failure waits an hour", d.hits.Load())
	}
	if slices.Contains(e.api.lastPoll().Labels, "runtime:codex") {
		t.Error("advertised an agent it does not have")
	}
}

func TestWorkerIgnoresRequestsItCannotVerify(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	m := d.manifest(AgentDownload{URL: "/1.2.3/codex", SHA256: sum([]byte(fakeCLI))})
	for _, asked := range []string{
		"unknown-agent",
		"codex@0.1.0",
		"codex 0.1.0",
		"codex https://evil.example/codex",
		"https://evil.example/codex",
		"codex;version=9.9.9",
	} {
		t.Run(asked, func(t *testing.T) {
			e := installEnv(t, d, m, asked)
			before := d.hits.Load()
			ctx, cancel := context.WithCancel(t.Context())
			done := e.start(ctx)
			waitFor(t, "a few polls", func() bool { e.api.mu.Lock(); defer e.api.mu.Unlock(); return len(e.api.polls) >= 3 })
			cancel()
			_ = waitRun(t, done)
			if d.hits.Load() != before {
				t.Error("downloaded for a request that is not an adapter of the manifest")
			}
			if slices.ContainsFunc(e.api.lastPoll().Labels, func(l string) bool { return strings.HasPrefix(l, "runtime:codex") }) {
				t.Error("advertised codex")
			}
		})
	}
}

func TestWorkerWithInstallOffNeitherSaysNorInstalls(t *testing.T) {
	d := newDownloads(t, map[string][]byte{"/1.2.3/codex": []byte(fakeCLI)})
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	e.api.install = "codex"
	ctx, cancel := context.WithCancel(t.Context())
	done := e.start(ctx)
	waitFor(t, "a few polls", func() bool { e.api.mu.Lock(); defer e.api.mu.Unlock(); return len(e.api.polls) >= 3 })
	cancel()
	_ = waitRun(t, done)
	if d.hits.Load() != 0 {
		t.Error("installed with auto_install_agents off")
	}
	if p := e.api.lastPoll(); slices.Contains(p.Capabilities, domain.AgentInstallCapability) || len(p.Agents) != len(runtimeCLIs) {
		t.Errorf("poll = %+v", p)
	}
}

func TestNewFromConfigTurnsInstallOffByConfigAndOnFleetMachines(t *testing.T) {
	off := false
	for name, tc := range map[string]struct {
		cfg   config.Worker
		fleet bool
		on    bool
	}{
		"default": {config.Worker{}, false, true},
		"off":     {config.Worker{AutoInstallAgents: &off}, false, false},
		"fleet":   {config.Worker{}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.ServerURL, cfg.WorkDir, cfg.MaxJobs = "http://localhost:1", t.TempDir(), 1
			w := newFromConfig(t.Context(), &cfg, hermeticEnv(), "v", slog.New(slog.DiscardHandler), tc.fleet)
			if (w.cfg.agents != nil) != tc.on {
				t.Fatalf("installer = %v, want on %v", w.cfg.agents != nil, tc.on)
			}
			if !tc.on {
				return
			}
			if !slices.ContainsFunc(w.cfg.Environ, func(kv string) bool {
				return strings.HasPrefix(kv, "PATH="+w.cfg.agents.binDir()+string(os.PathListSeparator))
			}) {
				t.Errorf("the agents directory is not first on PATH: %v", w.cfg.Environ)
			}
		})
	}
	t.Run("root", func(t *testing.T) {
		old := hostIDs
		hostIDs = func() (int, int) { return 0, 0 }
		t.Cleanup(func() { hostIDs = old })
		if a := setUpAgents(t.TempDir(), slog.New(slog.DiscardHandler)); a != nil {
			t.Error("a worker running as root installs agents")
		}
	})
}

func TestStatusListsAgentsAndSources(t *testing.T) {
	work := t.TempDir()
	bin := filepath.Join(agentsDir(work), "bin")
	must(t, os.MkdirAll(bin, 0o700))
	must(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeCLI), 0o700)) //nolint:gosec // test executable
	a := &agentInstaller{dir: agentsDir(work)}
	must(t, a.writeState(agentState{
		Installed: map[string]installedAgent{"codex": {Version: "1.2.3"}},
		Failed:    map[string]failedInstall{"opencode": {Version: "1.0.0", Code: domain.AgentInstallChecksum, Detail: "SHA-256 mismatch; the file was deleted", At: time.Now()}},
	}))
	sys := t.TempDir()
	must(t, os.WriteFile(filepath.Join(sys, "claude"), []byte(fakeCLI), 0o700)) //nolint:gosec // test executable
	t.Setenv("PATH", sys)
	var out bytes.Buffer
	Status(&config.Worker{WorkDir: work}, nil, &out)
	for _, want := range []string{"auto_install_agents: on", "claude-code: system (" + sys, "codex: installed 1.2.3 (" + bin, "opencode: missing; install of 1.0.0 failed", "the file was deleted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	Status(&config.Worker{WorkDir: work}, []string{config.EnvBootstrapToken + "=x"}, &out)
	if !strings.Contains(out.String(), "claude-code: image") || !strings.Contains(out.String(), "off (a fleet machine") {
		t.Errorf("fleet status:\n%s", out.String())
	}
	off := false
	out.Reset()
	Status(&config.Worker{WorkDir: work, AutoInstallAgents: &off}, nil, &out)
	if !strings.Contains(out.String(), "auto_install_agents: off\n") || !strings.Contains(out.String(), "codex: missing") {
		t.Errorf("status with install off:\n%s", out.String())
	}
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got := prependPath([]string{"A=1", "Path=/usr/bin"}, "/x"); got[1] != "Path=/x"+sep+"/usr/bin" {
		t.Errorf("got %v", got)
	}
	if got := prependPath([]string{"A=1"}, "/x"); got[1] != "PATH=/x" {
		t.Errorf("got %v", got)
	}
}

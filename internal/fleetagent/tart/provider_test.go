package tart

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/images"
)

const (
	testToken  = "fcb_TOKEN-abc_123"
	testServer = "http://192.0.2.10:8080"
	testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testEnv is a provider on a temporary directory, running the fake tart of
// testdata, with a signed image.
type testEnv struct {
	p    *Provider
	dir  string
	home string // the fake's TART_HOME
	key  ed25519.PrivateKey
	img  domain.Image
	// extraVMs are VMs running on the Mac outside the agent's Tart home: the
	// operator's own.
	extraVMs int
	// cache is the directory of the test image in Tart's cache.
	cache string
}

func newEnv(t *testing.T, edit func(*Config)) *testEnv {
	t.Helper()
	fake, err := filepath.Abs("testdata/tart")
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{dir: t.TempDir()}
	pub, key, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	cfg := defaults()
	cfg.StateDir = filepath.Join(e.dir, "state")
	cfg.TrustedKeysFile = filepath.Join(e.dir, "keys.pub")
	cfg.Tart = fake
	cfg.MinFreeGB = 10
	if err := os.WriteFile(cfg.TrustedKeysFile, []byte(images.FormatPublicKey(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&cfg)
	}
	e.p, err = New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.p.log = slog.New(slog.DiscardHandler)
	e.home = filepath.Join(cfg.StateDir, "tart-home")
	e.p.poll = 10 * time.Millisecond
	e.p.goos, e.p.goarch = "darwin", "arm64"
	e.p.memoryMB = func() (int, error) { return 16384, nil }
	e.p.freeGB = func(string) (int, error) { return 500, nil }
	e.p.lookPath = func(string) bool { return true }
	e.p.getenv = func(k string) string { return map[string]string{"REG_USER": "ci", "REG_PASS": "hunter2-registry"}[k] }
	e.p.cli.getenv = e.p.getenv
	e.p.vmProcesses = func(ctx context.Context) (int, error) {
		vms, err := e.p.cli.list(ctx)
		n := e.extraVMs
		for _, v := range vms {
			if v.Running {
				n++
			}
		}
		return n, err
	}
	e.p.hostNets = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("192.168.64.1/24"), netip.MustParsePrefix("10.0.0.7/24")}
	}
	e.p.pmset = func(context.Context) (string, error) {
		return "System-wide power settings:\n sleep                0\n displaysleep         10\n", nil
	}
	e.img = domain.Image{Name: "macos", Version: "2026.11.1", OS: domain.OSMacOS, Arch: domain.ArchARM64, Format: domain.ImageTart,
		URI: "oci://" + testRepo + ":2026.11.1", BuiltAt: time.Unix(1700000000, 0).UTC()}
	e.writeImage(t)
	t.Cleanup(func() {
		e.killAll()
		for _, r := range func() []*record { rs, _ := e.p.records(); return rs }() {
			_ = e.p.Destroy(context.WithoutCancel(t.Context()), r.ID)
		}
	})
	return e
}

const testRepo = "registry.example.com/forecrew/macos"

// writeImage puts in Tart's cache what a pull of the test image leaves there,
// and signs the image with the digest of its manifest. The fake's pull leaves a
// cache it finds alone.
func (e *testEnv) writeImage(t *testing.T) {
	t.Helper()
	config, nvram := []byte("{config}"), []byte("nvram")
	chunks := [][]byte{[]byte("AAAA"), []byte("BBBBBB")}
	sum := func(b []byte) string { d, _ := images.Digest(strings.NewReader(string(b))); return d }
	layers := []map[string]any{
		{"mediaType": mediaConfig, "digest": sum(config), "size": len(config)},
		{"mediaType": mediaNVRAM, "digest": sum(nvram), "size": len(nvram)},
	}
	for _, c := range chunks {
		layers = append(layers, map[string]any{"mediaType": mediaDisk, "digest": "sha256:" + strings.Repeat("0", 64), "size": 3,
			"annotations": map[string]string{annLayerSize: strconv.Itoa(len(c)), annLayerDigest: sum(c)}})
	}
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "layers": layers})
	if err != nil {
		t.Fatal(err)
	}
	e.img.Digest = sum(manifest)
	e.cache = e.p.cacheDir(testRepo, e.img.Digest)
	if err := os.MkdirAll(e.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{fileManifest: manifest, fileConfig: config, fileNVRAM: nvram, fileDisk: append(chunks[0], chunks[1]...)} {
		if err := os.WriteFile(filepath.Join(e.cache, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.img.Signature = images.Sign(e.key, e.img)
}

// killAll ends every VM process of the fake.
func (e *testEnv) killAll() {
	pids, _ := filepath.Glob(filepath.Join(e.home, "vms", "*", "pid"))
	for _, f := range pids {
		if raw, err := os.ReadFile(f); err == nil { // #nosec G304 -- a test
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

func (e *testEnv) request() fleet.CreateRequest {
	return fleet.CreateRequest{
		MachineID: uuid.New(),
		Shape:     domain.Shape{OS: domain.OSMacOS, Arch: domain.ArchARM64, CPU: 4, MemoryMB: 6144, DiskGB: 80, MaxJobs: 1},
		Image:     e.img,
		Bootstrap: &fleet.Bootstrap{ServerURL: testServer, Token: testToken},
		Tags:      map[string]string{fleet.TagMachine: "m", fleet.TagOrg: "o"},
	}
}

func (e *testEnv) file(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{e.home}, parts...)...)) // #nosec G304 -- a test
	if err != nil {
		return ""
	}
	return string(raw)
}

func (e *testEnv) vms(t *testing.T) []string {
	t.Helper()
	vms, err := e.p.cli.list(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range vms {
		names = append(names, v.Name)
	}
	return names
}

func (e *testEnv) create(t *testing.T) fleet.CreateRequest {
	t.Helper()
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestCreateListDestroy(t *testing.T) {
	t.Setenv("FORECREW_PROVIDER_TOKEN", "fcp_agents-own-secret")
	e := newEnv(t, func(c *Config) {
		c.RegistryUsername, c.RegistryPassword, c.RegistryHosts = "env:REG_USER", "env:REG_PASS", []string{"registry.example.com"}
	})
	req := e.request()
	req.NetworkAllow = []string{"10.1.0.0/16"}
	inst, err := e.p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	id, name := req.MachineID.String(), vmName(req.MachineID.String())
	if inst.ExternalID != id || inst.MachineID != req.MachineID || !inst.Running || inst.Tags[fleet.TagOrg] != "o" {
		t.Errorf("instance = %+v", inst)
	}
	if !slices.Equal(e.vms(t), []string{name}) {
		t.Errorf("vms = %v", e.vms(t))
	}
	// The image is pulled by the digest the signature covers, and cloned from it.
	ref := testRepo + "@" + e.img.Digest
	if pulled := e.file(t, "cache", "pulled"); !strings.Contains(pulled, ref+"\n") {
		t.Errorf("pulled = %q", pulled)
	}
	if src := strings.TrimSpace(e.file(t, "vms", name, "source")); src != ref {
		t.Errorf("cloned from %q", src)
	}
	if set := e.file(t, "vms", name, "set"); !strings.Contains(set, "--cpu 4") || !strings.Contains(set, "--memory 6144") || !strings.Contains(set, "--disk-size 80") {
		t.Errorf("set = %q", set)
	}
	args := e.file(t, "vms", name, "run-args")
	for _, want := range []string{"--no-graphics", "--no-audio", "--no-clipboard", "--net-softnet", "--net-softnet-block=192.168.64.1/32", "--net-softnet-allow=10.1.0.0/16,192.0.2.10/32", name} {
		if !strings.Contains(args, want) {
			t.Errorf("run args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "--dir") {
		t.Errorf("a host directory is shared: %q", args)
	}
	e.checkGuestAndCredentials(t, name, ref)

	list, err := e.p.List(t.Context())
	if err != nil || len(list) != 1 || list[0].ExternalID != id || !list[0].Running || list[0].Tags[fleet.TagMachine] != "m" {
		t.Errorf("list = %+v, %v", list, err)
	}
	if err := e.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if len(e.vms(t)) != 0 {
		t.Errorf("vms after destroy = %v", e.vms(t))
	}
	if recs, _ := e.p.records(); len(recs) != 0 {
		t.Errorf("records after destroy = %v", recs)
	}
	if _, err := os.Stat(e.p.logPath(id)); err == nil {
		t.Error("the log stayed")
	}
	if err := e.p.Destroy(t.Context(), id); err != nil {
		t.Errorf("destroying what is gone: %v", err)
	}
	if err := e.p.Destroy(t.Context(), "not-an-id"); err != nil {
		t.Errorf("destroying a stranger: %v", err)
	}
}

// checkGuestAndCredentials checks where the settings and the credentials went.
func (e *testEnv) checkGuestAndCredentials(t *testing.T, name, ref string) {
	t.Helper()
	// The settings went in through standard input, and nowhere else.
	if got := e.file(t, "guest-"+name+".stdin"); got != "FORECREW_SERVER_URL="+testServer+"\nFORECREW_BOOTSTRAP_TOKEN="+testToken+"\n" {
		t.Errorf("guest got %q", got)
	}
	exec := e.file(t, "vms", name, "exec")
	if !strings.Contains(exec, "sudo -n /bin/sh -c") || !strings.HasSuffix(strings.TrimSpace(exec), "sh "+guestDir+" 1.1.1.1 9.9.9.9") {
		t.Errorf("exec = %q", exec)
	}
	calls := e.file(t, "calls.log")
	if strings.Contains(calls+exec, testToken) {
		t.Errorf("the token is in an argument:\n%s", calls)
	}
	// Credentials reach the commands that pull, as environment, not arguments.
	if !strings.Contains(calls, "pull "+ref+" | registry-password=set") || !strings.Contains(calls, "clone "+ref+" "+name+" | registry-password=set") {
		t.Errorf("registry credentials did not reach pull and clone:\n%s", calls)
	}
	if strings.Contains(calls, "hunter2") || strings.Contains(calls, "list --source local --format json | registry-password=set") {
		t.Errorf("credentials leaked:\n%s", calls)
	}
	if !strings.Contains(calls, "agent-secret=unset") || strings.Contains(calls, "fcp_agents-own-secret") {
		t.Errorf("the agent's environment reached tart:\n%s", calls)
	}

}

func TestCreateAgainReplaces(t *testing.T) {
	e := newEnv(t, nil)
	req := e.create(t)
	req.Bootstrap = &fleet.Bootstrap{ServerURL: testServer, Token: "fcb_NEW"}
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if n := len(e.vms(t)); n != 1 {
		t.Errorf("vms = %v", e.vms(t))
	}
	if got := e.file(t, "guest-"+vmName(req.MachineID.String())+".stdin"); !strings.Contains(got, "fcb_NEW") {
		t.Errorf("the new token did not arrive: %q", got)
	}
}

func TestTwoVMLimit(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.create(t), e.create(t)
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("a third machine: %v", err)
	}
	if n := len(e.vms(t)); n != 2 {
		t.Errorf("the refused machine left a VM: %v", e.vms(t))
	}
	if recs, _ := e.p.records(); len(recs) != 2 {
		t.Errorf("the refused machine left a record: %v", recs)
	}
	if err := e.p.Destroy(t.Context(), a.MachineID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Errorf("after a destroy: %v", err)
	}
	_ = b
	caps, _ := e.p.Capabilities(t.Context())
	if caps.MaxMachines != 2 {
		t.Errorf("max machines = %d", caps.MaxMachines)
	}
}

func TestOtherRunningVMsCount(t *testing.T) {
	e := newEnv(t, nil)
	// A VM of the operator's own, running outside the agent's Tart home, where
	// `tart list` does not show it: Apple's limit is on the Mac.
	e.extraVMs = 1
	caps, _ := e.p.Capabilities(t.Context())
	if caps.MaxMachines != 1 || !slices.ContainsFunc(caps.Checks, func(c domain.ProviderCheck) bool { return c.Name == "other VMs" && c.Status == statusWarn }) {
		t.Errorf("capabilities with an operator VM = %+v", caps)
	}
	e.create(t)
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("a second machine next to an operator's VM: %v", err)
	}
	list, _ := e.p.List(t.Context())
	if len(list) != 1 {
		t.Errorf("the operator's VM is listed as ours: %+v", list)
	}
	// Two of the operator's leave no place at all, and never a limit of 0, which
	// would mean none.
	e.extraVMs = 2
	if caps, _ := e.p.Capabilities(t.Context()); caps.MaxMachines != 1 {
		t.Errorf("max machines = %d", caps.MaxMachines)
	}
	if err := e.p.Destroy(t.Context(), list[0].ExternalID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("a machine beside two operator VMs: %v", err)
	}
	// A count that cannot be made is an error, not room.
	e.p.vmProcesses = func(context.Context) (int, error) { return 0, errors.New("no ps") }
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("unknown count: %v", err)
	}
	if caps, _ := e.p.Capabilities(t.Context()); caps.MaxMachines != 2 {
		t.Errorf("max machines with an unknown count = %d", caps.MaxMachines)
	}
}

func TestRunLimitExceededIsNoCapacity(t *testing.T) {
	e := newEnv(t, nil)
	e.p.cfg.BootWaitSeconds = 5
	e.inject(t, "limit-run", "")
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("error = %v", err)
	}
	if len(e.vms(t)) != 0 {
		t.Errorf("a VM stayed: %v", e.vms(t))
	}
	if recs, _ := e.p.records(); len(recs) != 0 {
		t.Errorf("a record stayed: %v", recs)
	}
}

func TestMaxMachinesCanLowerTheLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 1 })
	e.create(t)
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("second machine: %v", err)
	}
}

func TestMemoryAndDiskCapacity(t *testing.T) {
	e := newEnv(t, nil)
	e.p.memoryMB = func() (int, error) { return 8192, nil }
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("memory: %v", err)
	}
	e.p.memoryMB = func() (int, error) { return 16384, nil }
	e.p.freeGB = func(string) (int, error) { return 40, nil }
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("disk: %v", err)
	}
	e.p.memoryMB = func() (int, error) { return 0, errors.New("no sysctl") }
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("unknown memory: %v", err)
	}
}

func TestCreateFailuresLeaveNothing(t *testing.T) {
	for _, c := range []struct {
		name, file, content, want string
	}{
		{"pull", "fail-pull", "denied: token hunter2-registry rejected", "tart pull"},
		{"clone", "fail-clone", "no space left", "tart clone"},
		{"set", "fail-set", "bad cpu", "tart set"},
		{"run crashes", "crash-run", "", "the VM ended before its guest agent answered"},
		{"no guest agent", "no-agent", "", "guest agent did not answer"},
		{"delivery", "fail-exec-deliver", "", "deliver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(c *Config) {
				c.RegistryUsername, c.RegistryPassword, c.RegistryHosts = "env:REG_USER", "env:REG_PASS", []string{"registry.example.com"}
			})
			e.p.cfg.BootWaitSeconds = 1
			e.inject(t, c.file, c.content)
			_, err := e.p.Create(t.Context(), e.request())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), testToken) {
				t.Errorf("a secret is in the error: %v", err)
			}
			if len(e.vms(t)) != 0 {
				t.Errorf("a VM stayed: %v", e.vms(t))
			}
			if recs, _ := e.p.records(); len(recs) != 0 {
				t.Errorf("a record stayed: %v", recs)
			}
		})
	}
}

// inject makes the fake fail as a file says; the delivery failure is a shim.
func (e *testEnv) inject(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if file == "fail-exec-deliver" {
		e.p.cli.bin = e.shim(t) // fails every exec but the guest agent's probe
		return
	}
	if err := os.WriteFile(filepath.Join(e.home, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lastID is the machine of the newest record or VM name.
func (e *testEnv) lastID(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.home, "guest-*.stdin"))
	if len(files) == 0 {
		return "none"
	}
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(files[0]), "guest-"+vmPrefix), ".stdin")
}

// shimFor is a tart that runs shell for one command and is the fake otherwise.
func (e *testEnv) shimFor(t *testing.T, cmd, body string) string {
	t.Helper()
	fake, _ := filepath.Abs("testdata/tart")
	path := filepath.Join(e.dir, "tart-shim-"+cmd)
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = %s ]; then %s; fi\nexec %s \"$@\"\n", cmd, body, fake)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	return path
}

// shim is a tart that fails exec of anything but the probe, and is the fake
// otherwise.
func (e *testEnv) shim(t *testing.T) string {
	t.Helper()
	fake, _ := filepath.Abs("testdata/tart")
	path := filepath.Join(e.dir, "tart-shim")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1 $2 $3\" in 'exec -i '*) echo 'guest says: boom' >&2; exit 1;; esac\nexec %s \"$@\"\n", fake)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	return path
}

func TestCreateRefusals(t *testing.T) {
	loop := func(r *fleet.CreateRequest) { r.Bootstrap.ServerURL = "http://127.0.0.1:8080" }
	for _, c := range []struct {
		name string
		edit func(e *testEnv, r *fleet.CreateRequest)
		want string
	}{
		{"no bootstrap", func(_ *testEnv, r *fleet.CreateRequest) { r.Bootstrap = nil }, "no bootstrap"},
		{"linux shape", func(_ *testEnv, r *fleet.CreateRequest) { r.Shape.OS = domain.OSLinux }, "macOS arm64"},
		{"amd64 shape", func(_ *testEnv, r *fleet.CreateRequest) { r.Shape.Arch = domain.ArchAMD64 }, "macOS arm64"},
		{"unknown setting", func(_ *testEnv, r *fleet.CreateRequest) { r.Shape.ProviderConfig = map[string]any{"node": "pve"} }, `"node"`},
		{"base_vm not allowed", func(_ *testEnv, r *fleet.CreateRequest) { r.Shape.ProviderConfig = map[string]any{"base_vm": "mine"} }, "allow_local_base"},
		{"loopback server", func(_ *testEnv, r *fleet.CreateRequest) { loop(r) }, "loopback"},
		{"bad server characters", func(_ *testEnv, r *fleet.CreateRequest) { r.Bootstrap.ServerURL = "http://192.0.2.10/?a=b&c=d" }, "characters"},
		{"bad token characters", func(_ *testEnv, r *fleet.CreateRequest) { r.Bootstrap.Token = "fcb_x\nFORECREW_X=1" }, "characters"},
		{"unsigned image", func(_ *testEnv, r *fleet.CreateRequest) { r.Image.Signature = "" }, "signature"},
		{"tampered image", func(_ *testEnv, r *fleet.CreateRequest) { r.Image.Digest = "sha256:" + strings.Repeat("a", 64) }, "signature"},
		{"wrong format", func(e *testEnv, r *fleet.CreateRequest) {
			r.Image.Format = domain.ImageQCOW2
			r.Image.Signature = images.Sign(e.key, r.Image)
		}, "tart format"},
		{"not an oci uri", func(e *testEnv, r *fleet.CreateRequest) {
			r.Image.URI = "https://registry.example.com/x"
			r.Image.Signature = images.Sign(e.key, r.Image)
		}, "oci://"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			req := e.request()
			c.edit(e, &req)
			_, err := e.p.Create(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want %q", err, c.want)
			}
			if len(e.vms(t)) != 0 || e.file(t, "cache", "pulled") != "" {
				t.Errorf("something was made or pulled: %v", e.vms(t))
			}
		})
	}
	t.Run("no trusted keys", func(t *testing.T) {
		e := newEnv(t, nil)
		_ = os.WriteFile(e.p.cfg.TrustedKeysFile, []byte("# none\n"), 0o600)
		if _, err := e.p.Create(t.Context(), e.request()); err == nil || !strings.Contains(err.Error(), "lists none") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("unresolvable server", func(t *testing.T) {
		e := newEnv(t, nil)
		req := e.request()
		req.Bootstrap.ServerURL = "http://nothing.invalid"
		if _, err := e.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "resolve the server") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestLocalBaseVM(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowLocalBase = true })
	req := e.request()
	req.Shape.ProviderConfig = map[string]any{"base_vm": "mine"}
	if _, err := e.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "not in the agent's Tart home") {
		t.Errorf("a VM that is not there: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(e.home, "vms", "mine"), 0o700)
	_ = os.WriteFile(filepath.Join(e.home, "vms", "mine", "disk"), []byte("50"), 0o600)
	req.Image.Signature = "" // not needed: the operator chose the VM
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if src := strings.TrimSpace(e.file(t, "vms", vmName(req.MachineID.String()), "source")); src != "mine" {
		t.Errorf("cloned from %q", src)
	}
	for _, bad := range []string{"../etc", "forecrew-" + uuid.New().String(), "-x", ""} {
		r := e.request()
		r.Shape.ProviderConfig = map[string]any{"base_vm": bad}
		if bad == "" {
			continue // an empty name means no override
		}
		if _, err := e.p.Create(t.Context(), r); err == nil || !strings.Contains(err.Error(), "not a name of a local VM") {
			t.Errorf("base_vm %q: %v", bad, err)
		}
	}
	r := e.request()
	r.Shape.ProviderConfig = map[string]any{"base_vm": 3}
	if _, err := e.p.Create(t.Context(), r); err == nil {
		t.Error("a number as base_vm")
	}
}

func TestGatewayIsBlockedAndChecked(t *testing.T) {
	for _, c := range []struct {
		name  string
		hosts []netip.Prefix
		ip    string
		want  string
	}{
		{"another gateway", []netip.Prefix{netip.MustParsePrefix("192.168.64.254/24")}, "", "set softnet_gateway: 192.168.64.254"},
		{"no address on the network", []netip.Prefix{netip.MustParsePrefix("10.0.0.7/24")}, "", "cannot be checked"},
		{"no host address at all", nil, "", "cannot be checked"},
		{"junk from tart", nil, "not an address", "tart ip said"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.p.hostNets = func() []netip.Prefix { return c.hosts }
			if c.ip != "" {
				e.inject(t, "guest-ip", c.ip)
			}
			_, err := e.p.Create(t.Context(), e.request())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
			// The settings never reached a machine whose host is open.
			if got := e.file(t, "guest-"+vmName(e.lastID(t))+".stdin"); got != "" {
				t.Errorf("settings were delivered: %q", got)
			}
			if len(e.vms(t)) != 0 {
				t.Errorf("a VM stayed: %v", e.vms(t))
			}
		})
	}
	t.Run("no address from tart", func(t *testing.T) {
		e := newEnv(t, nil)
		e.p.cfg.BootWaitSeconds = 1
		e.p.cli.bin = e.shimFor(t, "ip", "exit 1")
		if _, err := e.p.Create(t.Context(), e.request()); err == nil || !strings.Contains(err.Error(), "no address") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("the server is the gateway", func(t *testing.T) {
		e := newEnv(t, nil)
		req := e.request()
		req.Bootstrap.ServerURL = "http://192.168.64.1:8080"
		if _, err := e.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestPrivateResolverIsAllowed(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DNS = []string{"10.9.0.53", "1.1.1.1"} })
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	args := e.file(t, "vms", vmName(req.MachineID.String()), "run-args")
	if !strings.Contains(args, "--net-softnet-allow=192.0.2.10/32,10.9.0.53/32 ") && !strings.Contains(args, "--net-softnet-allow=10.9.0.53/32,192.0.2.10/32 ") {
		t.Errorf("a LAN resolver is not allowed: %q", args)
	}
}

func TestNoSoftnet(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Softnet, c.AllowNoSoftnet = false, true })
	e.p.hostNets = func() []netip.Prefix { return nil } // not looked at
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	name := vmName(req.MachineID.String())
	if args := e.file(t, "vms", name, "run-args"); strings.Contains(args, "softnet") {
		t.Errorf("run args %q", args)
	}
	if exec := e.file(t, "vms", name, "exec"); strings.Contains(exec, "1.1.1.1") {
		t.Errorf("resolvers were set without softnet: %q", exec)
	}
}

func TestImageIntegrity(t *testing.T) {
	flip := func(file string) func(e *testEnv) {
		return func(e *testEnv) {
			path := filepath.Join(e.cache, file)
			raw, _ := os.ReadFile(path) // #nosec G304 -- a test
			raw[0] ^= 0xff
			_ = os.WriteFile(path, raw, 0o600)
		}
	}
	rewrite := func(edit func(m map[string]any)) func(e *testEnv) {
		return func(e *testEnv) {
			path := filepath.Join(e.cache, fileManifest)
			raw, _ := os.ReadFile(path) // #nosec G304 -- a test
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			edit(m)
			raw, _ = json.Marshal(m)
			_ = os.WriteFile(path, raw, 0o600)
		}
	}
	layer := func(m map[string]any, i int) map[string]any { return m["layers"].([]any)[i].(map[string]any) }
	for _, c := range []struct {
		name, want string
		tamper     func(e *testEnv)
	}{
		{"a manifest that is not the signed one", "not the signed", func(e *testEnv) {
			f, _ := os.OpenFile(filepath.Join(e.cache, fileManifest), os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- a test
			_, _ = f.WriteString(" ")
			_ = f.Close()
		}},
		{"a disk layer that is not what the manifest names", "disk layer 0", flip(fileDisk)},
		{"a disk with a second chunk changed", "disk layer 1", func(e *testEnv) {
			path := filepath.Join(e.cache, fileDisk)
			raw, _ := os.ReadFile(path) // #nosec G304 -- a test
			raw[len(raw)-1] ^= 1
			_ = os.WriteFile(path, raw, 0o600)
		}},
		{"a disk with more than its layers cover", "layers cover", func(e *testEnv) {
			f, _ := os.OpenFile(filepath.Join(e.cache, fileDisk), os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- a test
			_, _ = f.WriteString("extra")
			_ = f.Close()
		}},
		{"a disk shorter than its layers", "disk layer 1", func(e *testEnv) { _ = os.Truncate(filepath.Join(e.cache, fileDisk), 5) }},
		{"a changed config", "config.json", flip(fileConfig)},
		{"a changed NVRAM", "nvram.bin", flip(fileNVRAM)},
		{"no disk file", "no such file", func(e *testEnv) { _ = os.Remove(filepath.Join(e.cache, fileDisk)) }},
		{"no manifest in the cache", "has no manifest.json", func(e *testEnv) { _ = os.Remove(filepath.Join(e.cache, fileManifest)) }},
		{"a manifest that is no JSON", "the manifest", func(e *testEnv) { _ = os.WriteFile(filepath.Join(e.cache, fileManifest), []byte("{"), 0o600) }},
		{"a manifest too large", "larger than", func(e *testEnv) {
			_ = os.WriteFile(filepath.Join(e.cache, fileManifest), []byte(strings.Repeat(" ", maxManifest+1)), 0o600)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			c.tamper(e)
			_, err := e.p.Create(t.Context(), e.request())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
			if len(e.vms(t)) != 0 {
				t.Errorf("a VM was cloned from an image that failed its check: %v", e.vms(t))
			}
		})
	}
	// A manifest that is the signed one, with layers that cannot be checked or
	// contradict themselves: the signed digest must be of the edited manifest, so
	// the test signs it again.
	for _, c := range []struct {
		name, want string
		edit       func(m map[string]any)
	}{
		{"an unknown layer", "cannot be checked", func(m map[string]any) {
			m["layers"] = append(m["layers"].([]any), map[string]any{"mediaType": "application/vnd.example.other", "digest": "sha256:" + strings.Repeat("1", 64)})
		}},
		{"a stacked disk", "stacked", func(m map[string]any) { layer(m, 2)["annotations"].(map[string]any)[annBlockSize] = "4096" }},
		{"a disk layer with no digest", "no usable", func(m map[string]any) { delete(layer(m, 2)["annotations"].(map[string]any), annLayerDigest) }},
		{"a disk layer with no size", "no usable", func(m map[string]any) { delete(layer(m, 3)["annotations"].(map[string]any), annLayerSize) }},
		{"a disk layer with a bad size", "no usable", func(m map[string]any) { layer(m, 3)["annotations"].(map[string]any)[annLayerSize] = "-4" }},
		{"two config layers", "more than one config", func(m map[string]any) { layer(m, 1)["mediaType"] = mediaConfig }},
		{"two NVRAM layers", "more than one NVRAM", func(m map[string]any) { layer(m, 0)["mediaType"] = mediaNVRAM }},
		{"no disk layers", "lacks", func(m map[string]any) { m["layers"] = m["layers"].([]any)[:2] }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			rewrite(c.edit)(e)
			raw, _ := os.ReadFile(filepath.Join(e.cache, fileManifest)) // #nosec G304 -- a test
			d, _ := images.Digest(strings.NewReader(string(raw)))
			newDir := e.p.cacheDir(testRepo, d)
			_ = os.Rename(e.cache, newDir)
			e.img.Digest, e.cache = d, newDir
			e.img.Signature = images.Sign(e.key, e.img)
			_, err := e.p.Create(t.Context(), e.request())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestVerifiedImageIsCheckedAgainWhenItChanges(t *testing.T) {
	e := newEnv(t, nil)
	e.create(t)
	if len(e.p.verified) != 1 {
		t.Fatalf("verified = %v", e.p.verified)
	}
	// A second machine takes the checked image as it is: damaging the file in a
	// way the fingerprint cannot see is out of scope, a changed file is not.
	path := filepath.Join(e.cache, fileDisk)
	raw, _ := os.ReadFile(path) // #nosec G304 -- a test
	raw[0] ^= 0xff
	_ = os.WriteFile(path, raw, 0o600)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, later, later)
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || !strings.Contains(err.Error(), "disk layer 0") {
		t.Errorf("a changed disk: %v", err)
	}
	// Pruning forgets what was checked.
	e.p.lastGC = time.Time{}
	_, _ = e.p.List(t.Context())
	if len(e.p.verified) != 0 {
		t.Errorf("verified after a prune = %v", e.p.verified)
	}
}

func TestRegistryHosts(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.RegistryUsername, c.RegistryPassword = "env:REG_USER", "env:REG_PASS"
		c.RegistryHosts = []string{"registry.example.com"}
	})
	req := e.create(t)
	calls := e.file(t, "calls.log")
	if !strings.Contains(calls, "pull "+testRepo+"@"+e.img.Digest+" | registry-password=set registry-host=registry.example.com") {
		t.Errorf("credentials did not go to the image's host:\n%s", calls)
	}
	_ = req
	// An image of another host is refused, with nothing sent there.
	other := e.request()
	other.Image.URI = "oci://evil.example.net/forecrew/macos:1"
	other.Image.Signature = images.Sign(e.key, other.Image)
	if _, err := e.p.Create(t.Context(), other); err == nil || !strings.Contains(err.Error(), "not in registry_hosts") {
		t.Errorf("an image of another registry: %v", err)
	}
	if strings.Contains(e.file(t, "calls.log"), "evil.example.net") {
		t.Error("tart was asked to pull from a host that is not allowed")
	}
	// The host is compared as written in lower case, port included.
	e.p.cfg.RegistryHosts = []string{"registry.example.com:5000"}
	if _, err := e.p.Create(t.Context(), e.request()); err == nil {
		t.Error("a port that is not listed was accepted")
	}
}

func TestSmallerDiskIsNotSet(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Shape.DiskGB = 40 // the image's disk is 50 GB
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if set := e.file(t, "vms", vmName(req.MachineID.String()), "set"); strings.Contains(set, "--disk-size") {
		t.Errorf("set = %q", set)
	}
}

func TestUnknownDiskIsNotSet(t *testing.T) {
	e := newEnv(t, nil)
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.home, "disk-null"), nil, 0o600) // `"Disk": null`, as of a running VM
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if set := e.file(t, "vms", vmName(req.MachineID.String()), "set"); strings.Contains(set, "--disk-size") {
		t.Errorf("set = %q", set)
	}
}

func TestUpdate(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.NetworkAllow = []string{"10.1.0.0/16"}
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	deadline := time.Now().Add(time.Hour).UTC()
	up := fleet.UpdateRequest{Tags: map[string]string{"forecrew:job": "j1"}, NetworkAllow: []string{"10.1.0.0/16"}, Deadline: deadline}
	for range 2 { // the same values again succeed
		if err := e.p.Update(t.Context(), id, up); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := e.p.load(id)
	if r.Tags["forecrew:job"] != "j1" || !r.Deadline.Equal(deadline) {
		t.Errorf("record = %+v", r)
	}
	up.NetworkAllow = []string{"10.2.0.0/16"}
	if err := e.p.Update(t.Context(), id, up); err == nil || !strings.Contains(err.Error(), "cannot change") {
		t.Errorf("a changed network: %v", err)
	}
	if err := e.p.Update(t.Context(), uuid.New().String(), up); err == nil {
		t.Error("updated a machine that does not exist")
	}
	if err := e.p.Update(t.Context(), "junk", up); err == nil {
		t.Error("updated a stranger")
	}
}

func TestDeadlineStopsTheVM(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Deadline = time.Now().Add(time.Second)
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	if !e.p.runningNow(t.Context(), vmName(id)) {
		t.Fatal("not running")
	}
	for end := time.Now().Add(10 * time.Second); e.p.runningNow(t.Context(), vmName(id)) && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
	}
	if e.p.runningNow(t.Context(), vmName(id)) {
		t.Fatal("the VM outlived its deadline")
	}
	// What is left is for forecrewd to destroy.
	list, _ := e.p.List(t.Context())
	if len(list) != 1 || list[0].Running {
		t.Errorf("list = %+v", list)
	}
}

func TestRestartFindsMachinesAndDeadlines(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Deadline = time.Now().Add(time.Hour)
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	// A new process: nothing in memory, the record and Tart say all.
	p2, err := New(e.p.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2.mu.Lock()
	armed := p2.timers[id] != nil
	p2.mu.Unlock()
	if !armed {
		t.Error("the deadline was not set again")
	}
	list, err := p2.List(t.Context())
	if err != nil || len(list) != 1 || !list[0].Running {
		t.Errorf("list = %+v, %v", list, err)
	}
	// An overdue deadline stops the VM at once.
	r, _ := p2.load(id)
	r.Deadline = time.Now().Add(-time.Second)
	_ = p2.save(r)
	if _, err := New(e.p.cfg, nil); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); e.p.runningNow(t.Context(), vmName(id)) && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
	}
	if e.p.runningNow(t.Context(), vmName(id)) {
		t.Error("an overdue machine was left running")
	}
}

func TestOrphans(t *testing.T) {
	e := newEnv(t, nil)
	orphan := uuid.New().String()
	// A VM of ours with no record, and one that is not ours.
	for _, n := range []string{vmName(orphan), "someone-elses", vmPrefix + "not-a-machine"} {
		_ = os.MkdirAll(filepath.Join(e.home, "vms", n), 0o700)
		_ = os.WriteFile(filepath.Join(e.home, "vms", n, "disk"), []byte("50"), 0o600)
	}
	// A record whose VM was never made: a crash between the two.
	lost := uuid.New().String()
	_ = os.MkdirAll(filepath.Dir(e.p.recordPath(lost)), 0o700)
	if err := e.p.save(&record{ID: lost, CreatedAt: time.Now().UTC(), Tags: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	list, err := e.p.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, i := range list {
		ids = append(ids, i.ExternalID)
		if i.ExternalID == orphan && i.Tags != nil {
			t.Errorf("the orphan has tags: %v", i.Tags)
		}
	}
	slices.Sort(ids)
	if want := []string{lost, orphan}; !slices.Equal(ids, slices.Sorted(slices.Values(want))) {
		t.Errorf("listed %v, want %v", ids, want)
	}
	for _, id := range []string{orphan, lost} {
		if err := e.p.Destroy(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if left := e.vms(t); slices.Contains(left, vmName(orphan)) || !slices.Contains(left, "someone-elses") {
		t.Errorf("vms = %v: the orphan must go and the stranger stay", left)
	}
	if recs, _ := e.p.records(); len(recs) != 0 {
		t.Errorf("records = %v", recs)
	}
}

func TestDestroyFailureKeepsTheRecord(t *testing.T) {
	e := newEnv(t, nil)
	req := e.create(t)
	id := req.MachineID.String()
	// Stop fails and the VM still runs: nothing is deleted, the record stays.
	_ = os.WriteFile(filepath.Join(e.home, "fail-stop"), []byte("stuck"), 0o600)
	if err := e.p.Destroy(t.Context(), id); err == nil {
		t.Fatal("destroy hid a stop that failed")
	}
	if r, _ := e.p.load(id); r == nil {
		t.Error("the record went with a VM still there")
	}
	_ = os.Remove(filepath.Join(e.home, "fail-stop"))
	// Delete fails: the record stays, and a retry finishes.
	_ = os.WriteFile(filepath.Join(e.home, "fail-delete"), []byte("busy"), 0o600)
	if err := e.p.Destroy(t.Context(), id); err == nil {
		t.Fatal("destroy hid a delete that failed")
	}
	if r, _ := e.p.load(id); r == nil {
		t.Error("the record went with a VM still there")
	}
	_ = os.Remove(filepath.Join(e.home, "fail-delete"))
	if err := e.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// A VM that ended by itself is not an error to stop.
	req2 := e.create(t)
	e.killAll()
	if err := e.p.Destroy(t.Context(), req2.MachineID.String()); err != nil {
		t.Errorf("a VM that had ended: %v", err)
	}
	// Tart that cannot list makes every step fail loudly.
	_ = os.WriteFile(filepath.Join(e.home, "fail-list"), []byte("broken"), 0o600)
	if err := e.p.Destroy(t.Context(), uuid.New().String()); err == nil {
		t.Error("destroy without a list")
	}
	if _, err := e.p.List(t.Context()); err == nil {
		t.Error("list without a list")
	}
	if _, err := e.p.Create(t.Context(), e.request()); err == nil {
		t.Error("create without a list")
	}
}

func TestPruneCache(t *testing.T) {
	e := newEnv(t, nil)
	for range 3 {
		if _, err := e.p.List(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.file(t, "prune"); got != "--entries caches --older-than 14\n" {
		t.Errorf("prune = %q", got)
	}
	e.p.lastGC = time.Time{}
	_ = os.WriteFile(filepath.Join(e.home, "fail-prune"), []byte("busy"), 0o600)
	if _, err := e.p.List(t.Context()); err != nil {
		t.Errorf("a failed prune failed the list: %v", err)
	}
	// A pull in progress is not waited for.
	e.p.lastGC = time.Time{}
	e.p.fetching.Lock()
	if _, err := e.p.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.p.fetching.Unlock()
	if !e.p.lastGC.IsZero() {
		t.Error("a skipped prune must be tried again next time")
	}
}

func TestCapabilities(t *testing.T) {
	e := newEnv(t, nil)
	c, err := e.p.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.OS, []domain.MachineOS{domain.OSMacOS}) || !slices.Equal(c.Arch, []domain.MachineArch{domain.ArchARM64}) ||
		!slices.Equal(c.ImageFormats, []domain.ImageFormat{domain.ImageTart}) || c.MaxMachines != 2 || c.MaxMemoryMB != 16384-hostReserveMB || c.MaxDiskGB != 500 || c.MaxCPU < 1 {
		t.Errorf("capabilities = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	for _, ch := range c.Checks {
		if ch.Status != statusOK {
			t.Errorf("check %+v", ch)
		}
	}
}

func TestChecks(t *testing.T) {
	status := func(e *testEnv, name string) string {
		for _, ch := range e.p.checks(t.Context()) {
			if ch.Name == name {
				return ch.Status
			}
		}
		return "missing"
	}
	for _, c := range []struct {
		name, check, want string
		edit              func(e *testEnv)
	}{
		{"not a Mac", "apple silicon", statusFail, func(e *testEnv) { e.p.goos = "linux" }},
		{"Intel Mac", "apple silicon", statusFail, func(e *testEnv) { e.p.goarch = "amd64" }},
		{"old tart", "tart", statusFail, func(e *testEnv) { _ = os.WriteFile(filepath.Join(e.home, "version"), []byte("2.20.1"), 0o600) }},
		{"newer tart", "tart", statusOK, func(e *testEnv) {
			_ = os.WriteFile(filepath.Join(e.home, "version"), []byte("tart 2.41.1 (abc)"), 0o600)
		}},
		{"tart is junk", "tart", statusFail, func(e *testEnv) { _ = os.WriteFile(filepath.Join(e.home, "version"), []byte("hello"), 0o600) }},
		{"no tart", "tart", statusFail, func(e *testEnv) { e.p.cli.bin = "/nonexistent/tart" }},
		{"low disk", "disk", statusFail, func(e *testEnv) { e.p.freeGB = func(string) (int, error) { return 5, nil } }},
		{"disk unknown", "disk", statusFail, func(e *testEnv) { e.p.freeGB = func(string) (int, error) { return 0, errors.New("x") } }},
		{"no keys", "image keys", statusFail, func(e *testEnv) { _ = os.Remove(e.p.cfg.TrustedKeysFile) }},
		{"sleeps", "sleep", statusWarn, func(e *testEnv) {
			e.p.pmset = func(context.Context) (string, error) { return " sleep   10\n", nil }
		}},
		{"pmset says nothing", "sleep", statusWarn, func(e *testEnv) { e.p.pmset = func(context.Context) (string, error) { return "", nil } }},
		{"no pmset", "sleep", statusWarn, func(e *testEnv) {
			e.p.pmset = func(context.Context) (string, error) { return "", errors.New("x") }
		}},
		{"no softnet", "softnet", statusFail, func(e *testEnv) { e.p.lookPath = func(string) bool { return false } }},
		{"softnet off", "softnet", statusFail, func(e *testEnv) { e.p.cfg.Softnet = false }},
		{"softnet off, allowed", "softnet", statusWarn, func(e *testEnv) { e.p.cfg.Softnet, e.p.cfg.AllowNoSoftnet = false, true }},
		{"credentials unset", "registry credentials", statusFail, func(e *testEnv) {
			e.p.cfg.RegistryUsername, e.p.cfg.RegistryPassword = "env:NOPE", "env:NOPE2"
		}},
		{"credentials set", "registry credentials", statusOK, func(e *testEnv) {
			e.p.cfg.RegistryUsername, e.p.cfg.RegistryPassword = "env:REG_USER", "env:REG_PASS"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			if err := os.MkdirAll(e.home, 0o700); err != nil {
				t.Fatal(err)
			}
			c.edit(e)
			if got := status(e, c.check); got != c.want {
				t.Errorf("%s = %s, want %s", c.check, got, c.want)
			}
		})
	}
}

func TestRegisteredAsTheTartKind(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tart.yaml")
	_ = os.WriteFile(cfg, []byte("state_dir: "+filepath.Join(dir, "s")+"\nmax_machines: 1\n"), 0o600)
	t.Setenv(envConfig, cfg)
	p, err := fleetagent.NewProvider(domain.ProviderTart)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := p.Capabilities(t.Context()); c.MaxMachines != 1 {
		t.Errorf("max machines = %d", c.MaxMachines)
	}
	_ = os.WriteFile(cfg, []byte("max_machines: 5\n"), 0o600)
	if _, err := fleetagent.NewProvider(domain.ProviderTart); err == nil {
		t.Error("a limit above Apple's was accepted")
	}
}

func TestNewRefusesBadRecords(t *testing.T) {
	dir := t.TempDir()
	cfg := defaults()
	cfg.StateDir = dir
	_ = os.MkdirAll(filepath.Join(dir, "machines"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "machines", uuid.New().String()+".json"), []byte("{"), 0o600)
	if _, err := New(cfg, nil); err == nil {
		t.Error("a broken record was ignored")
	}
	cfg.StateDir = "relative"
	if _, err := New(cfg, nil); err == nil {
		t.Error("a relative state_dir")
	}
}

func TestRecordsIgnoreForeignFiles(t *testing.T) {
	e := newEnv(t, nil)
	_ = os.MkdirAll(filepath.Join(e.p.cfg.StateDir, "machines"), 0o700)
	_ = os.WriteFile(filepath.Join(e.p.cfg.StateDir, "machines", "notes.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(e.p.cfg.StateDir, "machines", "junk.json"), []byte("x"), 0o600)
	recs, err := e.p.records()
	if err != nil || len(recs) != 0 {
		t.Errorf("records = %v, %v", recs, err)
	}
}

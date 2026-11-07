package microvm

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/guestinit"
	"github.com/forecrew/forecrew/internal/images"
)

// fakeVMM starts a shell that waits, with the machine's ID in its arguments as
// a VMM has.
type fakeVMM struct {
	firecracker
	fail bool
}

func (f fakeVMM) command(s spec) (*exec.Cmd, error) {
	if _, err := f.firecracker.command(s); err != nil {
		return nil, err
	}
	script := "while :; do sleep 1; done"
	if f.fail {
		script = "exit 3"
	}
	return exec.Command("sh", "-c", script, s.ID), nil // #nosec G204 -- a test
}

// testEnv is a provider on a temporary directory, with recorded host commands
// and a server of signed images.
type testEnv struct {
	p      *Provider
	dir    string
	key    ed25519.PrivateKey
	srv    *httptest.Server
	mu     sync.Mutex
	cmds   []string
	runErr func(cmd string) error
	img    domain.Image
}

func newEnv(t *testing.T, edit func(*Config)) *testEnv {
	t.Helper()
	e := &testEnv{dir: t.TempDir()}
	pub, key, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	cfg := defaults()
	cfg.StateDir = filepath.Join(e.dir, "state")
	cfg.TrustedKeysFile = filepath.Join(e.dir, "keys.pub")
	cfg.MaxMachines = 2
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
	e.p.run = func(_ context.Context, stdin, name string, args ...string) error {
		cmd := strings.TrimSpace(name + " " + strings.Join(args, " ") + " " + stdin)
		e.mu.Lock()
		e.cmds = append(e.cmds, cmd)
		e.mu.Unlock()
		if e.runErr != nil {
			return e.runErr(cmd)
		}
		return nil
	}
	e.p.chown = func(string, int, int) error { return nil }
	e.p.memoryMB = func() (int, error) { return 8192, nil }
	e.p.availableMB = func() (int, error) { return 8192, nil }
	e.p.localNets = func(netip.Prefix) []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")} }
	e.p.hasDevice = func(name string) bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return slices.ContainsFunc(e.cmds, func(c string) bool { return strings.HasPrefix(c, "ip tuntap add "+name+" ") })
	}
	fc := e.p.vmms[vmmFirecracker].(firecracker)
	e.p.vmms[vmmFirecracker] = fakeVMM{firecracker: fc}

	files := map[string][]byte{"/root.ext4": []byte("root file system"), "/vmlinux": []byte("kernel")}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(e.srv.Close)
	digest := func(b []byte) string { d, _ := images.Digest(strings.NewReader(string(b))); return d }
	e.img = domain.Image{Name: "linux", Version: "1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageRootfs,
		URI: e.srv.URL + "/root.ext4", Digest: digest(files["/root.ext4"]), BuiltAt: time.Unix(1700000000, 0).UTC(),
		Contents: domain.ImageContents{Kernel: &domain.ImageKernel{URI: e.srv.URL + "/vmlinux", Digest: digest(files["/vmlinux"])}}}
	e.img.Signature = images.Sign(key, e.img)
	t.Cleanup(func() {
		recs, _ := e.p.records()
		for _, r := range recs {
			_ = kill(r)
		}
	})
	return e
}

func (e *testEnv) request() fleet.CreateRequest {
	return fleet.CreateRequest{
		MachineID: uuid.New(),
		Shape:     domain.Shape{Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 1024, DiskGB: 1},
		Image:     e.img,
		Bootstrap: &fleet.Bootstrap{ServerURL: "http://192.0.2.10:8080", Token: "fcb_secret"},
		Tags:      map[string]string{fleet.TagMachine: "x"},
	}
}

func (e *testEnv) commands() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.cmds, "\n")
}

func TestCreateListDestroy(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.NetworkAllow = []string{"192.168.1.0/24"}
	inst, err := e.p.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	if inst.ExternalID != id || inst.MachineID != req.MachineID || !inst.Running {
		t.Errorf("instance = %+v", inst)
	}
	dir := e.p.vmms[vmmFirecracker].jailDir(id)
	checkMachine(t, e, dir)

	list, err := e.p.List(t.Context())
	if err != nil || len(list) != 1 || list[0].ExternalID != id || !list[0].Running || list[0].Tags[fleet.TagMachine] != "x" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	r, _ := e.p.load(id)

	before := len(e.cmds)
	if err := e.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	after := strings.Join(e.cmds[before:], "\n")
	e.mu.Unlock()
	if after != "nft -f - table inet forecrew_vm0 {}\ndelete table inet forecrew_vm0\nip link del fcvm0" {
		t.Errorf("destroy ran:\n%s", after)
	}
	if running(r) {
		t.Error("the VMM still runs")
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail is left: %v", err)
	}
	if list, _ := e.p.List(t.Context()); len(list) != 0 {
		t.Errorf("list after destroy = %+v", list)
	}
	if err := e.p.Destroy(t.Context(), id); err != nil {
		t.Errorf("destroying again: %v", err)
	}
}

// A retry carries a new token: the machine is made again.
func TestCreateAgainReplaces(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	first, _ := e.p.load(req.MachineID.String())
	req.Bootstrap.Token = "fcb_second"
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	second, _ := e.p.load(req.MachineID.String())
	if running(first) || !running(second) || first.PID == second.PID {
		t.Errorf("first %d running %v, second %d running %v", first.PID, running(first), second.PID, running(second))
	}
	if list, _ := e.p.List(t.Context()); len(list) != 1 {
		t.Errorf("list = %+v", list)
	}
}

func TestCreateCapacity(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 1 })
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("a second machine: %v", err)
	}
	e2 := newEnv(t, nil)
	big := e2.request()
	big.Shape.MemoryMB = 8000
	if _, err := e2.p.Create(t.Context(), big); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("more memory than the host: %v", err)
	}
	// The records leave room, but the host has given its memory to others.
	e2.p.availableMB = func() (int, error) { return 1500, nil }
	if _, err := e2.p.Create(t.Context(), e2.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("more memory than the host has left: %v", err)
	}
	e2.p.availableMB = func() (int, error) { return 0, errors.New("no meminfo") }
	if _, err := e2.p.Create(t.Context(), e2.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("memory unknown: %v", err)
	}
	gpu := e2.request()
	gpu.Shape.GPU = 1
	if _, err := e2.p.Create(t.Context(), gpu); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("a GPU on a host with none: %v", err)
	}
	if recs, _ := e2.p.records(); len(recs) != 0 {
		t.Errorf("records left: %d", len(recs))
	}
}

// Whatever fails, nothing is left behind.
func TestCreateFailuresLeaveNothing(t *testing.T) {
	cases := map[string]func(e *testEnv, req *fleet.CreateRequest){
		"bad signature": func(_ *testEnv, req *fleet.CreateRequest) { req.Image.Version = "2" },
		"swapped file": func(e *testEnv, req *fleet.CreateRequest) {
			other := req.Image
			other.Digest = "sha256:" + strings.Repeat("0", 64)
			other.Signature = images.Sign(e.key, other)
			req.Image = other
		},
		"no bootstrap":  func(_ *testEnv, req *fleet.CreateRequest) { req.Bootstrap = nil },
		"windows":       func(_ *testEnv, req *fleet.CreateRequest) { req.Shape.OS = domain.OSWindows },
		"too small":     func(_ *testEnv, req *fleet.CreateRequest) { req.Shape.DiskGB = 0 },
		"loopback":      func(_ *testEnv, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://127.0.0.1:8080" },
		"bad url":       func(_ *testEnv, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://[::1" },
		"bad port":      func(_ *testEnv, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://192.0.2.1:x" },
		"ipv6 only":     func(_ *testEnv, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://[2001:db8::1]:8080" },
		"unknown key":   func(_ *testEnv, req *fleet.CreateRequest) { req.Shape.ProviderConfig = map[string]any{"x": 1} },
		"qcow2":         func(_ *testEnv, req *fleet.CreateRequest) { req.Image.Format = domain.ImageQCOW2 },
		"nft fails": func(e *testEnv, _ *fleet.CreateRequest) {
			e.runErr = func(cmd string) error {
				if strings.HasPrefix(cmd, "ip tuntap") {
					return errors.New("no tap")
				}
				return nil
			}
		},
		"vmm exits": func(e *testEnv, _ *fleet.CreateRequest) {
			e.p.vmms[vmmFirecracker] = fakeVMM{firecracker: e.p.vmms[vmmFirecracker].(fakeVMM).firecracker, fail: true}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			req := e.request()
			breakIt(e, &req)
			if _, err := e.p.Create(t.Context(), req); err == nil {
				t.Fatal("no error")
			} else if strings.Contains(err.Error(), "fcb_") {
				t.Errorf("the error has the token: %v", err)
			}
			if recs, _ := e.p.records(); len(recs) != 0 {
				t.Errorf("records left: %d", len(recs))
			}
			jail := filepath.Join(e.p.cfg.StateDir, "jail", "firecracker")
			if entries, _ := os.ReadDir(jail); len(entries) != 0 {
				t.Errorf("jails left: %v", entries)
			}
		})
	}
}

func TestCreateHTTPSNeedsACA(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.CAFile = "/nonexistent/ca.pem" })
	req := e.request()
	req.Bootstrap.ServerURL = "https://192.0.2.10"
	if _, err := e.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Errorf("err = %v", err)
	}
	ca := filepath.Join(e.dir, "ca.pem")
	_ = os.WriteFile(ca, []byte("PEM"), 0o600)
	e.p.cfg.CAFile = ca
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	r, _ := e.p.load(req.MachineID.String())
	if r.Port != 443 {
		t.Errorf("port = %d", r.Port)
	}
}

func TestUpdate(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	before := strings.Count(e.commands(), "nft")
	if err := e.p.Update(t.Context(), id, fleet.UpdateRequest{Tags: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(e.commands(), "nft") != before {
		t.Error("the same network was applied again")
	}
	if err := e.p.Update(t.Context(), id, fleet.UpdateRequest{NetworkAllow: []string{"10.1.0.0/16"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.commands(), "ct original ip daddr 10.1.0.0/16 accept") {
		t.Error("the allowlist was not applied")
	}
	// A name that does not resolve opens nothing; the rest still applies.
	if err := e.p.Update(t.Context(), id, fleet.UpdateRequest{NetworkAllow: []string{"bad.invalid", "10.2.0.0/16"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.commands(), "ct original ip daddr 10.2.0.0/16 accept") {
		t.Error("the allowlist was not applied")
	}
	if err := e.p.Update(t.Context(), uuid.New().String(), fleet.UpdateRequest{}); err == nil {
		t.Error("an unknown machine was updated")
	}
	// The deadline kills the VMM whatever else happens.
	r, _ := e.p.load(id)
	if err := e.p.Update(t.Context(), id, fleet.UpdateRequest{Deadline: time.Now().Add(50 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); running(r) && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
	}
	if running(r) {
		t.Error("the deadline did not kill the machine")
	}
	if list, _ := e.p.List(t.Context()); len(list) != 1 || list[0].Running {
		t.Errorf("list = %+v", list)
	}
}

// A restarted agent finds its machines and their deadlines.
func TestRestartFindsMachines(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Deadline = time.Now().Add(time.Hour)
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	again, err := New(e.p.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := again.List(t.Context())
	if err != nil || len(list) != 1 || !list[0].Running {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if again.timers[req.MachineID.String()] == nil {
		t.Error("the deadline was lost")
	}
	// A killed VMM is listed as not running, for forecrewd to end it.
	r, _ := again.load(req.MachineID.String())
	if err := kill(r); err != nil {
		t.Fatal(err)
	}
	if list, _ := again.List(t.Context()); list[0].Running {
		t.Error("a dead machine runs")
	}
}

func TestRecordsIgnoreForeignFiles(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.p.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	machines := filepath.Join(e.p.cfg.StateDir, "machines")
	_ = os.WriteFile(filepath.Join(machines, "notes.txt"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(machines, "../../x.json"), nil, 0o600)
	if r, err := e.p.load("../x"); r != nil || err != nil {
		t.Errorf("load of a path = %v, %v", r, err)
	}
	if recs, err := e.p.records(); err != nil || len(recs) != 0 {
		t.Errorf("records = %v, %v", recs, err)
	}
	bad := uuid.New().String()
	_ = os.WriteFile(filepath.Join(machines, bad+".json"), []byte("{"), 0o600)
	if _, err := e.p.records(); err == nil {
		t.Error("a broken record was read")
	}
	if err := e.p.Destroy(t.Context(), bad); err == nil {
		t.Error("a broken record was destroyed blind")
	}
}

func TestRunning(t *testing.T) {
	if running(&record{ID: "x"}) || running(&record{ID: "no-such-machine-id", PID: os.Getpid()}) {
		t.Error("a process without the ID counts")
	}
	cmd := exec.Command("sh", "-c", "exit 0", "the-id")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if running(&record{ID: "the-id", PID: cmd.Process.Pid}) {
		t.Error("a process that ended counts")
	}
}

func TestCapabilities(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.GPUs = []string{"0000:01:00.0"} })
	c, err := e.p.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.OS, []domain.MachineOS{domain.OSLinux}) || !slices.Equal(c.ImageFormats, []domain.ImageFormat{domain.ImageRootfs}) ||
		c.MaxMachines != 2 || c.MaxMemoryMB != 8192-hostReserveMB || c.MaxDiskGB == 0 {
		t.Errorf("capabilities = %+v", c)
	}
	names := map[string]string{}
	for _, ch := range c.Checks {
		names[ch.Name] = ch.Status
	}
	for _, want := range []string{"kvm access", "root", "firecracker", "network tools", "ip forwarding", "image keys", "reflinks", "cloud-hypervisor"} {
		if names[want] == "" {
			t.Errorf("no check %q in %v", want, names)
		}
	}
	if names["image keys"] != statusOK {
		t.Errorf("image keys = %s", names["image keys"])
	}
	if err := c.Validate(); err != nil {
		t.Errorf("capabilities do not validate: %v", err)
	}
}

func TestMaxMachines(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 0; c.Subnet = "172.30.0.0/28" })
	if got := e.p.maxMachines(); got != 4 {
		t.Errorf("a /28 = %d machines", got)
	}
	e.p.cfg.Subnet, e.p.subnet = "172.30.0.0/16", mustPrefix("172.30.0.0/16")
	if got := e.p.maxMachines(); got != (8192-hostReserveMB)/minMachineMB {
		t.Errorf("from memory = %d", got)
	}
	e.p.memoryMB = func() (int, error) { return 0, errors.New("no") }
	if got := e.p.maxMachines(); got != 1 {
		t.Errorf("without memory = %d", got)
	}
}

func TestTrustedKeysMissing(t *testing.T) {
	e := newEnv(t, nil)
	_ = os.WriteFile(e.p.cfg.TrustedKeysFile, []byte("# none\n"), 0o600)
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || !strings.Contains(err.Error(), "lists none") {
		t.Errorf("no keys: %v", err)
	}
	_ = os.WriteFile(e.p.cfg.TrustedKeysFile, []byte("junk\n"), 0o600)
	if _, err := e.p.trustedKeys(); err == nil {
		t.Error("junk keys were read")
	}
	_ = os.Remove(e.p.cfg.TrustedKeysFile)
	if _, err := e.p.trustedKeys(); err == nil {
		t.Error("no key file: no error")
	}
}

func TestFetch(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.p.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.p.imageDir(), "x")
	for name, uri := range map[string]string{"missing": e.srv.URL + "/nothing", "s3": "s3://bucket/x", "unreachable": "http://127.0.0.1:1/x"} {
		if err := e.p.fetch(t.Context(), uri, e.img.Digest, path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := e.p.fetch(t.Context(), e.srv.URL+"/vmlinux", e.img.Digest, path); !errors.Is(err, images.ErrDigest) {
		t.Errorf("another file: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a wrong file was kept")
	}
	if _, err := e.p.cachePath("md5:abc"); err == nil {
		t.Error("a digest that names another path was accepted")
	}
	if entries, _ := os.ReadDir(e.p.imageDir()); len(entries) != 0 {
		t.Errorf("left in the cache: %v", entries)
	}
}

// A cached file that is not the one signed is fetched again; one that cannot be
// fetched refuses the machine.
func TestWrongCachedFileIsFetchedAgain(t *testing.T) {
	e := newEnv(t, nil)
	_ = e.p.prepareDirs()
	root, _ := e.p.cachePath(e.img.Digest)
	_ = os.WriteFile(root, []byte("not the image"), 0o600)
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatalf("a damaged cached file: %v", err)
	}
	if got, _ := os.ReadFile(root); string(got) != "root file system" {
		t.Errorf("cached file = %q", got)
	}

	e2 := newEnv(t, nil)
	_ = e2.p.prepareDirs()
	kernel, _ := e2.p.cachePath(e2.img.Contents.Kernel.Digest)
	_ = os.WriteFile(kernel, []byte("not the kernel"), 0o600)
	req := e2.request()
	req.Image.Contents.Kernel.URI = "s3://bucket/kernel"
	req.Image.Signature = images.Sign(e2.key, req.Image)
	if _, err := e2.p.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), "only http and https") {
		t.Errorf("a wrong placed file that cannot be fetched: %v", err)
	}
	if _, err := os.Stat(kernel); err == nil {
		t.Error("the wrong file was kept")
	}
}

// A cached file changed after it was checked is checked again before the next
// machine uses it, by this process too; one left alone is not read again.
func TestImageCheckedAtUse(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 3 })
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	root, _ := e.p.cachePath(e.img.Digest)
	sig := e.img.Signature
	before := e.p.verified[sig]
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	if !e.p.unchanged(sig, root, mustKernel(t, e)) {
		t.Error("a file left alone counts as changed")
	}
	// Same size, same times: only the change time and the content differ.
	st, _ := os.Stat(root)
	if err := os.WriteFile(root, []byte("ROOT FILE SYSTEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(root, st.ModTime(), st.ModTime())
	if e.p.unchanged(sig, root, mustKernel(t, e)) {
		t.Fatal("a rewritten file counts as unchanged")
	}
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatalf("a rewritten file is not fetched again: %v", err)
	}
	if got, _ := os.ReadFile(root); string(got) != "root file system" {
		t.Errorf("cached file = %q", got)
	}
	if e.p.verified[sig] == before {
		t.Error("the new file was not stamped")
	}
}

func mustKernel(t *testing.T, e *testEnv) string {
	t.Helper()
	k, err := e.p.cachePath(e.img.Contents.Kernel.Digest)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestDiskRoom(t *testing.T) {
	e := newEnv(t, nil)
	e.p.freeBytes = func(string) (int64, error) { return 5 << 29, nil }
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	// The first machine may still write about 1 GB of its disk, so a 2 GB disk
	// does not fit in 2.5 GB.
	req := e.request()
	req.Shape.DiskGB = 2
	if _, err := e.p.Create(t.Context(), req); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("a disk with no room: %v", err)
	}
	e.p.freeBytes = func(string) (int64, error) { return 0, errors.New("no") }
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Errorf("unknown room: %v", err)
	}
	if free, err := freeBytes(e.dir); err != nil || free <= 0 {
		t.Errorf("freeBytes = %d, %v", free, err)
	}
}

// The deadline ends the VMM and whatever it started.
func TestKillEndsTheSession(t *testing.T) {
	dir := t.TempDir()
	id := uuid.New().String()
	pidFile := filepath.Join(dir, "child")
	cmd := exec.Command("sh", "-c", `sleep 300 & echo $! >"$1"; while :; do sleep 1; done`, id, pidFile) // #nosec G204 -- a test
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	var child int
	for end := time.Now().Add(5 * time.Second); child == 0 && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		raw, _ := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if err := kill(&record{ID: id, PID: cmd.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(child, 0); err == nil {
		raw, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat"))
		if !strings.Contains(string(raw), ") Z") {
			t.Errorf("a process of the jail lives on: %s", raw)
		}
	}
}

// The shared cache disk is attached read-only, and nobody may write it.
func TestCacheDisk(t *testing.T) {
	e := newEnv(t, nil)
	_ = e.p.prepareDirs()
	cache := filepath.Join(e.p.cfg.StateDir, fileCache)
	if err := os.WriteFile(cache, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := e.request()
	if _, err := e.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(cache); st.Mode().Perm() != 0o444 {
		t.Errorf("cache disk mode = %v", st.Mode())
	}
	raw, _ := os.ReadFile(filepath.Join(e.p.vmms[vmmFirecracker].jailDir(req.MachineID.String()), fileVM))
	if !strings.Contains(string(raw), `"drive_id":"cache"`) || !strings.Contains(string(raw), "forecrew.cache=/dev/vdc") {
		t.Errorf("vm.json = %s", raw)
	}
}

func TestPruneCacheOfTheAgent(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	entries, _ := os.ReadDir(e.p.imageDir())
	for _, f := range entries {
		_ = os.Chtimes(filepath.Join(e.p.imageDir(), f.Name()), old, old)
	}
	e.p.lastGC = time.Time{}
	if _, err := e.p.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(e.p.imageDir()); len(left) != 0 {
		t.Errorf("old images kept: %v", left)
	}
	// A machine has its own disk, and a new one fetches again.
	if _, err := e.p.Create(t.Context(), e.request()); err != nil {
		t.Fatal(err)
	}
	// While a fetch runs the cleaning waits for another round.
	e.p.lastGC = time.Time{}
	e.p.fetching.Lock()
	e.p.pruneCache()
	e.p.fetching.Unlock()
	if !e.p.lastGC.IsZero() {
		t.Error("a skipped round counts as done")
	}
}

func TestPickVMM(t *testing.T) {
	for _, c := range []struct {
		gpu    int
		config map[string]any
		want   string
		bad    bool
	}{
		{0, nil, vmmFirecracker, false},
		{1, nil, vmmCloudHypervisor, false},
		{0, map[string]any{"vmm": "cloud-hypervisor"}, vmmCloudHypervisor, false},
		{1, map[string]any{"vmm": "firecracker"}, "", true},
		{0, map[string]any{"vmm": "qemu"}, "", true},
		{0, map[string]any{"vmm": 3}, "", true},
		{0, map[string]any{"node": "x"}, "", true},
	} {
		got, err := pickVMM(domain.Shape{GPU: c.gpu, ProviderConfig: c.config})
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("%d %v = %q, %v", c.gpu, c.config, got, err)
		}
	}
}

func TestCloudHypervisorCommand(t *testing.T) {
	ch := cloudHypervisor{bin: "/usr/bin/cloud-hypervisor", base: "/state/jail"}
	if ch.halt() != "poweroff" || ch.jailDir("id") != "/state/jail/cloud-hypervisor/id/root" {
		t.Error("halt or directory")
	}
	cmd, err := ch.command(spec{ID: "id", Dir: "/d", CPU: 2, MemoryMB: 2048, Tap: "fcvm1", MAC: "06:00:00:00:00:01", BootArgs: "a b",
		Cache: true, GPUs: []string{"0000:01:00.0"}, UID: 900001, GID: 900001})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--kernel /d/kernel", "--cpus boot=2", "--memory size=2048M", "--net tap=fcvm1,mac=06:00:00:00:00:01",
		"--disk path=/d/root.ext4,image_type=raw path=/d/config.img,image_type=raw path=/d/cache.ext4,image_type=raw,readonly=on",
		"--device path=/sys/bus/pci/devices/0000:01:00.0/", "--seccomp true"} {
		if !strings.Contains(args, want) {
			t.Errorf("no %q in %s", want, args)
		}
	}
	if cmd.SysProcAttr.Credential.Uid != 900001 {
		t.Error("not run as the machine's user")
	}
}

func TestFirecrackerCommand(t *testing.T) {
	dir := t.TempDir()
	fc := firecracker{jailer: "/usr/bin/jailer", bin: "/usr/bin/firecracker", base: "/state/jail"}
	cmd, err := fc.command(spec{ID: "id", Dir: dir, CPU: 1, MemoryMB: 512, Tap: "fcvm0", MAC: "m", BootArgs: "b", Cache: true, UID: 7, GID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "/usr/bin/jailer --id id --exec-file /usr/bin/firecracker --uid 7 --gid 7 --chroot-base-dir /state/jail -- --config-file vm.json --no-api" {
		t.Errorf("args = %s", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileVM))
	for _, want := range []string{`"vcpu_count":1`, `"mem_size_mib":512`, `"host_dev_name":"fcvm0"`, `"drive_id":"cache"`, `"is_read_only":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("no %s in %s", want, raw)
		}
	}
	if _, err := fc.command(spec{Dir: "/nonexistent"}); err == nil {
		t.Error("no error writing to a missing directory")
	}
}

func TestGPUs(t *testing.T) {
	e := newEnv(t, nil)
	sys := t.TempDir()
	old := sysPCI
	sysPCI = sys
	t.Cleanup(func() { sysPCI = old })
	_ = os.MkdirAll(filepath.Join(sys, "0000:01:00.0"), 0o755)
	_ = os.Symlink("../../../kernel/iommu_groups/12", filepath.Join(sys, "0000:01:00.0", "iommu_group"))
	var owned []string
	e.p.chown = func(path string, uid, _ int) error {
		owned = append(owned, fmt.Sprintf("%s=%d", path, uid))
		return nil
	}
	r := &record{GPUs: []string{"0000:01:00.0"}}
	if err := e.p.claimGPUs(r, 900003); err != nil {
		t.Fatal(err)
	}
	if err := e.p.releaseGPUs(r); err != nil {
		t.Fatal(err)
	}
	if strings.Join(owned, " ") != "/dev/vfio/12=900003 /dev/vfio/12=0" {
		t.Errorf("owned = %v", owned)
	}
	if err := e.p.claimGPUs(&record{GPUs: []string{"0000:09:00.0"}}, 1); err == nil {
		t.Error("a device with no IOMMU group was claimed")
	}
}

func TestLink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	_ = os.WriteFile(src, []byte("x"), 0o444)
	if err := link(src, filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if err := link(filepath.Join(dir, "missing"), filepath.Join(dir, "c")); err == nil {
		t.Error("no error for a missing file")
	}
	if err := clone(src, filepath.Join(dir, "b")); err == nil {
		t.Error("clone replaced a file")
	}
}

func TestRegistered(t *testing.T) {
	t.Setenv(envConfig, filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("TMPDIR", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "microvm.yaml")
	_ = os.WriteFile(cfgPath, []byte("state_dir: "+t.TempDir()+"\n"), 0o600)
	t.Setenv(envConfig, cfgPath)
	if _, err := newRegistered(); err != nil {
		t.Errorf("the registered factory: %v", err)
	}
	_ = os.WriteFile(cfgPath, []byte("nope: 1\n"), 0o600)
	if _, err := newRegistered(); err == nil {
		t.Error("a bad config was accepted")
	}
}

// checkMachine looks at what Create made in dir and ran on the host.
func checkMachine(t *testing.T, e *testEnv, dir string) {
	t.Helper()
	if st, err := os.Stat(filepath.Join(dir, fileRoot)); err != nil || st.Size() != 1<<30 {
		t.Errorf("root disk = %v, %v", st, err)
	}
	if k, err := os.ReadFile(filepath.Join(dir, fileKernel)); err != nil || string(k) != "kernel" {
		t.Errorf("kernel = %q, %v", k, err)
	}
	f, err := os.Open(filepath.Join(dir, fileConfig))
	if err != nil {
		t.Fatal(err)
	}
	s, err := guestinit.Decode(f)
	_ = f.Close()
	if err != nil || s.Token != "fcb_secret" || s.ServerAddrs[0] != "192.0.2.10" || s.CAPEM != "" {
		t.Errorf("settings = %v, %v", s.ServerAddrs, err)
	}
	var vm map[string]any
	raw, _ := os.ReadFile(filepath.Join(dir, fileVM))
	if err := json.Unmarshal(raw, &vm); err != nil {
		t.Fatal(err)
	}
	boot := vm["boot-source"].(map[string]any)["boot_args"].(string)
	for _, want := range []string{"forecrew.ip=172.30.0.2/30", "forecrew.gw=172.30.0.1", "forecrew.halt=reboot", images.GuestInitArgs, "reboot=k"} {
		if !strings.Contains(boot, want) {
			t.Errorf("no %q in %q", want, boot)
		}
	}
	if strings.Contains(string(raw), "fcb_secret") {
		t.Error("the token is in vm.json")
	}
	cmds := e.commands()
	for _, want := range []string{"table inet forecrew_vm0", "ct original ip daddr 192.0.2.10 ct original proto-dst 8080 accept", "ct original ip daddr 192.168.1.0/24 accept", "198.51.100.0/24",
		"ip tuntap add fcvm0 mode tap user 900000 group 900000", "ip addr add 172.30.0.1/30 dev fcvm0", "ip link set fcvm0 up"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("no %q in\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "fcb_secret") {
		t.Error("the token reached a host command")
	}

}

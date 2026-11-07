package guestinit

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSystem records what a boot asks of the machine.
type fakeSystem struct {
	mu         sync.Mutex
	calls      []string
	mountErr   error
	procErr    error
	clockErr   error
	networkErr error
	growErr    error
	cacheErr   error
	modulesErr error
	halted     string
}

func (f *fakeSystem) call(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeSystem) mountProc() error { f.call("proc"); return f.procErr }
func (f *fakeSystem) mountAll() error  { f.call("mount"); return f.mountErr }
func (f *fakeSystem) setClock(t time.Time) error {
	f.call("clock %d", t.Unix())
	return f.clockErr
}
func (f *fakeSystem) loadModules(p []string) error {
	f.call("modules %s", strings.Join(p, " "))
	return f.modulesErr
}
func (f *fakeSystem) network(ip, gw string) error {
	f.call("network %s %s", ip, gw)
	return f.networkErr
}
func (f *fakeSystem) growRoot(d string) error { f.call("grow %s", d); return f.growErr }
func (f *fakeSystem) mountCache(d, _ string) error {
	f.call("cache %s", d)
	return f.cacheErr
}
func (f *fakeSystem) halt(how string) error { f.halted = how + "!"; return nil }

func (f *fakeSystem) wait(pid int) int {
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return -1
	}
	return ws.ExitStatus()
}

// testGuest is a guest under a directory, with a config disk, a worker that
// writes down what it got, and a fake server.
type testGuest struct {
	*guest
	sys  *fakeSystem
	logs *bytes.Buffer
	dir  string
	srv  *httptest.Server
	seen []string
	// modprobe is what the kernel would run, as the exchange saw it.
	modprobe string
}

const workerScript = `#!/bin/sh
/usr/bin/env >"${0%/*}/worker.env"
/bin/cat "$3" >"${0%/*}/worker.yaml"
`

func newGuest(t *testing.T, cmdline string) *testGuest {
	t.Helper()
	dir := t.TempDir()
	g := &testGuest{sys: &fakeSystem{}, logs: &bytes.Buffer{}, dir: dir}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.seen = append(g.seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		if raw, err := os.ReadFile(filepath.Join(g.dir, "proc/sys/kernel/modprobe")); err == nil {
			g.modprobe = string(raw)
		}
		if r.Header.Get("Authorization") != "Bearer fcb_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"worker_token": "fcw_new", "name": "m1", "labels": []string{"linux"}, "max_jobs": 2})
	}))
	t.Cleanup(g.srv.Close)
	g.guest = &guest{sys: g.sys, root: dir, version: "v1", dockerWait: 300 * time.Millisecond,
		say: func(f string, a ...any) { fmt.Fprintf(g.logs, f+"\n", a...) }}
	write(t, filepath.Join(dir, "proc/cmdline"), cmdline, 0o644)
	write(t, filepath.Join(dir, "proc/sys/kernel/modprobe"), "/sbin/modprobe\n", 0o644)
	write(t, filepath.Join(dir, "etc/resolv.conf"), "old", 0o644)
	write(t, filepath.Join(dir, "opt/forecrew/forecrew-worker"), workerScript, 0o755)
	g.configDisk(t, "fcb_good")
	return g
}

func (g *testGuest) configDisk(t *testing.T, token string) {
	t.Helper()
	u, err := url.Parse(g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	// A name nobody resolves: the address comes from the settings alone.
	// The host's clock is ahead of the guest's: in 2100.
	raw, err := Encode(Settings{ServerURL: "http://forecrew.invalid:" + port, Token: token, ServerAddrs: []string{host}, Time: time.Unix(4102444800, 0)})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(g.dir, "dev/vdb"), string(raw), 0o600)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func bootContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestBoot(t *testing.T) {
	g := newGuest(t, "forecrew.ip=10.1.0.2/30 forecrew.gw=10.1.0.1 forecrew.dns=1.1.1.1 forecrew.halt=reboot forecrew.modules=/a.ko,/b.ko forecrew.cache=/dev/vdc")
	write(t, filepath.Join(g.dir, "etc/forecrew/labels"), "docker\n# a comment\n\n", 0o644)
	write(t, filepath.Join(g.dir, "etc/forecrew/env"), "PATH=/image/bin\nGOFLAGS=-mod=mod\nFORECREW_WORKER_TOKEN=stolen\nbad name=1\nnot a variable\n"+
		"HTTPS_PROXY=http://evil\nhttp_proxy=http://evil\nALL_PROXY=evil\nSSL_CERT_FILE=/evil.pem\nSSL_CERT_DIR=/evil\nGODEBUG=x509ignoreCN=1\n", 0o644)
	g.boot(bootContext(t))

	if len(g.seen) != 1 || g.seen[0] != "/worker/v1/bootstrap Bearer fcb_good" {
		t.Fatalf("requests = %q\n%s", g.seen, g.logs)
	}
	checkModprobe(t, g)
	if g.sys.halted != "reboot!" {
		t.Errorf("halted = %q", g.sys.halted)
	}
	want := "proc|mount|modules /a.ko /b.ko|network 10.1.0.2/30 10.1.0.1|grow /dev/vda|cache /dev/vdc|clock 4102444800"
	if got := strings.Join(g.sys.calls, "|"); got != want {
		t.Errorf("calls = %s", got)
	}
	if got := read(t, filepath.Join(g.dir, "etc/resolv.conf")); got != "nameserver 1.1.1.1\n" {
		t.Errorf("resolv.conf = %q", got)
	}
	// The disk holds nothing any more.
	if disk := read(t, filepath.Join(g.dir, "dev/vdb")); strings.Trim(disk, "\x00") != "" {
		t.Errorf("the config disk was not wiped: %q", disk)
	}
	workerDir := filepath.Join(g.dir, "opt/forecrew")
	env := read(t, filepath.Join(workerDir, "worker.env"))
	for _, want := range []string{"FORECREW_WORKER_TOKEN=fcw_new", "GOFLAGS=-mod=mod", "PATH=/image/bin", "HOME=/root"} {
		if !strings.Contains(env, want) {
			t.Errorf("no %q in the worker's environment:\n%s", want, env)
		}
	}
	for _, bad := range []string{"stolen", "fcb_good", "BOOTSTRAP", "evil", "GODEBUG"} {
		if strings.Contains(env, bad) {
			t.Errorf("%q in the worker's environment:\n%s", bad, env)
		}
	}
	if hosts := read(t, filepath.Join(g.dir, "etc/hosts")); !strings.Contains(hosts, "127.0.0.1 forecrew.invalid") {
		t.Errorf("hosts = %q", hosts)
	}
	yaml := read(t, filepath.Join(workerDir, "worker.yaml"))
	for _, want := range []string{"name: m1", "max_jobs: 2", "- docker", "- linux", "server_url: http://forecrew.invalid"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("no %q in worker.yaml:\n%s", want, yaml)
		}
	}
	if strings.Contains(yaml, "fcw_new") {
		t.Errorf("a token in worker.yaml:\n%s", yaml)
	}
	if st, err := os.Stat(filepath.Join(g.dir, "run/forecrew/worker.yaml")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("worker.yaml = %v, %v", st, err)
	}
}

// checkModprobe checks that the module loader was off during the exchange only.
func checkModprobe(t *testing.T, g *testGuest) {
	t.Helper()
	if g.modprobe != modprobeOff+"\n" {
		t.Errorf("during the exchange the kernel would run %q", g.modprobe)
	}
	if got := read(t, filepath.Join(g.dir, "proc/sys/kernel/modprobe")); got != "/sbin/modprobe\n" {
		t.Errorf("the module loader was not turned back on: %q", got)
	}
}

// A refused token ends the machine with nothing of the image started.
func TestBootRefusedToken(t *testing.T) {
	g := newGuest(t, "")
	g.configDisk(t, "fcb_bad")
	write(t, filepath.Join(g.dir, "usr/bin/dockerd"), "#!/bin/sh\ntouch \"$0.ran\"\n", 0o755)
	g.boot(bootContext(t))
	if _, err := os.Stat(filepath.Join(g.dir, "opt/forecrew/worker.env")); err == nil {
		t.Error("the worker ran")
	}
	if !strings.Contains(g.logs.String(), "bootstrap") || strings.Contains(g.logs.String(), "fcb_bad") {
		t.Errorf("logs = %s", g.logs)
	}
	if g.sys.halted == "" {
		t.Error("the machine did not end")
	}
}

func TestBootStopsEarly(t *testing.T) {
	for name, breakIt := range map[string]func(g *testGuest, t *testing.T){
		"mount": func(g *testGuest, _ *testing.T) { g.sys.mountErr = errors.New("no") },
		"proc":  func(g *testGuest, _ *testing.T) { g.sys.procErr = errors.New("no") },
		"loader not writable": func(g *testGuest, t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root writes anyway")
			}
			if err := os.Chmod(filepath.Join(g.dir, "proc/sys/kernel/modprobe"), 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"network":    func(g *testGuest, _ *testing.T) { g.sys.networkErr = errors.New("no") },
		"no cmdline": func(g *testGuest, _ *testing.T) { _ = os.Remove(filepath.Join(g.dir, "proc/cmdline")) },
		"no disk":    func(g *testGuest, _ *testing.T) { _ = os.Remove(filepath.Join(g.dir, "dev/vdb")) },
		"bad disk":   func(g *testGuest, t *testing.T) { write(t, filepath.Join(g.dir, "dev/vdb"), "junk", 0o600) },
		"read-only disk": func(g *testGuest, t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root writes anyway")
			}
			if err := os.Chmod(filepath.Join(g.dir, "dev/vdb"), 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"no worker": func(g *testGuest, _ *testing.T) { _ = os.Remove(filepath.Join(g.dir, "opt/forecrew/forecrew-worker")) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newGuest(t, "")
			breakIt(g, t)
			g.boot(bootContext(t))
			if g.sys.halted == "" {
				t.Error("the machine did not end")
			}
			if g.logs.Len() == 0 {
				t.Error("nothing says why")
			}
			if name != "no worker" && len(g.seen) != 0 {
				t.Errorf("the token was used: %q", g.seen)
			}
		})
	}
}

// A kernel built without modules has no loader to turn off, and boots.
func TestBootWithoutModules(t *testing.T) {
	g := newGuest(t, "")
	if err := os.Remove(filepath.Join(g.dir, "proc/sys/kernel/modprobe")); err != nil {
		t.Fatal(err)
	}
	g.boot(bootContext(t))
	if len(g.seen) != 1 || g.seen[0] != "/worker/v1/bootstrap Bearer fcb_good" {
		t.Fatalf("requests = %q\n%s", g.seen, g.logs)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "opt/forecrew/worker.env")); err != nil {
		t.Errorf("the worker did not start: %v\n%s", err, g.logs)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "proc/sys/kernel/modprobe")); err == nil {
		t.Error("a loader file was made")
	}
}

// A guest whose clock is ahead of the host's moment keeps it.
func TestClockOnlyMovesForward(t *testing.T) {
	g := newGuest(t, "")
	u, _ := url.Parse(g.srv.URL)
	raw, err := Encode(Settings{ServerURL: g.srv.URL, Token: "fcb_good", ServerAddrs: []string{u.Hostname()}, Time: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(g.dir, "dev/vdb"), string(raw), 0o600)
	g.sys.clockErr = errors.New("unused")
	g.boot(bootContext(t))
	if strings.Contains(strings.Join(g.sys.calls, "|"), "clock") || len(g.seen) != 1 {
		t.Errorf("calls = %v, requests = %v", g.sys.calls, g.seen)
	}
}

func TestClockFailureIsSaid(t *testing.T) {
	g := newGuest(t, "")
	g.sys.clockErr = errors.New("no clock")
	g.boot(bootContext(t))
	if !strings.Contains(g.logs.String(), "no clock") || len(g.seen) != 1 {
		t.Errorf("logs = %s, requests = %v", g.logs, g.seen)
	}
}

// A loader that cannot be turned back on is said, and the machine goes on.
func TestModprobeRestoreFailureIsSaid(t *testing.T) {
	g := newGuest(t, "")
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = os.Remove(filepath.Join(g.dir, "proc/sys/kernel/modprobe"))
		_ = os.Mkdir(filepath.Join(g.dir, "proc/sys/kernel/modprobe"), 0o755)
		g.seen = append(g.seen, "x")
		_, _ = w.Write([]byte(`{"worker_token":"fcw_new"}`))
	})
	g.boot(bootContext(t))
	if !strings.Contains(g.logs.String(), "back on") {
		t.Errorf("logs = %s", g.logs)
	}
}

// What fails on the way but leaves a working machine does not stop it.
func TestBootGoesOnWithoutTheExtras(t *testing.T) {
	g := newGuest(t, "forecrew.modules=/a.ko forecrew.cache=/dev/vdc")
	g.sys.growErr, g.sys.cacheErr, g.sys.modulesErr = errors.New("no grow"), errors.New("no cache"), errors.New("no modules")
	g.boot(bootContext(t))
	if len(g.seen) != 1 {
		t.Errorf("requests = %q", g.seen)
	}
	for _, want := range []string{"no grow", "no cache", "no modules"} {
		if !strings.Contains(g.logs.String(), want) {
			t.Errorf("no %q in %s", want, g.logs)
		}
	}
}

func TestStartsDockerAfterTheExchange(t *testing.T) {
	g := newGuest(t, "")
	sock := filepath.Join(g.dir, "var/run/docker.sock")
	write(t, filepath.Join(g.dir, "usr/bin/dockerd"), "#!/bin/sh\nenv >\""+g.dir+"/dockerd.env\"\nmkdir -p \""+filepath.Dir(sock)+"\"\ntouch \""+sock+"\"\n", 0o755)
	g.boot(bootContext(t))
	env := read(t, filepath.Join(g.dir, "dockerd.env"))
	if strings.Contains(env, "FORECREW") {
		t.Errorf("a token reached dockerd:\n%s", env)
	}
	if strings.Contains(g.logs.String(), "dockerd") && !strings.Contains(g.logs.String(), "ended") {
		t.Errorf("logs = %s", g.logs)
	}
}

func TestDockerThatDoesNotComeUp(t *testing.T) {
	g := newGuest(t, "")
	write(t, filepath.Join(g.dir, "usr/bin/dockerd"), "#!/bin/sh\nexit 1\n", 0o755)
	g.boot(bootContext(t))
	if !strings.Contains(g.logs.String(), "dockerd is not up") {
		t.Errorf("logs = %s", g.logs)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "opt/forecrew/worker.env")); err != nil {
		t.Error("the worker did not start without Docker")
	}
}

// The proxy of the image's environment must not see the exchange.
func TestExchangeIgnoresTheEnvironmentsProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	g := newGuest(t, "")
	g.boot(bootContext(t))
	if len(g.seen) != 1 {
		t.Errorf("requests = %q\n%s", g.seen, g.logs)
	}
}

func TestPinnedClientTrustsOnlyTheSettings(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"worker_token":"fcw_x"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	s := Settings{ServerURL: "https://example.com:" + port, Token: "t", ServerAddrs: []string{host}}
	if _, err := pinnedClient(s); err == nil {
		t.Error("an https server without a CA was accepted")
	}
	s.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	c, err := pinnedClient(s)
	if err != nil {
		t.Fatal(err)
	}
	// httptest's certificate is for example.com, by the pinned address.
	resp, err := c.Post(s.ServerURL+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	other := httptest.NewTLSServer(http.NotFoundHandler()) // same certificate
	defer other.Close()
	s.CAPEM = strings.Repeat("x", 10)
	if _, err := pinnedClient(s); err == nil {
		t.Error("a CA that is not one was accepted")
	}
	if _, err := pinnedClient(Settings{ServerURL: "::", ServerAddrs: []string{host}}); err == nil {
		t.Error("a bad URL was accepted")
	}
}

func TestPinnedClientDialFails(t *testing.T) {
	c, err := pinnedClient(Settings{ServerURL: "http://x.invalid:1", ServerAddrs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get("http://x.invalid:1/"); err == nil {
		_ = resp.Body.Close()
		t.Error("no error from a closed port")
	}
}

func TestFindCommandNeedsAnExecutable(t *testing.T) {
	g := newGuest(t, "")
	write(t, filepath.Join(g.dir, "a/dockerd"), "x", 0o644)
	write(t, filepath.Join(g.dir, "b/dockerd"), "x", 0o755)
	if got := g.findCommand("dockerd", []string{"PATH=/a:/b"}); got != filepath.Join(g.dir, "b/dockerd") {
		t.Errorf("found %q", got)
	}
	if got := g.findCommand("dockerd", []string{"HOME=/"}); got != "" {
		t.Errorf("found %q", got)
	}
}

// helper of TestNetworkInANamespace, in a namespace of its own.
func TestNetworkInANamespace(t *testing.T) {
	if os.Getenv("GUESTINIT_NETNS") == "" {
		if _, err := exec.LookPath("unshare"); err != nil {
			t.Skip("no unshare")
		}
		if err := exec.Command("unshare", "-Urn", "true").Run(); err != nil {
			t.Skip("no user namespaces here")
		}
		cmd := exec.Command("unshare", "-Urn", "sh", "-c", `ip link add d0 type dummy && exec "$0" -test.run '^TestNetworkInANamespace$' -test.v`, os.Args[0]) // #nosec G204 -- this test
		cmd.Env = append(os.Environ(), "GUESTINIT_NETNS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	if err := (machine{}).network("10.9.0.2/30", "10.9.0.1"); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"addr show d0": "10.9.0.2/30",
		"route show":   "default via 10.9.0.1",
		"link show lo": "UP",
	} {
		out, err := exec.Command("ip", strings.Fields(args)...).CombinedOutput() // #nosec G204 -- this test
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("ip %s = %s, %v", args, out, err)
		}
	}
	if err := (machine{}).network("10.9.0.2/30", ""); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("a second address: %v", err)
	}
}

// Unprivileged, the real system calls fail without changing anything.
func TestMachineUnprivileged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root these would change the machine")
	}
	m := machine{}
	if err := m.network("10.9.0.2/30", "10.9.0.1"); !errors.Is(err, syscall.EPERM) {
		t.Errorf("network: %v", err)
	}
	if err := m.network("10.9.0.2/30", "nonsense"); err == nil {
		t.Error("network: a bad gateway was accepted")
	}
	if err := m.mountAll(); err == nil {
		t.Error("mountAll: no error")
	}
	if err := m.mountProc(); err == nil {
		t.Error("mountProc: no error")
	}
	if err := m.setClock(time.Now()); err == nil {
		t.Error("setClock: no error")
	}
	notModule := filepath.Join(t.TempDir(), "x.ko")
	write(t, notModule, "x", 0o644)
	if err := m.loadModules([]string{"/nonexistent.ko", notModule}); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("loadModules: %v", err)
	}
	if err := m.growRoot("/nonexistent"); err == nil {
		t.Error("growRoot: no error")
	}
	if err := m.growRoot(notModule); err == nil {
		t.Error("growRoot of a file on another file system than the root: no error")
	}
	if err := m.mountCache("/nonexistent", t.TempDir()); err == nil {
		t.Error("mountCache: no error")
	}
	if err := m.halt("reboot"); err == nil {
		t.Error("halt: no error")
	}
	if err := m.halt("poweroff"); err == nil {
		t.Error("halt: no error")
	}
}

func TestMachineWait(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if got := (machine{}).wait(cmd.Process.Pid); got != 3 {
		t.Errorf("status = %d", got)
	}
	if got := (machine{}).wait(cmd.Process.Pid); got != -1 {
		t.Errorf("status of nothing = %d", got)
	}
}

func TestNetworkRefuses(t *testing.T) {
	if err := (machine{}).network("", ""); err != nil {
		t.Errorf("no ip: %v", err)
	}
	if err := (machine{}).network("nonsense", ""); err == nil {
		t.Error("a bad address was accepted")
	}
	if err := (machine{}).network("2001:db8::1/64", ""); err == nil {
		t.Error("an IPv6 address was accepted")
	}
}

func TestParseAck(t *testing.T) {
	ack := func(code int32) []byte {
		data := make([]byte, 20)
		data[0], data[1], data[2], data[3] = byte(code), byte(code>>8), byte(code>>16), byte(code>>24)
		return message(syscall.NLMSG_ERROR, 0, data)
	}
	if err := parseAck(ack(0)); err != nil {
		t.Error(err)
	}
	if err := parseAck(ack(-int32(syscall.EPERM))); !errors.Is(err, syscall.EPERM) {
		t.Errorf("err = %v", err)
	}
	if err := parseAck([]byte{1, 2}); err == nil {
		t.Error("no error for junk")
	}
}

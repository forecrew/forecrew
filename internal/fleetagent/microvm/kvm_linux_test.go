//go:build kvm

package microvm

import (
	"crypto/ed25519"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/images"
)

// probe stands in for git in the image: the worker runs it once it has its
// token. It reports to the fake server what this machine can reach.
const probe = `#!/bin/busybox ash
b=/bin/busybox
r() { $b wget -q -O /dev/null "http://$SERVER/report?$1" 2>/dev/null; }
for t in $TARGETS; do
	if $b ping -c 1 -W 1 "$t" >/dev/null 2>&1; then r "reach=$t"; else r "blocked=$t"; fi
done
if $b wget -q -T 2 -O /dev/null "http://$HOSTPORT/" 2>/dev/null; then r "reach=hostport"; else r "blocked=hostport"; fi
if docker info >/dev/null 2>&1; then r docker=ok; else r docker=no; fi
$b grep -q fcb_ /dev/vdb 2>/dev/null && r token=found
echo git version 2.50.0
`

// TestMicroVMs runs machines under the real Firecracker and jailer. It needs
// root, /dev/kvm, firecracker and jailer on PATH, Docker with alpine:latest,
// mke2fs 1.47.1 or newer, and FORECREW_TEST_KERNEL, a Firecracker guest kernel.
// FORECREW_TEST_DOCKER_IMAGE names an image with dockerd, for Docker inside.
func TestMicroVMs(t *testing.T) {
	kernel := os.Getenv("FORECREW_TEST_KERNEL")
	if os.Geteuid() != 0 || kernel == "" {
		t.Skip("needs root and FORECREW_TEST_KERNEL")
	}
	for _, c := range []string{"firecracker", "jailer", "nft", "ip"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
	hostIP := hostAddress(t)

	// The fake forecrewd: grants tokens, records reports, turns workers away.
	var mu sync.Mutex
	reports := map[string]bool{}
	grants := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/worker/v1/bootstrap":
			grants++
			_, _ = w.Write([]byte(`{"worker_token":"fcw_t","name":"m","max_jobs":1}`))
		case "/report":
			reports[r.URL.RawQuery] = true
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	var err error
	if srv.Listener, err = net.Listen("tcp", hostIP+":0"); err != nil {
		t.Fatal(err)
	}
	srv.Start()
	defer srv.Close()
	// Something else on the host, which machines must not reach.
	other, err := net.Listen("tcp", hostIP+":0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(other, http.NotFoundHandler()) }()
	defer func() { _ = other.Close() }()

	dir := t.TempDir()
	pub, key, _ := images.GenerateKey()
	cfg := defaults()
	cfg.StateDir, cfg.TrustedKeysFile, cfg.MaxMachines = filepath.Join(dir, "state"), filepath.Join(dir, "keys"), 4
	_ = os.WriteFile(cfg.TrustedKeysFile, []byte(images.FormatPublicKey(pub)+"\n"), 0o600)
	p, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.prepareDirs(); err != nil {
		t.Fatal(err)
	}

	n0, n1 := networkOf(p.subnet, 0), networkOf(p.subnet, 1)
	env := "SERVER=" + srv.Listener.Addr().String() + "\nHOSTPORT=" + other.Addr().String() +
		"\nTARGETS=" + strings.Join([]string{n0.Guest.String(), n1.Guest.String(), n0.Host.String(), n1.Host.String()}, " ") + "\n"
	ref := os.Getenv("FORECREW_TEST_DOCKER_IMAGE")
	if ref == "" {
		ref = "alpine:latest"
	}
	worker := buildWorker(t)
	files, err := images.Guest(worker, nil)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, images.File{Path: "usr/local/bin/git", Mode: 0o755, Data: []byte(probe)}, images.File{Path: images.GuestEnv, Mode: 0o644, Data: []byte(env)})
	conv := &images.Converter{CacheDir: filepath.Join(dir, "convert"), Docker: images.DockerCLI{}}
	root, err := conv.Convert(t.Context(), images.Request{Ref: ref, Files: files, NoAgentCLIs: true})
	if err != nil {
		t.Fatal(err)
	}
	img := placeImage(t, p, key, root.Path, kernel)

	start := time.Now()
	var ids []string
	for range 2 {
		req := fleet.CreateRequest{
			MachineID: uuid.New(), Image: img,
			Shape:     domain.Shape{Name: "s", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 1, MemoryMB: 1024, DiskGB: 4},
			Bootstrap: &fleet.Bootstrap{ServerURL: srv.URL, Token: "fcb_" + strconv.Itoa(len(ids))},
		}
		if _, err := p.Create(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, req.MachineID.String())
	}
	t.Logf("two machines created in %s", time.Since(start).Round(time.Millisecond))
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		mu.Lock()
		g := grants
		mu.Unlock()
		if g == 2 {
			t.Logf("both workers bootstrapped %s after the first create", time.Since(start).Round(time.Millisecond))
			break
		}
	}
	// The workers end once the server turns them away, and so do the machines.
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		list, _ := p.List(t.Context())
		if len(list) == 2 && !list[0].Running && !list[1].Running {
			break
		}
	}
	mu.Lock()
	for _, want := range []string{"blocked=" + n1.Guest.String(), "blocked=" + n0.Host.String(), "blocked=hostport"} {
		found := false
		for r := range reports {
			found = found || strings.Contains(r, want)
		}
		if !found {
			t.Errorf("no report %q in %v", want, reports)
		}
	}
	for r := range reports {
		if strings.Contains(r, "reach=172.") || strings.Contains(r, "reach=hostport") || strings.Contains(r, "token=found") {
			t.Errorf("report %q", r)
		}
	}
	if os.Getenv("FORECREW_TEST_DOCKER_IMAGE") != "" && !reports["docker=ok"] {
		t.Errorf("Docker does not work inside: %v", reports)
	}
	mu.Unlock()

	// An agent killed and restarted finds the machines and destroys what
	// forecrewd disowns: everything here.
	again, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := again.List(t.Context())
	if err != nil || len(list) != 2 {
		t.Fatalf("list after a restart = %v, %v", list, err)
	}
	for _, i := range list {
		if err := again.Destroy(t.Context(), i.ExternalID); err != nil {
			t.Fatal(err)
		}
	}
	assertNothingLeft(t, again, ids)
}

func assertNothingLeft(t *testing.T, p *Provider, ids []string) {
	t.Helper()
	out, _ := exec.Command("ip", "-o", "link").Output()
	if strings.Contains(string(out), tapPrefix) {
		t.Errorf("taps left:\n%s", out)
	}
	out, _ = exec.Command("nft", "list", "tables").Output()
	if strings.Contains(string(out), "forecrew_vm") {
		t.Errorf("rules left:\n%s", out)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Dir(p.vmms[vmmFirecracker].jailDir(id))); err == nil {
			t.Errorf("jail of %s left", id)
		}
		if out, _ := exec.Command("pgrep", "-f", id).Output(); len(out) > 0 {
			t.Errorf("processes of %s left: %s", id, out)
		}
	}
}

// placeImage signs the files and puts them in the cache, as an operator does.
func placeImage(t *testing.T, p *Provider, key ed25519.PrivateKey, root, kernel string) domain.Image {
	t.Helper()
	rd, _ := images.FileDigest(root)
	kd, _ := images.FileDigest(kernel)
	img := domain.Image{Name: "t", Version: "1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageRootfs,
		URI: "s3://x/root", Digest: rd, BuiltAt: time.Now().UTC().Truncate(time.Second),
		Contents: domain.ImageContents{Kernel: &domain.ImageKernel{URI: "s3://x/kernel", Digest: kd}}}
	img.Signature = images.Sign(key, img)
	for src, d := range map[string]string{root: rd, kernel: kd} {
		dst, _ := p.cachePath(d)
		if err := clone(src, dst); err != nil {
			t.Fatal(err)
		}
	}
	return img
}

// hostAddress is an IPv4 address of this host that is not loopback.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !strings.HasPrefix(n.IP.String(), "172.30.") {
			return n.IP.String()
		}
	}
	t.Skip("no address of this host to serve on")
	return ""
}

func buildWorker(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "forecrew-worker")
	cmd := exec.Command("go", "build", "-o", out, "github.com/forecrew/forecrew/cmd/forecrew-worker")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the worker: %v\n%s", err, msg)
	}
	return out
}

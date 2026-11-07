//go:build kvm

package images

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/guestinit"
)

// bootToken is the machine's one-time token in the test; it must reach the
// server and nothing else.
const bootToken = "fcb_boot-test-secret"

// hostile stands in for a repo that built the root disk: it replaces /bin/sh
// and git, which run on the machine, and prints everything it can find of the
// token to the console.
const hostile = `#!/bin/busybox ash
echo HOSTILE-RAN >/dev/console
for f in /dev/vdb /proc/1/environ /proc/cmdline /run/forecrew/* /run/forecrew-config/*; do
	/bin/busybox cat "$f" >/dev/console 2>&1
done
for p in /proc/[0-9]*; do
	/bin/busybox cat "$p/environ" 2>/dev/null | /bin/busybox tr '\0' '\n' | /bin/busybox grep -v WORKER_TOKEN >/dev/console
done
/bin/busybox df -k / >/dev/console
echo git version 2.50.0
`

// TestRootfsBootsAndTheWorkerReachesTheServer converts a Docker image into a
// root filesystem, boots it under QEMU with KVM (what Firecracker does for a
// microVM: a kernel, a root disk and a config disk), and checks that the
// machine trades its token, that the worker it starts reaches a fake server, and
// that the image's own code finds nothing of the token, though it is the
// image's /bin/sh, git and proxy settings.
//
// Needs /dev/kvm, qemu-system-x86_64, Docker with alpine:latest, mke2fs
// 1.47.1 or newer, and a kernel with virtio block and ext4 built in:
// FORECREW_TEST_ROOTFS_TAR and FORECREW_TEST_WORKER replace the Docker export
// and the worker build (CI uses them). FORECREW_TEST_KERNEL names one (a Firecracker CI kernel has everything);
// otherwise the host's /boot/vmlinuz-linux is used and its virtio_net
// modules are loaded by the guest's init.
func TestRootfsBootsAndTheWorkerReachesTheServer(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	qemu, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Skip("qemu-system-x86_64 is not installed")
	}
	realMke2fs(t)

	kernel, modules := bootKernel(t)
	worker := os.Getenv("FORECREW_TEST_WORKER")
	if worker == "" {
		worker = buildWorker(t)
	}

	files, err := Guest(worker, []string{"docker"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files,
		File{Path: "usr/local/bin/git", Mode: 0o755, Data: []byte(hostile)},
		File{Path: "bin/sh", Mode: 0o755, Data: []byte(hostile)},
		File{Path: GuestEnv, Mode: 0o644, Data: []byte("HTTP_PROXY=http://10.0.2.2:1\nFORECREW_WORKER_TOKEN=stolen\n")})
	var loaded []string
	for _, m := range modules {
		files = append(files, File{Path: "lib/modules/forecrew/" + m.name, Mode: 0o644, Data: m.data})
		loaded = append(loaded, "/lib/modules/forecrew/"+m.name)
	}
	c := &Converter{CacheDir: t.TempDir(), Docker: DockerCLI{}}
	var root Result
	if tarball := os.Getenv("FORECREW_TEST_ROOTFS_TAR"); tarball != "" {
		// CI exports the base image on the host and converts it where mke2fs is new enough.
		f, openErr := os.Open(tarball) // #nosec G304 -- the CI's file
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer func() { _ = f.Close() }()
		root, err = c.ConvertTar(t.Context(), f, "base", "ci", Request{Files: files, NoAgentCLIs: true})
	} else {
		root, err = c.Convert(t.Context(), Request{Ref: "alpine:latest", Files: files, NoAgentCLIs: true})
	}
	if err != nil {
		t.Fatal(err)
	}
	// The provider sizes the disk file; the guest grows the file system.
	if err := os.Truncate(root.Path, 2<<30); err != nil {
		t.Fatal(err)
	}

	// The fake server; the guest reaches the host at 10.0.2.2 on QEMU's user
	// network. It grants the token, then turns the worker away.
	var mu sync.Mutex
	var seen []string
	var bootstrapped time.Time
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/worker/v1/bootstrap" {
			bootstrapped = time.Now()
			_, _ = w.Write([]byte(`{"worker_token":"fcw_boot-test","name":"m1","labels":["linux"],"max_jobs":1}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"test"}}`))
	}))
	srv.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	// The config disk, as a provider makes it.
	disk, err := guestinit.Encode(guestinit.Settings{ServerURL: "http://forecrew.test:" + strconv.Itoa(port), Token: bootToken, ServerAddrs: []string{"10.0.2.2"}})
	if err != nil {
		t.Fatal(err)
	}
	cfgImg := filepath.Join(t.TempDir(), "config.img")
	if err := os.WriteFile(cfgImg, disk, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	var console syncBuffer
	boot := "console=ttyS0 root=/dev/vda rw panic=-1 reboot=t forecrew.ip=10.0.2.15/24 forecrew.gw=10.0.2.2 forecrew.dns=10.0.2.3 forecrew.halt=reboot"
	if len(loaded) > 0 {
		boot += " forecrew.modules=" + strings.Join(loaded, ",")
	}
	cmd := exec.CommandContext(ctx, qemu, "-enable-kvm", "-cpu", "host", "-m", "512", "-smp", "1", "-nographic", "-no-reboot", "-snapshot", // #nosec G204 -- test input
		"-kernel", kernel,
		"-append", boot+" "+GuestInitArgs,
		"-drive", "file="+root.Path+",if=virtio,format=raw", "-drive", "file="+cfgImg+",if=virtio,format=raw",
		"-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0")
	cmd.Stdout, cmd.Stderr = &console, &console
	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		cancel()
		<-exited
		if t.Failed() {
			t.Logf("guest console:\n%s", console.String())
		}
	}()

	// Wait for the machine to end: the server turns the worker away, so the
	// worker ends and the init ends the machine.
	select {
	case <-exited:
	case <-time.After(3 * time.Minute):
		t.Fatal("the machine did not end after the worker ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("requests = %q", seen)
	}
	t.Logf("from start to the bootstrap call: %s", bootstrapped.Sub(started).Round(time.Millisecond))
	if first := seen[0]; !strings.Contains(first, "/worker/v1/bootstrap") || !strings.Contains(first, "Bearer "+bootToken) {
		t.Errorf("first request = %q", first)
	}
	if second := seen[1]; !strings.Contains(second, "Bearer fcw_boot-test") {
		t.Errorf("second request = %q", second)
	}
	out := console.String()
	if !strings.Contains(out, "HOSTILE-RAN") {
		t.Error("the image's code did not run, so this proves nothing")
	}
	if strings.Contains(out, bootToken) {
		t.Error("the image's code found the bootstrap token")
	}
	if strings.Contains(out, "fcw_boot-test") {
		t.Error("the worker token reached the console")
	}
	if m := regexp.MustCompile(`(?m)^/dev/\S+\s+(\d+)`).FindStringSubmatch(out); m == nil {
		t.Error("no df output")
	} else if kb, _ := strconv.Atoi(m[1]); kb < 1<<20 {
		t.Errorf("the root file system is %d KB: it did not grow to its disk", kb)
	}
}

// syncBuffer is a buffer a running process writes to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type kernelModule struct {
	name string
	data []byte
}

// bootKernel returns the kernel to boot and the modules its guest must load.
func bootKernel(t *testing.T) (string, []kernelModule) {
	t.Helper()
	if k := os.Getenv("FORECREW_TEST_KERNEL"); k != "" {
		return k, nil
	}
	const host = "/boot/vmlinuz-linux"
	if f, err := os.Open(host); err != nil {
		t.Skip("set FORECREW_TEST_KERNEL: the host kernel is not readable")
	} else {
		_ = f.Close()
	}
	var mods []kernelModule
	for _, name := range []string{"failover", "net_failover", "virtio_net"} {
		out, err := exec.Command("modinfo", "-n", name).Output() // #nosec G204 -- fixed names
		if err != nil {
			t.Skipf("no module %s: %v", name, err)
		}
		path := strings.TrimSpace(string(out))
		data, err := os.ReadFile(path) // #nosec G304 -- modinfo's answer
		if err != nil {
			t.Skip(err)
		}
		if strings.HasSuffix(path, ".zst") {
			if data, err = exec.Command("zstd", "-dc", path).Output(); err != nil { // #nosec G204 -- modinfo's answer
				t.Skipf("cannot decompress %s: %v", path, err)
			}
		}
		mods = append(mods, kernelModule{name + ".ko", data})
	}
	return host, mods
}

// buildWorker builds a static forecrew-worker for the guest.
func buildWorker(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "forecrew-worker")
	cmd := exec.Command("go", "build", "-o", out, "github.com/forecrew/forecrew/cmd/forecrew-worker") // #nosec G204 -- fixed
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the worker: %v\n%s", err, msg)
	}
	return out
}

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRootfsBootsAndTheWorkerReachesTheServer converts a Docker image into a
// root filesystem, boots it under QEMU with KVM (what Firecracker does for a
// microVM: a kernel, a root disk and a config disk), and checks that the
// worker started by the image's init calls a fake server.
//
// Needs /dev/kvm, qemu-system-x86_64, Docker with alpine:latest, mke2fs
// 1.47.1 or newer, and a kernel with virtio block and ext4 built in:
// FORECREW_TEST_KERNEL names one (a Firecracker CI kernel has everything);
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
	worker := buildWorker(t)

	// A worker needs git; the image under test has none, and the test is not
	// about git, so a stub says it is there.
	files, err := Guest(worker, []string{"docker"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, File{Path: "usr/local/bin/git", Mode: 0o755, Data: []byte("#!/bin/sh\necho git version 2.50.0\n")})
	for _, m := range modules {
		files = append(files, File{Path: "lib/modules/forecrew/" + m.name, Mode: 0o644, Data: m.data})
	}
	if len(modules) > 0 {
		var list strings.Builder
		for _, m := range modules {
			list.WriteString("/lib/modules/forecrew/" + m.name + "\n")
		}
		files = append(files, File{Path: "etc/forecrew/modules", Mode: 0o644, Data: []byte(list.String())})
	}
	c := &Converter{CacheDir: t.TempDir(), Docker: DockerCLI{}}
	root, err := c.Convert(t.Context(), Request{Ref: "alpine:latest", Files: files})
	if err != nil {
		t.Fatal(err)
	}

	// The fake server; the guest reaches the host at 10.0.2.2 on QEMU's user network.
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
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

	// The config disk, as a provider makes it: bootstrap settings on a small ext4.
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	if err := os.Mkdir(cfgDir, 0o755); err != nil { // #nosec G301 -- a test directory
		t.Fatal(err)
	}
	env := "FORECREW_SERVER_URL=http://10.0.2.2:" + strconv.Itoa(port) + "\nFORECREW_WORKER_TOKEN=fcw_boot-test\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "bootstrap.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgImg := filepath.Join(t.TempDir(), "config.ext4")
	if out, err := exec.Command("mke2fs", "-q", "-t", "ext4", "-F", "-d", cfgDir, cfgImg, "4M").CombinedOutput(); err != nil { // #nosec G204 -- test input
		t.Fatalf("config disk: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	var console bytes.Buffer
	cmd := exec.CommandContext(ctx, qemu, "-enable-kvm", "-cpu", "host", "-m", "512", "-smp", "1", "-nographic", "-no-reboot", "-snapshot", // #nosec G204 -- test input
		"-kernel", kernel,
		"-append", "console=ttyS0 root=/dev/vda rw init="+"/"+GuestInit+" panic=-1 reboot=t forecrew.ip=10.0.2.15/24 forecrew.gw=10.0.2.2 forecrew.dns=10.0.2.3",
		"-drive", "file="+root.Path+",if=virtio,format=raw", "-drive", "file="+cfgImg+",if=virtio,format=raw,readonly=on",
		"-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0")
	cmd.Stdout, cmd.Stderr = &console, &console
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
	// worker ends and the init powers the machine off.
	select {
	case <-exited:
	case <-time.After(3 * time.Minute):
		t.Fatal("the machine did not power off after the worker ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the worker did not reach the server")
	}
	first := seen[0]
	if !strings.Contains(first, "/worker/v1/") || !strings.Contains(first, "Bearer fcw_boot-test") {
		t.Errorf("first request = %q", first)
	}
	if strings.Contains(console.String(), "fcw_boot-test") {
		t.Error("the token reached the console")
	}
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

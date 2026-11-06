package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

// fakeImageAdmin is an api.ImageAdmin that records what it gets.
type fakeImageAdmin struct {
	mu      sync.Mutex
	added   []images.AddInput
	deleted []string
	// busy lists image IDs that a live machine uses.
	busy map[string]bool
}

func (f *fakeImageAdmin) AddImage(_ context.Context, _ domain.Actor, in images.AddInput) (mcp.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, in)
	return mcp.ImageInfo{Image: in.Image(domain.NewID())}, nil
}

func (f *fakeImageAdmin) DeleteImage(_ context.Context, _ domain.Actor, ref string) (mcp.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[ref] {
		return mcp.ImageInfo{}, fmt.Errorf("in use: %w", mcp.ErrConflict)
	}
	f.deleted = append(f.deleted, ref)
	return mcp.ImageInfo{Name: "linux", Version: "x"}, nil
}

func TestImageListCommand(t *testing.T) {
	e := newRemoteEnv(t)
	img := testkit.TestImage(uuid.Nil(), "linux", "2026.1", time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC))
	e.svc.ListImagesFunc = func(context.Context, domain.Actor, mcp.ListImagesInput) ([]mcp.ImageInfo, error) {
		return []mcp.ImageInfo{{Image: img, Scope: "instance"}}, nil
	}
	out, errOut, code := e.run(t, "image", "list", "--name", "linux", "--os", "linux", "--arch", "amd64", "--format", "rootfs")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"NAME", "linux", "2026.1", "rootfs", "instance", "2026-03-04", "docker"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if in := e.svc.Calls()[0].Input.(mcp.ListImagesInput); in != (mcp.ListImagesInput{Name: "linux", OS: "linux", Arch: "amd64", Format: "rootfs", Limit: 500}) {
		t.Errorf("input = %+v", in)
	}
}

func TestImageBuildAddAndPrune(t *testing.T) {
	e := newRemoteEnv(t)
	dir := t.TempDir()
	disk := filepath.Join(dir, "linux.qcow2")
	if err := os.WriteFile(disk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, key, pub, trusted := filepath.Join(dir, "m.json"), filepath.Join(dir, "k"), filepath.Join(dir, "k.pub"), filepath.Join(dir, "trusted")

	// Keys: the public line is printed and written; a key is never replaced.
	out, errOut, code := e.run(t, "image", "keygen", "--out", key, "--public", pub)
	if code != 0 || !strings.HasPrefix(out, "ed25519:") || errOut != "" {
		t.Fatalf("keygen: code %d, %q, %q", code, out, errOut)
	}
	if _, _, code := e.run(t, "image", "keygen", "--out", key); code == 0 {
		t.Error("keygen replaced a key")
	}
	pubLine, _ := os.ReadFile(pub)
	if err := os.WriteFile(trusted, append([]byte("# project\n"), pubLine...), 0o600); err != nil {
		t.Fatal(err)
	}

	// An unsigned manifest cannot be registered; signing it fixes that.
	args := []string{"image", "manifest", "--name", "linux", "--version", "2026.1", "--os", "linux", "--arch", "amd64", "--format", "qcow2",
		"--file", disk, "--uri", "https://images.example.com/linux.qcow2", "--label", "docker", "--package", "git=2.50", "--built-at", "2026-03-04T05:06:07Z", "--out", manifest}
	if _, errOut, code := e.run(t, args...); code != 0 {
		t.Fatalf("manifest: %d %s", code, errOut)
	}
	if _, errOut, code := e.run(t, "image", "add", manifest); code == 0 || !strings.Contains(errOut, "not signed") {
		t.Errorf("unsigned add: %d %s", code, errOut)
	}
	if _, errOut, code := e.run(t, "image", "verify", manifest, "--trusted", trusted); code == 0 {
		t.Errorf("verified an unsigned manifest: %s", errOut)
	}
	if _, errOut, code := e.run(t, "image", "sign", manifest, "--key", key); code != 0 {
		t.Fatalf("sign: %d %s", code, errOut)
	}
	if out, errOut, code := e.run(t, "image", "verify", manifest, "--trusted", trusted, "--file", disk); code != 0 || !strings.Contains(out, "linux@2026.1") {
		t.Fatalf("verify: %d %q %s", code, out, errOut)
	}
	// A different file, or a different key, fails.
	if err := os.WriteFile(disk, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := e.run(t, "image", "verify", manifest, "--trusted", trusted, "--file", disk); code == 0 {
		t.Error("verified a tampered file")
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte(images.FormatPublicKey(mustPub(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := e.run(t, "image", "verify", manifest, "--trusted", other); code == 0 {
		t.Error("verified with the wrong key")
	}

	out, errOut, code = e.run(t, "image", "add", manifest, "--instance")
	if code != 0 || !strings.Contains(out, "Registered linux@2026.1 (linux, amd64, qcow2)") {
		t.Fatalf("add: %d %q %s", code, out, errOut)
	}
	if len(e.images.added) != 1 || !e.images.added[0].Instance || e.images.added[0].Contents.Packages[0].Name != "git" || !e.images.added[0].Signed() {
		t.Errorf("added = %+v", e.images.added)
	}
	if _, _, code := e.run(t, "image", "add", filepath.Join(dir, "missing.json")); code == 0 {
		t.Error("missing manifest accepted")
	}

	// Signing in one step.
	signed := filepath.Join(dir, "signed.json")
	if _, errOut, code := e.run(t, append(args[:len(args)-1], signed, "--key", key)...); code != 0 {
		t.Fatalf("manifest --key: %d %s", code, errOut)
	}
	if m, err := readManifest(signed); err != nil || !m.Signed() {
		t.Errorf("signed manifest: %+v, %v", m, err)
	}
	if stdout, _, code := e.run(t, args[:len(args)-2]...); code != 0 || !strings.Contains(stdout, `"name": "linux"`) {
		t.Errorf("manifest to stdout: %d %q", code, stdout)
	}
	for _, bad := range [][]string{
		{"--package", "nothing"}, {"--built-at", "yesterday"}, {"--format", "iso"}, {"--key", filepath.Join(dir, "none")},
	} {
		if _, _, code := e.run(t, append(append([]string{}, args[:len(args)-2]...), bad...)...); code == 0 {
			t.Errorf("manifest accepted %v", bad)
		}
	}

	out, _, code = e.run(t, "image", "delete", "abc")
	if code != 0 || !strings.Contains(out, "Removed linux@x") || e.images.deleted[0] != "abc" {
		t.Errorf("delete: %d %q %v", code, out, e.images.deleted)
	}
}

func mustPub(t *testing.T) []byte {
	t.Helper()
	pub, _, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestImagePrune(t *testing.T) {
	e := newRemoteEnv(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var all []mcp.ImageInfo
	account := domain.NewID()
	for day := 1; day <= 4; day++ {
		own := testkit.TestImage(account, "linux", fmt.Sprint(day), base.AddDate(0, 0, day))
		own.ID = domain.NewID()
		shared := testkit.TestImage(uuid.Nil(), "base", fmt.Sprint(day), base.AddDate(0, 0, day))
		shared.ID = domain.NewID()
		all = append(all, mcp.ImageInfo{Image: own, Scope: "organization"}, mcp.ImageInfo{Image: shared, Scope: "instance"})
	}
	e.svc.ListImagesFunc = func(context.Context, domain.Actor, mcp.ListImagesInput) ([]mcp.ImageInfo, error) { return all, nil }
	oldest := all[0].ID.String()

	out, _, code := e.run(t, "image", "prune", "--keep", "3", "--dry-run")
	if code != 0 || !strings.Contains(out, "Would remove linux@1") || strings.Contains(out, "base@") || len(e.images.deleted) != 0 {
		t.Fatalf("dry run: %d %q %v", code, out, e.images.deleted)
	}
	out, _, code = e.run(t, "image", "prune", "--keep", "3", "--instance")
	if code != 0 || !strings.Contains(out, "Removed linux@1") || !strings.Contains(out, "Removed base@1") || len(e.images.deleted) != 2 {
		t.Fatalf("prune: %d %q %v", code, out, e.images.deleted)
	}
	// An image in use stays; with nothing expired there is nothing to do.
	e.images.deleted, e.images.busy = nil, map[string]bool{oldest: true}
	out, _, code = e.run(t, "image", "prune", "--keep", "3")
	if code != 0 || !strings.Contains(out, "Kept linux@1 (amd64, rootfs): a live machine uses it") || len(e.images.deleted) != 0 {
		t.Fatalf("busy: %d %q", code, out)
	}
	if out, _, code = e.run(t, "image", "prune", "--keep", "9"); code != 0 || out != "Nothing to remove.\n" {
		t.Errorf("nothing: %d %q", code, out)
	}
}

func TestImageRootfsCommand(t *testing.T) {
	e := newRemoteEnv(t)
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker")
	if err := os.WriteFile(worker, []byte("w"), 0o700); err != nil { // #nosec G306 -- test file
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "mke2fs")
	script := "#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'mke2fs 1.47.2 (x)' >&2; exit 0; fi\nfor a; do last=$a; img=$prev; prev=$a; done\ntruncate -s \"$last\" \"$img\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil { // #nosec G306 -- test script
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "base.tar")
	if err := os.WriteFile(tarball, tinyTar(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	out, errOut, code := e.run(t, "image", "rootfs", "--tar", tarball, "--worker", worker, "--cache", cache, "--mke2fs", stub, "--label", "docker", "--size", "600")
	if code != 0 || !strings.HasPrefix(out, cache+"/base-") || !strings.HasSuffix(strings.TrimSpace(out), ".ext4") {
		t.Fatalf("rootfs: %d %q %s", code, out, errOut)
	}
	if st, err := os.Stat(strings.TrimSpace(out)); err != nil || st.Size() != 600<<20 {
		t.Errorf("file: %v, %v", st, err)
	}
	for name, args := range map[string][]string{
		"neither source": {"--worker", worker},
		"both sources":   {"--worker", worker, "--from", "x:1", "--tar", tarball},
		"no worker file": {"--tar", tarball, "--worker", filepath.Join(dir, "none")},
		"missing tar":    {"--tar", filepath.Join(dir, "none.tar"), "--worker", worker},
		"no worker flag": {"--tar", tarball},
	} {
		if _, _, code := e.run(t, append([]string{"image", "rootfs", "--cache", cache, "--mke2fs", stub}, args...)...); code == 0 {
			t.Errorf("%s accepted", name)
		}
	}
}

func tinyTar(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range []*tar.Header{{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "bin/sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2}} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = w.Write([]byte("sh"))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

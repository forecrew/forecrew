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

// imageWork is the files of an image build in a temporary directory.
type imageWork struct {
	dir, disk, manifest, key, pub, trusted string
	args                                   []string // image manifest ... --out <manifest>
}

func newImageWork(t *testing.T) imageWork {
	t.Helper()
	dir := t.TempDir()
	w := imageWork{dir: dir, disk: filepath.Join(dir, "linux.qcow2"), manifest: filepath.Join(dir, "m.json"),
		key: filepath.Join(dir, "k"), pub: filepath.Join(dir, "k.pub"), trusted: filepath.Join(dir, "trusted")}
	if err := os.WriteFile(w.disk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.args = []string{"image", "manifest", "--name", "linux", "--version", "2026.1", "--os", "linux", "--arch", "amd64", "--format", "qcow2",
		"--file", w.disk, "--uri", "https://images.example.com/linux.qcow2", "--label", "docker", "--package", "git=2.50", "--built-at", "2026-03-04T05:06:07Z"}
	return w
}

func (w imageWork) manifestArgs(extra ...string) []string {
	return append(append([]string{}, w.args...), extra...)
}

func TestImageKeysSignAndVerify(t *testing.T) {
	e := newRemoteEnv(t)
	w := newImageWork(t)

	// Keys: the public line is printed and written; a key is never replaced.
	out, errOut, code := e.run(t, "image", "keygen", "--out", w.key, "--public", w.pub)
	if code != 0 || !strings.HasPrefix(out, "ed25519:") || errOut != "" {
		t.Fatalf("keygen: code %d, %q, %q", code, out, errOut)
	}
	if _, _, code := e.run(t, "image", "keygen", "--out", w.key); code == 0 {
		t.Error("keygen replaced a key")
	}
	pubLine, _ := os.ReadFile(w.pub)
	if err := os.WriteFile(w.trusted, append([]byte("# project\n"), pubLine...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errOut, code := e.run(t, w.manifestArgs("--out", w.manifest)...); code != 0 {
		t.Fatalf("manifest: %d %s", code, errOut)
	}
	if _, _, code := e.run(t, "image", "verify", w.manifest, "--trusted", w.trusted); code == 0 {
		t.Error("verified an unsigned manifest")
	}
	if _, errOut, code := e.run(t, "image", "sign", w.manifest, "--key", w.key); code != 0 {
		t.Fatalf("sign: %d %s", code, errOut)
	}
	if out, errOut, code := e.run(t, "image", "verify", w.manifest, "--trusted", w.trusted, "--file", w.disk); code != 0 || !strings.Contains(out, "linux@2026.1") {
		t.Fatalf("verify: %d %q %s", code, out, errOut)
	}
	// A different file, or a different key, fails.
	if err := os.WriteFile(w.disk, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := e.run(t, "image", "verify", w.manifest, "--trusted", w.trusted, "--file", w.disk); code == 0 {
		t.Error("verified a tampered file")
	}
	if _, errOut, code := e.run(t, "image", "verify", w.manifest, "--trusted", w.trusted, "--kernel", w.disk); code == 0 || !strings.Contains(errOut, "--file") {
		t.Errorf("--kernel alone: %d %s", code, errOut)
	}
	other := filepath.Join(w.dir, "other")
	if err := os.WriteFile(other, []byte(images.FormatPublicKey(mustPub(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := e.run(t, "image", "verify", w.manifest, "--trusted", other); code == 0 {
		t.Error("verified with the wrong key")
	}
}

func TestImageManifestOptions(t *testing.T) {
	e := newRemoteEnv(t)
	w := newImageWork(t)
	if _, _, code := e.run(t, "image", "keygen", "--out", w.key); code != 0 {
		t.Fatal("keygen")
	}
	// Signing in one step, and standard output without --out.
	signed := filepath.Join(w.dir, "signed.json")
	if _, errOut, code := e.run(t, w.manifestArgs("--out", signed, "--key", w.key)...); code != 0 {
		t.Fatalf("manifest --key: %d %s", code, errOut)
	}
	if m, err := readManifest(signed); err != nil || !m.Signed() {
		t.Errorf("signed manifest: %+v, %v", m, err)
	}
	if stdout, _, code := e.run(t, w.args...); code != 0 || !strings.Contains(stdout, `"name": "linux"`) {
		t.Errorf("manifest to stdout: %d %q", code, stdout)
	}
	for _, bad := range [][]string{
		{"--package", "nothing"}, {"--built-at", "yesterday"}, {"--format", "iso"}, {"--key", filepath.Join(w.dir, "none")},
	} {
		if _, _, code := e.run(t, w.manifestArgs(bad...)...); code == 0 {
			t.Errorf("manifest accepted %v", bad)
		}
	}
	if _, _, code := e.run(t, "image", "sign", filepath.Join(w.dir, "none.json"), "--key", w.key); code == 0 {
		t.Error("signed a missing manifest")
	}
}

func TestImageAddAndDelete(t *testing.T) {
	e := newRemoteEnv(t)
	w := newImageWork(t)
	if _, _, code := e.run(t, "image", "keygen", "--out", w.key); code != 0 {
		t.Fatal("keygen")
	}
	if _, _, code := e.run(t, w.manifestArgs("--out", w.manifest)...); code != 0 {
		t.Fatal("manifest")
	}
	// An unsigned manifest cannot be registered.
	if _, errOut, code := e.run(t, "image", "add", w.manifest); code == 0 || !strings.Contains(errOut, "not signed") {
		t.Errorf("unsigned add: %d %s", code, errOut)
	}
	if _, _, code := e.run(t, "image", "sign", w.manifest, "--key", w.key); code != 0 {
		t.Fatal("sign")
	}
	out, errOut, code := e.run(t, "image", "add", w.manifest, "--instance")
	if code != 0 || !strings.Contains(out, "Registered linux@2026.1 (linux, amd64, qcow2)") {
		t.Fatalf("add: %d %q %s", code, out, errOut)
	}
	if len(e.images.added) != 1 || !e.images.added[0].Instance || e.images.added[0].Contents.Packages[0].Name != "git" || !e.images.added[0].Signed() {
		t.Errorf("added = %+v", e.images.added)
	}
	if _, _, code := e.run(t, "image", "add", filepath.Join(w.dir, "missing.json")); code == 0 {
		t.Error("missing manifest accepted")
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
	out, errOut, code := e.run(t, "image", "rootfs", "--tar", tarball, "--worker", worker, "--cache", cache, "--mke2fs", stub, "--label", "docker", "--size", "600", "--no-agent-clis", "--env", "PATH=/usr/bin")
	if code != 0 || !strings.HasPrefix(out, cache+"/base-") || !strings.HasSuffix(strings.TrimSpace(out), ".ext4") {
		t.Fatalf("rootfs: %d %q %s", code, out, errOut)
	}
	if st, err := os.Stat(strings.TrimSpace(out)); err != nil || st.Size() != 600<<20 {
		t.Errorf("file: %v, %v", st, err)
	}
	// A missing ip only warns.
	if !strings.Contains(errOut, "warning: the image lacks ip") {
		t.Errorf("no warning about ip: %q", errOut)
	}
	// The agent CLIs are required unless the image is for command jobs.
	if _, errOut, code := e.run(t, "image", "rootfs", "--tar", tarball, "--name", "agents", "--worker", worker, "--cache", cache, "--mke2fs", stub); code == 0 || !strings.Contains(errOut, "claude") {
		t.Errorf("agent image without CLIs: %d %s", code, errOut)
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
	for _, h := range []*tar.Header{
		{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "bin/sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2},
		{Name: "bin/git", Typeflag: tar.TypeReg, Mode: 0o755, Size: 2},
		{Name: "etc/ssl/certs/ca-certificates.crt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2},
	} {
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

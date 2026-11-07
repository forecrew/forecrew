package images

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeDocker serves a tar made by the test.
type fakeDocker struct {
	id        string
	env       []string
	envErr    error
	exportRef string
	tarball   []byte
	idErr     error
	exportErr error
	exports   int
}

func (f *fakeDocker) ID(context.Context, string) (string, error) { return f.id, f.idErr }

func (f *fakeDocker) Env(context.Context, string) ([]string, error) { return f.env, f.envErr }

func (f *fakeDocker) Export(_ context.Context, ref string) (io.ReadCloser, error) {
	f.exports++
	f.exportRef = ref
	return io.NopCloser(bytes.NewReader(f.tarball)), f.exportErr
}

type entry struct {
	name     string
	typ      byte
	content  string
	link     string
	mode     int64
	uid, gid int
}

func makeTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Uid: e.uid, Gid: e.gid, Uname: "someone"}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.content))
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func baseTar(t *testing.T) []byte {
	return makeTar(t,
		entry{name: "./", typ: tar.TypeDir, mode: 0o755},
		entry{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "bin/sh", typ: tar.TypeReg, mode: 0o755, content: "#!shell"},
		entry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "etc/os-release", typ: tar.TypeReg, mode: 0o644, content: "ID=test\n", uid: 1234, gid: 1234},
		entry{name: "etc/release", typ: tar.TypeSymlink, link: "os-release"},
		entry{name: "etc/alias", typ: tar.TypeLink, link: "/etc/os-release", mode: 0o644, uid: 1234, gid: 1234},
		entry{name: "secret/", typ: tar.TypeDir, mode: 0o500},
		entry{name: "secret/key", typ: tar.TypeReg, mode: 0o400, content: "k"},
		entry{name: "/opt/forecrew/forecrew-worker", typ: tar.TypeReg, mode: 0o644, content: "the base's own worker"},
		entry{name: "dev/null", typ: tar.TypeChar, mode: 0o666},
		// What the worker and its init need.
		entry{name: "bin/ip", typ: tar.TypeSymlink, link: "/bin/busybox"},
		entry{name: "usr/bin/git", typ: tar.TypeReg, mode: 0o755, content: "git"},
		entry{name: "etc/ssl/certs/ca-certificates.crt", typ: tar.TypeReg, mode: 0o644, content: "certs"},
		entry{name: "usr/local/bin/claude", typ: tar.TypeReg, mode: 0o755, content: "c"},
		entry{name: "usr/local/bin/codex", typ: tar.TypeReg, mode: 0o755, content: "c"},
		entry{name: "usr/local/bin/opencode", typ: tar.TypeReg, mode: 0o755, content: "c"},
	)
}

// debugfs runs a debugfs request on an ext4 file, skipping the test where it
// is not installed.
func debugfs(t *testing.T, img, request string) string {
	t.Helper()
	bin, err := exec.LookPath("debugfs")
	if err != nil {
		t.Skip("debugfs is not installed")
	}
	out, err := exec.Command(bin, "-R", request, img).Output() // #nosec G204 -- test input
	if err != nil {
		t.Fatalf("debugfs %q: %v\n%s", request, err, out)
	}
	return string(out)
}

// realMke2fs returns the installed mke2fs, skipping the test when it cannot
// build from a tar (before 1.47.1).
func realMke2fs(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("mke2fs")
	if err != nil {
		t.Skip("mke2fs is not installed")
	}
	if _, err := checkMke2fs(t.Context(), bin); err != nil {
		t.Skip(err)
	}
	return bin
}

// stubMke2fs is a mke2fs that says it is version and, asked for a file
// system, makes a file of the size asked for that starts with its
// arguments.
func stubMke2fs(t *testing.T, version string, fail bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mke2fs")
	script := "#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'mke2fs " + version + " (6-Mar-2025)' >&2; exit 0; fi\n"
	if fail {
		script += "echo 'no space' >&2; exit 1\n"
	} else {
		script += "for a; do last=$a; img=$prev; prev=$a; done\necho \"$@\" >\"$img\"\ntruncate -s \"$last\" \"$img\"\n"
	}
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	return p
}

func testGuest(t *testing.T) []File {
	t.Helper()
	worker := filepath.Join(t.TempDir(), "forecrew-worker")
	if err := os.WriteFile(worker, []byte("worker binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := Guest(worker, []string{"docker", "runtime:codex"})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestConvert(t *testing.T) {
	bin := realMke2fs(t)
	docker := &fakeDocker{id: "sha256:aaa", tarball: baseTar(t)}
	c := &Converter{CacheDir: filepath.Join(t.TempDir(), "cache"), Docker: docker, Mke2fs: bin}
	req := Request{Ref: "forecrew-env:ab12cd", Files: testGuest(t)}

	res, err := c.Convert(t.Context(), req)
	if err != nil || res.Cached || !strings.HasSuffix(res.Path, ".ext4") || !strings.HasPrefix(filepath.Base(res.Path), "forecrew-env-ab12cd-") {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	if st, err := os.Stat(res.Path); err != nil || st.Size() < minRootfsMB<<20 {
		t.Fatalf("file = %v, %v", st, err)
	}
	if got := debugfs(t, res.Path, "cat /etc/os-release"); got != "ID=test\n" {
		t.Errorf("os-release = %q", got)
	}
	// The guest files replace the base's, and the labels are the image's.
	if got := debugfs(t, res.Path, "cat /opt/forecrew/forecrew-worker"); got != "worker binary" {
		t.Errorf("worker = %q", got)
	}
	if got := debugfs(t, res.Path, "cat /etc/forecrew/labels"); got != "docker\nruntime:codex\n" {
		t.Errorf("labels = %q", got)
	}
	for request, want := range map[string]string{
		"stat /etc/release":    "os-release",
		"stat /etc/alias":      "Links: 2",
		"stat /secret/key":     "Mode:  0400",
		"stat /etc/os-release": "User:  1234",
	} {
		if got := debugfs(t, res.Path, request); !strings.Contains(got, want) {
			t.Errorf("%s: no %q in\n%s", request, want, got)
		}
	}
	if got := debugfs(t, res.Path, "ls /dev"); strings.Contains(got, "null") {
		t.Errorf("device node kept: %s", got)
	}
	if got := debugfs(t, res.Path, "ls /secret"); !strings.Contains(got, "key") {
		t.Errorf("a read-only directory lost its file: %s", got)
	}
}

func TestConvertCache(t *testing.T) {
	docker := &fakeDocker{id: "sha256:aaa", tarball: baseTar(t)}
	c := &Converter{CacheDir: filepath.Join(t.TempDir(), "cache"), Docker: docker, Mke2fs: stubMke2fs(t, "1.47.2", false)}
	req := Request{Ref: "forecrew-env:ab12cd", Files: testGuest(t), SizeMB: 700}

	res, err := c.Convert(t.Context(), req)
	if err != nil || res.Cached {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 400)
	n, _ := f.Read(head)
	_ = f.Close()
	args := head[:n]
	if i := bytes.IndexByte(args, 0); i >= 0 {
		args = args[:i]
	}
	if !strings.Contains(string(args), "-d ") || !strings.Contains(string(args), "-t ext4") || !strings.HasSuffix(strings.TrimSpace(string(args)), "700M") {
		t.Errorf("mke2fs args = %q", args)
	}
	// The same content is a cache hit; changing the image or a file is not.
	again, err := c.Convert(t.Context(), req)
	if err != nil || !again.Cached || again.Path != res.Path || docker.exports != 1 {
		t.Fatalf("second Convert = %+v, %v (%d exports)", again, err, docker.exports)
	}
	docker.id = "sha256:bbb"
	changed, err := c.Convert(t.Context(), req)
	if err != nil || changed.Cached || changed.Path == res.Path || changed.Key == res.Key {
		t.Fatalf("new image: %+v, %v", changed, err)
	}
	docker.id = "sha256:aaa"
	files := testGuest(t)
	files[0].Data = []byte("another worker")
	other, err := c.Convert(t.Context(), Request{Ref: req.Ref, Files: files, SizeMB: 700})
	if err != nil || other.Cached || other.Key == res.Key {
		t.Fatalf("new worker: %+v, %v", other, err)
	}
	// Nothing but finished files is in the cache.
	entries, _ := os.ReadDir(c.CacheDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".ext4") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestMke2fsVersion(t *testing.T) {
	for version, ok := range map[string]bool{"1.46.5": false, "1.47.0": false, "1.47.1": true, "1.47.4": true, "1.48.0": true, "2.0.0": true, "0.9.9": false} {
		_, err := checkMke2fs(t.Context(), stubMke2fs(t, version, false))
		if (err == nil) != ok {
			t.Errorf("mke2fs %s: %v", version, err)
		}
	}
	if _, err := checkMke2fs(t.Context(), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "e2fsprogs") {
		t.Errorf("missing: %v", err)
	}
	garbled := filepath.Join(t.TempDir(), "mke2fs")
	if err := os.WriteFile(garbled, []byte("#!/bin/sh\necho what >&2\n"), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	if _, err := checkMke2fs(t.Context(), garbled); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("garbled: %v", err)
	}
}

func TestConvertTar(t *testing.T) {
	c := &Converter{CacheDir: t.TempDir(), Mke2fs: stubMke2fs(t, "1.47.2", false)}
	req := Request{Files: testGuest(t), SizeMB: 600}
	res, err := c.ConvertTar(t.Context(), bytes.NewReader(baseTar(t)), "linux base", "sha256:from-packer", req)
	if err != nil || res.Cached || !strings.HasPrefix(filepath.Base(res.Path), "linux-base-") {
		t.Fatalf("ConvertTar = %+v, %v", res, err)
	}
	if st, _ := os.Stat(res.Path); st.Size() != 600<<20 {
		t.Fatalf("size = %d", st.Size())
	}
	again, err := c.ConvertTar(t.Context(), bytes.NewReader(nil), "linux base", "sha256:from-packer", req)
	if err != nil || !again.Cached {
		t.Fatalf("cache: %+v, %v", again, err)
	}
}

func TestConvertErrors(t *testing.T) {
	ctx := t.Context()
	good := baseTar(t)
	for _, tt := range []struct {
		name   string
		docker *fakeDocker
		req    Request
		mke2fs string
		want   string
	}{
		{"unknown image", &fakeDocker{idErr: errors.New("no such image")}, Request{Ref: "x"}, "", "no such image"},
		{"export fails", &fakeDocker{id: "i", exportErr: errors.New("daemon down")}, Request{Ref: "x"}, "", "daemon down"},
		{"not a tar", &fakeDocker{id: "i", tarball: []byte("not a tar, and long enough to be read as a header....................................................................................................................................................................................................................................................................")}, Request{Ref: "x"}, "", "read image tar"},
		{"path leaves the root", &fakeDocker{id: "i", tarball: makeTar(t, entry{name: "../evil", typ: tar.TypeReg, content: "x"})}, Request{Ref: "x"}, "", "leaves the root"},
		{"size too small", &fakeDocker{id: "i", tarball: makeTar(t, append(requiredEntries(), entry{name: "big", typ: tar.TypeReg, mode: 0o644, content: strings.Repeat("x", 3<<20)})...)}, Request{Ref: "x", SizeMB: 1, NoAgentCLIs: true}, "", "smaller than"},
		{"no mke2fs", &fakeDocker{id: "i", tarball: good}, Request{Ref: "x"}, filepath.Join(t.TempDir(), "missing"), "e2fsprogs"},
		{"mke2fs fails", &fakeDocker{id: "i", tarball: good}, Request{Ref: "x"}, stubMke2fs(t, "1.47.2", true), "no space"},
	} {
		mke2fs := tt.mke2fs
		if mke2fs == "" {
			mke2fs = stubMke2fs(t, "1.47.2", false)
		}
		c := &Converter{CacheDir: t.TempDir(), Docker: tt.docker, Mke2fs: mke2fs}
		_, err := c.Convert(ctx, tt.req)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.name, err, tt.want)
		}
		if entries, _ := os.ReadDir(c.CacheDir); len(entries) != 0 {
			t.Errorf("%s: %d leftovers in the cache", tt.name, len(entries))
		}
	}
}

func TestCacheName(t *testing.T) {
	for in, want := range map[string]string{
		"forecrew-env:AB12": "forecrew-env-ab12", "ghcr.io/forecrew/x:1.0": "ghcr.io-forecrew-x-1.0", "///": "image", "": "image",
		strings.Repeat("a", 200): strings.Repeat("a", 80),
	} {
		if got := cacheName(in); got != want {
			t.Errorf("cacheName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSizeFor(t *testing.T) {
	if got := sizeFor(tarStats{blocks: 100, entries: 10}); got != minRootfsMB {
		t.Errorf("small = %d", got)
	}
	// 2000 MB of big files.
	big := tarStats{blocks: 2000 << 20 / blockSize, entries: 100}
	if got := sizeFor(big); got != 2000*6/5+journalMB+rootfsHeadroomMB {
		t.Errorf("large = %d", got)
	}
	// A million one-byte files are 1 MB of content and 4 GB of blocks.
	small := tarStats{blocks: 1_000_000, entries: 1_000_000}
	if got := sizeFor(small); got < 4000 {
		t.Errorf("a million small files = %d MB", got)
	}
}

func TestBlocksFor(t *testing.T) {
	for size, want := range map[int64]int64{0: 0, 1: 1, 4096: 1, 4097: 2, 10 << 20: 2560} {
		if got := blocksFor(size); got != want {
			t.Errorf("blocksFor(%d) = %d, want %d", size, got, want)
		}
	}
}

// requiredEntries is what an image must hold to pass the checks.
func requiredEntries() []entry {
	return []entry{
		{name: "bin/sh", typ: tar.TypeReg, mode: 0o755, content: "sh"}, {name: "usr/sbin/ip", typ: tar.TypeReg, mode: 0o755, content: "ip"},
		{name: "usr/bin/git", typ: tar.TypeReg, mode: 0o755, content: "git"}, {name: "etc/pki/tls/certs/ca-bundle.crt", typ: tar.TypeReg, mode: 0o644, content: "c"},
	}
}

func TestConvertChecksWhatTheImageNeeds(t *testing.T) {
	c := &Converter{CacheDir: t.TempDir(), Mke2fs: stubMke2fs(t, "1.47.4", false)}
	// Nothing: everything is listed at once, with why.
	_, err := c.ConvertTar(t.Context(), bytes.NewReader(makeTar(t, entry{name: "etc/x", typ: tar.TypeReg, content: "x"})), "bare", "id", Request{})
	if err == nil {
		t.Fatal("an empty image converted")
	}
	for _, want := range []string{"sh (", "git (", "CA certificates", "claude", "codex", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
	// What the guest files bring counts, like a git stub in a test image.
	files := []File{{Path: "usr/local/bin/git", Mode: 0o755, Data: []byte("git")}}
	_, err = c.ConvertTar(t.Context(), bytes.NewReader(makeTar(t, requiredEntries()[0], requiredEntries()[1], requiredEntries()[3])), "no-git", "id", Request{Files: files, NoAgentCLIs: true})
	if err != nil {
		t.Errorf("git from the added files: %v", err)
	}
	// An image for command jobs does not need the agent CLIs; one for agents does.
	if _, err = c.ConvertTar(t.Context(), bytes.NewReader(makeTar(t, requiredEntries()...)), "cmd-only", "id", Request{NoAgentCLIs: true}); err != nil {
		t.Errorf("without the CLIs: %v", err)
	}
	if _, err = c.ConvertTar(t.Context(), bytes.NewReader(makeTar(t, requiredEntries()...)), "agent", "id", Request{}); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("an agent image without the CLIs: %v", err)
	}
}

func TestEnvFile(t *testing.T) {
	files, err := withEnv([]File{{Path: "a"}}, []string{
		"PATH=/opt/go/bin:/usr/bin", "LANG=C.UTF-8", "FORECREW_WORKER_TOKEN=x", "not a variable", "1BAD=x", "EMPTY=", "A-B=c",
	})
	if err != nil || len(files) != 2 || files[1].Path != GuestEnv {
		t.Fatalf("withEnv = %+v, %v", files, err)
	}
	if got := string(files[1].Data); got != "PATH=/opt/go/bin:/usr/bin\nLANG=C.UTF-8\nEMPTY=\n" {
		t.Errorf("env file = %q", got)
	}
	if none, err := withEnv(nil, nil); err != nil || len(none) != 0 {
		t.Errorf("no environment = %+v, %v", none, err)
	}
	if _, err := withEnv(nil, []string{"X=a\nFORECREW_SERVER_URL=http://evil"}); err == nil {
		t.Error("a newline in a value accepted")
	}
	if _, err := withEnv(nil, []string{"BIG=" + strings.Repeat("a", maxEnvBytes)}); err == nil {
		t.Error("an oversized environment accepted")
	}
}

func TestConvertWritesTheImageEnvironmentAndExportsByID(t *testing.T) {
	docker := &fakeDocker{id: "sha256:cafe", tarball: baseTar(t), env: []string{"PATH=/usr/local/go/bin:/usr/bin", "GOFLAGS=-mod=mod"}}
	c := &Converter{CacheDir: t.TempDir(), Docker: docker, Mke2fs: stubMke2fs(t, "1.47.4", false)}
	res, err := c.Convert(t.Context(), Request{Ref: "forecrew-env:moving-tag", Files: testGuest(t)})
	if err != nil {
		t.Fatal(err)
	}
	// A tag that moves between the ID and the export cannot swap the image.
	if docker.exportRef != "sha256:cafe" {
		t.Errorf("exported %q", docker.exportRef)
	}
	// The environment is part of the cache key: another one is another file.
	docker.env = []string{"PATH=/usr/bin"}
	other, err := c.Convert(t.Context(), Request{Ref: "forecrew-env:moving-tag", Files: testGuest(t)})
	if err != nil || other.Key == res.Key {
		t.Errorf("a changed environment shares a file: %+v, %v", other, err)
	}
	docker.envErr = errors.New("inspect failed")
	if _, err := c.Convert(t.Context(), Request{Ref: "x:1", Files: testGuest(t)}); err == nil {
		t.Error("environment error ignored")
	}
	docker.envErr, docker.env = nil, []string{"X=a\nb"}
	if _, err := c.Convert(t.Context(), Request{Ref: "x:2", Files: testGuest(t)}); err == nil {
		t.Error("bad environment accepted")
	}
}

func TestMkfsRefusesBigFilesBeforeOneFortySeven4(t *testing.T) {
	st := tarStats{largest: 3 << 30, entries: 1}
	for version, ok := range map[string]bool{"1.47.2": false, "1.47.3": false, "1.47.4": true} {
		c := &Converter{Mke2fs: stubMke2fs(t, version, false)}
		err := c.mkfs(t.Context(), "unused.tar", filepath.Join(t.TempDir(), "root.ext4"), 4096, st)
		if (err == nil) != ok || (!ok && !strings.Contains(err.Error(), "1.47.4")) {
			t.Errorf("mke2fs %s with a 3 GiB file: %v", version, err)
		}
	}
}

func TestMke2fsWithoutLibarchive(t *testing.T) {
	_, err := checkMke2fs(t.Context(), stubMke2fs(t, "1.47.4", true))
	if err == nil || !strings.Contains(err.Error(), "libarchive13t64") {
		t.Errorf("a mke2fs that cannot read a tar: %v", err)
	}
}

func TestGuestErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Guest(filepath.Join(dir, "missing"), nil); err == nil {
		t.Error("missing worker accepted")
	}
	if _, err := Guest(dir, nil); err == nil {
		t.Error("a directory accepted as the worker")
	}
	worker := filepath.Join(dir, "w")
	_ = os.WriteFile(worker, []byte("w"), 0o600)
	files, err := Guest(worker, nil)
	if err != nil || len(files) != 3 {
		t.Fatalf("no labels: %d files, %v", len(files), err)
	}
	// A dynamic binary would start through the image's own linker.
	if dynamic, err := exec.LookPath("ls"); err == nil {
		if raw, _ := os.ReadFile(dynamic); dynamicELF(raw) {
			if _, err := Guest(dynamic, nil); err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=0") {
				t.Errorf("a dynamic worker: %v", err)
			}
		}
	}
}

// The image cannot put its own files where the kernel starts PID 1.
func TestPrepareTarKeepsOptForecrew(t *testing.T) {
	files := []File{{Path: GuestWorker, Mode: 0o755, Data: []byte("ours")}}
	dst := filepath.Join(t.TempDir(), "out.tar")
	src := makeTar(t,
		entry{name: "opt/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "opt/forecrew/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "opt/forecrew/forecrew-worker2", typ: tar.TypeReg, mode: 0o755, content: "theirs"},
		entry{name: "opt/forecrew/evil", typ: tar.TypeSymlink, link: "/bin/sh"},
	)
	if _, err := prepareTar(bytes.NewReader(src), files, dst); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("theirs")) || bytes.Contains(raw, []byte("forecrew/evil")) {
		t.Error("the image's /opt/forecrew was kept")
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeReg} {
		bad := makeTar(t, entry{name: "opt", typ: typ, link: "/tmp", mode: 0o644})
		if _, err := prepareTar(bytes.NewReader(bad), files, dst); err == nil || !strings.Contains(err.Error(), "must be a directory") {
			t.Errorf("an /opt of type %c: %v", typ, err)
		}
	}
}

func TestPruneCache(t *testing.T) {
	dir := t.TempDir()
	old, recent, stale := filepath.Join(dir, "old.ext4"), filepath.Join(dir, "recent.ext4"), filepath.Join(dir, ".convert-1")
	for _, f := range []string{old, recent} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	week := time.Now().Add(-7 * 24 * time.Hour)
	for _, f := range []string{old, stale} {
		if err := os.Chtimes(f, week, week); err != nil {
			t.Fatal(err)
		}
	}
	got, err := PruneCache(dir, 24*time.Hour, true)
	if err != nil || len(got) != 2 {
		t.Fatalf("dry run = %v, %v", got, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("a dry run removed a file")
	}
	got, err = PruneCache(dir, 24*time.Hour, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("prune = %v, %v", got, err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("a recent file was removed")
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("an old file was kept")
	}
	if got, err := PruneCache(filepath.Join(dir, "missing"), time.Hour, false); err != nil || got != nil {
		t.Errorf("missing cache = %v, %v", got, err)
	}
	if _, err := PruneCache(recent, time.Hour, false); err == nil {
		t.Error("a file as the cache: no error")
	}
}

// A cache hit counts as a use, for PruneCache.
func TestCacheHitIsAUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.ext4")
	if hit(path) {
		t.Error("a missing file is a hit")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	week := time.Now().Add(-7 * 24 * time.Hour)
	_ = os.Chtimes(path, week, week)
	if !hit(path) {
		t.Fatal("no hit")
	}
	if st, _ := os.Stat(path); st.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Error("the hit did not mark the use")
	}
}

func TestCheckMke2fs(t *testing.T) {
	if err := CheckMke2fs(t.Context(), stubMke2fs(t, "1.47.4", false)); err != nil {
		t.Error(err)
	}
	if err := CheckMke2fs(t.Context(), stubMke2fs(t, "1.47.0", false)); err == nil {
		t.Error("an old mke2fs passed")
	}
	t.Setenv("PATH", t.TempDir())
	if err := CheckMke2fs(t.Context(), ""); err == nil {
		t.Error("no mke2fs passed")
	}
}

// toolboxBaseTar is the layout of packs/toolboxes/base: Node's slim Debian
// image with git and CA certificates, and the agent CLIs under
// /opt/forecrew-cli/node_modules/.bin, which its PATH names. It has no ip.
func toolboxBaseTar(t *testing.T) []byte {
	return makeTar(t,
		entry{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "bin/sh", typ: tar.TypeReg, mode: 0o755, content: "sh"},
		entry{name: "usr/bin/git", typ: tar.TypeReg, mode: 0o755, content: "git"},
		entry{name: "usr/local/bin/node", typ: tar.TypeReg, mode: 0o755, content: "node"},
		entry{name: "etc/ssl/certs/ca-certificates.crt", typ: tar.TypeReg, mode: 0o644, content: "certs"},
		entry{name: "opt/forecrew-cli/node_modules/.bin/claude", typ: tar.TypeSymlink, link: "../@anthropic-ai/claude-code/cli.js"},
		entry{name: "opt/forecrew-cli/node_modules/.bin/codex", typ: tar.TypeSymlink, link: "../@openai/codex/bin/codex.js"},
		entry{name: "opt/forecrew-cli/node_modules/.bin/opencode", typ: tar.TypeSymlink, link: "../opencode-ai/bin/opencode"},
	)
}

func TestConvertAcceptsForecrewsToolboxBase(t *testing.T) {
	c := &Converter{CacheDir: t.TempDir(), Mke2fs: stubMke2fs(t, "1.47.4", false)}
	env := []string{"PATH=/opt/forecrew-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if _, err := c.ConvertTar(t.Context(), bytes.NewReader(toolboxBaseTar(t)), "toolbox", "id", Request{Env: env}); err != nil {
		t.Fatalf("the toolbox base was refused: %v", err)
	}
	// Without the image's PATH the CLIs are not found: the layout is the reason, not the image.
	if _, err := c.ConvertTar(t.Context(), bytes.NewReader(toolboxBaseTar(t)), "no-path", "id2", Request{}); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("without the PATH: %v", err)
	}
}

func TestPathDirs(t *testing.T) {
	got := pathDirs([]string{"HOME=/root", "PATH=/opt/x/.bin:/usr/bin::/../etc/./y/:relative"})
	for _, want := range []string{"opt/x/.bin", "usr/bin", "etc/y", "relative", "bin"} {
		if !slices.Contains(got, want) {
			t.Errorf("no %q in %v", want, got)
		}
	}
	if len(pathDirs(nil)) != len(commandDirs) {
		t.Error("no PATH changes the directories")
	}
}

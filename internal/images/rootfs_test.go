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
	"strings"
	"testing"
)

// fakeDocker serves a tar made by the test.
type fakeDocker struct {
	id        string
	tarball   []byte
	idErr     error
	exportErr error
	exports   int
}

func (f *fakeDocker) ID(context.Context, string) (string, error) { return f.id, f.idErr }

func (f *fakeDocker) Export(context.Context, string) (io.ReadCloser, error) {
	f.exports++
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
	if err := checkMke2fs(t.Context(), bin); err != nil {
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
		"stat /opt/forecrew/forecrew-init": "Mode:  0755",
		"stat /etc/release":                "os-release",
		"stat /etc/alias":                  "Links: 2",
		"stat /secret/key":                 "Mode:  0400",
		"stat /etc/os-release":             "User:  1234",
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
		err := checkMke2fs(t.Context(), stubMke2fs(t, version, false))
		if (err == nil) != ok {
			t.Errorf("mke2fs %s: %v", version, err)
		}
	}
	if err := checkMke2fs(t.Context(), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "e2fsprogs") {
		t.Errorf("missing: %v", err)
	}
	garbled := filepath.Join(t.TempDir(), "mke2fs")
	if err := os.WriteFile(garbled, []byte("#!/bin/sh\necho what >&2\n"), 0o700); err != nil { // #nosec G306 -- a test script
		t.Fatal(err)
	}
	if err := checkMke2fs(t.Context(), garbled); err == nil || !strings.Contains(err.Error(), "version") {
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
		{"size too small", &fakeDocker{id: "i", tarball: makeTar(t, entry{name: "big", typ: tar.TypeReg, mode: 0o644, content: strings.Repeat("x", 3<<20)})}, Request{Ref: "x", SizeMB: 1}, "", "smaller than"},
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
	if got := sizeFor(10 << 20); got != minRootfsMB {
		t.Errorf("small = %d", got)
	}
	if got := sizeFor(2000 << 20); got != 2000*5/4+rootfsHeadroomMB {
		t.Errorf("large = %d", got)
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
	if err != nil || len(files) != 4 {
		t.Fatalf("no labels: %d files, %v", len(files), err)
	}
}

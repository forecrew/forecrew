package knowledge_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/testkit"
)

func tarball(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := h.Linkname // reused as the content of a regular file
		if h.Typeflag == tar.TypeReg {
			h.Linkname, h.Size = "", int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	err := knowledge.ExtractTarGz(tarball(t,
		tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader},
		tar.Header{Name: "acme-app-abc/", Typeflag: tar.TypeDir},
		tar.Header{Name: "acme-app-abc/docs/", Typeflag: tar.TypeDir},
		tar.Header{Name: "acme-app-abc/docs/a.md", Typeflag: tar.TypeReg, Linkname: "hello"},
		tar.Header{Name: "acme-app-abc/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "docs", "a.md")); err != nil || string(b) != "hello" {
		t.Errorf("a.md = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("a symlink was written")
	}

	for name, in := range map[string]*bytes.Buffer{
		"escape":   tarball(t, tar.Header{Name: "top/../../evil", Typeflag: tar.TypeReg, Linkname: "x"}),
		"not gzip": bytes.NewBufferString("plain"),
	} {
		if err := knowledge.ExtractTarGz(in, t.TempDir()); !errors.Is(err, knowledge.ErrArchive) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var cut bytes.Buffer
	full := tarball(t, tar.Header{Name: "top/a", Typeflag: tar.TypeReg, Linkname: strings.Repeat("x", 4000)})
	cut.Write(full.Bytes()[:full.Len()/2])
	if err := knowledge.ExtractTarGz(&cut, t.TempDir()); err == nil {
		t.Error("a truncated archive was accepted")
	}
}

func TestArchiveSnapshotsAndGenerated(t *testing.T) {
	tr := testkit.NewTracker(nil)
	tr.Install("acme/app")
	files := map[string]string{
		"go.mod":               "module github.com/acme/app\n\ngo 1.26\n\nrequire (\n\tgithub.com/direct/one v1.2.0\n\tgithub.com/indirect/two v0.3.0 // indirect\n)\n\nrequire github.com/direct/three v2.0.0\n",
		"cmd/app/main.go":      "package main\n",
		"internal/q/q.go":      "package q\n",
		"internal/q/q_test.go": "package q\n",
		"web/package.json":     `{"name":"web","dependencies":{"react":"^19"},"devDependencies":{"vitest":"^3"}}`,
		"web/src/a.ts":         "export const a = 1\n",
		"README.md":            "# App\n",
	}
	for p, c := range files {
		if err := tr.SetFile("acme/app", "abc123", p, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	src := knowledge.ArchiveSnapshots{Archiver: tr, Analyzer: analyzer.Local{}, TempDir: t.TempDir()}
	snap, err := src.Snapshot(t.Context(), "acme/app", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.Packages, []string{"cmd/app", "internal/q", "web"}) {
		t.Errorf("packages = %v", snap.Packages)
	}
	var names []string
	for _, d := range snap.Dependencies {
		names = append(names, d.Kind+":"+d.Name)
	}
	if !slices.Equal(names, []string{"go:github.com/direct/one", "go:github.com/direct/three", "npm:react", "npm-dev:vitest"}) {
		t.Errorf("dependencies = %v", names)
	}
	if entries, _ := os.ReadDir(src.TempDir); len(entries) != 0 {
		t.Errorf("the temporary folder was kept: %v", entries)
	}
	if _, err := src.Snapshot(t.Context(), "acme/app", "unknown"); err == nil {
		t.Error("an unknown ref was analyzed")
	}

	checkGenerated(t, snap)
}

func checkGenerated(t *testing.T, snap knowledge.Snapshot) {
	t.Helper()
	acct, proj := uuid.New(), uuid.New()
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	gen := knowledge.BuildGenerated(acct, proj, "acme/app", "abc123", snap, now)
	byPath := map[string]domain.KnowledgeEntry{}
	for _, e := range gen {
		byPath[e.Path] = e
		if err := e.Validate(); err != nil || !e.Generated || e.Section != domain.SectionGenerated || e.Version != "abc123" || e.Kind() != domain.KnowledgeGeneratedKind {
			t.Errorf("%s: %+v, %v", e.Path, e, err)
		}
	}
	root := byPath["generated/components/root"]
	if root.Title != "Component . (go)" || !strings.Contains(root.Body, "Build: `go build ./...`") {
		t.Errorf("root component = %+v", root)
	}
	if web := byPath["generated/components/web"]; !strings.Contains(web.Body, "Stack: node") {
		t.Errorf("web component = %+v", web)
	}
	if p := byPath["generated/packages"]; !strings.Contains(p.Body, "- `internal/q`") {
		t.Errorf("packages = %q", p.Body)
	}
	if d := byPath["generated/dependencies"]; !strings.Contains(d.Body, "github.com/direct/one v1.2.0 (go)") || strings.Contains(d.Body, "indirect/two") || !strings.Contains(d.Body, "## web") {
		t.Errorf("dependencies = %q", d.Body)
	}

	empty := knowledge.BuildGenerated(acct, proj, "acme/app", "abc", knowledge.Snapshot{}, now)
	if len(empty) != 2 || !strings.Contains(empty[0].Body, "No packages") || !strings.Contains(empty[1].Body, "No direct dependencies") {
		t.Errorf("empty generated = %+v", empty)
	}
}

// Every kind of entry counts against the limits, not only regular files: a
// million folders or links is as bad as a million files.
func TestExtractTarGzLimits(t *testing.T) {
	many := func(typ byte, n int) *bytes.Buffer {
		hs := make([]tar.Header, n)
		for i := range hs {
			hs[i] = tar.Header{Name: fmt.Sprintf("top/e%d", i), Typeflag: typ, Linkname: "x"}
			if typ == tar.TypeDir {
				hs[i].Name += "/"
				hs[i].Linkname = ""
			}
			if typ == tar.TypeReg {
				hs[i].Linkname = "x"
			}
		}
		return tarball(t, hs...)
	}
	restore := knowledge.LowerArchiveLimits(5, 100, 50, 64<<10)
	defer restore()
	for name, in := range map[string]*bytes.Buffer{
		"folders":  many(tar.TypeDir, 6),
		"symlinks": many(tar.TypeSymlink, 6),
		"files":    many(tar.TypeReg, 6),
		"too many bytes": tarball(t,
			tar.Header{Name: "top/a", Typeflag: tar.TypeReg, Linkname: strings.Repeat("x", 60)},
			tar.Header{Name: "top/b", Typeflag: tar.TypeReg, Linkname: strings.Repeat("y", 60)}),
		"long stream": tarball(t, tar.Header{Name: "top/a", Typeflag: tar.TypeReg, Linkname: strings.Repeat("z", 100<<10)}),
	} {
		if err := knowledge.ExtractTarGz(in, t.TempDir()); !errors.Is(err, knowledge.ErrArchive) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Within the limits, a file over the single-file limit is only left out.
	dir := t.TempDir()
	err := knowledge.ExtractTarGz(tarball(t,
		tar.Header{Name: "top/small", Typeflag: tar.TypeReg, Linkname: "ok"},
		tar.Header{Name: "top/big", Typeflag: tar.TypeReg, Linkname: strings.Repeat("b", 60)}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small")); err != nil {
		t.Errorf("small: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big")); err == nil {
		t.Error("a file over the single-file limit was written")
	}
}

package knowledge

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/analyzer"
)

// Limits of a downloaded repo: forecrewd only reads it, to analyze it.
const (
	maxArchiveFiles = 100_000
	maxArchiveBytes = 512 << 20
	maxArchiveFile  = 8 << 20
	maxPackages     = 300
)

// ErrArchive means a repo archive is unusable or over the limits above.
var ErrArchive = errors.New("repo archive rejected")

// Dependency is a direct dependency of a component.
type Dependency struct {
	Component string `json:"component"` // component path, "." for the root
	Name      string `json:"name"`
	Version   string `json:"version"`
	Kind      string `json:"kind"` // go, npm or npm-dev
}

// Snapshot is what the generated section is built from: the analyzer's
// facts of the repo at one commit, its package list and its direct
// dependencies.
type Snapshot struct {
	Facts        analyzer.Facts `json:"facts"`
	Packages     []string       `json:"packages"`
	Dependencies []Dependency   `json:"dependencies"`
}

// SnapshotSource analyzes the repo at a commit.
type SnapshotSource interface {
	Snapshot(ctx context.Context, repo, ref string) (Snapshot, error)
}

// Archiver downloads a repo; github.Tracker implements it.
type Archiver interface {
	Archive(ctx context.Context, repo, ref string) (io.ReadCloser, error)
}

// ArchiveSnapshots is the SnapshotSource that downloads the repo's tarball
// into a temporary folder, runs the analyzer there and removes it. Nothing
// in the repo is run.
type ArchiveSnapshots struct {
	Archiver Archiver
	Analyzer analyzer.Analyzer
	// TempDir is where folders are made; the system default when empty.
	TempDir string
}

// Snapshot implements SnapshotSource.
func (a ArchiveSnapshots) Snapshot(ctx context.Context, repo, ref string) (Snapshot, error) {
	rc, err := a.Archiver.Archive(ctx, repo, ref)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = rc.Close() }()
	dir, err := os.MkdirTemp(a.TempDir, "forecrew-kb-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("make a folder for %s: %w", repo, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := ExtractTarGz(rc, dir); err != nil {
		return Snapshot{}, fmt.Errorf("unpack %s at %s: %w", repo, ref, err)
	}
	facts, err := a.Analyzer.Analyze(ctx, dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("analyze %s at %s: %w", repo, ref, err)
	}
	pkgs, deps, err := packagesAndDeps(os.DirFS(dir), facts)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read the packages of %s: %w", repo, err)
	}
	return Snapshot{Facts: facts, Packages: pkgs, Dependencies: deps}, nil
}

// ExtractTarGz unpacks a GitHub archive into dir, dropping its top-level
// folder. Only folders and regular files are written; a name that leaves
// dir, a link, or an archive over the limits is refused (ErrArchive).
func ExtractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrArchive, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var seen archiveTotals
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrArchive, err)
		}
		if err := extractEntry(tr, h, dir, &seen); err != nil {
			return err
		}
	}
}

type archiveTotals struct {
	files int
	bytes int64
}

// extractEntry writes one tar entry under dir.
func extractEntry(tr io.Reader, h *tar.Header, dir string, seen *archiveTotals) error {
	_, name, _ := strings.Cut(h.Name, "/")
	if name == "" || h.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	target := path.Clean(name)
	if !fs.ValidPath(target) || target == "." {
		return fmt.Errorf("%w: unsafe name %q", ErrArchive, h.Name)
	}
	dst := filepath.Join(dir, filepath.FromSlash(target))
	switch h.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(dst, 0o750)
	case tar.TypeReg:
		seen.files++
		seen.bytes += h.Size
		if seen.files > maxArchiveFiles || seen.bytes > maxArchiveBytes {
			return fmt.Errorf("%w: over %d files or %d bytes", ErrArchive, maxArchiveFiles, int64(maxArchiveBytes))
		}
		if h.Size > maxArchiveFile {
			return nil // too big to be a manifest or a doc
		}
		return writeFile(dst, io.LimitReader(tr, h.Size))
	}
	return nil // links and devices are never written
}

func writeFile(dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst was checked to be inside the folder
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// packagesAndDeps lists the Go package folders and npm package folders of
// the checkout and the direct dependencies of the components' go.mod and
// package.json files.
func packagesAndDeps(fsys fs.FS, facts analyzer.Facts) ([]string, []Dependency, error) {
	pkgs, err := packageFolders(fsys)
	if err != nil {
		return nil, nil, err
	}
	var deps []Dependency
	for _, c := range facts.Components {
		for _, m := range c.Manifests {
			deps = append(deps, manifestDeps(fsys, c.Path, m)...)
		}
	}
	return pkgs, deps, nil
}

// packageFolders returns the folders with non-test Go files and the folders
// below the root with a package.json, sorted, at most maxPackages.
func packageFolders(fsys fs.FS) ([]string, error) {
	seen := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", "dist", "build", ".git", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		dir := path.Dir(p)
		if (strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go")) || (path.Base(p) == "package.json" && dir != ".") {
			seen[dir] = true
		}
		return nil
	})
	pkgs := slices.Sorted(maps.Keys(seen))
	return pkgs[:min(len(pkgs), maxPackages)], err
}

// manifestDeps returns the direct dependencies of one manifest of a
// component; a missing or broken manifest has none.
func manifestDeps(fsys fs.FS, component, manifest string) []Dependency {
	data, err := fs.ReadFile(fsys, manifest)
	if err != nil {
		return nil
	}
	switch path.Base(manifest) {
	case "go.mod":
		return goDeps(component, data)
	case "package.json":
		deps, err := npmDeps(component, data)
		if err != nil {
			return nil
		}
		return deps
	}
	return nil
}

// goDeps returns the requirements of a go.mod that are not // indirect.
func goDeps(component string, data []byte) []Dependency {
	var out []Dependency
	inBlock := false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case !inBlock:
			rest, ok := strings.CutPrefix(line, "require ")
			if !ok || strings.HasPrefix(rest, "(") {
				continue
			}
			line = rest
		}
		spec, comment, _ := strings.Cut(line, "//")
		if strings.Contains(comment, "indirect") {
			continue
		}
		if f := strings.Fields(spec); len(f) == 2 {
			out = append(out, Dependency{Component: component, Name: f[0], Version: f[1], Kind: "go"})
		}
	}
	return out
}

func npmDeps(component string, data []byte) ([]Dependency, error) {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	var out []Dependency
	for _, name := range slices.Sorted(maps.Keys(pkg.Dependencies)) {
		out = append(out, Dependency{Component: component, Name: name, Version: pkg.Dependencies[name], Kind: "npm"})
	}
	for _, name := range slices.Sorted(maps.Keys(pkg.DevDependencies)) {
		out = append(out, Dependency{Component: component, Name: name, Version: pkg.DevDependencies[name], Kind: "npm-dev"})
	}
	return out, nil
}

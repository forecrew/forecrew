package images

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Bounds of a conversion: the tar of an image is the input of a
// container image a repo controls, so its size and file count are limited.
const (
	maxTarBytes   = 64 << 30
	maxTarEntries = 5_000_000
	// minRootfsMB and rootfsHeadroomMB give a machine room to write.
	minRootfsMB      = 512
	rootfsHeadroomMB = 256
	// convertVersion changes the cache key of every rootfs when the way
	// they are made changes.
	convertVersion = "1"
)

// Docker exports the contents of a Docker image; *DockerCLI does.
type Docker interface {
	// ID returns the content ID of the image ref names (sha256:...).
	ID(ctx context.Context, ref string) (string, error)
	// Env returns the image's Config.Env, the environment its containers
	// start with, as NAME=value entries.
	Env(ctx context.Context, ref string) ([]string, error)
	// Export returns the flattened file system of the image as a tar. The
	// converter passes the content ID, so a tag that moves meanwhile cannot
	// give another image than the one the cache key names.
	Export(ctx context.Context, ref string) (io.ReadCloser, error)
}

// Converter makes the ext4 root filesystems microVMs boot from Docker images,
// so an environment (T23) or a toolbox (T48) image works unchanged as
// machine contents. The result is cached by the content of the image, the
// files added to it and its size: the same environment hash gives the same
// file.
type Converter struct {
	// CacheDir holds the finished files; it is created when missing.
	CacheDir string
	Docker   Docker
	// Mke2fs is the mke2fs command, "mke2fs" when empty.
	Mke2fs string
}

// Request asks for the root filesystem of an image.
type Request struct {
	// Ref is a Docker image reference, such as forecrew-env:<hash>.
	Ref string
	// Files are added to the image, replacing files of the same path: the
	// guest files and the worker (Guest).
	Files []File
	// SizeMB is the size of the file system; 0 sizes it from the content,
	// with room to write.
	SizeMB int
	// Env is the environment the image's processes get, written to
	// /etc/forecrew/env for forecrew-start to apply. Convert reads it from
	// the Docker image (its Config.Env) and ignores this field.
	Env []string
	// NoAgentCLIs skips the check for the agent CLIs: an image for command
	// jobs only does not need them.
	NoAgentCLIs bool
}

// Result is a root filesystem.
type Result struct {
	// Path is the ext4 file in the cache.
	Path string
	// Key identifies the content; Cached says that Path already existed.
	Key    string
	Cached bool
}

// Convert returns the root filesystem of req.Ref, building it unless the
// cache has it.
func (c *Converter) Convert(ctx context.Context, req Request) (Result, error) {
	id, err := c.Docker.ID(ctx, req.Ref)
	if err != nil {
		return Result{}, err
	}
	if req.Env, err = c.Docker.Env(ctx, id); err != nil {
		return Result{}, err
	}
	if req.Files, err = withEnv(req.Files, req.Env); err != nil {
		return Result{}, fmt.Errorf("convert %s: %w", req.Ref, err)
	}
	key := rootfsKey(id, req)
	out := filepath.Join(c.CacheDir, cacheName(req.Ref)+"-"+key[:32]+".ext4")
	if _, err := os.Stat(out); err == nil {
		return Result{Path: out, Key: key, Cached: true}, nil
	}
	rc, err := c.Docker.Export(ctx, id)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = rc.Close() }()
	if err := c.build(ctx, rc, req, out); err != nil {
		return Result{}, fmt.Errorf("convert %s: %w", req.Ref, err)
	}
	return Result{Path: out, Key: key}, nil
}

// ConvertTar is Convert for a tar that a build already made (the export of a
// Packer docker build), with the content ID the caller computed as key.
func (c *Converter) ConvertTar(ctx context.Context, tarball io.Reader, name, id string, req Request) (Result, error) {
	files, err := withEnv(req.Files, req.Env)
	if err != nil {
		return Result{}, fmt.Errorf("convert %s: %w", name, err)
	}
	req.Files = files
	key := rootfsKey(id, req)
	out := filepath.Join(c.CacheDir, cacheName(name)+"-"+key[:32]+".ext4")
	if _, err := os.Stat(out); err == nil {
		return Result{Path: out, Key: key, Cached: true}, nil
	}
	if err := c.build(ctx, tarball, req, out); err != nil {
		return Result{}, fmt.Errorf("convert %s: %w", name, err)
	}
	return Result{Path: out, Key: key}, nil
}

// rootfsKey covers everything that changes the file.
func rootfsKey(imageID string, req Request) string {
	h := sha256.New()
	fmt.Fprintf(h, "forecrew-rootfs %s\nimage %q\nsize %d\n", convertVersion, imageID, req.SizeMB)
	for _, f := range req.Files {
		fmt.Fprintf(h, "file %q %o %d\n", f.Path, f.Mode, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

var notNameChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// cacheName makes a reference fit a file name: forecrew-env:ab12 is
// forecrew-env-ab12.
func cacheName(ref string) string {
	n := strings.Trim(notNameChars.ReplaceAllString(strings.ToLower(ref), "-"), "-.")
	if n == "" {
		return "image"
	}
	return n[:min(len(n), 80)]
}

// build writes the file system to out through a temporary file, so a failed
// or interrupted conversion leaves nothing the cache would take for a result.
func (c *Converter) build(ctx context.Context, src io.Reader, req Request, out string) error {
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil { // #nosec G301 -- a cache others on the host may read
		return err
	}
	work, err := os.MkdirTemp(c.CacheDir, ".convert-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()

	prepared := filepath.Join(work, "root.tar")
	stats, err := prepareTar(src, req.Files, prepared)
	if err != nil {
		return err
	}
	if err := stats.missing(req.NoAgentCLIs); err != nil {
		return err
	}
	size := req.SizeMB
	if need := sizeFor(stats); size == 0 {
		size = need
	} else if size < need-rootfsHeadroomMB {
		return fmt.Errorf("--size %d MB is smaller than the %d MB the content needs", size, need-rootfsHeadroomMB)
	}
	img := filepath.Join(work, "root.ext4")
	if err := c.mkfs(ctx, prepared, img, size, stats); err != nil {
		return err
	}
	return os.Rename(img, out)
}

// File system block and inode sizes, and what the journal and the other
// metadata take: a fixed amount, as mke2fs sizes the journal by the file
// system.
const (
	blockSize   = 4096
	inodeSize   = 256
	journalMB   = 128
	fastSymlink = 60 // a shorter target lives in the inode, longer ones take a block
)

// sizeFor sizes a file system for what the tar holds: every file takes whole
// blocks, so a million small files take far more than their bytes; every entry
// an inode and a directory a block. A fifth more for what mke2fs adds, the
// journal, and room for the job to write; never below minRootfsMB.
func sizeFor(st tarStats) int {
	bytes := st.blocks*blockSize + int64(st.entries)*inodeSize
	return max(minRootfsMB, int(bytes>>20)*6/5+journalMB+rootfsHeadroomMB)
}

// tarStats is what a prepared tar holds.
type tarStats struct {
	bytes   int64 // sizes of regular files, for the limit
	blocks  int64 // blocks the files, directories and long links take
	entries int
	largest int64           // size of the largest regular file
	found   map[string]bool // the paths the checks look for, as met
}

// blocksFor counts the blocks a regular file of size bytes takes.
func blocksFor(size int64) int64 { return (size + blockSize - 1) / blockSize }

// prepareTar copies the tar of an image to dst with files added. It refuses
// paths that leave the root, drops device nodes and sockets (the guest
// mounts devtmpfs), clears user and group names, and bounds size and count.
func prepareTar(src io.Reader, files []File, dst string) (tarStats, error) {
	f, err := os.Create(dst) // #nosec G304 -- a path in the temporary directory
	if err != nil {
		return tarStats{}, err
	}
	defer func() { _ = f.Close() }()
	c := &tarCopier{w: tar.NewWriter(f), replaced: map[string]bool{}, dirs: map[string]bool{".": true}, st: tarStats{found: map[string]bool{}}}
	for _, file := range files {
		c.replaced[path.Clean(file.Path)] = true
	}
	if err := c.copy(tar.NewReader(src)); err != nil {
		return c.st, err
	}
	if err := c.add(files); err != nil {
		return c.st, err
	}
	if err := c.w.Close(); err != nil {
		return c.st, err
	}
	return c.st, f.Close()
}

type tarCopier struct {
	w        *tar.Writer
	replaced map[string]bool // paths the added files take over
	dirs     map[string]bool // directories written so far
	st       tarStats
}

// copy writes the entries of r that belong in the file system.
func (c *tarCopier) copy(r *tar.Reader) error {
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read image tar: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "/"))
		if name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("image tar entry %q leaves the root", h.Name)
		}
		if c.replaced[name] && h.Typeflag != tar.TypeDir {
			continue
		}
		if c.st.entries++; c.st.entries > maxTarEntries {
			return fmt.Errorf("image has more than %d files", maxTarEntries)
		}
		if err := c.entry(h, name, r); err != nil {
			return err
		}
	}
}

func (c *tarCopier) entry(h *tar.Header, name string, r io.Reader) error {
	switch h.Typeflag {
	case tar.TypeReg:
		if c.st.bytes += h.Size; c.st.bytes > maxTarBytes {
			return fmt.Errorf("image is larger than %d GB", maxTarBytes>>30)
		}
		c.st.blocks += blocksFor(h.Size)
		c.st.largest = max(c.st.largest, h.Size)
	case tar.TypeDir:
		if c.dirs[name] {
			return nil // already written, as the parent of an earlier entry
		}
		c.dirs[name] = true
		c.st.blocks++
	case tar.TypeSymlink:
		if len(h.Linkname) >= fastSymlink {
			c.st.blocks++
		}
	case tar.TypeLink:
	default:
		return nil // device nodes, sockets and pipes
	}
	c.st.met(name)
	// Parents first: mke2fs cannot add a file to a directory it has not seen.
	if err := addDirs(c.w, c.dirs, path.Dir(name), &c.st); err != nil {
		return err
	}
	h.Name, h.Uname, h.Gname, h.PAXRecords, h.Format = name, "", "", nil, tar.FormatUnknown
	if h.Typeflag == tar.TypeLink {
		h.Linkname = path.Clean(strings.TrimPrefix(h.Linkname, "/"))
	}
	if err := c.w.WriteHeader(h); err != nil {
		return err
	}
	if _, err := io.CopyN(c.w, r, h.Size); err != nil {
		return fmt.Errorf("copy image tar: %w", err)
	}
	return nil
}

// add writes the added files, each after its missing parent directories.
func (c *tarCopier) add(files []File) error {
	for _, file := range files {
		name := path.Clean(file.Path)
		if err := addDirs(c.w, c.dirs, path.Dir(name), &c.st); err != nil {
			return err
		}
		if err := c.w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: file.Mode, Size: int64(len(file.Data))}); err != nil {
			return err
		}
		if _, err := c.w.Write(file.Data); err != nil {
			return err
		}
		c.st.bytes += int64(len(file.Data))
		c.st.blocks += blocksFor(int64(len(file.Data)))
		c.st.entries++
		c.st.met(name)
	}
	return nil
}

// addDirs writes the directory dir and its missing parents, shallowest first.
func addDirs(w *tar.Writer, have map[string]bool, dir string, st *tarStats) error {
	if have[dir] {
		return nil
	}
	if err := addDirs(w, have, path.Dir(dir), st); err != nil {
		return err
	}
	have[dir] = true
	st.entries++
	st.blocks++
	return w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755})
}

var mke2fsVersion = regexp.MustCompile(`mke2fs (\d+)\.(\d+)\.(\d+)`)

// mke2fsTarVersion is the first mke2fs that reads a tar, and mke2fsBigFile
// the first that copies files of 2 GiB and more with -d.
var (
	mke2fsTarVersion = [3]int{1, 47, 1}
	mke2fsBigFile    = [3]int{1, 47, 4}
)

const maxSmallFile = 2<<30 - 1

// mkfs makes an ext4 file system of sizeMB from the tar. mke2fs reads a tar
// itself from 1.47.1, and keeps the owners it records, which a tree unpacked
// by an ordinary user could not; so older ones are refused. Before 1.47.4 -d
// fails on files of 2 GiB and more, which is refused up front too.
func (c *Converter) mkfs(ctx context.Context, tarPath, img string, sizeMB int, st tarStats) error {
	bin := c.Mke2fs
	if bin == "" {
		bin = "mke2fs"
	}
	version, err := checkMke2fs(ctx, bin)
	if err != nil {
		return err
	}
	if st.largest > maxSmallFile && versionLess(version, mke2fsBigFile) {
		return fmt.Errorf("the image has a file of %d MB: mke2fs %d.%d.%d cannot copy files of 2 GiB or more with -d: install e2fsprogs 1.47.4 or newer",
			st.largest>>20, version[0], version[1], version[2])
	}
	// -N: one inode per file with room to grow, never below mke2fs's default.
	args := []string{"-q", "-t", "ext4", "-F", "-L", "forecrew-root", "-m", "0", "-N", strconv.Itoa(max(st.entries*3/2+4096, sizeMB*64)),
		"-d", tarPath, img, strconv.Itoa(sizeMB) + "M"}
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- bin is the operator's mke2fs; the rest are fixed flags and our paths
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mke2fs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// checkMke2fs returns the version of bin, and fails unless it is mke2fs 1.47.1
// or newer and can read a tar: it tries a small one, which fails where the
// libarchive it needs is missing (libarchive13t64 on Debian and Ubuntu).
func checkMke2fs(ctx context.Context, bin string) ([3]int, error) {
	var v [3]int
	out, err := exec.CommandContext(ctx, bin, "-V").CombinedOutput() // #nosec G204 -- see mkfs
	if err != nil && len(out) == 0 {
		return v, fmt.Errorf("mke2fs: %w (install e2fsprogs 1.47.1 or newer)", err)
	}
	m := mke2fsVersion.FindSubmatch(out)
	if m == nil {
		return v, fmt.Errorf("mke2fs: cannot tell its version from %q", strings.TrimSpace(string(out)))
	}
	for i := range v {
		v[i], _ = strconv.Atoi(string(m[i+1]))
	}
	if versionLess(v, mke2fsTarVersion) {
		return v, fmt.Errorf("mke2fs %d.%d.%d cannot build a file system from a tar (it keeps file owners): install e2fsprogs 1.47.1 or newer", v[0], v[1], v[2])
	}
	return v, probeTar(ctx, bin)
}

// probeTar makes a file system from a one-file tar, to find out that mke2fs
// can read a tar at all.
func probeTar(ctx context.Context, bin string) error {
	dir, err := os.MkdirTemp("", "forecrew-mke2fs-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "probe", Mode: 0o644, Size: 1}); err != nil {
		return err
	}
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		return err
	}
	tarPath := filepath.Join(dir, "probe.tar")
	if err := os.WriteFile(tarPath, b.Bytes(), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "-q", "-t", "ext4", "-F", "-d", tarPath, filepath.Join(dir, "probe.ext4"), "1M") // #nosec G204 -- see mkfs
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mke2fs cannot read a tar (is libarchive installed? libarchive13t64 on Debian and Ubuntu): %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// requirement is a thing an image must have: one of paths.
type requirement struct {
	what  string
	paths []string
}

var commandDirs = []string{"bin", "usr/bin", "sbin", "usr/sbin", "usr/local/bin", "usr/local/sbin"}

func command(name, why string) requirement {
	r := requirement{what: name + " (" + why + ")"}
	for _, d := range commandDirs {
		r.paths = append(r.paths, d+"/"+name)
	}
	return r
}

// requirements are what an image needs to run the worker and its init; the
// agent CLIs too unless the image is for command jobs only.
func requirements(noAgentCLIs bool) []requirement {
	reqs := []requirement{
		command("sh", "the init and the start script are shell scripts"),
		command("ip", "iproute2: the init configures the network"),
		command("git", "the worker clones with it"),
		{what: "CA certificates (the worker and git speak HTTPS)", paths: []string{"etc/ssl/certs/ca-certificates.crt", "etc/pki/tls/certs/ca-bundle.crt", "etc/ssl/ca-bundle.pem", "etc/ssl/cert.pem"}},
	}
	if !noAgentCLIs {
		for _, cli := range []string{"claude", "codex", "opencode"} {
			reqs = append(reqs, command(cli, "the agent CLI jobs run; --no-agent-clis for an image of command jobs"))
		}
	}
	return reqs
}

// wanted is every path the requirements look for.
func wanted() map[string]bool {
	w := map[string]bool{}
	for _, r := range requirements(false) {
		for _, p := range r.paths {
			w[p] = true
		}
	}
	return w
}

// met records that the tar has name, if a requirement looks for it.
func (st *tarStats) met(name string) {
	if wantedPaths[name] {
		st.found[name] = true
	}
}

var wantedPaths = wanted()

// missing fails with everything the image lacks, so one conversion shows the
// whole list.
func (st tarStats) missing(noAgentCLIs bool) error {
	var lacks []string
	for _, r := range requirements(noAgentCLIs) {
		if !slices.ContainsFunc(r.paths, func(p string) bool { return st.found[p] }) {
			lacks = append(lacks, r.what)
		}
	}
	if len(lacks) == 0 {
		return nil
	}
	return fmt.Errorf("the image lacks %s: install them in the image (docs/images.md, What an image needs)", strings.Join(lacks, "; "))
}

// withEnv adds /etc/forecrew/env, the image's environment as NAME=value
// lines, to files. forecrew-start reads it as data. Names must be shell-safe
// and not FORECREW_*, values single-line; the whole is bounded.
func withEnv(files []File, env []string) ([]File, error) {
	var b strings.Builder
	for _, e := range env {
		name, _, ok := strings.Cut(e, "=")
		switch {
		case !ok || !envName.MatchString(name) || strings.HasPrefix(name, "FORECREW_"):
			continue // not a variable, or one the worker reserves
		case strings.ContainsAny(e, "\n\r\x00"):
			return nil, fmt.Errorf("environment variable %s has a newline or NUL in its value", name)
		}
		b.WriteString(e + "\n")
	}
	if b.Len() > maxEnvBytes {
		return nil, fmt.Errorf("the image's environment is larger than %d KB", maxEnvBytes>>10)
	}
	out := slices.Clone(files)
	if b.Len() == 0 {
		return out, nil
	}
	return append(out, File{Path: GuestEnv, Mode: 0o644, Data: []byte(b.String())}), nil
}

const maxEnvBytes = 256 << 10

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

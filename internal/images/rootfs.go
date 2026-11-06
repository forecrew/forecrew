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
	// Export returns the flattened file system of the image as a tar.
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
	key := rootfsKey(id, req)
	out := filepath.Join(c.CacheDir, cacheName(req.Ref)+"-"+key[:32]+".ext4")
	if _, err := os.Stat(out); err == nil {
		return Result{Path: out, Key: key, Cached: true}, nil
	}
	rc, err := c.Docker.Export(ctx, req.Ref)
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
	size := req.SizeMB
	if size == 0 {
		size = sizeFor(stats.bytes)
	} else if int64(size) < stats.bytes>>20 {
		return fmt.Errorf("--size %d MB is smaller than the %d MB of content", size, stats.bytes>>20)
	}
	img := filepath.Join(work, "root.ext4")
	if err := c.mkfs(ctx, prepared, img, size, stats.entries); err != nil {
		return err
	}
	return os.Rename(img, out)
}

// sizeFor sizes a file system for bytes of content: a quarter more for
// metadata and some room for the job, and not below minRootfsMB.
func sizeFor(bytes int64) int {
	return max(minRootfsMB, int(bytes>>20)*5/4+rootfsHeadroomMB)
}

type tarStats struct {
	bytes   int64
	entries int
}

// prepareTar copies the tar of an image to dst with files added. It refuses
// paths that leave the root, drops device nodes and sockets (the guest
// mounts devtmpfs), clears user and group names, and bounds size and count.
func prepareTar(src io.Reader, files []File, dst string) (tarStats, error) {
	f, err := os.Create(dst) // #nosec G304 -- a path in the temporary directory
	if err != nil {
		return tarStats{}, err
	}
	defer func() { _ = f.Close() }()
	w := tar.NewWriter(f)
	replaced := map[string]bool{}
	for _, file := range files {
		replaced[path.Clean(file.Path)] = true
	}
	var st tarStats
	dirs := map[string]bool{".": true}
	r := tar.NewReader(src)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("read image tar: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "/"))
		if name == ".." || strings.HasPrefix(name, "../") {
			return st, fmt.Errorf("image tar entry %q leaves the root", h.Name)
		}
		if replaced[name] && h.Typeflag != tar.TypeDir {
			continue
		}
		if st.entries++; st.entries > maxTarEntries {
			return st, fmt.Errorf("image has more than %d files", maxTarEntries)
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		default:
			continue
		}
		if h.Typeflag == tar.TypeReg {
			if st.bytes += h.Size; st.bytes > maxTarBytes {
				return st, fmt.Errorf("image is larger than %d GB", maxTarBytes>>30)
			}
		}
		if h.Typeflag == tar.TypeDir {
			dirs[name] = true
		}
		h.Name, h.Uname, h.Gname, h.PAXRecords, h.Format = name, "", "", nil, tar.FormatUnknown
		if h.Typeflag == tar.TypeLink {
			h.Linkname = path.Clean(strings.TrimPrefix(h.Linkname, "/"))
		}
		if err := w.WriteHeader(h); err != nil {
			return st, err
		}
		if _, err := io.Copy(w, r); err != nil {
			return st, fmt.Errorf("copy image tar: %w", err)
		}
	}
	for _, file := range files {
		name := path.Clean(file.Path)
		if err := addDirs(w, dirs, path.Dir(name), &st); err != nil {
			return st, err
		}
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: file.Mode, Size: int64(len(file.Data))}); err != nil {
			return st, err
		}
		if _, err := w.Write(file.Data); err != nil {
			return st, err
		}
		st.bytes += int64(len(file.Data))
		st.entries++
	}
	if err := w.Close(); err != nil {
		return st, err
	}
	return st, f.Close()
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
	return w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755})
}

var mke2fsVersion = regexp.MustCompile(`mke2fs (\d+)\.(\d+)\.(\d+)`)

// mkfs makes an ext4 file system of sizeMB from the tar. mke2fs reads a tar
// itself from 1.47.1, and keeps the owners it records, which a tree unpacked
// by an ordinary user could not; so older ones are refused.
func (c *Converter) mkfs(ctx context.Context, tarPath, img string, sizeMB, entries int) error {
	bin := c.Mke2fs
	if bin == "" {
		bin = "mke2fs"
	}
	if err := checkMke2fs(ctx, bin); err != nil {
		return err
	}
	// -N: one inode per file with room to grow, never below mke2fs's default.
	args := []string{"-q", "-t", "ext4", "-F", "-L", "forecrew-root", "-m", "0", "-N", strconv.Itoa(max(entries*3/2+4096, sizeMB*64)),
		"-d", tarPath, img, strconv.Itoa(sizeMB) + "M"}
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- bin is the operator's mke2fs; the rest are fixed flags and our paths
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mke2fs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// checkMke2fs fails unless bin is mke2fs 1.47.1 or newer.
func checkMke2fs(ctx context.Context, bin string) error {
	out, err := exec.CommandContext(ctx, bin, "-V").CombinedOutput() // #nosec G204 -- see mkfs
	if err != nil && len(out) == 0 {
		return fmt.Errorf("mke2fs: %w (install e2fsprogs 1.47.1 or newer)", err)
	}
	m := mke2fsVersion.FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("mke2fs: cannot tell its version from %q", strings.TrimSpace(string(out)))
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(string(m[i+1]))
	}
	if v[0] < 1 || (v[0] == 1 && (v[1] < 47 || (v[1] == 47 && v[2] < 1))) {
		return fmt.Errorf("mke2fs %d.%d.%d cannot build a file system from a tar (it keeps file owners): install e2fsprogs 1.47.1 or newer", v[0], v[1], v[2])
	}
	return nil
}

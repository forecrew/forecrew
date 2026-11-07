package microvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/images"
)

// maxFetch bounds a file fetched for an image, like the converter's limit.
const maxFetch = 64 << 30

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// cachePath returns where the file of a digest is kept. The digest comes from
// forecrewd, so it is checked before it names a file.
func (p *Provider) cachePath(digest string) (string, error) {
	if !sha256Digest.MatchString(digest) {
		return "", fmt.Errorf("digest %q is not sha256", digest)
	}
	return filepath.Join(p.imageDir(), strings.TrimPrefix(digest, "sha256:")), nil
}

func (p *Provider) imageDir() string { return filepath.Join(p.cfg.StateDir, "images") }

// ensureImage returns the files of img, fetched if needed. The signature is
// checked every time, as it costs nothing, and the digests of the files before
// their first use by this process: nothing else writes to the cache. The machine
// is refused when any of it fails.
func (p *Provider) ensureImage(ctx context.Context, img domain.Image) (root, kernel string, err error) {
	if img.Format != domain.ImageRootfs || img.Contents.Kernel == nil {
		return "", "", fmt.Errorf("image %s@%s: a microVM boots a rootfs image with a kernel, not %s", img.Name, img.Version, img.Format)
	}
	keys, err := p.trustedKeys()
	if err != nil {
		return "", "", err
	}
	if err := images.Verify(img, keys); err != nil {
		return "", "", err
	}
	if root, err = p.cachePath(img.Digest); err != nil {
		return "", "", err
	}
	if kernel, err = p.cachePath(img.Contents.Kernel.Digest); err != nil {
		return "", "", err
	}
	p.fetching.Lock()
	defer p.fetching.Unlock()
	if err := p.fetch(ctx, img.URI, img.Digest, root); err != nil {
		return "", "", fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	if err := p.fetch(ctx, img.Contents.Kernel.URI, img.Contents.Kernel.Digest, kernel); err != nil {
		return "", "", fmt.Errorf("image %s@%s kernel: %w", img.Name, img.Version, err)
	}
	if !p.verified[img.Signature] {
		if err := p.verifyFiles(ctx, img, root, kernel, keys); err != nil {
			return "", "", err
		}
		p.verified[img.Signature] = true
	}
	now := time.Now()
	for _, f := range []string{root, kernel} {
		_ = os.Chtimes(f, now, now) // a file that stays old is only fetched again
	}
	// The jail's user reads the kernel through a hard link, also when an
	// operator put the file there.
	return root, kernel, os.Chmod(kernel, 0o444) // #nosec G302 -- a kernel is no secret
}

// verifyFiles checks the files of img and, when a cached one is not what was
// signed (damaged, or placed by mistake), removes it and fetches it once more.
func (p *Provider) verifyFiles(ctx context.Context, img domain.Image, root, kernel string, keys []ed25519Key) error {
	err := images.VerifyFile(img, root, kernel, keys)
	if !errors.Is(err, images.ErrDigest) {
		return err
	}
	for path, want := range map[string]string{root: img.Digest, kernel: img.Contents.Kernel.Digest} {
		if got, derr := images.FileDigest(path); derr != nil || got != want {
			p.log.Warn("a cached image file is not the one signed: fetching it again", "file", filepath.Base(path))
			if rerr := os.Remove(path); rerr != nil {
				return errors.Join(err, rerr)
			}
		}
	}
	if err := p.fetch(ctx, img.URI, img.Digest, root); err != nil {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	if err := p.fetch(ctx, img.Contents.Kernel.URI, img.Contents.Kernel.Digest, kernel); err != nil {
		return fmt.Errorf("image %s@%s kernel: %w", img.Name, img.Version, err)
	}
	return images.VerifyFile(img, root, kernel, keys)
}

func (p *Provider) trustedKeys() ([]ed25519Key, error) {
	return fleetagent.LoadTrustedKeys(p.cfg.TrustedKeysFile)
}

// fetch makes path the file of digest unless it is there. Only http and https
// are fetched; for another scheme an operator puts the file in the cache.
func (p *Provider) fetch(ctx context.Context, uri, digest, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if u, err := url.Parse(uri); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("only http and https are fetched: put the file in %s as %s", p.imageDir(), filepath.Base(path))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(p.imageDir(), ".fetch-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxFetch+1))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if n > maxFetch {
		return fmt.Errorf("fetch: the file is larger than %d GB", maxFetch>>30)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%w (file is %s, signed is %s)", images.ErrDigest, got, digest)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// clone copies src to dst, as a reflink where the file system has them, which
// is what makes a machine's disk instant. The copy is the machine's alone.
func clone(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- a path of this package
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path of this package
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) != nil { // #nosec G115 -- file descriptors
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
	}
	return out.Close()
}

// reflink reports whether src clones to dst without copying.
func reflink(src, dst string) bool {
	in, err := os.Open(src) // #nosec G304 -- a path of this package
	if err != nil {
		return false
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) // #nosec G304 -- a path of this package
	if err != nil {
		return false
	}
	defer func() { _ = out.Close() }()
	return unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil // #nosec G115 -- file descriptors
}

// link makes dst the file src, read-only, for what machines share. A hard link
// costs nothing; on another file system it is a copy.
func link(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	if err := clone(src, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o444) // #nosec G302 -- a shared read-only file
}

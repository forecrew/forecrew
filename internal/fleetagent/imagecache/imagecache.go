// Package imagecache fetches the image file of a machine to the fleet agent's
// host and checks it before anything uses it: the signature against the keys the
// agent trusts, and the file against the digest that signature covers. It is for
// images that are one file, for a provider that boots from a local copy (Hyper-V).
package imagecache

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/images"
)

// maxFetch bounds a file fetched for an image, like the converter's limit.
const maxFetch = 64 << 30

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Cache is a directory of verified image files, named by their digest.
type Cache struct {
	// Dir holds the files. It must be closed to everyone but the agent's user.
	Dir string
	// KeysFile lists the public keys images must be signed with, one per line.
	KeysFile string
	// Ext is added to a file's name, with its dot: a hypervisor may want to see
	// the kind of file.
	Ext string
	// Format is the one format this cache serves.
	Format domain.ImageFormat
	HTTP   *http.Client
	Log    *slog.Logger

	mu sync.Mutex // one fetch at a time; also guards verified
	// verified is, by signature, what the file looked like (size and time of
	// change) when its digest was last checked: a file that looks otherwise is
	// checked again, since something may have written to it.
	verified map[string]fingerprint
}

// New returns a cache of Dir; log may be nil.
func New(dir, keysFile, ext string, format domain.ImageFormat, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}
	return &Cache{Dir: dir, KeysFile: keysFile, Ext: ext, Format: format, HTTP: &http.Client{Timeout: 30 * time.Minute}, Log: log, verified: map[string]fingerprint{}}
}

// Path returns where the file of digest is kept. The digest comes from
// forecrewd, so it is checked before it names a file.
func (c *Cache) Path(digest string) (string, error) {
	if !sha256Digest.MatchString(digest) {
		return "", fmt.Errorf("digest %q is not sha256", digest)
	}
	return filepath.Join(c.Dir, strings.TrimPrefix(digest, "sha256:")+c.Ext), nil
}

// Ensure returns the file of img, fetched if needed. The signature is checked
// every time, as it costs nothing, and the file's digest before its first use by
// this process: nothing else writes to the cache. The machine is refused when any
// of it fails.
func (c *Cache) Ensure(ctx context.Context, img domain.Image) (string, error) {
	if img.Format != c.Format || img.Contents.Kernel != nil {
		return "", fmt.Errorf("image %s@%s: this location starts %s images, not %s", img.Name, img.Version, c.Format, img.Format)
	}
	keys, err := fleetagent.LoadTrustedKeys(c.KeysFile)
	if err != nil {
		return "", err
	}
	if err := images.Verify(img, keys); err != nil {
		return "", err
	}
	path, err := c.Path(img.Digest)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return "", err
	}
	if err := c.fetch(ctx, img.URI, img.Digest, path); err != nil {
		return "", fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	if now, err := look(path); err != nil || c.verified[img.Signature] != now {
		if err := c.verifyFile(ctx, img, path, keys); err != nil {
			return "", err
		}
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now) // a file that stays old is only fetched again
	fp, err := look(path)
	if err != nil {
		return "", err
	}
	c.verified[img.Signature] = fp
	return path, nil
}

// verifyFile checks the file of img and, when the cached one is not what was
// signed (damaged, or placed by mistake), removes it and fetches it once more.
func (c *Cache) verifyFile(ctx context.Context, img domain.Image, path string, keys []ed25519.PublicKey) error {
	err := images.VerifyFile(img, path, "", keys)
	if !errors.Is(err, images.ErrDigest) {
		return err
	}
	c.Log.Warn("a cached image file is not the one signed: fetching it again", "file", filepath.Base(path))
	if rerr := os.Remove(path); rerr != nil {
		return errors.Join(err, rerr)
	}
	if err := c.fetch(ctx, img.URI, img.Digest, path); err != nil {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	return images.VerifyFile(img, path, "", keys)
}

// fetch makes path the file of digest unless it is there. Only http and https
// are fetched; for another scheme an operator puts the file in the cache.
func (c *Cache) fetch(ctx context.Context, uri, digest, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if u, err := url.Parse(uri); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("only http and https are fetched: put the file in %s as %s", c.Dir, filepath.Base(path))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: %s", resp.Status)
	}
	tmp, err := os.CreateTemp(c.Dir, ".fetch-*")
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

// fingerprint is what a file looks like from outside.
type fingerprint struct {
	size int64
	mod  time.Time
}

func look(path string) (fingerprint, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprint{st.Size(), st.ModTime()}, nil
}

// Prune removes files unused for maxAge and what an interrupted fetch left.
func (c *Cache) Prune(maxAge time.Duration) ([]string, error) {
	removed, err := images.PruneCache(c.Dir, maxAge, false)
	if len(removed) > 0 {
		c.mu.Lock()
		clear(c.verified) // what is gone is checked again if it comes back
		c.mu.Unlock()
	}
	return removed, err
}

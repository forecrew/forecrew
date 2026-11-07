package microvm

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// The organization's cache disk (docs/fleet.md, Organization caches): one
// encrypted file per organization and pool on the host, of which each machine
// gets its own copy, opened through dm-crypt on the host so the guest sees a
// plain disk and the host's disk holds only ciphertext.
const (
	// fileOrgCacheImage is the machine's copy of its organization's cache,
	// encrypted; fileOrgCache is the block device of it opened, in the jail.
	fileOrgCacheImage = "orgcache.img"
	fileOrgCache      = "orgcache"
	// cacheKeyFile holds the key every cache key is derived from.
	cacheKeyFile = "cache.key"
	// cacheKeySize is the key of aes-xts-plain64 with 256-bit AES.
	cacheKeySize = 64
	mapperPrefix = "forecrew-cache"
)

// cachesDir holds the organizations' caches: <org>/<pool>.img and its stamp.
func (p *Provider) cachesDir() string { return filepath.Join(p.cfg.StateDir, "caches") }

// orgCacheOf says whose cache a machine gets: the organization of its tags and
// the pool of its shape, both checked before they name a file. None for a
// machine with no organization, a warm one, or when caches are off.
func (p *Provider) orgCacheOf(req fleet.CreateRequest) string {
	org := req.Tags[fleet.TagOrg]
	pool := domain.OrDefaultPool(req.Shape.Pool)
	if p.cfg.OrgCacheGB <= 0 || !validID.MatchString(org) || !domain.ValidName(pool) {
		return ""
	}
	return org + "/" + pool
}

func (p *Provider) orgCachePath(cache string) string {
	return filepath.Join(p.cachesDir(), cache+".img")
}

func mapperName(slot int) string { return mapperPrefix + strconv.Itoa(slot) }

// cacheKey is the key of one organization's cache of one pool: an HMAC of its
// name under the host's cache key, so no two caches share a key and the
// file of one opened as another's reads as noise. The host's key is made at
// first use and only root reads it.
func (p *Provider) cacheKey(cache string) ([]byte, error) {
	path := filepath.Join(p.cfg.StateDir, cacheKeyFile)
	master, err := os.ReadFile(path) // #nosec G304 -- a path of this package
	if errors.Is(err, fs.ErrNotExist) {
		master = make([]byte, 32)
		_, _ = rand.Read(master)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path of this package
		if err != nil {
			return nil, fmt.Errorf("cache key: %w", err)
		}
		_, err = f.Write(master)
		if err := errors.Join(err, f.Close()); err != nil {
			return nil, fmt.Errorf("cache key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("cache key: %w", err)
	}
	if len(master) != 32 {
		return nil, fmt.Errorf("cache key: %s is not 32 bytes", path)
	}
	m := hmac.New(sha512.New, master)
	m.Write([]byte("forecrew organization cache\x00" + cache))
	return m.Sum(nil), nil
}

// stampFile is a stamp on disk, next to the cache it is of.
type stampFile struct {
	Dev   uint64 `json:"dev"`
	Ino   uint64 `json:"ino"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Ctime int64  `json:"ctime"`
}

// readStamp returns what the cache file was when it was last written back.
func readStamp(path string) (stamp, bool) {
	raw, err := os.ReadFile(path + ".stamp") // #nosec G304 -- a path of this package
	if err != nil {
		return stamp{}, false
	}
	var w stampFile
	if json.Unmarshal(raw, &w) != nil {
		return stamp{}, false
	}
	return stamp{dev: w.Dev, ino: w.Ino, size: w.Size, mtime: w.Mtime, ctime: w.Ctime}, true
}

func writeStamp(path string, s stamp) error {
	raw, err := json.Marshal(stampFile{Dev: s.dev, Ino: s.ino, Size: s.size, Mtime: s.mtime, Ctime: s.ctime})
	if err != nil {
		return err
	}
	return os.WriteFile(path+".stamp", raw, 0o600)
}

// takeCache gives the machine in dir its copy of cache: the organization's
// file when it is what was last written back, or a new empty one. A file
// changed since by anyone but this agent is not used: it is removed, and the
// machine starts with an empty cache. It says whether the copy is new and
// needs a file system.
func (p *Provider) takeCache(cache, dir string) (bool, error) {
	src := p.orgCachePath(cache)
	dst := filepath.Join(dir, fileOrgCacheImage)
	p.caching.Lock()
	defer p.caching.Unlock()
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		return false, err
	}
	now, err := stampOf(src)
	switch was, ok := readStamp(src); {
	case err == nil && ok && now == was:
		if err := clone(src, dst); err != nil {
			return false, err
		}
		_ = os.Chtimes(src+".stamp", time.Now(), time.Now()) // in use: the pruning goes by the stamp
		return false, nil
	case err == nil:
		p.log.Warn("an organization's cache changed since it was written: starting it again empty", "cache", cache)
		_ = os.Remove(src + ".stamp")
		if err := os.Remove(src); err != nil {
			return false, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a path of this package
	if err != nil {
		return false, err
	}
	err = f.Truncate(int64(p.cfg.OrgCacheGB) << 30)
	return true, errors.Join(err, f.Close())
}

// openCache gives the machine of r its organization's cache: its copy, opened
// through dm-crypt with the organization's key, which goes to cryptsetup on
// its standard input, and a device node of it in the jail for the VMM.
func (p *Provider) openCache(ctx context.Context, r *record, dir string, uid int) error {
	key, err := p.cacheKey(r.OrgCache)
	if err != nil {
		return err
	}
	fresh, err := p.takeCache(r.OrgCache, dir)
	if err != nil {
		return err
	}
	name := mapperName(r.Slot)
	err = p.run(ctx, string(key), "cryptsetup", "open", "--type", "plain", "--cipher", "aes-xts-plain64", "--key-size", "512",
		"--hash", "plain", "--key-file=-", "--keyfile-size", strconv.Itoa(cacheKeySize), filepath.Join(dir, fileOrgCacheImage), name)
	if err != nil {
		return err
	}
	device := filepath.Join("/dev/mapper", name)
	if fresh {
		if err := p.run(ctx, "", "mkfs.ext4", "-q", "-L", "forecrew-cache", device); err != nil {
			return err
		}
	}
	node := filepath.Join(dir, fileOrgCache)
	if err := p.mknod(device, node); err != nil {
		return err
	}
	return p.chown(node, uid, uid)
}

// closeCache closes the machine's cache and, when its VMM ran, makes its copy
// the organization's cache: the last machine to end wins. A machine that never
// ran leaves the cache as it was.
func (p *Provider) closeCache(ctx context.Context, r *record, dir string) error {
	if p.hasMapping(mapperName(r.Slot)) {
		if err := p.run(ctx, "", "cryptsetup", "close", mapperName(r.Slot)); err != nil {
			return err
		}
	}
	copyPath := filepath.Join(dir, fileOrgCacheImage)
	if r.PID <= 0 || r.Abuse != "" {
		return nil
	}
	if _, err := os.Stat(copyPath); err != nil {
		return nil //nolint:nilerr // written back already, or never made
	}
	src := p.orgCachePath(r.OrgCache)
	p.caching.Lock()
	defer p.caching.Unlock()
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		return err
	}
	if err := os.Rename(copyPath, src); err != nil {
		return err
	}
	s, err := stampOf(src)
	if err != nil {
		return err
	}
	return writeStamp(src, s)
}

// pruneCaches removes organizations' caches no machine took for days.
func (p *Provider) pruneCaches(older time.Duration) {
	p.caching.Lock()
	defer p.caching.Unlock()
	stamps, _ := filepath.Glob(filepath.Join(p.cachesDir(), "*", "*.img.stamp"))
	for _, s := range stamps {
		st, err := os.Stat(s)
		if err != nil || time.Since(st.ModTime()) < older {
			continue
		}
		img := s[:len(s)-len(".stamp")]
		if err := os.Remove(img); err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("remove an unused cache", "error", err)
			continue
		}
		_ = os.Remove(s)
		p.log.Info("removed an unused organization cache", "cache", filepath.Base(filepath.Dir(img))+"/"+filepath.Base(img))
	}
}

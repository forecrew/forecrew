package tart

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/images"
)

// pruneEvery spaces the cleaning of Tart's image cache.
const pruneEvery = time.Hour

var (
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// repoPath is a registry host and repository, as OCI names them.
	repoPath = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)
	localVM  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ociRef is the reference an image is pulled by: its repository, pinned by the
// digest the image's signature covers, so what is pulled is what was signed, and
// a tag moved in the registry changes nothing. The URI and digest come from
// forecrewd, so they are checked before they become an argument.
func ociRef(img domain.Image) (string, error) {
	u, err := url.Parse(img.URI)
	if err != nil || u.Scheme != "oci" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("image %s@%s: a Tart image's URI is oci://registry/name, got %q", img.Name, img.Version, img.URI)
	}
	if !sha256Digest.MatchString(img.Digest) {
		return "", fmt.Errorf("image %s@%s: digest %q is not sha256", img.Name, img.Version, img.Digest)
	}
	repo := u.Host + u.Path
	if i := strings.LastIndexByte(repo, '/'); i >= 0 {
		last := repo[i+1:]
		if j := strings.IndexAny(last, ":@"); j >= 0 {
			repo = repo[:i+1] + last[:j]
		}
	}
	if !repoPath.MatchString(repo) {
		return "", fmt.Errorf("image %s@%s: %q is not a registry repository", img.Name, img.Version, repo)
	}
	return repo + "@" + img.Digest, nil
}

// source returns what a machine is cloned from, and makes it available: for an
// image, its signature is checked against the keys of this host every time, and
// the registry checks the content against the digest the signature covers when
// Tart pulls it. A shape's base_vm names a local VM instead, when this host's
// operator allows it.
func (p *Provider) source(ctx context.Context, cfg providerConfig, img domain.Image) (string, error) {
	if cfg.BaseVM != "" {
		if !p.cfg.AllowLocalBase {
			return "", fmt.Errorf("the shape names a local VM (base_vm): set allow_local_base in tart.yaml on this Mac to run unsigned VMs")
		}
		if !localVM.MatchString(cfg.BaseVM) || strings.HasPrefix(cfg.BaseVM, vmPrefix) {
			return "", fmt.Errorf("base_vm %q is not a name of a local VM of this Mac", cfg.BaseVM)
		}
		vms, err := p.cli.list(ctx)
		if err != nil {
			return "", err
		}
		for _, v := range vms {
			if v.Name == cfg.BaseVM {
				return v.Name, nil
			}
		}
		return "", fmt.Errorf("base_vm %q is not on this Mac", cfg.BaseVM)
	}
	if img.Format != domain.ImageTart || img.OS != domain.OSMacOS || img.Arch != domain.ArchARM64 {
		return "", fmt.Errorf("image %s@%s: a Tart location runs macOS arm64 images in the tart format, not %s %s %s", img.Name, img.Version, img.OS, img.Arch, img.Format)
	}
	keys, err := fleetagent.LoadTrustedKeys(p.cfg.TrustedKeysFile)
	if err != nil {
		return "", err
	}
	if err := images.Verify(img, keys); err != nil {
		return "", err
	}
	ref, err := ociRef(img)
	if err != nil {
		return "", err
	}
	p.fetching.Lock()
	defer p.fetching.Unlock()
	if err := p.cli.pull(ctx, ref); err != nil {
		return "", fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	return ref, nil
}

// pruneCache removes pulled images unused for a while, at most once in
// pruneEvery. A machine is a clone with its own disk, so nothing depends on the
// cache.
func (p *Provider) pruneCache(ctx context.Context) {
	p.mu.Lock()
	due := time.Since(p.lastGC) >= pruneEvery
	if due {
		p.lastGC = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	if !p.fetching.TryLock() {
		p.mu.Lock()
		p.lastGC = time.Time{} // a pull must not hold up a reconcile: next time
		p.mu.Unlock()
		return
	}
	defer p.fetching.Unlock()
	if err := p.cli.prune(ctx, p.cfg.ImageCacheDays); err != nil {
		p.log.Warn("clean the image cache", "error", err)
	}
}

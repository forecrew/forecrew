package tart

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
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

// cloneVM makes the VM name from the shape's image, or base_vm.
//
// For an image: the signature (which covers the digest but not the URI) is checked
// against the keys of this host every time; the URI's host must be one of
// registry_hosts, so that credentials and requests go only where this Mac's
// operator says; and Tart pulls registry/name@digest. Tart itself does not check
// that what it pulled is what the digest names, so the agent does: the manifest
// Tart cached must hash to the signed digest, and the files it cached must match
// the manifest's digests (verifyPulled). A pulled image that cannot be checked is
// refused. The lock on the cache is kept until the clone is made, so that nothing
// prunes or pulls over a checked image, and the files are looked at once more
// right before the clone, so that what is cloned is what was checked.
//
// A shape's base_vm names a VM of the agent's own Tart home instead, when this
// host's operator allows it.
func (p *Provider) cloneVM(ctx context.Context, cfg providerConfig, img domain.Image, name string) error {
	if cfg.BaseVM != "" {
		src, err := p.localSource(ctx, cfg.BaseVM)
		if err != nil {
			return err
		}
		return p.cli.clone(ctx, src, name)
	}
	if img.Format != domain.ImageTart || img.OS != domain.OSMacOS || img.Arch != domain.ArchARM64 {
		return fmt.Errorf("image %s@%s: a Tart location runs macOS arm64 images in the tart format, not %s %s %s", img.Name, img.Version, img.OS, img.Arch, img.Format)
	}
	keys, err := fleetagent.LoadTrustedKeys(p.cfg.TrustedKeysFile)
	if err != nil {
		return err
	}
	if err := images.Verify(img, keys); err != nil {
		return err
	}
	ref, err := ociRef(img)
	if err != nil {
		return err
	}
	if len(p.cfg.RegistryHosts) > 0 && !slices.Contains(p.cfg.RegistryHosts, strings.ToLower(registryOf(ref))) {
		return fmt.Errorf("image %s@%s: its registry %s is not in registry_hosts of tart.yaml", img.Name, img.Version, registryOf(ref))
	}
	repo, _, _ := strings.Cut(ref, "@")
	p.fetching.Lock()
	defer p.fetching.Unlock()
	if err := p.cli.pull(ctx, ref); err != nil {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	fp, err := p.verifyPulled(ctx, repo, img.Digest)
	if err != nil {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, err)
	}
	if p.beforeClone != nil {
		p.beforeClone()
	}
	if now, err := fingerprint(p.cacheDir(repo, img.Digest)); err != nil || now != fp {
		return fmt.Errorf("image %s@%s changed in Tart's cache after it was checked", img.Name, img.Version)
	}
	return p.cli.clone(ctx, ref, name)
}

// localSource finds the VM base_vm names, in the agent's own Tart home.
func (p *Provider) localSource(ctx context.Context, name string) (string, error) {
	if !p.cfg.AllowLocalBase {
		return "", fmt.Errorf("the shape names a local VM (base_vm): set allow_local_base in tart.yaml on this Mac to run unsigned VMs")
	}
	if !localVM.MatchString(name) || strings.HasPrefix(name, vmPrefix) {
		return "", fmt.Errorf("base_vm %q is not a name of a local VM of this Mac", name)
	}
	vms, err := p.cli.list(ctx)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(vms, func(v vmInfo) bool { return v.Name == name }) {
		return "", fmt.Errorf("base_vm %q is not in the agent's Tart home (%s)", name, p.cli.tartHome())
	}
	return name, nil
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
	clear(p.verified) // what comes back is pulled again, and checked again
}

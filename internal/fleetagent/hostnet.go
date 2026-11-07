package fleetagent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/images"

	"crypto/ed25519"
)

// LoadTrustedKeys reads the public keys images must be signed with, one per
// line, from the agent's own file.
func LoadTrustedKeys(path string) ([]ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's file
	if err != nil {
		return nil, fmt.Errorf("trusted image keys: %w", err)
	}
	keys, err := images.ParsePublicKeys(string(raw))
	if err != nil {
		return nil, fmt.Errorf("trusted image keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("trusted image keys: %s lists none", path)
	}
	return keys, nil
}

// ResolveAllow turns entries of network_allow, hosts or CIDRs, into networks,
// for a guest that has IPv4 only.
func ResolveAllow(ctx context.Context, r *net.Resolver, entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range entries {
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Addr().Is4() {
				out = append(out, p.Masked())
			}
			continue
		}
		addrs, err := Lookup4(ctx, r, e)
		if err != nil {
			return nil, fmt.Errorf("network_allow: %w", err)
		}
		for _, a := range addrs {
			out = append(out, netip.PrefixFrom(a, 32))
		}
	}
	return out, nil
}

// Limits of ResolveAvailable: lookups at once, and how long one may take.
// lookup is Lookup4; tests count the lookups through it.
var (
	resolveAtOnce  = 8
	resolveTimeout = 5 * time.Second
	lookup         = Lookup4
)

// ResolveAvailable is ResolveAllow for a long list a location does not own,
// such as an organization's egress: names are looked up at once, and one that
// does not resolve opens nothing. It is returned in missed, and the rest still
// apply, so a registry that is down does not stop every machine. At most
// resolveAtOnce names are looked up at once, each for resolveTimeout, so a
// long list neither floods the resolver nor holds a machine up for long.
func ResolveAvailable(ctx context.Context, r *net.Resolver, entries []string) (nets []netip.Prefix, missed []string) {
	found := make([][]netip.Addr, len(entries))
	failed := make([]bool, len(entries))
	var wg sync.WaitGroup
	slots := make(chan struct{}, resolveAtOnce)
	for i, e := range entries {
		if _, err := netip.ParsePrefix(e); err == nil {
			continue
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			lctx, cancel := context.WithTimeout(ctx, resolveTimeout)
			defer cancel()
			addrs, err := lookup(lctx, r, e)
			found[i], failed[i] = addrs, err != nil
		})
	}
	wg.Wait()
	for i, e := range entries {
		if p, err := netip.ParsePrefix(e); err == nil {
			if p.Addr().Is4() {
				nets = append(nets, p.Masked())
			}
			continue
		}
		if failed[i] {
			missed = append(missed, e)
			continue
		}
		for _, a := range found[i] {
			nets = append(nets, netip.PrefixFrom(a, 32))
		}
	}
	return nets, missed
}

// Lookup4 returns the IPv4 addresses of a host or address, sorted.
func Lookup4(ctx context.Context, r *net.Resolver, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return nil, nil
		}
		return []netip.Addr{a}, nil
	}
	found, err := r.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(found))
	for _, a := range found {
		out = append(out, a.Unmap())
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return out, nil
}

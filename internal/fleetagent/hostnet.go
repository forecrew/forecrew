package fleetagent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

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

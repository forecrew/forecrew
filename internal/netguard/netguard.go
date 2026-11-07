// Package netguard keeps forecrewd and workers from reaching private or
// metadata addresses on behalf of a URL someone typed: a cited page, a
// tracker, a model provider. A name is checked when it is resolved and the
// connection again when it is made, so a name that changes its address in
// between reaches nothing private.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
)

// ErrRefused means an address may not be reached: it is private, or the URL
// is not a plain http(s) one.
var ErrRefused = errors.New("refused")

// blocked are the ranges that are not public beyond what netip.Addr's
// predicates cover.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
}

// Public reports whether a is an address on the public internet.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Local reports whether a is a loopback or private address: where a
// self-hosted model runs. Link-local addresses are not, because cloud
// metadata lives there.
func Local(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate()
}

// metadata are the addresses of cloud metadata services outside link-local
// ranges: Alibaba Cloud's and AWS's IPv6 one.
var metadata = []netip.Addr{netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("fd00:ec2::254")}

// broadcast is the limited broadcast address.
var broadcast = netip.MustParseAddr("255.255.255.255")

// Allowed reports whether a may be reached. Without local, only a public
// address. With local, where an operator lets a server reach its own
// networks (a self-hosted model, a tracker on a tailnet), anything but
// link-local addresses (169.254.0.0/16, fe80::/10), the known metadata
// addresses (100.100.100.200, fd00:ec2::254), multicast, the unspecified and
// the broadcast address: loopback, private, carrier-grade NAT (Tailscale's
// 100.64.0.0/10), 192.0.0.0/24, 198.18.0.0/15 and 240.0.0.0/4 are allowed.
func Allowed(a netip.Addr, local bool) bool {
	if !local {
		return Public(a)
	}
	a = a.Unmap()
	return a.IsValid() && !a.IsLinkLocalUnicast() && !a.IsMulticast() && !a.IsUnspecified() && a != broadcast && !slices.Contains(metadata, a)
}

// Dial returns the Control of a net.Dialer that refuses an address Allowed
// does not allow.
func Dial(local bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: address %s: %w", ErrRefused, address, err)
		}
		if !Allowed(ap.Addr(), local) {
			return fmt.Errorf("%w: %s is not a public address", ErrRefused, ap.Addr())
		}
		return nil
	}
}

// GuardDial refuses a connection to an address that is not public: the
// Control of a net.Dialer.
func GuardDial(network, address string, c syscall.RawConn) error {
	return Dial(false)(network, address, c)
}

// Transport returns a transport that only connects to addresses Allowed
// allows. No proxy: a proxy would make the connection, and the guard would
// see the proxy's address and not the target's.
func Transport(local bool) *http.Transport {
	return &http.Transport{
		DialContext:       (&net.Dialer{Control: Dial(local)}).DialContext,
		DisableKeepAlives: true,
	}
}

// PublicTransport returns a transport that only connects to public
// addresses.
func PublicTransport() http.RoundTripper { return Transport(false) }

// Lookup resolves a host; LookupHost is the system resolver's.
type Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

// LookupHost resolves host with the system resolver.
func LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// AllowHost fails with ErrRefused unless every address of host, an IP or a
// name that lookup resolves, is public. A host that does not resolve is
// another error.
func AllowHost(ctx context.Context, lookup func(ctx context.Context, host string) ([]netip.Addr, error), host string) error {
	return AllowHostLocal(ctx, lookup, host, false)
}

// AllowHostLocal is AllowHost that, with local, also allows loopback and
// private addresses.
func AllowHostLocal(ctx context.Context, lookup func(ctx context.Context, host string) ([]netip.Addr, error), host string, local bool) error {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else if addrs, err = lookup(ctx, host); err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no address", host)
	}
	for _, a := range addrs {
		if !Allowed(a, local) {
			return fmt.Errorf("%w: %s is %s, not a public address", ErrRefused, host, a.Unmap())
		}
	}
	return nil
}

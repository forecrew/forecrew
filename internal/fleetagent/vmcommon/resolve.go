package vmcommon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/forecrew/forecrew/internal/fleetagent"
)

// ResolveServer finds where forecrewd is for a guest: its IPv4 addresses and
// port. A machine on another host or VLAN cannot reach this host's loopback, so
// an address that is one is an error that says what to set.
func ResolveServer(ctx context.Context, r *net.Resolver, serverURL string) ([]netip.Addr, int, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return nil, 0, errors.New("bad server URL")
	}
	addrs, err := fleetagent.Lookup4(ctx, r, u.Hostname())
	if err != nil {
		return nil, 0, fmt.Errorf("resolve the server: %w", err)
	}
	for _, a := range addrs {
		if a.IsLoopback() {
			return nil, 0, fmt.Errorf("the server's address %s is this host's loopback, which a machine cannot reach: set public_base_url to an address machines can reach", a)
		}
	}
	if len(addrs) == 0 {
		return nil, 0, fmt.Errorf("the server %s has no IPv4 address, and machines have IPv4 only", u.Hostname())
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		if port, err = strconv.Atoi(u.Port()); err != nil || port < 1 || port > 65535 {
			return nil, 0, errors.New("bad server URL")
		}
	}
	return addrs, port, nil
}

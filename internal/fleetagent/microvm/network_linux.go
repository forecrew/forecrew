package microvm

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/fleetagent"
)

// tapPrefix starts the name of every machine's tap device.
const tapPrefix = "fcvm"

// network is a machine's own /30: no bridge, so machines have no link to each
// other, only routes through the host, which the rules below refuse.
type network struct {
	Tap   string
	Table string
	Host  netip.Addr
	Guest netip.Addr
	MAC   string
}

// networkOf returns the network of a slot.
func networkOf(subnet netip.Prefix, slot int) network {
	base := binary.BigEndian.Uint32(subnet.Masked().Addr().AsSlice()) + uint32(slot)*4 // #nosec G115 -- a slot is small
	var host, guest [4]byte
	binary.BigEndian.PutUint32(host[:], base+1)
	binary.BigEndian.PutUint32(guest[:], base+2)
	return network{
		Tap: tapPrefix + strconv.Itoa(slot), Table: "forecrew_vm" + strconv.Itoa(slot),
		Host: netip.AddrFrom4(host), Guest: netip.AddrFrom4(guest),
		MAC: fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", guest[0], guest[1], guest[2], guest[3]),
	}
}

// blocked is what a machine may not reach unless allowed: private, shared,
// link-local (metadata endpoints) and other special networks. The host's other
// addresses end in its input chain, which refuses them all.
var blocked = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
}

// allowed is the machine's way past those rules.
type allowed struct {
	// Server is forecrewd, which a machine must reach wherever it runs: its
	// addresses, on its port only.
	Server []netip.Addr
	Port   int
	DNS    netip.Addr
	// Nets are the location's network_allow. They open networks beyond the
	// host, never the host itself or other machines.
	Nets []netip.Prefix
	// Subnet holds every machine and the host's ends of their taps.
	Subnet netip.Prefix
	// Local are the networks the host is connected to, blocked as the
	// private ones are.
	Local []netip.Prefix
}

// rules are the machine's nftables table. A table each makes removing a machine
// one command, and changing its allowlist one atomic file. The allow rules look
// at the original address: forecrewd in a container on this host is reached by a
// DNAT that happens before the forward chain sees the packet.
//
// The host takes from a machine only forecrewd's port and DNS; network_allow is
// for the forward chain alone. Other machines stay closed whatever it says.
func rules(n network, a allowed) string {
	var only strings.Builder // what the machine reaches anyway
	for _, s := range a.Server {
		fmt.Fprintf(&only, "\t\tiifname %q ct original ip daddr %s ct original proto-dst %d accept\n", n.Tap, s, a.Port)
	}
	fmt.Fprintf(&only, "\t\tiifname %q ct original ip daddr %s ct original proto-dst 53 accept\n", n.Tap, a.DNS)
	spoof := fmt.Sprintf("\t\tiifname %q ip saddr != %s drop\n\t\tiifname %q meta nfproto != ipv4 drop\n", n.Tap, n.Guest, n.Tap)
	closed := slices.Clone(blocked)
	for _, p := range a.Local {
		closed = append(closed, p.Masked().String())
	}
	var b strings.Builder
	// Adding the table first makes deleting it work the first time too.
	fmt.Fprintf(&b, "table inet %s {}\ndelete table inet %s\ntable inet %s {\n", n.Table, n.Table, n.Table)
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter - 10; policy accept;\n")
	b.WriteString(spoof + only.String())
	fmt.Fprintf(&b, "\t\tiifname %q drop\n\t}\n", n.Tap)
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter - 10; policy accept;\n")
	b.WriteString(spoof + only.String())
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr %s drop\n", n.Tap, a.Subnet.Masked())
	for _, p := range a.Nets {
		fmt.Fprintf(&b, "\t\tiifname %q ct original ip daddr %s accept\n", n.Tap, p)
	}
	fmt.Fprintf(&b, "\t\tiifname %q ip daddr { %s } drop\n", n.Tap, strings.Join(closed, ", "))
	fmt.Fprintf(&b, "\t\tiifname %q accept\n", n.Tap)
	fmt.Fprintf(&b, "\t\toifname %q ct state established,related accept\n", n.Tap)
	fmt.Fprintf(&b, "\t\toifname %q drop\n\t}\n", n.Tap)
	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname != %q masquerade\n\t}\n}\n", n.Guest, n.Tap)
	return b.String()
}

// localNets are the IPv4 networks of this host's interfaces, but its machines'.
func localNets(subnet netip.Prefix) []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		p, err := netip.ParsePrefix(n.String())
		if err != nil || !p.Addr().Is4() || p.Addr().IsLoopback() || subnet.Contains(p.Addr()) {
			continue
		}
		if p = p.Masked(); !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// applyRules loads the machine's table.
func (p *Provider) applyRules(ctx context.Context, r *record, n network) error {
	nets, err := fleetagent.ResolveAllow(ctx, p.resolver, r.Allow)
	if err != nil {
		return err
	}
	a := allowed{Port: r.Port, DNS: p.dns, Nets: nets, Subnet: p.subnet, Local: p.localNets(p.subnet)}
	for _, s := range r.Server {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("record of %s: %w", r.ID, err)
		}
		a.Server = append(a.Server, addr)
	}
	return p.run(ctx, rules(n, a), "nft", "-f", "-")
}

// addNetwork makes the tap and its rules. The rules come first: a device with no
// rules must not exist for the guest to reach.
func (p *Provider) addNetwork(ctx context.Context, r *record, n network) error {
	if err := p.applyRules(ctx, r, n); err != nil {
		return err
	}
	uid := strconv.Itoa(p.cfg.UIDBase + r.Slot)
	for _, args := range [][]string{
		{"tuntap", "add", n.Tap, "mode", "tap", "user", uid, "group", uid},
		{"addr", "add", n.Host.String() + "/30", "dev", n.Tap},
		{"link", "set", n.Tap, "up"},
	} {
		if err := p.run(ctx, "", "ip", args...); err != nil {
			return err
		}
	}
	return nil
}

// removeNetwork removes what exists of it.
func (p *Provider) removeNetwork(ctx context.Context, n network) error {
	if err := p.run(ctx, fmt.Sprintf("table inet %s {}\ndelete table inet %s\n", n.Table, n.Table), "nft", "-f", "-"); err != nil {
		return err
	}
	if !p.hasDevice(n.Tap) {
		return nil
	}
	return p.run(ctx, "", "ip", "link", "del", n.Tap)
}

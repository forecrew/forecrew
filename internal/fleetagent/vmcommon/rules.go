package vmcommon

import (
	"net"
	"net/netip"
	"slices"
)

// Blocked is what a machine may not reach unless allowed: private, shared,
// link-local (metadata endpoints) and other special networks. It is the list
// the microVM provider uses.
var Blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/3"),
}

// Rule is one line of a machine's outbound rules. A rule that matches decides:
// the first match in the list wins, and what no rule matches is allowed (the
// internet).
type Rule struct {
	Allow bool
	Dest  netip.Prefix
	// Proto is "tcp", "udp" or empty for any; Port 0 is any.
	Proto string
	Port  int
}

// RuleInput is what decides a machine's rules.
type RuleInput struct {
	// Server is forecrewd, which a machine must reach wherever it runs: its
	// addresses, on its port only.
	Server []netip.Addr
	Port   int
	// DNS are the resolvers the machine may use. With none, port 53 is open to
	// any destination, since a resolver on the LAN would otherwise be blocked.
	DNS []netip.Addr
	// Never are what network_allow cannot open, whatever it says, even
	// 0.0.0.0/0: the addresses of the hypervisor host (its management port and
	// its SSH included) and the network that holds the other machines.
	Never []netip.Prefix
	// Allow are the location's network_allow, opened beyond the defaults.
	Allow []netip.Prefix
	// Local are the other networks the host is connected to, blocked as the
	// private ones are, so that network_allow can open them.
	Local []netip.Prefix
}

// Rules returns a machine's outbound rules, in order: forecrewd on its port first,
// then DNS and what network_allow can never open (a resolver that is listed comes
// first, so that it may be the host's own; port 53 to anywhere comes after what
// is never opened), then the allowlist, then
// everything private blocked, and the internet after that. Inbound is closed by
// each provider's own means.
func Rules(in RuleInput) []Rule {
	var out []Rule
	for _, s := range in.Server {
		out = append(out, Rule{Allow: true, Dest: netip.PrefixFrom(s, 32), Proto: "tcp", Port: in.Port})
	}
	any4 := netip.MustParsePrefix("0.0.0.0/0")
	never := func() {
		for _, p := range in.Never {
			out = append(out, Rule{Dest: p.Masked()})
		}
	}
	dns := in.DNS
	if len(dns) == 0 {
		// Port 53 to anywhere would reach the host's own, so what is never opened
		// comes first.
		never()
		out = append(out, Rule{Allow: true, Dest: any4, Proto: "udp", Port: 53}, Rule{Allow: true, Dest: any4, Proto: "tcp", Port: 53})
	}
	for _, d := range dns {
		out = append(out, Rule{Allow: true, Dest: netip.PrefixFrom(d, 32), Proto: "udp", Port: 53},
			Rule{Allow: true, Dest: netip.PrefixFrom(d, 32), Proto: "tcp", Port: 53})
	}
	if len(dns) > 0 {
		never() // a resolver that is listed is allowed even on the host
	}
	for _, p := range in.Allow {
		out = append(out, Rule{Allow: true, Dest: p.Masked()})
	}
	for _, p := range Blocked {
		out = append(out, Rule{Dest: p})
	}
	for _, p := range in.Local {
		out = append(out, Rule{Dest: p.Masked()})
	}
	return out
}

// IfaceAddr is an IPv4 address of one of the host's network interfaces.
type IfaceAddr struct {
	Name   string
	Prefix netip.Prefix
}

// InterfaceAddrs lists the IPv4 addresses of this host's interfaces.
func InterfaceAddrs() []IfaceAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []IfaceAddr
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(n.String()); err == nil && p.Addr().Is4() {
					out = append(out, IfaceAddr{Name: i.Name, Prefix: p})
				}
			}
		}
	}
	return out
}

// HostNets splits the host's addresses for a machine on the interface bridge:
// never are every address of the host itself and the network of the bridge, which
// holds the other machines; local are the other networks it is connected to.
func HostNets(addrs []IfaceAddr, bridge string) (never, local []netip.Prefix) {
	for _, a := range addrs {
		if a.Prefix.Addr().IsLoopback() {
			continue
		}
		never = appendNew(never, netip.PrefixFrom(a.Prefix.Addr(), 32))
		if a.Name == bridge {
			never = appendNew(never, a.Prefix.Masked())
		}
	}
	for _, a := range addrs {
		if p := a.Prefix.Masked(); !a.Prefix.Addr().IsLoopback() && !slices.Contains(never, p) {
			local = appendNew(local, p)
		}
	}
	return never, local
}

func appendNew(s []netip.Prefix, p netip.Prefix) []netip.Prefix {
	if slices.Contains(s, p) {
		return s
	}
	return append(s, p)
}

package vmcommon

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"
)

func TestMetaRoundTrip(t *testing.T) {
	id := uuid.New()
	m := NewMeta(id)
	m.Tags = map[string]string{"forecrew:machine": id.String(), "team": "a"}
	m.Allow, m.Server, m.Port = []string{"10.1.0.0/16"}, []string{"192.0.2.7"}, 8443
	m.Deadline = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	got, ok := ParseMeta(m.Encode())
	if !ok {
		t.Fatal("did not parse")
	}
	if got.ID() != id || got.Tags["team"] != "a" || got.Port != 8443 || !got.Deadline.Equal(m.Deadline) || got.Server[0] != "192.0.2.7" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMetaRefuses(t *testing.T) {
	id := uuid.New().String()
	long := strings.Repeat("x", maxTagText+1)
	tests := map[string]string{
		"empty":         "",
		"text":          "my own VM",
		"wrong marker":  `{"forecrew":"other","machine":"` + id + `"}`,
		"bad id":        `{"forecrew":"forecrew-machine/1","machine":"nope"}`,
		"unknown field": `{"forecrew":"forecrew-machine/1","machine":"` + id + `","extra":1}`,
		"trailing":      `{"forecrew":"forecrew-machine/1","machine":"` + id + `"} {}`,
		"long tag":      `{"forecrew":"forecrew-machine/1","machine":"` + id + `","tags":{"a":"` + long + `"}}`,
		"too big":       `{"forecrew":"forecrew-machine/1","machine":"` + id + `","tags":{"a":"` + strings.Repeat("y", maxMetaBytes) + `"}}`,
		"bad utf8":      "\xff",
	}
	for name, notes := range tests {
		if _, ok := ParseMeta(notes); ok {
			t.Errorf("%s: parsed", name)
		}
	}
	many := NewMeta(uuid.New())
	many.Tags = map[string]string{}
	for i := range maxTags + 1 {
		many.Tags[strings.Repeat("k", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	if _, ok := ParseMeta(many.Encode()); ok {
		t.Error("too many tags parsed")
	}
}

func TestRules(t *testing.T) {
	server := []netip.Addr{netip.MustParseAddr("192.0.2.7")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	rules := Rules(RuleInput{Server: server, Port: 443, DNS: dns, Never: []netip.Prefix{netip.MustParsePrefix("198.51.100.1/32")},
		Allow: []netip.Prefix{netip.MustParsePrefix("10.5.5.9/16")}, Local: []netip.Prefix{netip.MustParsePrefix("203.0.113.4/24")}})
	want := []Rule{
		{true, netip.MustParsePrefix("192.0.2.7/32"), "tcp", 443},
		{true, netip.MustParsePrefix("1.1.1.1/32"), "udp", 53},
		{true, netip.MustParsePrefix("1.1.1.1/32"), "tcp", 53},
		{false, netip.MustParsePrefix("198.51.100.1/32"), "", 0},
		{true, netip.MustParsePrefix("10.5.0.0/16"), "", 0},
	}
	if len(rules) != len(want)+len(Blocked)+1 {
		t.Fatalf("%d rules", len(rules))
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rule %d = %+v, want %+v", i, rules[i], w)
		}
	}
	for _, r := range rules[len(want):] {
		if r.Allow {
			t.Errorf("an allow after the allowlist: %+v", r)
		}
	}
	if last := rules[len(rules)-1]; last.Dest != netip.MustParsePrefix("203.0.113.0/24") {
		t.Errorf("the host's own network is not blocked: %+v", last)
	}
}

func TestRulesWithoutDNSOpensPort53AfterWhatIsNeverOpened(t *testing.T) {
	rules := Rules(RuleInput{Server: []netip.Addr{netip.MustParseAddr("192.0.2.7")}, Port: 80, Never: []netip.Prefix{netip.MustParsePrefix("198.51.100.1/32")}})
	want := []Rule{
		{true, netip.MustParsePrefix("192.0.2.7/32"), "tcp", 80},
		{false, netip.MustParsePrefix("198.51.100.1/32"), "", 0},
		{true, netip.MustParsePrefix("0.0.0.0/0"), "udp", 53},
		{true, netip.MustParsePrefix("0.0.0.0/0"), "tcp", 53},
	}
	for i, w := range want {
		if rules[i] != w {
			t.Errorf("rule %d = %+v, want %+v", i, rules[i], w)
		}
	}
}

func TestResolveServer(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		url  string
		port int
		err  string
	}{
		{"https://192.0.2.7", 443, ""},
		{"http://192.0.2.7", 80, ""},
		{"http://192.0.2.7:8080", 8080, ""},
		{"http://127.0.0.1:8080", 0, "loopback"},
		{"http://[2001:db8::1]", 0, "no IPv4"},
		{"http://192.0.2.7:99999", 0, "bad server URL"},
		{"http://192.0.2.7:x", 0, "bad server URL"},
		{"::bad", 0, "bad server URL"},
		{"http://", 0, "bad server URL"},
	}
	for _, tt := range tests {
		addrs, port, err := ResolveServer(ctx, net.DefaultResolver, tt.url)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: err = %v, want %q", tt.url, err, tt.err)
			}
			continue
		}
		if err != nil || port != tt.port || len(addrs) != 1 {
			t.Errorf("%s: %v %d %v", tt.url, addrs, port, err)
		}
	}
}

func TestResolveServerLookupError(t *testing.T) {
	r := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }}
	if _, _, err := ResolveServer(context.Background(), r, "https://forecrew.invalid"); err == nil || !strings.Contains(err.Error(), "resolve the server") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeadlines(t *testing.T) {
	var d Deadlines
	var fired atomic.Int32
	d.Set("a", time.Now().Add(20*time.Millisecond), func() { fired.Add(1) })
	d.Set("b", time.Now().Add(time.Hour), func() { fired.Add(10) })
	if d.Armed() != 2 {
		t.Fatalf("armed = %d", d.Armed())
	}
	d.Stop("b")
	d.Set("c", time.Now().Add(-time.Second), func() { fired.Add(100) }) // already past
	d.Set("zero", time.Time{}, func() { fired.Add(1000) })
	time.Sleep(150 * time.Millisecond)
	if fired.Load() != 101 {
		t.Fatalf("fired = %d", fired.Load())
	}
	// A new time replaces the old one.
	d.Set("a", time.Now().Add(time.Hour), func() { fired.Add(10000) })
	d.Set("a", time.Time{}, nil)
	if d.Armed() != 1 { // "c" has fired but stays until stopped
		t.Fatalf("armed = %d", d.Armed())
	}
}

// An allowlist of everything opens the internet and nothing of the host: the
// host's addresses and the network of the other machines stay closed, and are
// decided before it.
func TestAllowEverythingNeverOpensTheHost(t *testing.T) {
	addrs := []IfaceAddr{
		{"lo", netip.MustParsePrefix("127.0.0.1/8")},
		{"vmbr0", netip.MustParsePrefix("192.168.1.10/24")},
		{"eno1", netip.MustParsePrefix("203.0.113.9/28")},
		{"tailscale0", netip.MustParsePrefix("100.64.0.5/32")},
	}
	never, local := HostNets(addrs, "vmbr0")
	wantNever := []netip.Prefix{netip.MustParsePrefix("192.168.1.10/32"), netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("203.0.113.9/32"), netip.MustParsePrefix("100.64.0.5/32")}
	if !slices.Equal(never, wantNever) {
		t.Errorf("never = %v", never)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/28")}; !slices.Equal(local, want) {
		t.Errorf("local = %v", local)
	}
	rules := Rules(RuleInput{Server: []netip.Addr{netip.MustParseAddr("192.168.1.10")}, Port: 8443, Never: never, Allow: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, Local: local})
	decide := func(ip string, proto string, port int) bool {
		a := netip.MustParseAddr(ip)
		for _, r := range rules {
			if r.Dest.Contains(a) && (r.Proto == "" || r.Proto == proto) && (r.Port == 0 || r.Port == port) {
				return r.Allow
			}
		}
		return true // the internet
	}
	for _, c := range []struct {
		ip    string
		port  int
		allow bool
	}{
		{"192.168.1.10", 8006, false}, // the host's management port
		{"192.168.1.10", 22, false},
		{"203.0.113.9", 22, false},   // a public address of the host
		{"192.168.1.55", 80, false},  // another machine on the bridge
		{"192.168.1.10", 8443, true}, // forecrewd, on its port, even on the host
		{"203.0.113.3", 80, true},    // another network the host is on: the allowlist may open it, as in a microVM
		{"8.8.8.8", 443, true},
		{"10.9.9.9", 443, true}, // opened by 0.0.0.0/0, as the allowlist says
	} {
		if got := decide(c.ip, "tcp", c.port); got != c.allow {
			t.Errorf("%s:%d allowed = %v, want %v", c.ip, c.port, got, c.allow)
		}
	}
}

func TestInterfaceAddrs(t *testing.T) {
	for _, a := range InterfaceAddrs() {
		if !a.Prefix.Addr().Is4() || a.Name == "" {
			t.Errorf("addr %+v", a)
		}
	}
}

package microvm

import (
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNetworkOf(t *testing.T) {
	n := networkOf(mustPrefix("172.30.0.0/16"), 65)
	if n.Tap != "fcvm65" || n.Table != "forecrew_vm65" || n.Host.String() != "172.30.1.5" || n.Guest.String() != "172.30.1.6" || n.MAC != "06:00:ac:1e:01:06" {
		t.Errorf("network = %+v", n)
	}
}

func testAllowed() allowed {
	return allowed{Server: []netip.Addr{netip.MustParseAddr("192.168.1.5")}, Port: 8080, DNS: netip.MustParseAddr("1.1.1.1"),
		Nets: []netip.Prefix{mustPrefix("10.0.0.0/8")}, Subnet: mustPrefix("172.30.0.0/16"), Local: []netip.Prefix{mustPrefix("198.51.100.7/24")}}
}

func TestRules(t *testing.T) {
	r := rules(networkOf(mustPrefix("172.30.0.0/16"), 0), testAllowed())
	for _, want := range []string{
		`iifname "fcvm0" ip saddr != 172.30.0.2 drop`, `iifname "fcvm0" meta nfproto != ipv4 drop`,
		"ct original ip daddr 192.168.1.5 ct original proto-dst 8080 accept", "ct original ip daddr 10.0.0.0/8 accept",
		"169.254.0.0/16", "198.51.100.0/24", `iifname "fcvm0" ip daddr 172.30.0.0/16 drop`, `oifname "fcvm0" drop`, "masquerade",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("no %q in\n%s", want, r)
		}
	}
	if strings.Index(r, "10.0.0.0/8 accept") > strings.Index(r, "ip daddr {") {
		t.Error("the allowlist comes after the blocked networks")
	}
	// The host takes forecrewd and DNS only: the allowlist is not in input.
	input := r[strings.Index(r, "chain input"):strings.Index(r, "chain forward")]
	if strings.Contains(input, "10.0.0.0/8") {
		t.Errorf("the allowlist opens the host:\n%s", input)
	}
	if strings.Index(r, "172.30.0.0/16 drop") > strings.Index(r, "10.0.0.0/8 accept") {
		t.Error("the allowlist comes before the machines' subnet")
	}
}

// The rules load into a real nftables, in a network namespace of the test's own.
func TestRulesLoad(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("no nft")
	}
	if err := exec.Command("unshare", "-Urn", "nft", "list", "ruleset").Run(); err != nil {
		t.Skip("no user and network namespaces here")
	}
	dir := t.TempDir()
	n := networkOf(mustPrefix("172.30.0.0/16"), 3)
	text := rules(n, testAllowed())
	file, del := filepath.Join(dir, "rules.nft"), filepath.Join(dir, "delete.nft")
	if err := os.WriteFile(file, []byte(text+text), 0o600); err != nil { // twice: loading again replaces
		t.Fatal(err)
	}
	if err := os.WriteFile(del, []byte("table inet forecrew_vm3 {}\ndelete table inet forecrew_vm3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("unshare", "-Urn", "sh", "-c", `nft -f "$0" && nft list tables && nft -f "$1" && echo deleted && nft list tables`, file, del).CombinedOutput() // #nosec G204 -- a test
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, text)
	}
	if strings.Count(string(out), "table inet forecrew_vm3") != 1 || !strings.HasSuffix(strings.TrimSpace(string(out)), "deleted") {
		t.Errorf("tables = %s", out)
	}
}

// TestIsolation builds a host with two machines, a LAN and the internet in
// namespaces of the test's own, loads the rules of both machines and tries
// every way out of machine 0.
func TestIsolation(t *testing.T) {
	for _, c := range []string{"nft", "python3", "nsenter", "unshare"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
	if err := exec.Command("unshare", "-Urnm", "-pf", "--mount-proc", "true").Run(); err != nil {
		t.Skip("no user, network, mount and PID namespaces here")
	}
	closed := map[string]bool{
		"internet": false, "lan": true, "forecrewd": false, "host-lan-other-port": true, "host-public-other-port": true,
		"own-tap": true, "other-tap": true, "other-machine": true, "host-ipv6": true,
	}
	with := func(changes map[string]bool) map[string]bool {
		out := maps.Clone(closed)
		maps.Copy(out, changes)
		return out
	}
	subnet := mustPrefix("172.30.0.0/16")
	for name, c := range map[string]struct {
		nets []netip.Prefix
		want map[string]bool // true: blocked
	}{
		"default": {nil, closed},
		// The LAN opens, the host does not.
		"lan allowed": {[]netip.Prefix{mustPrefix("192.168.50.0/24")}, with(map[string]bool{"lan": false})},
		// The machines stay apart, whatever the allowlist says.
		"subnet allowed": {[]netip.Prefix{subnet}, closed},
		"everything":     {[]netip.Prefix{mustPrefix("0.0.0.0/0")}, with(map[string]bool{"lan": false})},
	} {
		t.Run(name, func(t *testing.T) {
			got, out := runLab(t, c.nets, subnet)
			for what, blocked := range c.want {
				if g, ok := got[what]; !ok || g != blocked {
					t.Errorf("%s: blocked = %v, want %v", what, g, blocked)
				}
			}
			if t.Failed() {
				t.Logf("lab:\n%s", out)
			}
		})
	}
}

// runLab loads the rules of machines 0 and 1 with nets allowed into the lab,
// and returns what machine 0 found blocked.
func runLab(t *testing.T, nets []netip.Prefix, subnet netip.Prefix) (map[string]bool, string) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for slot := range 2 {
		a := allowed{Server: []netip.Addr{netip.MustParseAddr("192.168.50.10")}, Port: 8080, DNS: netip.MustParseAddr("1.1.1.1"),
			Nets: nets, Subnet: subnet, Local: []netip.Prefix{mustPrefix("192.168.50.0/24")}}
		f := filepath.Join(dir, "rules"+strconv.Itoa(slot))
		if err := os.WriteFile(f, []byte(rules(networkOf(subnet, slot), a)), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	out, err := exec.Command("unshare", "-Urnm", "-pf", "--mount-proc", "bash", "testdata/lab.sh", files[0], files[1]).CombinedOutput() // #nosec G204 -- a test
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if status, what, ok := strings.Cut(strings.TrimSpace(line), " "); ok && (status == "REACH" || status == "blocked") {
			got[what] = status == "blocked"
		}
	}
	return got, string(out)
}

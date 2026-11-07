package microvm

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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
	return allowed{Server: []netip.Addr{netip.MustParseAddr("192.168.1.5")}, Port: 8080, DNS: netip.MustParseAddr("1.1.1.1"), Nets: []netip.Prefix{mustPrefix("10.0.0.0/8")}}
}

func TestRules(t *testing.T) {
	r := rules(networkOf(mustPrefix("172.30.0.0/16"), 0), testAllowed())
	for _, want := range []string{
		`iifname "fcvm0" ip saddr != 172.30.0.2 drop`, `iifname "fcvm0" meta nfproto != ipv4 drop`,
		"ct original ip daddr 192.168.1.5 ct original proto-dst 8080 accept", "ct original ip daddr 10.0.0.0/8 accept",
		"169.254.0.0/16", `oifname "fcvm0" drop`, "masquerade",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("no %q in\n%s", want, r)
		}
	}
	if strings.Index(r, "10.0.0.0/8 accept") > strings.Index(r, "ip daddr {") {
		t.Error("the allowlist comes after the blocked networks")
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

func TestResolve(t *testing.T) {
	got, err := resolve(t.Context(), net.DefaultResolver, []string{"10.0.0.5/8", "192.0.2.1", "2001:db8::/32", "::1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if !strings.HasPrefix(strings.Join(s, " "), "10.0.0.0/8 192.0.2.1/32 127.0.0.1/32") {
		t.Errorf("resolved = %v", s)
	}
	if _, err := resolve(t.Context(), net.DefaultResolver, []string{"nothing.invalid"}); err == nil {
		t.Error("a name that does not resolve was accepted")
	}
}

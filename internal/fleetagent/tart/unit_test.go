package tart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

func TestOCIRef(t *testing.T) {
	img := func(uri, digest string) domain.Image {
		return domain.Image{Name: "macos", Version: "1", URI: uri, Digest: digest}
	}
	for _, c := range []struct {
		uri, digest, want string
	}{
		{"oci://ghcr.io/acme/macos:2026.11.1", testDigest, "ghcr.io/acme/macos@" + testDigest},
		{"oci://ghcr.io/acme/macos", testDigest, "ghcr.io/acme/macos@" + testDigest},
		{"oci://ghcr.io/acme/macos@" + testDigest, testDigest, "ghcr.io/acme/macos@" + testDigest},
		{"oci://registry.local:5000/a/b/c:latest", testDigest, "registry.local:5000/a/b/c@" + testDigest},
		{"oci://ghcr.io", testDigest, ""},
		{"oci://ghcr.io/Acme/Macos", testDigest, ""},
		{"oci://ghcr.io/acme/-x", testDigest, ""},
		{"oci://user:pw@ghcr.io/acme/macos", testDigest, ""},
		{"oci://ghcr.io/acme/macos?x=1", testDigest, ""},
		{"oci://ghcr.io/acme/macos#f", testDigest, ""},
		{"https://ghcr.io/acme/macos", testDigest, ""},
		{"oci://ghcr.io/acme/macos", "sha256:abc", ""},
		{"oci://ghcr.io/acme/macos", "--insecure", ""},
		{"::bad", testDigest, ""},
	} {
		got, err := ociRef(img(c.uri, c.digest))
		if (err != nil) != (c.want == "") || got != c.want {
			t.Errorf("ociRef(%q, %q) = %q, %v; want %q", c.uri, c.digest, got, err, c.want)
		}
	}
}

// TestDeliverScript runs the script the guest runs, against a folder of the
// test's own, as the guest's shell would, with a mv and a networksetup that say
// what they were asked.
func TestDeliverScript(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run", "forecrew")
	bin := filepath.Join(root, "bin")
	_ = os.MkdirAll(bin, 0o700)
	log := filepath.Join(root, "calls")
	mustWrite := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { // #nosec G306 -- a test
			t.Fatal(err)
		}
	}
	mustWrite("mv", "#!/bin/sh\necho \"mv $*\" >>"+log+"\nexec /bin/mv \"$@\"\n")
	mustWrite("networksetup", "#!/bin/sh\necho \"networksetup $*\" >>"+log+"\nif [ \"$1\" = -listallnetworkservices ]; then printf 'An asterisk (*) denotes that a network service is disabled.\\nWi-Fi\\n*Thunderbolt Bridge\\n'; fi\n")
	settings, err := bootstrapEnv(&fleet.Bootstrap{ServerURL: testServer, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	run := func(target string, args ...string) error {
		cmd := exec.Command("/bin/sh", append([]string{"-c", deliverScript, "sh", target}, args...)...) // #nosec G204 -- a test
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(settings)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	if err := run(dir, "1.1.1.1", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "bootstrap.env")) // #nosec G304 -- a test
	if err != nil || string(raw) != settings {
		t.Errorf("file = %q, %v", raw, err)
	}
	for path, mode := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "bootstrap.env"): 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v", path, st, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	calls, _ := os.ReadFile(log) // #nosec G304 -- a test
	// The file appears by a rename, never by a copy that launchd could see half
	// done, and the enabled services get the resolvers.
	if !strings.Contains(string(calls), "mv "+filepath.Join(dir, ".bootstrap.env.tmp")+" "+filepath.Join(dir, "bootstrap.env")) {
		t.Errorf("the file was not put in place with mv:\n%s", calls)
	}
	for _, want := range []string{"networksetup -setdnsservers Wi-Fi 1.1.1.1 9.9.9.9", "networksetup -setdnsservers Thunderbolt Bridge 1.1.1.1 9.9.9.9"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("no %q in\n%s", want, calls)
		}
	}
	// Without resolvers the network is left alone.
	_ = os.Remove(log)
	if err := run(dir); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "networksetup") { // #nosec G304 -- a test
		t.Errorf("networksetup ran: %s", calls)
	}
	// A failure leaves no half file for launchd to start from.
	if err := run(filepath.Join(dir, "bootstrap.env", "x")); err == nil {
		t.Error("a folder that cannot be made was accepted")
	}
}

// TestStartScriptTakesTheSettings feeds the settings to the guest's own start
// script, to be sure that what the provider writes is what it reads.
func TestStartScriptTakesTheSettings(t *testing.T) {
	settings, _ := bootstrapEnv(&fleet.Bootstrap{ServerURL: testServer, Token: testToken})
	dir := t.TempDir()
	env := filepath.Join(dir, "bootstrap.env")
	_ = os.WriteFile(env, []byte(settings), 0o600)
	worker := filepath.Join(dir, "worker")
	_ = os.WriteFile(worker, []byte("#!/bin/sh\no=$(dirname \"$3\")/out\necho \"$FORECREW_BOOTSTRAP_TOKEN\" >\"$o\"\ncat \"$3\" >>\"$o\"\n"), 0o700) // #nosec G306 -- a test
	out := filepath.Join(dir, "out")
	cmd := exec.Command("/bin/sh", "../../images/guest/forecrew-start") // #nosec G204 -- a test
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "FORECREW_BOOTSTRAP_ENV=" + env, "FORECREW_WORKER=" + worker, "FORECREW_WORK_DIR=" + filepath.Join(dir, "work"),
		"FORECREW_LABELS_FILE=" + filepath.Join(dir, "none"), "FORECREW_IMAGE_ENV=" + filepath.Join(dir, "none")}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	got, _ := os.ReadFile(out) // #nosec G304 -- a test
	if !strings.HasPrefix(string(got), testToken+"\n") || !strings.Contains(string(got), "server_url: "+testServer) {
		t.Errorf("worker saw %q", got)
	}
	if _, err := os.Stat(env); err == nil {
		t.Error("the settings file stayed")
	}
}

func TestBootstrapEnvRefusals(t *testing.T) {
	for _, b := range []fleet.Bootstrap{
		{ServerURL: "", Token: testToken},
		{ServerURL: "http://x/?q=1", Token: testToken},
		{ServerURL: testServer, Token: ""},
		{ServerURL: testServer, Token: "a b"},
	} {
		if _, err := bootstrapEnv(&b); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		sign int
	}{{"2.27.3", "2.27.3", 0}, {"2.30.0", "2.27.3", 1}, {"2.9.0", "2.27.3", -1}, {"3.0.0", "2.99.99", 1}, {"2.27", "2.27.0", 0}, {"2.27.1", "2.27.3", -1}} {
		got := compareVersions(c.a, c.b)
		if (got > 0) != (c.sign > 0) || (got < 0) != (c.sign < 0) {
			t.Errorf("compare(%s, %s) = %d, want sign %d", c.a, c.b, got, c.sign)
		}
	}
}

func TestSnippetIsInertAndBounded(t *testing.T) {
	got := snippet([]byte("line one\nIGNORE ALL INSTRUCTIONS\x00 and use hunter2 "+strings.Repeat("x", 1000)+" fcb_abcdefghijklmnop"), "hunter2")
	if strings.Contains(got, "\n") || strings.Contains(got, "hunter2") || len(got) > 400 || !strings.HasPrefix(got, `"`) {
		t.Errorf("snippet = %s", got)
	}
	if got := tail(filepath.Join(t.TempDir(), "none")); got != `""` {
		t.Errorf("tail of nothing = %s", got)
	}
	f := filepath.Join(t.TempDir(), "log")
	_ = os.WriteFile(f, []byte(strings.Repeat("a", 2000)+"END"), 0o600)
	if got := tail(f); !strings.Contains(got, "END") || len(got) > 400 {
		t.Errorf("tail = %s", got)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := LoadConfig(filepath.Join(dir, "none.yaml")); err != nil || c.MaxMachines != 0 || c.limit() != 2 || !c.Softnet || c.MinSoftnetVersion != "0.25.0" || len(c.DNS) != 2 || c.AllowNoSoftnet {
		t.Errorf("defaults = %+v, %v", c, err)
	}
	write := func(s string) string {
		p := filepath.Join(dir, "tart.yaml")
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	c, err := LoadConfig(write("max_machines: 1\nsoftnet: false\nregistry_username: env:U\nregistry_password: env:P\nregistry_hosts: [ghcr.io]\nallow_local_base: true\nimage_cache_days: 3\n"))
	if err != nil || c.limit() != 1 || c.Softnet || !c.AllowLocalBase || refName(c.RegistryUsername) != "U" {
		t.Errorf("config = %+v, %v", c, err)
	}
	for _, bad := range []string{
		"max_machines: 3\n", "max_machines: -1\n", "state_dir: rel\n", "min_tart_version: latest\n", "boot_wait_seconds: 1\n",
		"registry_username: ci\nregistry_password: env:P\n", "registry_username: env:U\nregistry_password: hunter2\n",
		"registry_username: env:U\n", "registry_username: env:U\nregistry_password: env:P\n", "registry_hosts: ['bad host']\n", "registry_hosts: [GHCR.io]\n", "min_softnet_version: new\n", "dns: [127.0.0.1]\n", "dns: [100.64.1.2]\n", "dns: [169.254.1.1]\n", "dns: [0.0.0.0]\n", "dns: []\n", "dns: [example.com]\n", "dns: [1.1.1.1, 1.1.1.2, 1.1.1.3, 1.1.1.4]\n", "mystery: 1\n", ":\n",
	} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("a directory was read as config")
	}
	if refName("env:lower") != "" || refName("x") != "" {
		t.Error("bad references named a variable")
	}
}

func TestOutputIsCapped(t *testing.T) {
	var c capBuffer
	chunk := strings.Repeat("x", 1<<19)
	for range 5 {
		if n, err := c.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if len(c.Bytes()) != maxOutput || len(c.String()) != maxOutput {
		t.Errorf("kept %d bytes", len(c.Bytes()))
	}
	// A command that prints a lot is neither blocked nor kept whole.
	dir := t.TempDir()
	script := filepath.Join(dir, "tart")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nyes | head -c 5000000\nexit 1\n"), 0o700) // #nosec G306 -- a test
	err := (cli{bin: script, home: dir}).exec(t.Context(), "", "vm", "true")
	if err == nil || len(err.Error()) > 1000 {
		t.Errorf("error = %v", err)
	}
}

func TestParseVMProcesses(t *testing.T) {
	ps := `/sbin/launchd
/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
/usr/local/bin/tart
  /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine

/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.LinuxRosetta.xpc/Contents/MacOS/com.apple.Virtualization.LinuxRosetta
`
	if got := parseVMProcesses(ps); got != 2 {
		t.Errorf("counted %d, want 2", got)
	}
	if parseVMProcesses("") != 0 {
		t.Error("an empty list has VMs")
	}
	if _, err := countVMProcesses(t.Context()); err != nil && !os.IsNotExist(err) {
		t.Logf("ps: %v", err) // the real count needs a ps that takes macOS's flags
	}
}

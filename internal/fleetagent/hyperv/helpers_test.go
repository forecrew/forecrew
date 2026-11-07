package hyperv

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent/imagecache"
	"github.com/forecrew/forecrew/internal/fleetagent/vmcommon"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/testkit"
)

const diskBody = "a windows disk"

type rig struct {
	t     *testing.T
	ps    *testkit.FakePowerShell
	p     *Provider
	cfg   Config
	key   ed25519.PrivateKey
	img   domain.Image
	keys  string
	base  string
	fetch *int
}

// newRig starts a fake host and a provider that trusts one key and fetches a
// signed image from a local server into a directory of the test.
func newRig(t *testing.T) *rig {
	t.Helper()
	fetched := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched++
		_, _ = w.Write([]byte(diskBody))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	pub, key, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ps: testkit.NewFakePowerShell(), key: key, keys: filepath.Join(dir, "keys.pub"), fetch: &fetched}
	if err := os.WriteFile(r.keys, []byte(images.FormatPublicKey(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := images.Digest(strings.NewReader(diskBody))
	if err != nil {
		t.Fatal(err)
	}
	r.img = domain.Image{ID: uuid.New(), Name: "forecrew-windows", Version: "2026.11.1", OS: domain.OSWindows, Arch: domain.ArchAMD64, Format: domain.ImageVHDX,
		URI: srv.URL + "/windows.vhdx", Digest: digest, BuiltAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	r.img.Signature = images.Sign(key, r.img)
	r.cfg = Config{StateDir: `C:\FC`, Switch: "Lab", Generation: 1, MaxMachines: 3, PowerShell: "powershell.exe", ImageCacheDays: 14, ScriptTimeout: time.Minute, TrustedKeysFile: r.keys}
	r.rebuild()
	r.p.cache = imagecache.New(filepath.Join(dir, "images"), r.keys, ".vhdx", domain.ImageVHDX, nil)
	r.base, _ = r.p.cache.Path(digest)
	r.ps.Bases[r.base] = 60
	return r
}

func (r *rig) rebuild() {
	r.t.Helper()
	cache := (*imagecache.Cache)(nil)
	if r.p != nil {
		cache = r.p.cache
	}
	p, err := New(r.cfg, r.ps, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if cache != nil {
		p.cache = cache
	}
	// The host this runs on, whatever it is: an adapter on the machines' switch and
	// one on a public network.
	p.ifaces = func() []vmcommon.IfaceAddr {
		return []vmcommon.IfaceAddr{
			{Name: "Loopback", Prefix: netip.MustParsePrefix("127.0.0.1/8")},
			{Name: "vEthernet (Lab)", Prefix: netip.MustParsePrefix("192.168.1.10/24")},
			{Name: "Ethernet", Prefix: netip.MustParsePrefix("203.0.113.9/28")},
		}
	}
	r.p = p
}

func (r *rig) request() fleet.CreateRequest {
	id := uuid.New()
	return fleet.CreateRequest{
		MachineID: id,
		Shape:     domain.Shape{Name: "windows-medium", OS: domain.OSWindows, Arch: domain.ArchAMD64, CPU: 4, MemoryMB: 8192, DiskGB: 80, ProviderConfig: map[string]any{}},
		Image:     r.img,
		Bootstrap: &fleet.Bootstrap{ServerURL: "https://192.0.2.7", Token: "fcb_AAAAbbbb1234-secret_token"},
		Tags:      map[string]string{fleet.TagOrg: uuid.New().String(), "site": "lab"},
	}
}

func (r *rig) create(req fleet.CreateRequest) fleet.Instance {
	r.t.Helper()
	inst, err := r.p.Create(context.Background(), req)
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	return inst
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting")
}

func failScript(script, msg string) func(string) string {
	return func(s string) string {
		if s == script {
			return msg
		}
		return ""
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

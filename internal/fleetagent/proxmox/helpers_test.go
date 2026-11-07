package proxmox

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/testkit"
)

const tplID = 9000

type rig struct {
	t    *testing.T
	pve  *testkit.FakeProxmox
	p    *Provider
	cfg  Config
	key  ed25519.PrivateKey
	img  domain.Image
	dir  string
	keys string
}

// newRig starts a fake cluster with a template made from a signed image, and a
// provider that trusts the fake's certificate.
func newRig(t *testing.T) *rig {
	t.Helper()
	pve := testkit.NewFakeProxmox(t)
	dir := t.TempDir()
	pub, key, err := images.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, pve: pve, key: key, dir: dir, keys: filepath.Join(dir, "keys.pub")}
	write(t, r.keys, images.FormatPublicKey(pub)+"\n", 0o600)
	write(t, filepath.Join(dir, "token"), pve.Token+"\n", 0o600)
	write(t, filepath.Join(dir, "ca.pem"), string(pve.CAPEM()), 0o644)
	r.cfg = Config{
		APIURL: pve.URL(), TokenFile: filepath.Join(dir, "token"), CAFile: filepath.Join(dir, "ca.pem"), TrustedKeysFile: r.keys,
		Nodes: []string{"pve"}, Bridge: "vmbr0", MaxMachines: 4, PollInterval: time.Millisecond,
	}
	r.img = domain.Image{ID: uuid.New(), Name: "forecrew-linux", Version: "2026.11.1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageQCOW2,
		URI: "https://images.example.com/linux.qcow2", Digest: "sha256:" + strings.Repeat("ab", 32), BuiltAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	r.img.Signature = images.Sign(key, r.img)
	pve.AddTemplate(tplID, "pve", imageTag(r.img), nil)
	r.rebuild()
	return r
}

func (r *rig) rebuild() {
	r.t.Helper()
	p, err := New(r.cfg, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	// Names are not looked up in the DNS of the machine the tests run on.
	p.resolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }}
	r.p = p
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// request is a machine of the rig's image.
func (r *rig) request() fleet.CreateRequest {
	id := uuid.New()
	return fleet.CreateRequest{
		MachineID: id,
		Shape: domain.Shape{Name: "linux-small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 40, BootTimeoutSeconds: 5,
			ProviderConfig: map[string]any{"node": "pve", "template": tplID}},
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

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func asAPI(err error, target **APIError) bool { return errors.As(err, target) }

func testkitPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

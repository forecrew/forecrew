package images

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewManifest(t *testing.T) {
	disk, kernel := writeFile(t, "root.ext4", "rootfs"), writeFile(t, "vmlinux", "kernel")
	built := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	good := Build{
		Name: "linux", Version: "2026.05.1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageRootfs,
		File: disk, URI: "https://images.example.com/linux.ext4", Kernel: kernel, KernelURI: "https://images.example.com/vmlinux",
		Labels: []string{"Docker", "runtime:codex", "docker"}, Packages: []domain.ImagePackage{{Name: "git", Version: "2.50"}}, BuiltAt: built,
	}
	m, err := NewManifest(good)
	if err != nil {
		t.Fatal(err)
	}
	wantDisk, _ := FileDigest(disk)
	if m.Digest != wantDisk || m.Contents.Kernel == nil || m.Contents.Kernel.Digest == wantDisk || m.Signed() ||
		strings.Join(m.Contents.Labels, ",") != "docker,runtime:codex" {
		t.Fatalf("manifest = %+v", m)
	}

	// Sign, encode, parse and register: the same image comes back.
	_, key, _ := GenerateKey()
	signed := m.SignedWith(key)
	data, err := signed.Encode()
	if err != nil || !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("Encode: %q, %v", data, err)
	}
	back, err := ParseManifest(data)
	if err != nil || back.Validate() != nil || !back.Signed() {
		t.Fatalf("ParseManifest: %+v, %v", back, err)
	}
	account := domain.NewID()
	img := back.Image(account)
	if img.AccountID != account || img.Validate() != nil || ManifestOf(img).Digest != m.Digest {
		t.Fatalf("image = %+v", img)
	}
	if err := Verify(img, nil); err == nil {
		t.Fatal("verified with no keys")
	}
	if img2 := m.Image(uuid.Nil()); img2.Validate() == nil {
		t.Fatal("an unsigned manifest made a valid image")
	}

	for name, mutate := range map[string]func(*Build){
		"no file or digest":    func(b *Build) { b.File = "" },
		"both file and digest": func(b *Build) { b.Digest = "sha256:" + strings.Repeat("a", 64) },
		"missing file":         func(b *Build) { b.File = filepath.Join(t.TempDir(), "nope") },
		"missing kernel":       func(b *Build) { b.Kernel = filepath.Join(t.TempDir(), "nope") },
		"no kernel on rootfs":  func(b *Build) { b.Kernel = "" },
		"bad name":             func(b *Build) { b.Name = "Not Valid" },
		"no build time":        func(b *Build) { b.BuiltAt = time.Time{} },
	} {
		b := good
		mutate(&b)
		if _, err := NewManifest(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A digest of the build's manifest stands in for the file of an AMI.
	ami := Build{Name: "linux", Version: "1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageAMI,
		URI: "ami:eu-west-1/ami-0123456789abcdef0", Digest: "sha256:" + strings.Repeat("a", 64), BuiltAt: built}
	if _, err := NewManifest(ami); err != nil {
		t.Fatalf("ami: %v", err)
	}

	for _, bad := range []string{"", "{", `{"name":"x","bogus":1}`, string(data) + `{}`} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("ParseManifest(%q) accepted", bad)
		}
	}
}

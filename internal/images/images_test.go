package images

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

func signedImage(t *testing.T, path string, key ed25519.PrivateKey) domain.Image {
	t.Helper()
	digest, err := FileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	img := domain.Image{
		Name: "linux", Version: "1.0.0", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageQCOW2,
		URI: "https://images.example.com/linux.qcow2", Digest: digest, BuiltAt: time.Now().UTC(),
	}
	img.Signature = Sign(key, img)
	if err := img.Validate(); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestVerifyFile(t *testing.T) {
	pub, key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, _ := GenerateKey()
	path := filepath.Join(t.TempDir(), "linux.qcow2")
	if err := os.WriteFile(path, []byte("disk contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	img := signedImage(t, path, key)

	tampered := img
	tampered.Version = "1.0.1" // a signature fits one image only
	forged := img
	forged.Signature = "ed25519:" + strings.Repeat("A", 86) + "=="
	unsigned := img
	unsigned.Signature = ""
	badBase64 := img
	badBase64.Signature = "ed25519:!!!"

	for _, tt := range []struct {
		name string
		img  domain.Image
		keys []ed25519.PublicKey
		want error
	}{
		{"trusted key", img, []ed25519.PublicKey{otherPub, pub}, nil},
		{"unknown key", img, []ed25519.PublicKey{otherPub}, ErrSignature},
		{"no keys", img, nil, ErrSignature},
		{"another version", tampered, []ed25519.PublicKey{pub}, ErrSignature},
		{"forged signature", forged, []ed25519.PublicKey{pub}, ErrSignature},
		{"no signature", unsigned, []ed25519.PublicKey{pub}, ErrSignature},
		{"bad encoding", badBase64, []ed25519.PublicKey{pub}, ErrSignature},
		{"short key is ignored", img, []ed25519.PublicKey{{1, 2}}, ErrSignature},
	} {
		if err := VerifyFile(tt.img, path, tt.keys); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	// A tampered file fails on its digest, though the manifest is untouched.
	if err := os.WriteFile(path, []byte("disk contents, changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(img, path, []ed25519.PublicKey{pub}); !errors.Is(err, ErrDigest) {
		t.Fatalf("tampered file: %v", err)
	}
	if err := VerifyFile(img, filepath.Join(t.TempDir(), "missing"), []ed25519.PublicKey{pub}); err == nil {
		t.Fatal("missing file verified")
	}
	if _, err := FileDigest(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("digest of a missing file")
	}
}

func TestKeys(t *testing.T) {
	pub, key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePublicKey(" " + FormatPublicKey(pub) + "\n")
	if err != nil || !got.Equal(pub) {
		t.Fatalf("public key round trip: %v, %v", got, err)
	}
	for _, bad := range []string{"", "rsa:AAAA", "ed25519:!!!", "ed25519:AAAA"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("ParsePublicKey(%q) accepted", bad)
		}
	}
	list := "# project key\n" + FormatPublicKey(pub) + "\n\n" + FormatPublicKey(pub) + "\n"
	if keys, err := ParsePublicKeys(list); err != nil || len(keys) != 2 {
		t.Fatalf("ParsePublicKeys = %v, %v", keys, err)
	}
	if _, err := ParsePublicKeys("nope\n"); err == nil {
		t.Fatal("bad list accepted")
	}

	path := filepath.Join(t.TempDir(), "image.key")
	if err := WritePrivateKey(path, key); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	if err := WritePrivateKey(path, key); err == nil {
		t.Fatal("a key file was replaced")
	}
	back, err := ReadPrivateKey(path)
	if err != nil || !back.Equal(key) {
		t.Fatalf("private key round trip: %v", err)
	}
	if _, err := ReadPrivateKey(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing key read")
	}
	for _, bad := range []string{"", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"} {
		if _, err := ParsePrivateKey([]byte(bad)); err == nil {
			t.Errorf("ParsePrivateKey(%q) accepted", bad)
		}
	}
}

func TestSignatureCoversKernelAndBuildTime(t *testing.T) {
	pub, key, _ := GenerateKey()
	kernel := writeFile(t, "vmlinux", "kernel")
	kd, _ := FileDigest(kernel)
	img := testkit.TestImage(uuid.Nil(), "linux", "1", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	img.Contents.Kernel.Digest = kd
	img.Signature = Sign(key, img)
	keys := []ed25519.PublicKey{pub}
	if err := Verify(img, keys); err != nil {
		t.Fatal(err)
	}
	if err := VerifyKernel(img, kernel); err != nil {
		t.Fatalf("right kernel: %v", err)
	}

	// An organization admin swaps the kernel under an instance image: the
	// manifest still names a kernel, but not the one that was signed.
	swapped := img
	swapped.Contents.Kernel = &domain.ImageKernel{URI: img.Contents.Kernel.URI, Digest: "sha256:" + strings.Repeat("ee", 32)}
	if err := Verify(swapped, keys); !errors.Is(err, ErrSignature) {
		t.Errorf("swapped kernel digest: %v", err)
	}
	// Or leaves the digest and serves another file.
	other := writeFile(t, "other", "another kernel")
	if err := VerifyKernel(img, other); !errors.Is(err, ErrDigest) {
		t.Errorf("another kernel file: %v", err)
	}
	// A re-dated build is another signature.
	redated := img
	redated.BuiltAt = img.BuiltAt.Add(48 * time.Hour)
	if err := Verify(redated, keys); !errors.Is(err, ErrSignature) {
		t.Errorf("re-dated build: %v", err)
	}
	// Sub-second precision, which the database drops, does not matter.
	precise := img
	precise.BuiltAt = img.BuiltAt.Add(123456789)
	if err := Verify(precise, keys); err != nil {
		t.Errorf("sub-second build time: %v", err)
	}
	noKernel := img
	noKernel.Contents.Kernel = nil
	if err := VerifyKernel(noKernel, kernel); !errors.Is(err, ErrDigest) {
		t.Errorf("no kernel: %v", err)
	}
	if err := VerifyKernel(img, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing kernel verified")
	}
}

// Package images is the machine image pipeline (docs/contracts/fleet.md,
// Images): signing and checking images, the manifest a build leaves, the
// retention rule, the resolver the fleet controller uses, and the converter
// that turns a Docker image into a microVM root filesystem. Starting
// machines is the fleet agent's job.
package images

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// Errors of Verify.
var (
	// ErrSignature means no trusted key signed the image.
	ErrSignature = errors.New("image signature is not valid for any trusted key")
	// ErrDigest means the file is not the one the image's digest names.
	ErrDigest = errors.New("image file does not match its digest")
)

const (
	keyPrefix = "ed25519:"
	// maxKeyFile bounds a key file read from disk.
	maxKeyFile = 1 << 16
)

// GenerateKey returns a new signing key.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// FormatPublicKey returns the form fleet agents are configured with:
// ed25519:<base64 of the 32 bytes>.
func FormatPublicKey(k ed25519.PublicKey) string {
	return keyPrefix + base64.StdEncoding.EncodeToString(k)
}

// ParsePublicKey reads a key in the form of FormatPublicKey.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, ok := strings.CutPrefix(strings.TrimSpace(s), keyPrefix)
	if !ok {
		return nil, fmt.Errorf("public key must be %s<base64>", keyPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(b)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %s<base64 of %d bytes>", keyPrefix, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePublicKeys reads one public key per line; blank lines and lines that
// start with # are skipped.
func ParsePublicKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParsePublicKey(line)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// MarshalPrivateKey encodes a signing key as PKCS #8 PEM, the form
// WritePrivateKey stores and ParsePrivateKey reads.
func MarshalPrivateKey(k ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("encode private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKey reads a key written by MarshalPrivateKey.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("private key must be a PKCS #8 PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key must be an Ed25519 key")
	}
	return ek, nil
}

// WritePrivateKey stores a private key at path with mode 0600; it never
// replaces a file.
func WritePrivateKey(path string, k ed25519.PrivateKey) error {
	pemBytes, err := MarshalPrivateKey(k)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's path
	if err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		_ = f.Close()
		return fmt.Errorf("write private key: %w", err)
	}
	return f.Close()
}

// ReadPrivateKey reads the key file at path.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	f, err := os.Open(path) // #nosec G304 -- the operator's path
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return ParsePrivateKey(data)
}

// Sign returns the signature of img: ed25519:<base64> over its signing
// payload, which names the image's name, version, os, arch, format and
// digest, so it fits that image only.
func Sign(key ed25519.PrivateKey, img domain.Image) string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, img.SigningPayload()))
}

// Verify checks that one of keys signed img. It does not look at the file:
// VerifyFile does.
func Verify(img domain.Image, keys []ed25519.PublicKey) error {
	b, ok := strings.CutPrefix(img.Signature, "ed25519:")
	if !ok {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, ErrSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(b)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, ErrSignature)
	}
	payload := img.SigningPayload()
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, payload, sig) {
			return nil
		}
	}
	return fmt.Errorf("image %s@%s: %w", img.Name, img.Version, ErrSignature)
}

// Digest returns the sha256:<hex> digest of r.
func Digest(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// FileDigest returns the digest of the file at path.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the caller's path
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Digest(f)
}

// VerifyFile is what a fleet agent does before it first uses an image it
// fetched to path: the signature, which covers the image's digest, its
// kernel's digest and its build time, must be valid for a trusted key, the
// file must be the one the signed digest names and, for a rootfs image, the
// kernel file fetched to kernelPath must be the one the signed kernel digest
// names. A rootfs image without its kernel file fails, so a kernel cannot be
// swapped or skipped.
func VerifyFile(img domain.Image, path, kernelPath string, keys []ed25519.PublicKey) error {
	if err := Verify(img, keys); err != nil {
		return err
	}
	if err := checkDigest(img, "", path, img.Digest); err != nil {
		return err
	}
	if k := img.Contents.Kernel; k != nil {
		if kernelPath == "" {
			return fmt.Errorf("image %s@%s is a rootfs image: its kernel file is needed to check it: %w", img.Name, img.Version, ErrDigest)
		}
		return checkDigest(img, "kernel ", kernelPath, k.Digest)
	}
	return nil
}

func checkDigest(img domain.Image, what, path, want string) error {
	got, err := FileDigest(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("image %s@%s %s%w (file is %s, signed is %s)", img.Name, img.Version, what, ErrDigest, got, want)
	}
	return nil
}

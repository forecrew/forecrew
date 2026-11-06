package images

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// MaxManifestBytes bounds a manifest read from disk or a request.
const MaxManifestBytes = 1 << 20

// placeholderSignature stands in for the signature of a manifest that is
// not signed yet, only to run Image.Validate over everything else.
const placeholderSignature = "ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="

// Manifest is what a build leaves next to its image: $defs/image of
// fleet.schema.json. A build on a host (a Windows image from a user's ISO)
// leaves it unsigned; a person signs it with `forecrew image sign` and
// registers it.
type Manifest struct {
	Name      string               `json:"name"`
	Version   string               `json:"version"`
	OS        domain.MachineOS     `json:"os"`
	Arch      domain.MachineArch   `json:"arch"`
	Format    domain.ImageFormat   `json:"format"`
	URI       string               `json:"uri"`
	Digest    string               `json:"digest"`
	Signature string               `json:"signature,omitempty"`
	BuiltAt   time.Time            `json:"built_at"`
	Contents  domain.ImageContents `json:"contents"`
}

// ManifestOf returns the manifest of an image.
func ManifestOf(i domain.Image) Manifest {
	return Manifest{Name: i.Name, Version: i.Version, OS: i.OS, Arch: i.Arch, Format: i.Format, URI: i.URI,
		Digest: i.Digest, Signature: i.Signature, BuiltAt: i.BuiltAt, Contents: i.Contents}
}

// Image returns the image the manifest describes, for an organization (the
// zero ID is instance-wide). It does not check it: Validate does.
func (m Manifest) Image(accountID uuid.UUID) domain.Image {
	return domain.Image{AccountID: accountID, Name: m.Name, Version: m.Version, OS: m.OS, Arch: m.Arch, Format: m.Format,
		URI: m.URI, Digest: m.Digest, Signature: m.Signature, Contents: m.Contents, BuiltAt: m.BuiltAt.UTC()}
}

// Validate checks the manifest as Image.Validate does, except that the
// signature may be missing; Signed says whether it is there.
func (m Manifest) Validate() error {
	img := m.Image(uuid.Nil())
	if !m.Signed() {
		img.Signature = placeholderSignature
	}
	if img.BuiltAt.IsZero() {
		return errors.Join(domain.ErrInvalidRecord, errors.New("built_at is required"))
	}
	return img.Validate()
}

// Signed reports whether the manifest carries a signature.
func (m Manifest) Signed() bool { return m.Signature != "" }

// SignedWith returns the manifest signed by key.
func (m Manifest) SignedWith(key ed25519.PrivateKey) Manifest {
	m.Signature = Sign(key, m.Image(uuid.Nil()))
	return m
}

// Encode returns the manifest as indented JSON with a final newline.
func (m Manifest) Encode() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return b.Bytes(), nil
}

// ParseManifest decodes a manifest, refusing unknown fields and anything
// after it.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Manifest{}, errors.New("manifest: unexpected data after the manifest")
	}
	return m, nil
}

// Build is what the pipeline knows about an image it just built.
type Build struct {
	Name, Version string
	OS            domain.MachineOS
	Arch          domain.MachineArch
	Format        domain.ImageFormat
	// File is the image file; its digest goes in the manifest. Images that
	// are not one file (an AMI, a Tart image) set Digest, of the build's
	// manifest, instead.
	File   string
	Digest string
	// URI is where the image will be fetched from.
	URI string
	// Kernel and KernelURI describe the kernel file of a rootfs image.
	Kernel, KernelURI string
	Labels            []string
	Packages          []domain.ImagePackage
	BuiltAt           time.Time
}

// NewManifest computes the digests of the files of b and returns the
// unsigned manifest, checked by Validate.
func NewManifest(b Build) (Manifest, error) {
	m := Manifest{Name: b.Name, Version: b.Version, OS: b.OS, Arch: b.Arch, Format: b.Format, URI: b.URI, Digest: b.Digest,
		BuiltAt: b.BuiltAt.UTC(), Contents: domain.ImageContents{Labels: domain.NormalizeLabels(b.Labels), Packages: b.Packages}}
	if (b.File == "") == (b.Digest == "") {
		return Manifest{}, errors.New("exactly one of the image file and the digest is needed")
	}
	if b.File != "" {
		d, err := FileDigest(b.File)
		if err != nil {
			return Manifest{}, err
		}
		m.Digest = d
	}
	if b.Kernel != "" {
		d, err := FileDigest(b.Kernel)
		if err != nil {
			return Manifest{}, err
		}
		m.Contents.Kernel = &domain.ImageKernel{URI: b.KernelURI, Digest: d}
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

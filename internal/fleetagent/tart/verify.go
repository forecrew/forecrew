package tart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/images"
)

// What Tart writes for an image, and what its manifest says of it
// (Tart's OCI/Manifest.swift and VMDirectory.swift).
const (
	mediaConfig = "application/vnd.cirruslabs.tart.config.v1"
	mediaNVRAM  = "application/vnd.cirruslabs.tart.nvram.v1"
	mediaDisk   = "application/vnd.cirruslabs.tart.disk.v2"

	annLayerSize   = "org.cirruslabs.tart.uncompressed-size"
	annLayerDigest = "org.cirruslabs.tart.uncompressed-content-digest"
	// Annotations of stacked (ASIF overlay) disks, which are no flat file.
	annBlockSize  = "org.cirruslabs.tart.disk.block-size"
	annChunkCount = "org.cirruslabs.tart.disk-file-chunk-count"

	fileManifest = "manifest.json"
	fileConfig   = "config.json"
	fileNVRAM    = "nvram.bin"
	fileDisk     = "disk.img"

	maxManifest = 4 << 20
)

type ociLayer struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
}

type ociManifest struct {
	Layers []ociLayer `json:"layers"`
}

// cacheDir is where Tart keeps an image it pulled by digest:
// <TART_HOME>/cache/OCIs/<registry>/<repository>/<digest>.
func (p *Provider) cacheDir(repo, digest string) string {
	return filepath.Join(p.cli.tartHome(), "cache", "OCIs", filepath.FromSlash(repo), digest)
}

// verifyPulled checks what Tart cached for repo@digest against the signed
// digest, since Tart does not. The manifest, as the registry sent it, must hash
// to the digest the signature covers. The files must then be what the manifest
// says: config and NVRAM by their layer digests, and the disk, which Tart
// stores uncompressed, chunk by chunk against the layers' uncompressed content
// digests. A layer that cannot be checked (another media type, a disk that is
// not a flat file, a missing annotation) refuses the image. The result is kept
// until the files change, since the disk is large. The caller holds fetching.
func (p *Provider) verifyPulled(ctx context.Context, repo, digest string) error {
	dir := p.cacheDir(repo, digest)
	fp, err := fingerprint(dir)
	if err != nil {
		return fmt.Errorf("the cache of Tart has no %s of this image: %w", fileManifest, err)
	}
	if p.verified[digest] == fp {
		return nil
	}
	raw, err := readLimited(filepath.Join(dir, fileManifest), maxManifest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("the manifest Tart pulled is %s, not the signed %s: %w", got, digest, images.ErrDigest)
	}
	var m ociManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("the manifest: %w", err)
	}
	if err := checkLayers(ctx, dir, m.Layers); err != nil {
		return err
	}
	p.verified[digest] = fp
	return nil
}

// fingerprint says when the files of an image last changed, so that a checked
// image is not hashed again until they do.
func fingerprint(dir string) (string, error) {
	var out strings.Builder
	for _, f := range []string{fileManifest, fileConfig, fileNVRAM, fileDisk} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			if f == fileManifest {
				return "", err
			}
			continue // reported by the check
		}
		fmt.Fprintf(&out, "%s:%d:%d;", f, st.Size(), st.ModTime().UnixNano())
	}
	return out.String(), nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a path of the agent's own Tart home
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return raw, nil
}

func checkLayers(ctx context.Context, dir string, layers []ociLayer) error {
	var config, nvram *ociLayer
	var disk []ociLayer
	for i, l := range layers {
		switch l.MediaType {
		case mediaConfig:
			if config != nil {
				return errors.New("the manifest has more than one config layer")
			}
			config = &layers[i]
		case mediaNVRAM:
			if nvram != nil {
				return errors.New("the manifest has more than one NVRAM layer")
			}
			nvram = &layers[i]
		case mediaDisk:
			disk = append(disk, l)
		default:
			return fmt.Errorf("the manifest has a layer of type %q, which cannot be checked", l.MediaType)
		}
	}
	if config == nil || nvram == nil || len(disk) == 0 {
		return errors.New("the manifest lacks a config, an NVRAM or a disk layer")
	}
	for file, l := range map[string]*ociLayer{fileConfig: config, fileNVRAM: nvram} {
		got, err := images.FileDigest(filepath.Join(dir, file))
		if err != nil {
			return err
		}
		if got != l.Digest {
			return fmt.Errorf("%s is %s, not the %s of its layer: %w", file, got, l.Digest, images.ErrDigest)
		}
	}
	return checkDisk(ctx, filepath.Join(dir, fileDisk), disk)
}

// checkDisk hashes the disk in the chunks the layers describe, in order, and
// requires them to cover it exactly.
func checkDisk(ctx context.Context, path string, layers []ociLayer) error {
	f, err := os.Open(path) // #nosec G304 -- a path of the agent's own Tart home
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var total int64
	for i, l := range layers {
		size, err := strconv.ParseInt(l.Annotations[annLayerSize], 10, 64)
		want := l.Annotations[annLayerDigest]
		if err != nil || size <= 0 || !sha256Digest.MatchString(want) {
			return fmt.Errorf("disk layer %d has no usable uncompressed size and digest, so the disk cannot be checked", i)
		}
		for _, k := range []string{annBlockSize, annChunkCount} {
			if _, stacked := l.Annotations[k]; stacked {
				return fmt.Errorf("disk layer %d is part of a stacked disk, which is not one flat file, so the disk cannot be checked", i)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		h := sha256.New()
		if n, err := io.CopyN(h, f, size); err != nil {
			return fmt.Errorf("disk layer %d: %d of %d bytes: %w", i, n, size, err)
		}
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != want {
			return fmt.Errorf("disk layer %d is %s, not the %s the manifest names: %w", i, got, want, images.ErrDigest)
		}
		total += size
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != total {
		return fmt.Errorf("the disk is %d bytes and its layers cover %d: %w", st.Size(), total, images.ErrDigest)
	}
	return nil
}

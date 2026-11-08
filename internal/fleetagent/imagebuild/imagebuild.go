// Package imagebuild builds the Windows image of a location on the host that will
// run it, from an ISO its owner supplies, by running Packer with the template in
// deploy/images/windows (docs/images.md). Forecrew cannot hand out Windows, so
// the result is an unsigned manifest: a person checks it, signs it with a key
// that stays off the host, and registers it under their organization.
package imagebuild

import (
	"bufio"
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/images"
)

// Name is the name of the image built, and MinFreeGB the room a build needs: the
// ISO is read, a 60 GB disk is made and converted.
const (
	Name      = "forecrew-windows"
	MinFreeGB = 100
)

// Labels are what the image offers (provision-windows.ps1 writes the same list
// to the image); TestLabelsMatchTheTemplate keeps them together.
var Labels = []string{"git", "go", "node", "python", "runtime:claude-code", "runtime:codex", "runtime:opencode"}

var (
	versionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	checksumRE = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`)
)

// Steps of a build and the percent each is worth when it starts, which
// progress.json and the screens that show a build say. The build itself runs
// for about an hour inside the fourth step, which cannot be measured: the
// percent is where the build stands, not an estimate of the time left.
const (
	stepChecking = "checking"
	stepISO      = "checking the ISO"
	stepPlugins  = "fetching Packer's plugins"
	stepBuilding = "building the image: about an hour"
	stepManifest = "writing the manifest"
	stepDone     = "done"
	stepFailed   = "failed"
)

var stepPercent = map[string]int{stepChecking: 2, stepISO: 5, stepPlugins: 10, stepBuilding: 15, stepManifest: 95, stepDone: 100}

// Options is one build.
type Options struct {
	// TemplatesDir is deploy/images of the release the agent is from; Packer is
	// the command.
	TemplatesDir string
	Packer       string
	// ISO is a Windows Server ISO the owner is licensed for, ISOChecksum its
	// sha256:<hex> (checked here, whatever Packer does), and LicenseConfirmed
	// the owner's statement that the licence covers building and running it.
	ISO              string
	ISOChecksum      string
	LicenseConfirmed bool
	// WindowsImageName is the edition inside the ISO; empty keeps the template's.
	WindowsImageName string
	// ChocolateyInstallerSHA256 is the hash of the install.ps1 the owner reviewed.
	ChocolateyInstallerSHA256 string
	// Worker is forecrew-worker.exe for windows/amd64.
	Worker string
	// GuestTools and GuestToolsSHA256 are the virtio-win installer, for an image
	// a Proxmox location starts; empty for Hyper-V.
	GuestTools, GuestToolsSHA256 string
	Version                      string
	// URIBase is where the image will be fetched from by other hosts.
	URIBase string
	// OutDir holds the build, its log, its progress and the manifest.
	OutDir string
	// CacheDir, when set, is a cache of images (a Hyper-V location's) the built
	// file is put in under its digest, so the host needs no download.
	CacheDir string
	// Switch is the Hyper-V switch of the build VM.
	Switch string
	// GOOS says which builder runs: Hyper-V on windows, QEMU on linux.
	GOOS string
}

// Result is what a build leaves.
type Result struct {
	Image, Manifest, Digest, Cached string
	Format                          domain.ImageFormat
}

// Exec runs a command in dir with its output to w; tests replace it.
type Exec func(ctx context.Context, dir string, w io.Writer, env []string, name string, args ...string) error

// Builder runs builds.
type Builder struct {
	Exec Exec
	// DiskFreeGB is the room on a file system.
	DiskFreeGB func(path string) (int, error)
	Now        func() time.Time
}

// Run builds the image. It writes progress.json in OutDir as it goes, so a
// person can follow a build of about an hour, and build.log with what Packer
// printed (nothing in it is a secret: the build's password never leaves Packer).
func (b Builder) Run(ctx context.Context, o Options) (Result, error) {
	b.fill()
	if err := o.validate(); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(o.OutDir, 0o750); err != nil {
		return Result{}, err
	}
	prog := &progress{path: filepath.Join(o.OutDir, ProgressFile), now: b.Now}
	res, err := b.run(ctx, o, prog)
	if err != nil {
		prog.set(stepFailed, "failed", err.Error())
		return Result{}, err
	}
	prog.set(stepDone, "done", "")
	return res, nil
}

func (b *Builder) fill() {
	if b.Exec == nil {
		b.Exec = execCommand
	}
	if b.Now == nil {
		b.Now = time.Now
	}
}

func (b Builder) run(ctx context.Context, o Options, prog *progress) (Result, error) {
	prog.set(stepChecking, "running", "")
	if b.DiskFreeGB != nil {
		free, err := b.DiskFreeGB(o.OutDir)
		if err != nil {
			return Result{}, fmt.Errorf("free space of %s: %w", o.OutDir, err)
		}
		if free < MinFreeGB {
			return Result{}, fmt.Errorf("%s has %d GB free and a build needs %d GB", o.OutDir, free, MinFreeGB)
		}
	}
	prog.set(stepISO, "running", "")
	got, err := fileSHA256(o.ISO)
	if err != nil {
		return Result{}, fmt.Errorf("iso: %w", err)
	}
	if !strings.EqualFold(got, strings.TrimPrefix(o.ISOChecksum, "sha256:")) {
		return Result{}, fmt.Errorf("the ISO is sha256:%s, not the %s you gave: get it again from your licence portal", got, o.ISOChecksum)
	}
	if err := checkWorker(o.Worker); err != nil {
		return Result{}, err
	}
	source, format, err := o.builder()
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Join(o.TemplatesDir, "windows")
	vars, cleanup, err := writeVars(o, source)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	log, err := os.OpenFile(filepath.Join(o.OutDir, "build.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = log.Close() }()
	env := append(os.Environ(), "CHECKPOINT_DISABLE=1", "PACKER_NO_COLOR=1")
	prog.set(stepPlugins, "running", "")
	if err := b.Exec(ctx, dir, log, env, o.packer(), "init", "."); err != nil {
		return Result{}, fmt.Errorf("packer init: %w (see %s)", err, log.Name())
	}
	prog.set(stepBuilding, "running", "")
	if err := b.Exec(ctx, dir, log, env, o.packer(), "build", "-only=windows."+source, "-var-file="+vars, "-timestamp-ui", "."); err != nil {
		return Result{}, fmt.Errorf("packer build: %w (see %s)", err, log.Name())
	}
	prog.set(stepManifest, "running", "")
	return b.finish(o, format)
}

func (b Builder) finish(o Options, format domain.ImageFormat) (Result, error) {
	file := o.outputFile(format)
	if _, err := os.Stat(file); err != nil {
		return Result{}, fmt.Errorf("the build left no image at %s: %w", file, err)
	}
	pkgs, err := readPackages(filepath.Join(o.OutDir, "packages-windows-amd64.txt"))
	if err != nil {
		return Result{}, err
	}
	m, err := images.NewManifest(images.Build{
		Name: Name, Version: o.Version, OS: domain.OSWindows, Arch: domain.ArchAMD64, Format: format, File: file,
		URI: strings.TrimRight(o.URIBase, "/") + "/" + filepath.Base(file), Labels: Labels, Packages: pkgs, BuiltAt: b.Now(),
	})
	if err != nil {
		return Result{}, err
	}
	raw, err := m.Encode()
	if err != nil {
		return Result{}, err
	}
	manifest := filepath.Join(o.OutDir, fmt.Sprintf("%s-%s.manifest.json", Name, o.Version))
	if err := os.WriteFile(manifest, raw, 0o644); err != nil { // #nosec G306 -- a manifest holds no secret
		return Result{}, err
	}
	res := Result{Image: file, Manifest: manifest, Digest: m.Digest, Format: format}
	if o.CacheDir != "" {
		if res.Cached, err = cacheFile(o.CacheDir, file, m.Digest, format); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

func (o Options) validate() error {
	var bad []error
	if !o.LicenseConfirmed {
		bad = append(bad, errors.New("confirm that you hold a Windows Server licence that covers building and running this image (--license-confirmed): Forecrew does not provide Windows"))
	}
	if !checksumRE.MatchString(o.ISOChecksum) {
		bad = append(bad, errors.New("--iso-checksum must be sha256:<64 hex digits>, from your licence portal"))
	}
	if !sha256RE.MatchString(o.ChocolateyInstallerSHA256) {
		bad = append(bad, errors.New("--chocolatey-sha256 must be the SHA-256 of the Chocolatey install.ps1 you reviewed"))
	}
	if !versionRE.MatchString(o.Version) {
		bad = append(bad, fmt.Errorf("--version %q is not a version such as 2026.11.1", o.Version))
	}
	if !strings.HasPrefix(o.URIBase, "https://") && !strings.HasPrefix(o.URIBase, "http://") {
		bad = append(bad, errors.New("--uri-base must be an http or https address other hosts fetch the image from, without credentials"))
	}
	if (o.GuestTools == "") != (o.GuestToolsSHA256 == "") || (o.GuestToolsSHA256 != "" && !sha256RE.MatchString(o.GuestToolsSHA256)) {
		bad = append(bad, errors.New("--guest-tools and --guest-tools-sha256 go together; the hash is 64 hex digits"))
	}
	if o.WindowsImageName != "" && !nameRE.MatchString(o.WindowsImageName) {
		bad = append(bad, fmt.Errorf("--windows-image-name %q is not an edition name", o.WindowsImageName))
	}
	if o.Switch != "" && !nameRE.MatchString(o.Switch) {
		bad = append(bad, fmt.Errorf("--switch %q is not a switch name", o.Switch))
	}
	for name, p := range map[string]string{"--iso": o.ISO, "--worker": o.Worker, "--templates": o.TemplatesDir, "--out": o.OutDir} {
		if !filepath.IsAbs(p) {
			bad = append(bad, fmt.Errorf("%s must be an absolute path, got %q", name, p))
		}
	}
	if o.TemplatesDir != "" {
		if _, err := os.Stat(filepath.Join(o.TemplatesDir, "windows", "windows.pkr.hcl")); err != nil {
			bad = append(bad, fmt.Errorf("--templates %s is not deploy/images of Forecrew: it has no windows/windows.pkr.hcl", o.TemplatesDir))
		}
	}
	return errors.Join(bad...)
}

func (o Options) packer() string {
	if o.Packer == "" {
		return "packer"
	}
	return o.Packer
}

// builder says which Packer source runs here and the format it leaves: QEMU on
// Linux (the host of a Proxmox location) makes a qcow2, Hyper-V a vhdx.
func (o Options) builder() (source string, format domain.ImageFormat, err error) {
	switch o.GOOS {
	case "linux":
		return "qemu.windows", domain.ImageQCOW2, nil
	case "windows":
		return "hyperv-iso.windows", domain.ImageVHDX, nil
	}
	return "", "", fmt.Errorf("a Windows image is built on Linux (QEMU) or Windows (Hyper-V), not %s", o.GOOS)
}

func (o Options) outputFile(format domain.ImageFormat) string {
	file := fmt.Sprintf("forecrew-windows-%s-amd64", o.Version)
	if format == domain.ImageVHDX {
		return filepath.Join(o.OutDir, "windows-hyperv", "Virtual Hard Disks", file+".vhdx")
	}
	return filepath.Join(o.OutDir, "windows-qcow2", file+".qcow2")
}

// writeVars writes the build's variables to a file only this user reads: what is
// put on a command line shows in the process list.
func writeVars(o Options, source string) (path string, cleanup func(), err error) {
	vars := map[string]any{
		"version": o.Version, "iso_path": o.ISO, "iso_checksum": o.ISOChecksum, "license_confirmed": o.LicenseConfirmed,
		"chocolatey_installer_sha256": o.ChocolateyInstallerSHA256, "worker_binary": o.Worker, "output_dir": o.OutDir,
	}
	if o.WindowsImageName != "" {
		vars["windows_image_name"] = o.WindowsImageName
	}
	if o.GuestTools != "" {
		vars["guest_tools_installer"], vars["guest_tools_sha256"] = o.GuestTools, o.GuestToolsSHA256
	}
	if o.Switch != "" && strings.HasPrefix(source, "hyperv") {
		vars["hyperv_switch"] = o.Switch
	}
	dir, err := os.MkdirTemp("", "forecrew-build-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	raw, err := json.Marshal(vars)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	path = filepath.Join(dir, "build.pkrvars.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// checkWorker refuses a worker that is not a Windows program for amd64: the
// image would start and fail only at its first job.
func checkWorker(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("--worker %s is not a Windows program (build it with GOOS=windows GOARCH=amd64): %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return fmt.Errorf("--worker %s is not for amd64", path)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the owner's file
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readPackages reads the name=version lines the build downloaded from the image.
func readPackages(path string) ([]domain.ImagePackage, error) {
	f, err := os.Open(path) // #nosec G304 -- written by the build
	if err != nil {
		return nil, fmt.Errorf("packages of the image: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []domain.ImagePackage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		name, version, ok := strings.Cut(line, "=")
		if !ok || name == "" || version == "" {
			return nil, fmt.Errorf("packages of the image: bad line %q", fleet.CleanError(line))
		}
		out = append(out, domain.ImagePackage{Name: name, Version: version})
	}
	return out, sc.Err()
}

// cacheFile puts the built image in a location's cache under its digest, by a
// hard link or else a copy.
func cacheFile(dir, file, digest string, format domain.ImageFormat) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !sha256RE.MatchString(hex) {
		return "", fmt.Errorf("digest %q is not sha256", digest)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, hex+"."+string(format))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.Link(file, dst); err == nil {
		return dst, nil
	}
	in, err := os.Open(file) // #nosec G304 -- the build's output
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the cache
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return "", err
	}
	return dst, out.Close()
}

func execCommand(ctx context.Context, dir string, w io.Writer, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- the owner's packer, fixed arguments
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.Env = dir, w, w, env
	return cmd.Run()
}

// Progress is what progress.json says: where a build stands, for a person or a
// screen that follows a long build. Error holds the reason of a failure, with
// no secret in it.
type Progress struct {
	Step    string `json:"step"`
	State   string `json:"state"` // running, done or failed
	Percent int    `json:"percent"`
	Error   string `json:"error"`
	Updated string `json:"updated"`
}

// ProgressFile is the name of the progress file in a build's output directory.
const ProgressFile = "progress.json"

// ReadProgress reads the progress of the build in dir.
func ReadProgress(dir string) (Progress, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ProgressFile)) // #nosec G304 -- the build's own directory
	if err != nil {
		return Progress{}, err
	}
	var p Progress
	if err := json.Unmarshal(raw, &p); err != nil {
		return Progress{}, fmt.Errorf("progress of the build: %w", err)
	}
	return p, nil
}

// progress writes progress.json.
type progress struct {
	path    string
	now     func() time.Time
	percent int
}

// set records a step. A failure keeps the percent the build had reached.
func (p *progress) set(step, state, why string) {
	if pct, ok := stepPercent[step]; ok {
		p.percent = pct
	}
	raw, _ := json.Marshal(Progress{Step: step, State: state, Percent: p.percent, Error: fleet.CleanError(why), Updated: p.now().UTC().Format(time.RFC3339)})
	_ = os.WriteFile(p.path, raw, 0o644) // #nosec G306 -- progress holds no secret
}

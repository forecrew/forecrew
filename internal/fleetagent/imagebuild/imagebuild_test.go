package imagebuild

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
)

const isoBody = "pretend this is a Windows ISO"

// minimalPE is the smallest file debug/pe reads: a DOS stub that points at a PE
// signature and a file header, for a machine.
func minimalPE(machine uint16) []byte {
	b := make([]byte, 128)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[0x44:], machine)
	return b
}

type call struct {
	dir  string
	args []string
}

type rig struct {
	t     *testing.T
	opts  Options
	calls []call
	vars  map[string]any
	// outputs is written by the fake Packer on build.
	skipOutput, skipPackages bool
	failOn                   string
	goos                     string
	b                        Builder
}

func newRig(t *testing.T, goos string) *rig {
	t.Helper()
	root := t.TempDir()
	write := func(path string, data []byte) string {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("images/windows/windows.pkr.hcl", []byte("# template"))
	iso := write("win.iso", []byte(isoBody))
	worker := write("forecrew-worker.exe", minimalPE(0x8664))
	sum, err := fileSHA256(iso)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, goos: goos}
	r.opts = Options{
		TemplatesDir: filepath.Join(root, "images"), ISO: iso, ISOChecksum: "sha256:" + sum, LicenseConfirmed: true,
		ChocolateyInstallerSHA256: strings.Repeat("ab", 32), Worker: worker, Version: "2026.11.1", URIBase: "https://images.example.com/",
		OutDir: filepath.Join(root, "out"), GOOS: goos,
	}
	r.b = Builder{
		DiskFreeGB: func(string) (int, error) { return 500, nil },
		Now:        func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
		Exec:       r.exec,
	}
	return r
}

// exec is the fake Packer: it records its calls and, on build, reads the
// variables it was given and leaves what a build leaves.
func (r *rig) exec(_ context.Context, dir string, w io.Writer, _ []string, name string, args ...string) error {
	r.calls = append(r.calls, call{dir, append([]string{name}, args...)})
	_, _ = io.WriteString(w, "packer says hello\n")
	if r.failOn != "" && args[0] == r.failOn {
		return errors.New("exit status 1")
	}
	if args[0] != "build" {
		return nil
	}
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "-var-file="); ok {
			r.readVars(v)
		}
	}
	if !r.skipOutput {
		format := map[string]domain.ImageFormat{"linux": domain.ImageQCOW2, "windows": domain.ImageVHDX}[r.goos]
		out := r.opts.outputFile(format)
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		_ = os.WriteFile(out, []byte("the built disk"), 0o600)
	}
	if !r.skipPackages {
		_ = os.WriteFile(filepath.Join(r.opts.OutDir, "packages-windows-amd64.txt"), []byte("git=2.56.0.4\ngolang=1.27.3\n"), 0o600)
	}
	return nil
}

func (r *rig) readVars(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		r.t.Errorf("var file: %v", err)
	}
	r.vars = map[string]any{}
	_ = json.Unmarshal(raw, &r.vars)
	if st, _ := os.Stat(path); st.Mode().Perm()&0o077 != 0 {
		r.t.Errorf("var file mode %v", st.Mode())
	}
}

func (r *rig) run() (Result, error) { return r.b.Run(context.Background(), r.opts) }

func (r *rig) progress() map[string]string {
	raw, err := os.ReadFile(filepath.Join(r.opts.OutDir, "progress.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestBuildOnLinuxMakesAQcow2ForProxmox(t *testing.T) {
	r := newRig(t, "linux")
	r.opts.GuestTools, r.opts.GuestToolsSHA256 = "/tools/virtio-win-guest-tools.exe", strings.Repeat("cd", 32)
	r.opts.WindowsImageName = "Windows Server 2022 SERVERSTANDARD"
	res, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != domain.ImageQCOW2 || !strings.HasSuffix(res.Image, "forecrew-windows-2026.11.1-amd64.qcow2") {
		t.Errorf("result = %+v", res)
	}
	if len(r.calls) != 2 || r.calls[0].args[1] != "init" || r.calls[1].args[1] != "build" || !slices.Contains(r.calls[1].args, "-only=windows.qemu.windows") {
		t.Fatalf("calls = %+v", r.calls)
	}
	if !strings.HasSuffix(r.calls[1].dir, filepath.Join("images", "windows")) {
		t.Errorf("ran in %s", r.calls[1].dir)
	}
	for _, a := range r.calls[1].args { // nothing of the build is on the command line
		if strings.Contains(a, "ab"+"ab") || strings.Contains(a, "win.iso") {
			t.Errorf("a variable is in the arguments: %s", a)
		}
	}
	if r.vars["license_confirmed"] != true || r.vars["guest_tools_sha256"] != strings.Repeat("cd", 32) || r.vars["windows_image_name"] == nil || r.vars["hyperv_switch"] != nil {
		t.Errorf("vars = %v", r.vars)
	}
	// The manifest is unsigned and valid, and says what the image is.
	raw, err := os.ReadFile(res.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := images.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Signed() || m.Name != Name || m.OS != domain.OSWindows || m.Format != domain.ImageQCOW2 || m.Digest != res.Digest ||
		m.URI != "https://images.example.com/forecrew-windows-2026.11.1-amd64.qcow2" || !slices.Equal(m.Contents.Labels, domain.NormalizeLabels(Labels)) ||
		len(m.Contents.Packages) != 2 || m.Contents.Packages[1].Name != "golang" {
		t.Errorf("manifest = %+v", m)
	}
	if p := r.progress(); p["state"] != "done" || p["error"] != "" {
		t.Errorf("progress = %v", p)
	}
	if log, _ := os.ReadFile(filepath.Join(r.opts.OutDir, "build.log")); !strings.Contains(string(log), "packer says hello") {
		t.Errorf("log = %q", log)
	}
	if res.Cached != "" {
		t.Errorf("cached %s without a cache", res.Cached)
	}
}

func TestBuildOnWindowsMakesAVhdxAndCachesIt(t *testing.T) {
	r := newRig(t, "windows")
	r.opts.Switch = "Lab"
	r.opts.CacheDir = filepath.Join(t.TempDir(), "images")
	res, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != domain.ImageVHDX || !strings.Contains(res.Image, "Virtual Hard Disks") || !slices.Contains(r.calls[1].args, "-only=windows.hyperv-iso.windows") {
		t.Errorf("result %+v, calls %+v", res, r.calls)
	}
	if r.vars["hyperv_switch"] != "Lab" {
		t.Errorf("vars = %v", r.vars)
	}
	want := filepath.Join(r.opts.CacheDir, strings.TrimPrefix(res.Digest, "sha256:")+".vhdx")
	if res.Cached != want {
		t.Errorf("cached = %s, want %s", res.Cached, want)
	}
	if got, _ := images.FileDigest(res.Cached); got != res.Digest {
		t.Errorf("the cached file is %s", got)
	}
	// Again: it is there already.
	if _, err := r.run(); err != nil {
		t.Errorf("second build: %v", err)
	}
}

func TestBuildRefuses(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *rig)
		want string
	}{
		{"licence not confirmed", func(r *rig) { r.opts.LicenseConfirmed = false }, "licence"},
		{"no checksum", func(r *rig) { r.opts.ISOChecksum = "" }, "iso-checksum"},
		{"md5 checksum", func(r *rig) { r.opts.ISOChecksum = "md5:abc" }, "iso-checksum"},
		{"wrong checksum", func(r *rig) { r.opts.ISOChecksum = "sha256:" + strings.Repeat("0", 64) }, "not the"},
		{"chocolatey hash", func(r *rig) { r.opts.ChocolateyInstallerSHA256 = "abc" }, "chocolatey-sha256"},
		{"bad version", func(r *rig) { r.opts.Version = "../x" }, "version"},
		{"uri", func(r *rig) { r.opts.URIBase = "ftp://x" }, "uri-base"},
		{"tools without hash", func(r *rig) { r.opts.GuestTools = "/t.exe" }, "guest-tools"},
		{"hash without tools", func(r *rig) { r.opts.GuestToolsSHA256 = strings.Repeat("a", 64) }, "guest-tools"},
		{"bad tools hash", func(r *rig) { r.opts.GuestTools, r.opts.GuestToolsSHA256 = "/t.exe", "zz" }, "guest-tools"},
		{"edition", func(r *rig) { r.opts.WindowsImageName = "a;b" }, "edition"},
		{"switch", func(r *rig) { r.opts.Switch = "a;b" }, "switch"},
		{"relative iso", func(r *rig) { r.opts.ISO = "win.iso" }, "absolute"},
		{"no templates", func(r *rig) { r.opts.TemplatesDir = t.TempDir() }, "not deploy/images"},
		{"missing iso", func(r *rig) {
			_ = os.Remove(r.opts.ISO)
		}, "iso"},
		{"iso is a directory", func(r *rig) { r.opts.ISO = t.TempDir() }, "iso"},
		{"worker is not a program", func(r *rig) { _ = os.WriteFile(r.opts.Worker, []byte("#!/bin/sh"), 0o600) }, "not a Windows program"},
		{"worker for arm64", func(r *rig) { _ = os.WriteFile(r.opts.Worker, minimalPE(0xaa64), 0o600) }, "not for amd64"},
		{"macos host", func(r *rig) { r.opts.GOOS = "darwin" }, "Linux (QEMU) or Windows"},
		{"little room", func(r *rig) { r.b.DiskFreeGB = func(string) (int, error) { return 20, nil } }, "needs 100 GB"},
		{"room unknown", func(r *rig) { r.b.DiskFreeGB = func(string) (int, error) { return 0, errors.New("nope") } }, "free space"},
		{"packer init fails", func(r *rig) { r.failOn = "init" }, "packer init"},
		{"packer build fails", func(r *rig) { r.failOn = "build" }, "packer build"},
		{"no image left", func(r *rig) { r.skipOutput = true }, "left no image"},
		{"no package list", func(r *rig) { r.skipPackages = true }, "packages of the image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, "linux")
			tt.do(r)
			_, err := r.run()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if _, statErr := os.Stat(filepath.Join(r.opts.OutDir, "progress.json")); statErr == nil {
				if p := r.progress(); p["state"] != "failed" || p["error"] == "" {
					t.Errorf("progress = %v", p)
				}
			}
			if _, statErr := os.Stat(r.opts.outputFile(domain.ImageQCOW2) + ".manifest.json"); statErr == nil {
				t.Error("a manifest was written")
			}
		})
	}
}

func TestBadPackageLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p")
	for _, body := range []string{"nonsense\n", "=1\n", "a=\n"} {
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := readPackages(path); err == nil {
			t.Errorf("%q accepted", body)
		}
	}
	_ = os.WriteFile(path, []byte("\ngit=2\n\n"), 0o600)
	if got, err := readPackages(path); err != nil || len(got) != 1 {
		t.Errorf("%v %v", got, err)
	}
}

func TestCacheFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "disk")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := cacheFile(filepath.Join(dir, "c"), file, "sha256:zz", domain.ImageVHDX); err == nil {
		t.Error("a bad digest named a file")
	}
	blocker := filepath.Join(dir, "blocker")
	_ = os.WriteFile(blocker, nil, 0o600)
	if _, err := cacheFile(filepath.Join(blocker, "c"), file, "sha256:"+strings.Repeat("a", 64), domain.ImageVHDX); err == nil {
		t.Error("a cache under a file")
	}
	if _, err := cacheFile(filepath.Join(dir, "c2"), filepath.Join(dir, "missing"), "sha256:"+strings.Repeat("b", 64), domain.ImageVHDX); err == nil {
		t.Error("a missing file cached")
	}
}

func TestDefaultExecRunsACommand(t *testing.T) {
	var out strings.Builder
	if err := execCommand(context.Background(), t.TempDir(), &out, os.Environ(), "sh", "-c", "echo hi; echo err >&2"); err != nil {
		t.Skipf("no sh: %v", err)
	}
	if !strings.Contains(out.String(), "hi") || !strings.Contains(out.String(), "err") {
		t.Errorf("output = %q", out.String())
	}
	if err := execCommand(context.Background(), t.TempDir(), io.Discard, nil, "sh", "-c", "exit 3"); err == nil {
		t.Error("a failing command succeeded")
	}
	b := Builder{}
	b.fill()
	if b.Exec == nil || b.Now == nil || (Options{}).packer() != "packer" || (Options{Packer: "/p"}).packer() != "/p" {
		t.Error("defaults")
	}
}

// The labels the manifest declares are the ones the image writes for itself.
func TestLabelsMatchTheTemplate(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/images/windows/provision-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)Set-Content -LiteralPath \(Join-Path \$root 'labels'\) -Value @\(([^)]*)\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("provision-windows.ps1 writes no labels")
	}
	var got []string
	for f := range strings.SplitSeq(string(m[1]), ",") {
		got = append(got, strings.Trim(strings.TrimSpace(f), "'"))
	}
	if !slices.Equal(got, Labels) {
		t.Errorf("template labels %v, manifest labels %v", got, Labels)
	}
}

package fleetagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/imagebuild"
)

// jobRig is a host with templates, an ISO directory and a worker, and a fake
// Packer that leaves what a build leaves.
type jobRig struct {
	t      *testing.T
	root   string
	runner fleetagent.ImageRunner
	req    fleet.ImageBuildRequest
	mu     sync.Mutex
	calls  [][]string
	// during runs inside the fake Packer's build, to look at what the agent does meanwhile.
	during func()
	fail   bool
}

func newJobRig(t *testing.T) *jobRig {
	t.Helper()
	root := t.TempDir()
	put := func(path string, data []byte) string {
		p := filepath.Join(root, path)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, data, 0o600))
		return p
	}
	put("images/windows/windows.pkr.hcl", []byte("# template"))
	pe := make([]byte, 128)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x40)
	copy(pe[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(pe[0x44:], 0x8664)
	worker := put("forecrew-worker.exe", pe)
	body := []byte("pretend this is a Windows ISO")
	iso := put("isos/win.iso", body)
	sum := sha256.Sum256(body)
	r := &jobRig{t: t, root: root}
	r.req = fleet.ImageBuildRequest{ID: uuid.New(), State: domain.ImageBuildRequested, Version: "2026.11.1", ISO: iso, ISOChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		LicenseConfirmed: true, ChocolateySHA256: strings.Repeat("ab", 32)}
	r.runner = fleetagent.ImageRunner{
		Settings: config.FleetAgentImageBuild{TemplatesDir: filepath.Join(root, "images"), Worker: worker, OutDir: filepath.Join(root, "out"), URIBase: "https://images.example.com", ISODir: filepath.Join(root, "isos")},
		Builder: imagebuild.Builder{
			DiskFreeGB: func(string) (int, error) { return 500, nil },
			Exec:       r.exec,
		},
		GOOS: "linux", Look: time.Millisecond, Heartbeat: 5 * time.Millisecond,
	}
	return r
}

func (r *jobRig) exec(_ context.Context, _ string, w io.Writer, _ []string, name string, args ...string) error {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	_, _ = io.WriteString(w, "packer says hello\n")
	if args[0] != "build" {
		return nil
	}
	if r.during != nil {
		r.during()
	}
	if r.fail {
		return errors.New("exit status 1")
	}
	out := filepath.Join(r.root, "out", r.req.ID.String())
	file := filepath.Join(out, "windows-qcow2", "forecrew-windows-2026.11.1-amd64.qcow2")
	must(r.t, os.MkdirAll(filepath.Dir(file), 0o755))
	must(r.t, os.WriteFile(file, []byte("the built disk"), 0o600))
	return os.WriteFile(filepath.Join(out, "packages-windows-amd64.txt"), []byte("git=2.56.0.4\n"), 0o600)
}

// build runs a build, returning what it reported on the way and its end.
func (r *jobRig) build() (reports []fleet.ImageBuildReport, end fleet.ImageBuildReport) {
	var mu sync.Mutex
	end = r.runner.Build(r.t.Context(), r.req, func(p fleet.ImageBuildReport) {
		mu.Lock()
		reports = append(reports, p)
		mu.Unlock()
	})
	mu.Lock()
	defer mu.Unlock()
	return slicesClone(reports), end
}

func slicesClone(in []fleet.ImageBuildReport) []fleet.ImageBuildReport {
	return append([]fleet.ImageBuildReport{}, in...)
}

func TestRunnerBuildsFromTheRequestAndTheHostsSettings(t *testing.T) {
	r := newJobRig(t)
	// Hold the build open until the runner has read its progress twice.
	r.during = func() { time.Sleep(40 * time.Millisecond) }
	reports, end := r.build()
	if end.State != domain.ImageBuildDone || end.Percent != 100 || end.Step != "done" || !strings.HasSuffix(end.Manifest, "forecrew-windows-2026.11.1.manifest.json") || !strings.HasPrefix(end.Digest, "sha256:") {
		t.Fatalf("end %+v", end)
	}
	if !strings.Contains(end.Manifest, r.req.ID.String()) {
		t.Errorf("a build has a directory of its own: %s", end.Manifest)
	}
	if _, err := os.Stat(end.Manifest); err != nil {
		t.Errorf("the unsigned manifest is on the host: %v", err)
	}
	if reports[0].State != domain.ImageBuildRunning || reports[0].Step != "starting" {
		t.Fatalf("first report %+v", reports[0])
	}
	var building int
	for _, p := range reports {
		if p.Step == "building the image: about an hour" && p.Percent == 15 {
			building++
		}
		if p.State != domain.ImageBuildRunning {
			t.Errorf("a report before the end is %+v", p)
		}
	}
	if building < 2 {
		t.Errorf("the build step is reported again as a heartbeat: %+v", reports)
	}
	if got := len(r.calls); got != 2 || r.calls[0][1] != "init" || r.calls[1][1] != "build" {
		t.Fatalf("packer calls %v", r.calls)
	}
}

func TestRunnerReportsAFailureWithItsPercent(t *testing.T) {
	r := newJobRig(t)
	r.fail = true
	_, end := r.build()
	if end.State != domain.ImageBuildFailed || end.Percent != 15 || !strings.Contains(end.Error, "packer build") {
		t.Fatalf("end %+v", end)
	}
}

func TestRunnerKeepsTheChecksOfTheBuild(t *testing.T) {
	for name, tt := range map[string]struct {
		edit func(r *jobRig)
		want string
	}{
		"no licence":        {func(r *jobRig) { r.req.LicenseConfirmed = false }, "licence"},
		"another checksum":  {func(r *jobRig) { r.req.ISOChecksum = "sha256:" + strings.Repeat("0", 64) }, "not the"},
		"a bad version":     {func(r *jobRig) { r.req.Version = "../x" }, "version"},
		"no chocolatey":     {func(r *jobRig) { r.req.ChocolateySHA256 = "" }, "chocolatey"},
		"a missing ISO":     {func(r *jobRig) { r.req.ISO = filepath.Join(r.root, "isos", "none.iso") }, "ISO is not at the path"},
		"outside iso_dir":   {func(r *jobRig) { r.req.ISO = filepath.Join(r.root, "forecrew-worker.exe") }, "image_build.iso_dir"},
		"through ..":        {func(r *jobRig) { r.req.ISO = filepath.Join(r.root, "isos", "..", "forecrew-worker.exe") }, "image_build.iso_dir"},
		"the directory":     {func(r *jobRig) { r.req.ISO = filepath.Join(r.root, "isos") }, "regular file"},
		"no iso_dir at all": {func(r *jobRig) { r.runner.Settings.ISODir = filepath.Join(r.root, "nowhere") }, "iso_dir"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newJobRig(t)
			tt.edit(r)
			_, end := r.build()
			if end.State != domain.ImageBuildFailed || !strings.Contains(end.Error, tt.want) {
				t.Fatalf("end %+v, want %q", end, tt.want)
			}
			if len(r.calls) != 0 {
				t.Errorf("packer ran: %v", r.calls)
			}
		})
	}
}

func TestRunnerRefusesALinkOutOfTheISODirectory(t *testing.T) {
	r := newJobRig(t)
	link := filepath.Join(r.root, "isos", "link.iso")
	if err := os.Symlink(filepath.Join(r.root, "forecrew-worker.exe"), link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	r.req.ISO = link
	_, end := r.build()
	if end.State != domain.ImageBuildFailed || !strings.Contains(end.Error, "image_build.iso_dir") {
		t.Fatalf("end %+v", end)
	}
}

func TestRunnerRefusesAnISOThatIsNotARegularFile(t *testing.T) {
	r := newJobRig(t)
	dir := filepath.Join(r.root, "isos", "dir.iso")
	must(t, os.Mkdir(dir, 0o755))
	r.req.ISO = dir
	_, end := r.build()
	if end.State != domain.ImageBuildFailed || !strings.Contains(end.Error, "regular file") {
		t.Fatalf("end %+v", end)
	}
	if fifo := filepath.Join(r.root, "isos", "pipe.iso"); mkfifo(fifo) == nil {
		r.req.ISO = fifo // opening it would block until someone writes
		done := make(chan fleet.ImageBuildReport, 1)
		go func() { _, e := r.build(); done <- e }()
		select {
		case e := <-done:
			if e.State != domain.ImageBuildFailed || !strings.Contains(e.Error, "regular file") {
				t.Fatalf("pipe: %+v", e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a pipe in iso_dir blocked the agent")
		}
	}
}

func TestRunnerCleansUpAStoppedBuild(t *testing.T) {
	r := newJobRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	r.runner.Builder.Exec = func(ctx context.Context, _ string, w io.Writer, _ []string, _ string, args ...string) error {
		if args[0] != "build" {
			return nil
		}
		_, _ = io.WriteString(w, "building\n")
		cancel() // forecrewd ended the build
		<-ctx.Done()
		return ctx.Err()
	}
	end := r.runner.Build(ctx, r.req, func(fleet.ImageBuildReport) {})
	if end.State != domain.ImageBuildFailed {
		t.Fatalf("end %+v", end)
	}
	if _, err := os.Stat(filepath.Join(r.root, "out", r.req.ID.String())); !os.IsNotExist(err) {
		t.Fatalf("the output folder of a stopped build is still there: %v", err)
	}
}

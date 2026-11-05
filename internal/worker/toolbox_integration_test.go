//go:build docker && !windows

package worker

import (
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/runtime"
)

// These tests build the toolbox images of packs/toolboxes on the local Docker
// engine: the first run downloads the scanners and takes a few minutes.

// toolboxWorker returns a worker on the real engine with the toolbox files of
// the packs, running the scripted runtime of sc.
func toolboxWorker(t *testing.T, sc runtime.Script) *testEnv {
	t.Helper()
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil { //nolint:gosec // fixed command
		t.Skipf("no Docker engine: %v: %s", err, out)
	}
	e := newEnv(t, scriptAdapter(t, sc))
	files, err := fs.Sub(forecrew.Files, "packs/toolboxes")
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.DockerBin, e.cfg.Toolboxes = "docker", files
	e.rebuild()
	t.Cleanup(func() {
		_ = e.w.docker.removeLeftovers(context.Background())
		removeToolboxImages(e)
	})
	return e
}

// removeToolboxImages removes the toolbox images the worker of e used, which
// its markers name. The base image goes after the images built on it. Docker's
// build cache stays, so building them again is quick.
func removeToolboxImages(e *testEnv) {
	marks, _ := os.ReadDir(string(e.w.imageMarkers()))
	var refs []string
	for _, m := range marks {
		ref, _ := os.ReadFile(filepath.Join(string(e.w.imageMarkers()), m.Name())) //nolint:gosec // test path
		if strings.HasPrefix(string(ref), toolboxImageRepo+":") {
			refs = append(refs, string(ref))
		}
	}
	for range 2 {
		for _, ref := range refs {
			_ = exec.Command("docker", "image", "rm", "--", ref).Run() //nolint:gosec // test command
		}
	}
}

// packToolbox returns a toolbox of packs/toolboxes.
func packToolbox(t *testing.T, name string) domain.Toolbox {
	t.Helper()
	all, err := roles.LoadToolboxes(forecrew.Files)
	if err != nil {
		t.Fatal(err)
	}
	box, ok := all[name]
	if !ok {
		t.Fatalf("no toolbox %s in the packs", name)
	}
	return box
}

// fakeSecret is a token the scanner recognizes, assembled here so that no
// scanner of this repo sees it.
func fakeSecret() string { return "gh" + "p_R2q9T5xK8mN3vB7cL1zD4fG6hJ0wS2yU8aE5" }

func TestSecurityToolboxScansTheWorkTree(t *testing.T) {
	// The server listens on all interfaces, as a forecrewd must to be reached
	// from a container when it runs on the worker's machine.
	var gateway string
	e := toolboxWorker(t, runtime.Script{})
	srv := httptest.NewUnstartedServer(e.api.srv.Config.Handler)
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot listen on all interfaces: %v", err)
	}
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(l.Addr().String())
	gateway = "http://host.docker.internal:" + port + "/job-mcp"
	e.cfg.Client = NewClient("http://127.0.0.1:"+port, testWorkerTok, nil)
	e.cfg.Client.BackoffMin, e.cfg.Client.BackoffMax = time.Millisecond, 5*time.Millisecond
	e.cfg.Runtimes = runtime.NewRegistry(scriptAdapter(t, runtime.Script{Commands: []runtime.ScriptCommand{
		{Argv: []string{"gitleaks", "dir", ".", "--no-banner", "--redact", "--report-format", "json", "--report-path", "findings.json"}},
		{Argv: []string{"sh", "-c", `echo "uid=$(id -u) home=$HOME"; ls /var/run/docker.sock /run/docker.sock 2>&1; env | cut -d= -f1 | sort | tr '\n' ' '; echo`}},
		{Argv: []string{"sh", "-c", "curl -sS -m 20 -o /dev/null -w 'gateway=%{http_code}' " + gateway}},
		{Argv: []string{"sh", "-c", "claude --version; codex --version; semgrep --version; trivy --version | head -1; osv-scanner --version | head -1"}},
	}}))
	e.rebuild()
	pushFile(t, e.remote, "fixtures/config.txt", `token = "`+fakeSecret()+"\"\n")

	s := e.spec(func(s *domain.JobSpec) {
		box := packToolbox(t, "security")
		s.Toolbox, s.Timeout = &box, domain.Duration(10*time.Minute)
		s.JobMCP = &domain.JobMCP{Token: testJobMCPTok}
	})
	c := e.doComplete(s)
	logs := strings.Join(logsOf(e, s.JobID), "\n")
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v\nlogs:\n%s", c, logs)
	}

	// gitleaks found the secret; the report it wrote in the container is a
	// change of the work tree that the worker checked and pushed.
	if !strings.Contains(logs, "leaks found: 1") || !strings.Contains(logs, "exit code 1") {
		t.Errorf("gitleaks did not report the secret:\n%s", logs)
	}
	var findings []struct{ RuleID, File string }
	if err := json.Unmarshal([]byte(runGit(t, e.remote, "show", "agent/task-1:findings.json")), &findings); err != nil || len(findings) != 1 || findings[0].File != "fixtures/config.txt" {
		t.Errorf("pushed findings = %+v, %v", findings, err)
	}
	if strings.Contains(runGit(t, e.remote, "show", "agent/task-1:findings.json"), fakeSecret()) {
		t.Error("the report holds the secret itself")
	}

	// The container: the worker's uid, an empty home, no Docker socket, only
	// the job's variables, and forecrewd reachable.
	for _, want := range []string{"uid=" + strconv.Itoa(os.Getuid()), "home=" + runtime.ContainerHome, "No such file", "FORECREW_JOB_MCP_TOKEN", "ANTHROPIC_API_KEY", "gateway=404", "Claude Code", "codex-cli", "1."} {
		if !strings.Contains(logs, want) {
			t.Errorf("job log lacks %q:\n%s", want, logs)
		}
	}
	for _, bad := range []string{testRepoToken, "GITHUB_TOKEN", "FORECREW_WORKER_TOKEN", "srw"} {
		if strings.Contains(logs, bad) {
			t.Errorf("job log has %q:\n%s", bad, logs)
		}
	}
	for _, cli := range []string{"semgrep: ", "gitleaks: 8.", "osv-scanner: ", "trivy: "} {
		if !strings.Contains(logs, "toolbox security cli "+cli) {
			t.Errorf("no version line for %q in:\n%s", cli, logs)
		}
	}
	if left := dockerObjects(t, s.JobID); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
	e.noSecrets()

	// A second job reuses the images.
	s2 := e.spec(func(s2 *domain.JobSpec) {
		box := packToolbox(t, "security")
		s2.Toolbox, s2.Branch = &box, "agent/task-2"
	})
	if c := e.doComplete(s2); c.Status != StatusSucceeded {
		t.Fatalf("second completion = %+v\nlogs: %q", c, logsOf(e, s2.JobID))
	}
	if st := e.api.statuses(s2.JobID); !strings.Contains(strings.Join(st, ","), "toolbox image cached") || strings.Contains(strings.Join(st, ","), "building toolbox image") {
		t.Errorf("second job statuses = %v", st)
	}
}

package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// toolboxFiles are the toolbox files of the tests: a base image and a
// toolbox that starts from it.
func toolboxFiles() fstest.MapFS {
	return fstest.MapFS{
		"base/Dockerfile":     {Data: []byte("FROM node:24\nCOPY package.json /opt/\n")},
		"base/package.json":   {Data: []byte("{}")},
		"security/Dockerfile": {Data: []byte("ARG FORECREW_BASE\nFROM ${FORECREW_BASE}\nRUN true\n")},
		"plain/Dockerfile":    {Data: []byte("FROM alpine:3\n")},
	}
}

var securityBox = domain.Toolbox{
	Name: "security", Dockerfile: "security/Dockerfile",
	CLIs: []domain.ToolCLI{{Name: "echo", Version: "echo echo 1.0"}, {Name: "gone", Version: "gone-cli --version"}},
	MCPServers: []domain.MCPServer{
		{Name: "scanner", Command: "scan", Args: []string{"--stdio"}},
	},
}

// toolboxEngine is a fake Docker engine for toolbox jobs: images exist once
// built or pulled, and `docker run` runs the command on the host, in the
// directory given with -w, with the -e variables exported (a bare -e NAME is
// the client's own variable). What the real engine isolates is checked by
// the Docker integration tests.
type toolboxEngine struct{ *engine }

func newToolboxEngine(t *testing.T) *toolboxEngine {
	t.Helper()
	state := t.TempDir()
	cases := `
version) echo 29.0.0;;
image) if [ "$2" = inspect ]; then for a; do last=$a; done; [ -e ` + state + `/img-$(echo "$last" | tr '/:' '__') ] && echo sha256:x || exit 1; fi;;
pull) [ -e ` + state + `/fail-pull ] && { echo "pull broke" >&2; exit 1; }; for a; do last=$a; done; touch ` + state + `/img-$(echo "$last" | tr '/:' '__');;
build) [ -e ` + state + `/fail-build ] && { echo "build broke" >&2; exit 1; }
  prev=; for a; do [ "$prev" = --tag ] && touch ` + state + `/img-$(echo "$a" | tr '/:' '__'); prev=$a; done;;
ps) echo c1;;
network) if [ "$2" = ls ]; then echo n1; fi;;
run)
  [ -e ` + state + `/run125 ] && { echo "docker: Error response from daemon: boom" >&2; exit 125; }
  shift; wd=.
  while [ $# -gt 0 ]; do
    case "$1" in
      -w) wd=$2; shift 2;;
      -e) case "$2" in *=*) export "$2";; esac; shift 2;;
      --) shift; break;;
      -v|--tmpfs|--name|--label|--user|--cap-drop|--security-opt|--pids-limit|--entrypoint|--network|--shm-size|--add-host) shift 2;;
      *) shift;;
    esac
  done
  shift
  cd "$wd" && exec "$@";;`
	bin, log := fakeDocker(t, cases)
	return &toolboxEngine{&engine{bin: bin, log: log, state: state}}
}

// toolboxEnv is a worker with a fake engine whose scripted adapter runs fn
// with the job's container.
func toolboxEnv(t *testing.T, fn func(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error)) (*testEnv, *toolboxEngine) {
	t.Helper()
	g := newToolboxEngine(t)
	e := newEnv(t, funcAdapter{fn})
	e.cfg.DockerBin, e.cfg.Toolboxes = g.bin, toolboxFiles()
	e.rebuild()
	return e, g
}

func toolboxSpec(e *testEnv, tb domain.Toolbox, mod ...func(*domain.JobSpec)) domain.JobSpec {
	return e.spec(append([]func(*domain.JobSpec){func(s *domain.JobSpec) { s.Toolbox = &tb }}, mod...)...)
}

// inBox runs a shell script in the container of spec and returns its output
// lines.
func inBox(ctx context.Context, spec runtime.RunSpec, script string) ([]string, error) {
	var out []string
	var mu sync.Mutex
	p := spec.Container.Wrap(runtime.Process{Path: "sh", Args: []string{"-c", script}, Dir: spec.WorkDir, Env: envList(spec.Env)})
	exit, err := runtime.Exec(ctx, p, func(b []byte) { mu.Lock(); out = append(out, string(b)); mu.Unlock() }, nil)
	if errors.Is(err, runtime.ErrRunFailed) { // as the adapters report a failed CLI
		err = fmt.Errorf("%w: %s: %s", runtime.ErrRunFailed, strings.TrimPrefix(err.Error(), runtime.ErrRunFailed.Error()+": "), exit.Stderr)
	}
	return out, err
}

func envList(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func TestToolboxJobRunsInTheContainer(t *testing.T) {
	var seen []string
	var gotSpec runtime.RunSpec
	e, g := toolboxEnv(t, func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		gotSpec = spec
		var err error
		seen, err = inBox(ctx, spec, `echo "key=[$ANTHROPIC_API_KEY] home=$HOME evidence=$FORECREW_EVIDENCE_DIR"; pwd; echo found > out.txt; echo hi > "$FORECREW_EVIDENCE_DIR/shot.txt"`)
		return runtime.Result{}, err
	})
	s := toolboxSpec(e, securityBox)
	c := e.doComplete(s)
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v\nlogs: %q", c, logsOf(e, s.JobID))
	}
	// The files the agent wrote in the container are checked and pushed by the
	// worker like those of a host job.
	if got := runGit(t, e.remote, "show", "agent/task-1:out.txt"); got != "found" {
		t.Errorf("pushed out.txt = %q", got)
	}
	dir := filepath.Join(e.workDir, "jobs", s.JobID.String())
	if want := "key=[" + testAPIKey + "] home=" + runtime.ContainerHome + " evidence=" + dir + ".evidence"; len(seen) < 2 || seen[0] != want || seen[1] != dir {
		t.Errorf("the job saw %q, want %q and %q", seen, want, dir)
	}
	if gotSpec.Container == nil || len(gotSpec.MCPServers) != 1 || gotSpec.WorkDir != dir {
		t.Errorf("run spec = %+v", gotSpec)
	}

	got := calls(t, g.log)
	id := s.JobID.String()
	cache := filepath.Join(e.workDir, "repos", "acme", "app.git")
	for _, parts := range [][]string{
		{"build --tag forecrew-toolbox:base-", "/Dockerfile"},
		{"build --tag forecrew-toolbox:security-", "--build-arg FORECREW_BASE=forecrew-toolbox:base-", "security/Dockerfile"},
		{"network create", "--label forecrew.job=" + id},
		{"run --rm --init", "--label forecrew.job=" + id, " -i ", "--user", "--cap-drop ALL", "--security-opt no-new-privileges",
			"--pids-limit 4096", "--entrypoint ", "--network forecrew-" + id, "--shm-size 512m",
			"--tmpfs " + runtime.ContainerHome + ":rw,exec,nosuid,nodev,mode=0700,uid=", "--tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777",
			"-v " + dir + ":" + dir + " ", "-v " + dir + ".evidence:" + dir + ".evidence ", "-v " + cache + ":" + cache + ":ro ", "-w " + dir,
			"-e ANTHROPIC_API_KEY ", "-e HOME=" + runtime.ContainerHome, "-e GIT_CONFIG_NOSYSTEM=1", "-- forecrew-toolbox:security-"},
		{"rm -f -v c1"},
		{"network rm n1"},
	} {
		if !hasCall(got, parts...) {
			t.Errorf("no call with %q in\n%s", parts, strings.Join(got, "\n"))
		}
	}
	// A secret is never in a command line, and neither is the repo token.
	for _, c := range got {
		for _, secret := range []string{testAPIKey, testRepoToken, testJobMCPTok} {
			if strings.Contains(c, secret) {
				t.Errorf("a docker call has a secret: %s", c)
			}
		}
		if strings.Contains(c, "docker.sock") || strings.Contains(c, "--privileged") || strings.Contains(c, "--network host") || strings.Contains(c, "SYS_ADMIN") {
			t.Errorf("a docker call widens the container: %s", c)
		}
	}
	logs := logsOf(e, s.JobID)
	if !slices.Contains(logs, "toolbox security cli echo: echo 1.0") || !slices.ContainsFunc(logs, func(l string) bool { return strings.HasPrefix(l, "toolbox security cli gone: ") }) {
		t.Errorf("CLI versions in the job log: %q", logs)
	}
	e.noSecrets()
}

func TestToolboxImageIsReused(t *testing.T) {
	e, g := toolboxEnv(t, func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		return runtime.Result{}, nil
	})
	run := func() []string {
		if c := e.doComplete(toolboxSpec(e, securityBox)); c.Status != StatusSucceeded {
			t.Fatalf("completion = %+v", c)
		}
		return calls(t, g.log)
	}
	first := run()
	if n := countCalls(first, "build "); n != 2 {
		t.Errorf("first job built %d images, want the base and the toolbox:\n%s", n, strings.Join(first, "\n"))
	}
	second := run()[len(first):]
	if hasCall(second, "build") || hasCall(second, "pull") {
		t.Errorf("second job rebuilt the image:\n%s", strings.Join(second, "\n"))
	}
	// Every use is recorded, so that pruning finds the images.
	if ents, err := os.ReadDir(filepath.Join(e.workDir, "env-images")); err != nil || len(ents) != 2 {
		t.Errorf("image markers = %v, %v (want the toolbox and the base)", ents, err)
	}
	// Changing a toolbox file builds a new image.
	e.cfg.Toolboxes = toolboxFiles()
	e.cfg.Toolboxes.(fstest.MapFS)["security/Dockerfile"] = &fstest.MapFile{Data: []byte("ARG FORECREW_BASE\nFROM ${FORECREW_BASE}\nRUN false\n")}
	e.rebuild()
	third := run()
	if n := countCalls(third[len(first)+len(second):], "build "); n != 1 {
		t.Errorf("a changed Dockerfile built %d images, want the toolbox only", n)
	}
}

func countCalls(got []string, part string) int {
	n := 0
	for _, c := range got {
		if strings.Contains(c, part) {
			n++
		}
	}
	return n
}

func TestToolboxPullsAnImage(t *testing.T) {
	e, g := toolboxEnv(t, func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		return runtime.Result{}, nil
	})
	s := toolboxSpec(e, domain.Toolbox{Name: "ready", Image: "ghcr.io/x/ready:1"})
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	got := calls(t, g.log)
	if !hasCall(got, "pull -- ghcr.io/x/ready:1") || !hasCall(got, "-- ghcr.io/x/ready:1") {
		t.Errorf("calls:\n%s", strings.Join(got, "\n"))
	}
	if hasCall(got, "build") {
		t.Errorf("an image toolbox was built:\n%s", strings.Join(got, "\n"))
	}
}

func TestToolboxJobFailures(t *testing.T) {
	tests := []struct {
		name   string
		flags  []string
		box    domain.Toolbox
		mod    func(*testEnv)
		status string
		want   string
	}{
		{"build fails", []string{"fail-build"}, securityBox, nil, StatusFailed, "toolbox security: base image: docker build"},
		{"pull fails", []string{"fail-pull"}, domain.Toolbox{Name: "r", Image: "x:1"}, nil, StatusFailed, "toolbox r: "},
		{"missing dockerfile", nil, domain.Toolbox{Name: "nope", Dockerfile: "nope/Dockerfile"}, nil, StatusFailed, "dockerfile nope/Dockerfile"},
		{"no toolbox files", nil, securityBox, func(e *testEnv) { e.cfg.Toolboxes = nil }, StatusFailed, "no toolbox files"},
		{"docker run fails", []string{"run125"}, securityBox, nil, StatusRetry, "boom"},
		{"no docker", nil, securityBox, func(e *testEnv) { e.cfg.DockerBin = "" }, StatusRetry, "no Docker engine"},
		{"root worker", nil, securityBox, func(*testEnv) { hostIDs = func() (int, int) { return 0, 0 } }, StatusFailed, "runs as root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() { hostIDs = func() (int, int) { return os.Getuid(), os.Getgid() } })
			e, g := toolboxEnv(t, func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
				_, err := inBox(ctx, spec, "true")
				return runtime.Result{}, err
			})
			g.set(t, tt.flags...)
			if tt.mod != nil {
				tt.mod(e)
				e.rebuild()
			}
			s := toolboxSpec(e, tt.box)
			c := e.doComplete(s)
			if c.Status != tt.status || !strings.Contains(c.Error, tt.want) {
				t.Fatalf("completion = %+v, want %s containing %q", c, tt.status, tt.want)
			}
			if tt.status == StatusFailed && c.HeadSHA != "" {
				t.Errorf("a failed job pushed %q", c.HeadSHA)
			}
			if tt.mod == nil && tt.flags != nil && !hasCall(calls(t, g.log), "ps -aq --filter label=forecrew.job="+s.JobID.String()) {
				t.Error("a failed job was not cleaned up")
			}
		})
	}
}

func TestToolboxJobTakesTheAgentErrorsAsFailures(t *testing.T) {
	e, _ := toolboxEnv(t, func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		_, err := inBox(ctx, spec, "exit 3")
		return runtime.Result{}, err
	})
	c := e.doComplete(toolboxSpec(e, securityBox))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "exit status 3") {
		t.Fatalf("completion = %+v", c)
	}
}

func TestToolboxGatewayURL(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    string
		viaHost bool
	}{
		{"http://localhost:8080/job-mcp", "http://host.docker.internal:8080/job-mcp", true},
		{"http://127.0.0.1:8080/job-mcp", "http://host.docker.internal:8080/job-mcp", true},
		{"https://[::1]:8443/job-mcp", "https://host.docker.internal:8443/job-mcp", true},
		{"http://localhost/job-mcp", "http://host.docker.internal/job-mcp", true},
		{"https://forecrew.example.com/job-mcp", "https://forecrew.example.com/job-mcp", false},
		{"http://10.0.0.5:8080/job-mcp", "http://10.0.0.5:8080/job-mcp", false},
		{"http://forecrewd:8080/job-mcp", "http://forecrewd:8080/job-mcp", false},
		{"://bad", "://bad", false},
	} {
		got, via := containerURL(tt.in)
		if got != tt.want || via != tt.viaHost {
			t.Errorf("containerURL(%q) = %q, %v; want %q, %v", tt.in, got, via, tt.want, tt.viaHost)
		}
	}
}

func TestToolboxGatewayInTheContainer(t *testing.T) {
	var url string
	e, g := toolboxEnv(t, func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		url = spec.JobMCP.URL
		out, err := inBox(ctx, spec, `echo "token=[$FORECREW_JOB_MCP_TOKEN]"`)
		if len(out) != 1 || out[0] != "token=["+testJobMCPTok+"]" {
			t.Errorf("the token in the container: %q", out)
		}
		return runtime.Result{}, err
	})
	s := toolboxSpec(e, securityBox, func(s *domain.JobSpec) { s.JobMCP = &domain.JobMCP{Token: testJobMCPTok} })
	// The fake API is on 127.0.0.1: the container is told to use the host.
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if !strings.HasPrefix(url, "http://host.docker.internal:") || !strings.HasSuffix(url, "/job-mcp") {
		t.Errorf("gateway URL for the container = %q", url)
	}
	got := calls(t, g.log)
	if !hasCall(got, "--add-host host.docker.internal:host-gateway", "-e FORECREW_JOB_MCP_TOKEN ") {
		t.Errorf("calls:\n%s", strings.Join(got, "\n"))
	}
	for _, c := range got {
		if strings.Contains(c, testJobMCPTok) {
			t.Errorf("the job MCP token is in a command line: %s", c)
		}
	}
	e.noSecrets()
}

func TestToolboxHasNoHostGatewayForAPublicServer(t *testing.T) {
	e, g := toolboxEnv(t, func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		return runtime.Result{}, nil
	})
	// The server is forecrew.example.com; its requests go to the fake API.
	api := e.api.srv.Listener.Addr().String()
	e.cfg.Client = NewClient("http://forecrew.example.com", testWorkerTok, &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, api)
		},
	}})
	e.cfg.Client.BackoffMin, e.cfg.Client.BackoffMax = time.Millisecond, 5*time.Millisecond
	e.rebuild()
	s := toolboxSpec(e, securityBox)
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if hasCall(calls(t, g.log), "--add-host") {
		t.Error("a public server needs no host gateway")
	}
}

func TestToolboxFiles(t *testing.T) {
	files := fstest.MapFS{"a/Dockerfile": {Data: []byte("FROM x"), Mode: 0o644}, "run.sh": {Data: []byte("#!/bin/sh"), Mode: 0o755}}
	dir := t.TempDir()
	if err := extractFS(files, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a", "Dockerfile")); err != nil || string(b) != "FROM x" {
		t.Errorf("extracted Dockerfile = %q, %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Errorf("run.sh = %v, %v (want it executable)", info, err)
	}
	var h1, h2, h3 strings.Builder
	if err := hashFS(&h1, files); err != nil {
		t.Fatal(err)
	}
	if err := hashFS(&h2, files); err != nil || h1.String() != h2.String() {
		t.Errorf("hash is not stable: %v", err)
	}
	files["a/Dockerfile"] = &fstest.MapFile{Data: []byte("FROM y"), Mode: 0o644}
	if err := hashFS(&h3, files); err != nil || h3.String() == h1.String() {
		t.Errorf("hash ignores contents: %v", err)
	}
	files["link"] = &fstest.MapFile{Data: []byte("a"), Mode: 0o777 | os.ModeSymlink}
	if err := hashFS(&h3, files); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symlink was hashed: %v", err)
	}
	delete(files, "link")
	defer func(n int) { maxToolboxFiles = n }(maxToolboxFiles)
	maxToolboxFiles = 1
	if err := hashFS(&h3, files); err == nil || !strings.Contains(err.Error(), "more than 1 toolbox files") {
		t.Errorf("too many files: %v", err)
	}
}

func TestToolboxRefusesReservedCredentialNames(t *testing.T) {
	for _, name := range []string{
		"PATH", "HOME", "path", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "docker_config",
		"XDG_RUNTIME_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH", "HTTPS_PROXY", "http_proxy", "NO_PROXY", "ALL_PROXY", "LC_ALL", "TMPDIR",
	} {
		if !reservedCredential(name) {
			t.Errorf("%s is allowed as a credential name", name)
		}
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "CODEX_API_KEY", "SCAN_KEY", "FORECREW_JOB_MCP_TOKEN", "LDAP_PASSWORD", "PROXYPASS"} {
		if reservedCredential(name) {
			t.Errorf("%s is refused as a credential name", name)
		}
	}
	e, g := toolboxEnv(t, func(context.Context, runtime.RunSpec, func(domain.JobEvent)) (runtime.Result, error) {
		t.Error("the agent ran")
		return runtime.Result{}, nil
	})
	s := toolboxSpec(e, securityBox)
	s.Env["LD_PRELOAD"] = "/x.so"
	c := e.doComplete(s)
	if c.Status != StatusFailed || !strings.Contains(c.Error, "LD_PRELOAD") {
		t.Errorf("completion = %+v", c)
	}
	if hasCall(calls(t, g.log), "run --rm") {
		t.Error("a container was started")
	}
}

// named gives an adapter another name, so that a spec can ask for a real one.
type named struct {
	funcAdapter
	name string
}

func (n named) Name() string { return n.name }

func TestToolboxForwardsTheWorkersOwnKey(t *testing.T) {
	var out []string
	adapter := named{funcAdapter{func(ctx context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		var err error
		out, err = inBox(ctx, spec, `echo "key=[$ANTHROPIC_API_KEY]"`)
		return runtime.Result{}, err
	}}, runtime.ClaudeCode}
	g := newToolboxEngine(t)
	e := newEnv(t, adapter)
	e.cfg.DockerBin, e.cfg.Toolboxes = g.bin, toolboxFiles()
	e.cfg.Environ = append(os.Environ(), "ANTHROPIC_API_KEY="+testAPIKey)
	e.rebuild()
	// The spec has no key: the worker's own goes into the container, by name.
	s := toolboxSpec(e, securityBox, func(s *domain.JobSpec) { s.Runtime, s.Env = runtime.ClaudeCode, nil })
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if len(out) != 1 || out[0] != "key=["+testAPIKey+"]" {
		t.Errorf("the job saw %q", out)
	}
	for _, c := range calls(t, g.log) {
		if strings.Contains(c, testAPIKey) {
			t.Errorf("the key is in a command line: %s", c)
		}
	}
	if !hasCall(calls(t, g.log), "-e ANTHROPIC_API_KEY ") {
		t.Error("the key was not passed by name")
	}
}

func TestToolboxContainerIsGoneBeforeTheEvidenceIsRead(t *testing.T) {
	e, g := toolboxEnv(t, func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		return runtime.Result{}, os.WriteFile(filepath.Join(spec.EvidenceDir, "shot.txt"), []byte("evidence"), 0o600)
	})
	s := toolboxSpec(e, securityBox, func(s *domain.JobSpec) { s.JobMCP = &domain.JobMCP{Token: testJobMCPTok} })
	e.api.attach[s.JobID] = []Attachment{{Path: runtime.EvidencePrefix + "shot.txt", Name: "shot"}}
	var removedFirst bool
	e.api.onUpload = func(string) { removedFirst = hasCall(calls(t, g.log), "rm -f -v c1") }
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if got := e.api.uploaded()["shot"]; got != "evidence" {
		t.Fatalf("uploads = %v", e.api.uploaded())
	}
	if !removedFirst {
		t.Error("the container was still there when the evidence was uploaded")
	}
}

package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// engine is a fake Docker engine for the job tests: image, network and
// container calls are recorded and answered from files in state (a file named
// after a behaviour switches it on), and `docker run` of the job container
// runs the command on the host, in the directory mounted at /work, with the
// -e variables exported. What the real engine isolates is checked by the
// integration tests.
type engine struct {
	bin, log, state string
}

func newEngine(t *testing.T) *engine {
	t.Helper()
	state := t.TempDir()
	cases := `
image) case "$2" in
  inspect) [ -e ` + state + `/built ] && echo "sha256:img repo@sha256:digest" || exit 1;;
esac;;
pull|build|tag) [ -e ` + state + `/fail-$1 ] && { echo "$1 broke" >&2; exit 1; }; touch ` + state + `/built;;
inspect) if [ -e ` + state + `/exited ]; then echo false; else echo true; fi;;
exec) [ -e ` + state + `/unhealthy ] && exit 1; exit 0;;
logs) echo "db: starting up";;
ps) echo c1;;
network) if [ "$2" = ls ]; then echo n1; fi;;
run)
  if [ "$2" = -d ]; then echo cid; exit 0; fi
  if [ -e ` + state + `/run125 ]; then echo "docker: daemon boom" >&2; exit 125; fi
  shift; prev=; src=; wd=/work
  while [ $# -gt 0 ]; do
    case "$prev" in -v) src=${1%%:*};; -w) wd=$1;; -e) export "$1";; esac
    case "$1" in forecrew-env:*) shift; break;; esac
    prev=$1; shift
  done
  cd "$src${wd#/work}" && exec "$@";;`
	bin, log := fakeDocker(t, cases)
	return &engine{bin: bin, log: log, state: state}
}

func (g *engine) set(t *testing.T, flags ...string) {
	t.Helper()
	for _, f := range flags {
		if err := os.WriteFile(filepath.Join(g.state, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// dockerEnv returns a command env wired to the fake engine.
func dockerEnv(t *testing.T) (*testEnv, *engine) {
	t.Helper()
	e := newCommandEnv(t)
	g := newEngine(t)
	e.cfg.DockerBin = g.bin
	e.rebuild()
	return e, g
}

func dockerSpec(e *testEnv, shell string, env *domain.Environment, mod ...func(*domain.JobSpec)) domain.JobSpec {
	return e.commandJob(shell, append([]func(*domain.JobSpec){func(s *domain.JobSpec) { s.Environment = env }}, mod...)...)
}

// hasCall reports whether a recorded call contains all of the parts.
func hasCall(got []string, parts ...string) bool {
	return slices.ContainsFunc(got, func(c string) bool {
		return !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(c, p) })
	})
}

func logsOf(e *testEnv, id uuid.UUID) []string {
	var out []string
	for _, ev := range e.api.eventsOf(id) {
		if ev.Type == domain.EventLog {
			out = append(out, ev.Text)
		}
	}
	return out
}

func TestDockerJobRunsInTheContainer(t *testing.T) {
	e, g := dockerEnv(t)
	e.cfg.Environ = append(os.Environ(), "ANTHROPIC_API_KEY="+testAPIKey, "LC_ALL=C", "TZ=UTC")
	e.rebuild()
	env := &domain.Environment{
		Name: "go", Provider: domain.ProviderDocker, Image: "golang:1.27", Setup: []string{"echo prepared\nmake deps"},
		Env:      map[string]string{"A": "1"},
		Versions: map[string]string{"go": "echo go1.27 linux"},
		Services: map[string]domain.Service{
			"main-db": {Image: "postgres:17", Env: map[string]string{"POSTGRES_DB": "t"}, Ports: []int{5432, 9999},
				Health: &domain.Health{Command: "pg_isready", Interval: domain.Duration(time.Millisecond), Retries: 3}},
			"cache": {Image: "redis:7"},
		},
	}
	script := `echo "svc=$MAIN_DB_HOST:$MAIN_DB_PORT cache=$CACHE_HOST[$CACHE_PORT] a=$A home=$HOME key=[$ANTHROPIC_API_KEY] loc=$LC_ALL/$TZ"; pwd; ` + writeJUnit("reports/unit.xml")
	s := dockerSpec(e, script, env)
	c := e.doComplete(s)

	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v\nlogs: %q", c, logsOf(e, s.JobID))
	}
	run := onlyRun(t, c)
	if run.Status != domain.TestRunFailed || run.Versions["go"] != "go1.27 linux" || run.Versions["image"] != "repo@sha256:digest" || run.Versions["os-release"] == "" {
		t.Errorf("run = %+v", run)
	}
	logs := logsOf(e, s.JobID)
	if !slices.Contains(logs, "svc=main-db:5432 cache=cache[] a=1 home=/tmp key=[] loc=C/UTC") {
		t.Errorf("job saw: %q", logs)
	}

	got := calls(t, g.log)
	id := s.JobID.String()
	for _, parts := range [][]string{
		{"pull -- golang:1.27"},
		{"build --tag forecrew-env:", "--file", "Dockerfile"},
		{"network create", "--label forecrew.job=" + id, "forecrew-" + id},
		{"run -d --name forecrew-" + id + "-main-db", "--pids-limit 4096", "-- postgres:17", "--network forecrew-" + id, "--network-alias main-db", "no-new-privileges", "-e POSTGRES_DB=t", "postgres:17"},
		{"exec forecrew-" + id + "-main-db sh -c pg_isready"},
		{"run --rm --init", "--pids-limit 4096", "-- forecrew-env:", "--user", "--cap-drop ALL", "--security-opt no-new-privileges", "--entrypoint ", "--network forecrew-" + id, "-w /work", "-e MAIN_DB_HOST=main-db", "-e HOME=/tmp"},
		{"rm -f -v c1"},
		{"network rm n1"},
	} {
		if !hasCall(got, parts...) {
			t.Errorf("no call with %q in\n%s", parts, strings.Join(got, "\n"))
		}
	}
	assertContained(t, got)
	// The image use is recorded for pruning.
	if ents, err := os.ReadDir(filepath.Join(e.workDir, "env-images")); err != nil || len(ents) < 2 {
		t.Errorf("image markers = %v, %v (want the environment image and the pulled one)", ents, err)
	}
	e.noSecrets()
}

func TestDockerJobReusesTheImage(t *testing.T) {
	e, g := dockerEnv(t)
	env := &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Dockerfile: "docs/guide.md", Context: "docs", Setup: []string{"true"}}
	run := func() []string {
		s := dockerSpec(e, "true", env, func(s *domain.JobSpec) { s.Command.Reports = []string{"none"} })
		if c := e.doComplete(s); c.Status != StatusSucceeded {
			t.Fatalf("completion = %+v", c)
		}
		return calls(t, g.log)
	}
	first := run()
	if !hasCall(first, "build --tag forecrew-env:", "-base", "--file", "docs/guide.md") || !hasCall(first, "build --tag forecrew-env:", "Dockerfile") {
		t.Errorf("first run did not build base and derived images:\n%s", strings.Join(first, "\n"))
	}
	n := len(first)
	second := run()[n:]
	if hasCall(second, "build") || hasCall(second, "pull") {
		t.Errorf("second run rebuilt the image:\n%s", strings.Join(second, "\n"))
	}
}

func TestDockerJobFailures(t *testing.T) {
	healthy := domain.Service{Image: "db:1", Health: &domain.Health{Command: "ok", Interval: domain.Duration(time.Millisecond), Retries: 2}}
	tests := []struct {
		name  string
		flags []string
		env   domain.Environment
		want  string
		logs  string
		early bool // fails before anything is created
	}{
		{"pull fails", []string{"fail-pull"}, domain.Environment{Image: "x:1"}, "environment image", "", false},
		{"build fails", []string{"fail-build"}, domain.Environment{Dockerfile: "docs/guide.md"}, "environment image", "", false},
		{"missing dockerfile", nil, domain.Environment{Dockerfile: "nope/Dockerfile"}, "dockerfile nope/Dockerfile", "", true},
		{"bad devcontainer", nil, domain.Environment{Devcontainer: "nope.json"}, "devcontainer nope.json", "", true},
		{"never healthy", []string{"unhealthy"}, domain.Environment{Image: "x:1", Services: map[string]domain.Service{"db": healthy}}, "service db: not healthy after 2 tries", "service log: db: starting up", false},
		{"service exits", []string{"exited"}, domain.Environment{Image: "x:1", Services: map[string]domain.Service{"db": healthy}}, "service db: the container exited", "service log: db: starting up", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, g := dockerEnv(t)
			g.set(t, tt.flags...)
			env := tt.env
			env.Name, env.Provider = "d", domain.ProviderDocker
			s := dockerSpec(e, "true", &env)
			c := e.doComplete(s)
			if c.Status != StatusFailed || !strings.Contains(c.Error, tt.want) || len(c.TestRuns) != 0 {
				t.Fatalf("completion = %+v, want failure containing %q", c, tt.want)
			}
			if tt.logs != "" && !slices.Contains(logsOf(e, s.JobID), tt.logs) {
				t.Errorf("job log = %q, want %q", logsOf(e, s.JobID), tt.logs)
			}
			if !tt.early && !hasCall(calls(t, g.log), "ps -aq --filter label=forecrew.job="+s.JobID.String()) {
				t.Error("a failed job was not cleaned up")
			}
		})
	}
}

func TestDockerJobWithoutDocker(t *testing.T) {
	e := newCommandEnv(t)
	c := e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"}))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "no Docker") {
		t.Errorf("completion = %+v", c)
	}
	c = e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "m", Provider: domain.ProviderMachine,
		Services: map[string]domain.Service{"db": {Image: "x:1"}}}))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "services are only for provider docker") {
		t.Errorf("machine with services: completion = %+v", c)
	}
	c = e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "a", Provider: domain.ProviderActions, RunsOn: "ubuntu-24.04"}))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "not run by workers") {
		t.Errorf("actions: completion = %+v", c)
	}
}

func TestDockerJobTimesOutWaitingForAService(t *testing.T) {
	e, g := dockerEnv(t)
	g.set(t, "unhealthy")
	slow := domain.Service{Image: "db:1", Health: &domain.Health{Command: "ok", Interval: domain.Duration(time.Minute), Retries: 5}}
	s := dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1", Services: map[string]domain.Service{"db": slow}},
		func(s *domain.JobSpec) { s.Timeout = domain.Duration(300 * time.Millisecond) })
	start := time.Now()
	c := e.doComplete(s)
	if c.Status != StatusFailed || !strings.Contains(c.Error, "timeout") || time.Since(start) > 10*time.Second {
		t.Errorf("completion = %+v after %s", c, time.Since(start))
	}
}

func TestDockerJobCancelRemovesContainers(t *testing.T) {
	e, g := dockerEnv(t)
	marker := filepath.Join(t.TempDir(), "started")
	e.api.heartbeat = func(uuid.UUID, int) (int, string) {
		if _, err := os.Stat(marker); err == nil {
			return 200, "cancel"
		}
		return 200, "continue"
	}
	s := dockerSpec(e, "touch "+marker+"; sleep 300", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"})
	start := time.Now()
	e.do(s)
	if time.Since(start) > 15*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times, want not at all", n)
	}
	if !hasCall(calls(t, g.log), "rm -f -v c1") {
		t.Error("the canceled job's containers were not removed")
	}
}

func TestPortProbe(t *testing.T) {
	got := portProbe([]int{5432, 80})
	for _, want := range []string{"nc -z localhost 5432", "/dev/tcp/localhost/5432", "nc -z localhost 80", "&&"} {
		if !strings.Contains(got, want) {
			t.Errorf("portProbe = %q, missing %q", got, want)
		}
	}
}

func TestWaitHealthyByPortAndRunning(t *testing.T) {
	e, g := dockerEnv(t)
	byPort := domain.Service{Image: "db:1", Ports: []int{5432}}
	plain := domain.Service{Image: "app:1"}
	s := dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1", Services: map[string]domain.Service{"db": byPort, "app": plain}},
		func(s *domain.JobSpec) { s.Command.Reports = []string{"none"} })
	if c := e.doComplete(s); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	if !hasCall(calls(t, g.log), "exec", "-db", "nc -z localhost 5432") {
		t.Errorf("the port was not probed:\n%s", strings.Join(calls(t, g.log), "\n"))
	}
}

func TestDockerContainerEnvironment(t *testing.T) {
	e, _ := dockerEnv(t)
	e.cfg.Environ = []string{"PATH=/host/bin", "HOME=/home/w", "USER=w", "LANG=C", "LC_CTYPE=C", "TZ=UTC", "ANTHROPIC_API_KEY=k", "DOCKER_HOST=unix:///x"}
	e.rebuild()
	dj := &dockerJob{env: map[string]string{"DB_HOST": "db"}}
	r := &jobRun{w: e.w, docker: dj, spec: e.commandJob("true", func(s *domain.JobSpec) {
		s.Command.Env = map[string]string{"B": "2"}
		s.Environment = &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x", Env: map[string]string{"A": "1"}}
	})}
	want := []string{"LANG=C", "LC_CTYPE=C", "TZ=UTC", "HOME=/tmp", "DB_HOST=db", "A=1", "B=2"}
	if got := r.commandEnv("mark"); !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}

func TestDerivedDockerfile(t *testing.T) {
	got := derivedDockerfile("golang:1.27", []string{"go version", "echo \"a\"\nRUN evil"})
	want := "FROM golang:1.27\nRUN [\"sh\",\"-c\",\"go version\"]\nRUN [\"sh\",\"-c\",\"echo \\\"a\\\"\\nRUN evil\"]\n"
	if got != want {
		t.Errorf("Dockerfile = %q, want %q", got, want)
	}
}

// assertContained checks that no docker call widens the container.
func assertContained(t *testing.T, got []string) {
	t.Helper()
	mounts := 0
	for _, c := range got {
		for _, bad := range []string{"--privileged", "docker.sock", "--network host", "--net host", "--pid host", "--pid=host", "--ipc", "--publish", testAPIKey} {
			if strings.Contains(c, bad) {
				t.Errorf("call %q contains %q", c, bad)
			}
		}
		if strings.Contains(c, ":/work") {
			mounts++
		}
	}
	if mounts < 3 { // the command, the version commands and the os query
		t.Errorf("the work tree was mounted %d times", mounts)
	}
}

func TestDockerJobRefusesRoot(t *testing.T) {
	e, g := dockerEnv(t)
	old := hostIDs
	hostIDs = func() (int, int) { return 0, 0 }
	defer func() { hostIDs = old }()
	c := e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"}))
	if c.Status != StatusFailed || !strings.Contains(c.Error, "refuse uid 0") {
		t.Errorf("completion = %+v", c)
	}
	if hasCall(calls(t, g.log), "run") {
		t.Error("a container ran as root")
	}
	hostIDs = func() (int, int) { return -1, -1 } // Windows
	if c := e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"},
		func(s *domain.JobSpec) { s.Command.Reports = []string{"none"} })); c.Status != StatusSucceeded {
		t.Errorf("without a uid: completion = %+v", c)
	}
	if !hasCall(calls(t, g.log), "--user 1000:1000") {
		t.Error("no fallback user")
	}
}

func TestDockerRunFailureIsGivenBack(t *testing.T) {
	e, g := dockerEnv(t)
	g.set(t, "run125")
	c := e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"}))
	if c.Status != StatusRetry || !strings.Contains(c.Error, "daemon boom") || len(c.TestRuns) != 0 {
		t.Errorf("completion = %+v, want a retry", c)
	}
}

func TestCommandExitingWith125IsTheCommands(t *testing.T) {
	e, _ := dockerEnv(t)
	s := dockerSpec(e, "exit 125", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"})
	if c := e.doComplete(s); c.Status != StatusSucceeded || onlyRun(t, c).ExitCode != 125 {
		t.Errorf("completion = %+v, want the command's exit code 125", c)
	}
}

func TestDockerNetworkFailureIsGivenBack(t *testing.T) {
	e := newCommandEnv(t)
	bin, _ := fakeDocker(t, `network) echo "docker: pool exhausted" >&2; exit 1;;`)
	e.cfg.DockerBin = bin
	e.rebuild()
	c := e.doComplete(dockerSpec(e, "true", &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "x:1"}))
	if c.Status != StatusRetry || !strings.Contains(c.Error, "pool exhausted") {
		t.Errorf("completion = %+v, want a retry", c)
	}
}

func TestDockerFromRefOfAFailedBuildIsNotRecorded(t *testing.T) {
	e, g := dockerEnv(t)
	g.set(t, "fail-build")
	red := runtime.NewRedactor(nil)
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM late:1\n")
	r := &jobRun{w: e.w, dir: dir, red: red, ev: &eventBuffer{jobID: domain.NewID(), client: e.cfg.Client, red: red, now: e.w.cfg.Now, log: e.cfg.Logger}}
	dj := &dockerJob{r: r, d: e.w.docker}
	r.docker = dj
	src := envSource{Dockerfile: "Dockerfile", Context: "."}
	if err := dj.build(t.Context(), src, "forecrew-env:x"); err == nil {
		t.Fatal("the build did not fail")
	}
	if e.w.imageMarkers().tracked("late:1") {
		t.Error("the base of a failed build was recorded for pruning")
	}
	// After a successful build that pulled it, it is recorded.
	_ = os.Remove(filepath.Join(g.state, "fail-build"))
	if err := dj.build(t.Context(), src, "forecrew-env:x"); err != nil {
		t.Fatal(err)
	}
	if !e.w.imageMarkers().tracked("late:1") {
		t.Error("the base pulled by a build was not recorded")
	}
}

func TestDockerPullTracksOnlyWhatTheWorkerPulled(t *testing.T) {
	e, g := dockerEnv(t)
	red := runtime.NewRedactor(nil)
	r := &jobRun{w: e.w, red: red, ev: &eventBuffer{jobID: domain.NewID(), client: e.cfg.Client, red: red, now: e.w.cfg.Now, log: e.cfg.Logger}}
	dj := &dockerJob{r: r, d: e.w.docker}
	r.docker = dj
	markers := e.w.imageMarkers()

	// Missing on the daemon: pulled and recorded.
	if err := dj.pull(t.Context(), "svc:1"); err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls(t, g.log), "pull -- svc:1") || !markers.tracked("svc:1") {
		t.Errorf("not pulled or recorded: %q", calls(t, g.log))
	}
	// Present, recorded: not pulled again, use recorded.
	before := len(calls(t, g.log))
	if err := dj.pull(t.Context(), "svc:1"); err != nil {
		t.Fatal(err)
	}
	if hasCall(calls(t, g.log)[before:], "pull") {
		t.Error("pulled an image the daemon has")
	}
	// Present and not recorded: the operator's, left alone.
	if err := dj.pull(t.Context(), "mine:1"); err != nil {
		t.Fatal(err)
	}
	if markers.tracked("mine:1") {
		t.Error("an image of the operator was recorded for pruning")
	}
	// A failing pull is an error.
	_ = os.Remove(filepath.Join(g.state, "built"))
	g.set(t, "fail-pull")
	if err := dj.pull(t.Context(), "other:1"); err == nil {
		t.Error("a failing pull succeeded")
	}
}

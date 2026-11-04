//go:build docker && !windows

package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// These tests need a Docker engine on this machine and the images alpine and
// postgres:18 (pulled when missing): go test -tags docker ./internal/worker.

func dockerWorker(t *testing.T) *testEnv {
	t.Helper()
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil { //nolint:gosec // fixed command
		t.Skipf("no Docker engine: %v: %s", err, out)
	}
	e := newCommandEnv(t)
	e.cfg.DockerBin = "docker"
	e.rebuild()
	t.Cleanup(func() { // whatever a failed test leaves behind
		_ = e.w.docker.removeLeftovers(t.Context())
	})
	return e
}

// pushFile adds a file to main of the remote.
func pushFile(t *testing.T, remote, path, content string) {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", remote, ".")
	writeFile(t, work, path, content)
	runGit(t, work, "add", "--all")
	runGit(t, work, "commit", "--quiet", "-m", "add "+path)
	runGit(t, work, "push", "--quiet", "origin", "main")
}

// dockerObjects lists the containers and networks carrying the job's label.
func dockerObjects(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	filter := "label=" + jobLabelKey + "=" + id.String()
	var all []string
	for _, args := range [][]string{{"ps", "-aq", "--filter", filter}, {"network", "ls", "-q", "--filter", filter}} {
		out, err := exec.Command("docker", args...).Output() //nolint:gosec // test command
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, strings.Fields(string(out))...)
	}
	return all
}

func TestDockerImageEnvironment(t *testing.T) {
	e := dockerWorker(t)
	script := "id -u > uid.txt; id -u; echo $HOME; ls -ld /work | cut -c1-10; " + writeJUnit("reports/unit.xml") + "exit 0"
	s := dockerSpec(e, script, &domain.Environment{
		Name: "alpine", Provider: domain.ProviderDocker, Image: "alpine:latest", Env: map[string]string{"GREETING": "hi"},
		Versions: map[string]string{"sh": "busybox | head -1"},
	}, func(s *domain.JobSpec) { s.Timeout = domain.Duration(5 * time.Minute) })
	c := e.doComplete(s)
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v\nlogs: %q", c, logsOf(e, s.JobID))
	}
	run := onlyRun(t, c)
	if uid := strconv.Itoa(os.Getuid()); !slices.Contains(logsOf(e, s.JobID), uid) {
		t.Errorf("the job did not run as uid %s: %q", uid, logsOf(e, s.JobID))
	}
	if !strings.Contains(run.Versions["os-release"], "Alpine") || !strings.Contains(run.Versions["image"], "sha256:") || !strings.Contains(run.Versions["sh"], "BusyBox") {
		t.Errorf("versions = %v", run.Versions)
	}
	if left := dockerObjects(t, s.JobID); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestDockerfileEnvironmentIsCached(t *testing.T) {
	e := dockerWorker(t)
	pushFile(t, e.remote, "ci/Dockerfile", "FROM alpine:latest\nRUN echo built > /marker\n")
	env := &domain.Environment{Name: "built", Provider: domain.ProviderDocker, Dockerfile: "ci/Dockerfile", Context: "ci", Setup: []string{"echo setup >> /marker"}}
	run := func() domain.JobSpec {
		s := dockerSpec(e, "cat /marker; "+writeJUnit("reports/unit.xml"), env, func(s *domain.JobSpec) { s.Timeout = domain.Duration(5 * time.Minute) })
		if c := e.doComplete(s); c.Status != StatusSucceeded {
			t.Fatalf("completion = %+v\nlogs: %q", c, logsOf(e, s.JobID))
		}
		return s
	}
	first := run()
	if !slices.Contains(logsOf(e, first.JobID), "built") || !slices.Contains(e.api.statuses(first.JobID), "building environment") {
		t.Errorf("first run: logs %q, statuses %v", logsOf(e, first.JobID), e.api.statuses(first.JobID))
	}
	second := run()
	if slices.Contains(e.api.statuses(second.JobID), "building environment") || !slices.Contains(e.api.statuses(second.JobID), "environment image cached") {
		t.Errorf("second run rebuilt the image: %v", e.api.statuses(second.JobID))
	}
	if !slices.Contains(logsOf(e, second.JobID), "setup") {
		t.Errorf("second run did not see the setup layer: %q", logsOf(e, second.JobID))
	}
}

func TestPostgresServiceIsReachable(t *testing.T) {
	e := dockerWorker(t)
	env := &domain.Environment{
		Name: "pg", Provider: domain.ProviderDocker, Image: "postgres:18",
		Services: map[string]domain.Service{"db": {
			Image: "postgres:18", Env: map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"}, Ports: []int{5432},
			Health: &domain.Health{Command: "pg_isready -U postgres -h localhost", Interval: domain.Duration(time.Second), Retries: 60},
		}},
	}
	s := dockerSpec(e, `psql -h "$DB_HOST" -p "$DB_PORT" -U postgres -tAc 'select 40 + 2'; `+writeJUnit("reports/unit.xml"), env,
		func(s *domain.JobSpec) { s.Timeout = domain.Duration(5 * time.Minute) })
	c := e.doComplete(s)
	if c.Status != StatusSucceeded || !slices.Contains(logsOf(e, s.JobID), "42") {
		t.Fatalf("completion = %+v\nlogs: %q", c, logsOf(e, s.JobID))
	}
	if left := dockerObjects(t, s.JobID); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestCanceledDockerJobIsCleanedUp(t *testing.T) {
	e := dockerWorker(t)
	// The command marks the work tree once it runs; the heartbeat then
	// answers cancel.
	e.api.heartbeat = func(id uuid.UUID, _ int) (int, string) {
		if _, err := os.Stat(filepath.Join(e.workDir, "jobs", id.String(), "started")); err == nil {
			return 200, "cancel"
		}
		return 200, "continue"
	}
	env := &domain.Environment{Name: "svc", Provider: domain.ProviderDocker, Image: "alpine:latest",
		Services: map[string]domain.Service{"db": {
			Image: "postgres:18", Env: map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust"},
			Health: &domain.Health{Command: "pg_isready -U postgres -h localhost", Interval: domain.Duration(time.Second), Retries: 60},
		}}}
	s := dockerSpec(e, "touch started; sleep 300", env, func(s *domain.JobSpec) { s.Timeout = domain.Duration(5 * time.Minute) })
	e.do(s)
	if n := e.api.completionCount(s.JobID); n != 0 {
		t.Errorf("completed %d times, want not at all", n)
	}
	if left := dockerObjects(t, s.JobID); len(left) > 0 {
		t.Errorf("left behind after the cancel: %v", left)
	}
}

func TestDockerSocketIsNotReachable(t *testing.T) {
	e := dockerWorker(t)
	script := `ls -l /var/run/docker.sock /run/docker.sock; which docker; test "$(id -u)" != 0 && echo not-root; ` + writeJUnit("reports/unit.xml") + `ls -l /var/run/docker.sock`
	s := dockerSpec(e, script, &domain.Environment{Name: "a", Provider: domain.ProviderDocker, Image: "alpine:latest"})
	c := e.doComplete(s)
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	run := onlyRun(t, c)
	logs := strings.Join(logsOf(e, s.JobID), "\n")
	if run.ExitCode == 0 || !strings.Contains(logs, "No such file") || strings.Contains(logs, "srw") || !strings.Contains(logs, "not-root") {
		t.Errorf("exit code %d, logs:\n%s", run.ExitCode, logs)
	}
}

func TestLeftoversOfACrashedWorkerAreRemoved(t *testing.T) {
	e := dockerWorker(t)
	id := domain.NewID().String()
	out, err := exec.Command("docker", "run", "-d", "--label", jobLabelKey+"="+id, "--label", workerLabelKey+"="+e.w.docker.worker, "alpine:latest", "sleep", "300").CombinedOutput() //nolint:gosec // test command
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := e.w.docker.removeLeftovers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+jobLabelKey+"="+id).Output(); strings.TrimSpace(string(left)) != "" { //nolint:gosec // test command
		t.Errorf("container still there: %s", left)
	}
}

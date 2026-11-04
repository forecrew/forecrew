package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
)

// fakeDocker writes a docker client that appends every call (its arguments
// on one line) to a log and answers with the shell `case` arms in cases,
// keyed on the first argument. Calls without an arm succeed silently. It
// returns the binary and the log.
func fakeDocker(t *testing.T, cases string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "docker"), filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncase \"$1\" in\n" + cases + "\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return bin, log
}

// calls returns the calls the fake docker received.
func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log) //nolint:gosec // test path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestDetectLabelsAddsDocker(t *testing.T) {
	bin, _ := fakeDocker(t, `version) echo 29.1.0;;`)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := &config.Worker{Labels: []string{"go"}, WorkDir: t.TempDir()}
	labels, dockerBin, info := detectLabels(t.Context(), cfg, os.Environ())
	if !slices.Contains(labels, "docker") || !slices.Contains(labels, "go") || dockerBin != "docker" || info != "29.1.0" {
		t.Errorf("labels = %v, bin = %q, info = %q", labels, dockerBin, info)
	}

	bad, _ := fakeDocker(t, `version) echo "Cannot connect to the Docker daemon" >&2; exit 1;;`)
	t.Setenv("PATH", filepath.Dir(bad)+string(os.PathListSeparator)+os.Getenv("PATH"))
	labels, dockerBin, info = detectLabels(t.Context(), cfg, os.Environ())
	if slices.Contains(labels, "docker") || dockerBin != "" || !strings.Contains(info, "Cannot connect") {
		t.Errorf("without an engine: labels = %v, bin = %q, info = %q", labels, dockerBin, info)
	}

	t.Setenv("PATH", t.TempDir())
	if labels, dockerBin, _ = detectLabels(t.Context(), cfg, os.Environ()); slices.Contains(labels, "docker") || dockerBin != "" {
		t.Errorf("without a client: labels = %v, bin = %q", labels, dockerBin)
	}
}

func TestDockerOutputError(t *testing.T) {
	bin, _ := fakeDocker(t, `info) exit 3;; ps) echo boom >&2; exit 1;;`)
	d := newDockerCLI(bin, t.TempDir(), nil)
	if _, err := d.output(t.Context(), "ps"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want stderr", err)
	}
	if _, err := d.output(t.Context(), "info"); err == nil || !strings.Contains(err.Error(), "docker info") {
		t.Errorf("err = %v, want the exit error", err)
	}
}

func TestRemoveLabeled(t *testing.T) {
	bin, log := fakeDocker(t, `ps) echo c1; echo c2;; network) if [ "$2" = ls ]; then echo n1; fi;;`)
	d := newDockerCLI(bin, "/work", nil)
	if err := d.removeJob(t.Context(), "job-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ps -aq --filter label=forecrew.job=job-1", "rm -f -v c1 c2",
		"network ls -q --filter label=forecrew.job=job-1", "network rm n1",
	}
	if got := calls(t, log); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}

	if err := d.removeLeftovers(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := calls(t, log); !strings.Contains(got[4], "label=forecrew.worker="+d.worker) {
		t.Errorf("leftovers filter = %q", got[4])
	}

	failing, _ := fakeDocker(t, `ps) echo nope >&2; exit 1;; rm) echo busy >&2; exit 1;; network) echo down >&2; exit 1;;`)
	err := newDockerCLI(failing, "/work", nil).removeJob(t.Context(), "j")
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "down") {
		t.Errorf("err = %v, want both failures", err)
	}
	withIDs, _ := fakeDocker(t, `ps) echo c1;; rm) echo busy >&2; exit 1;;`)
	if err := newDockerCLI(withIDs, "/work", nil).removeJob(t.Context(), "j"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v, want the failed removal", err)
	}
}

func TestPruneImages(t *testing.T) {
	bin, log := fakeDocker(t, `image) case "$2" in
  ls) printf 'forecrew-env:old\nforecrew-env:unknown\n';;
  rm) case "$4" in gone:1) echo "Error: No such image: gone:1" >&2; exit 1;; busy:1) echo "image is being used by container" >&2; exit 1;; esac;;
esac;;`)
	d := newDockerCLI(bin, "/work", nil)
	m := imageMarkers(filepath.Join(t.TempDir(), "env-images"))
	now := time.Now()
	old, week := now.Add(-20*24*time.Hour), now.Add(-time.Hour)
	for _, ref := range []string{"forecrew-env:old", "postgres:18", "gone:1", "busy:1"} {
		m.touch(ref, old)
	}
	m.touch("redis:7", week)

	removed, err := d.pruneImages(t.Context(), m, 14*24*time.Hour, now)
	if err == nil || !strings.Contains(err.Error(), "being used") {
		t.Errorf("err = %v, want the image in use reported", err)
	}
	slices.Sort(removed)
	if want := []string{"forecrew-env:old", "postgres:18"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v (calls %q)", removed, want, calls(t, log))
	}
	for ref, want := range map[string]bool{
		"forecrew-env:old": false, "postgres:18": false, // removed
		"gone:1":               false, // already gone: forgotten
		"busy:1":               true,  // kept, tried again
		"redis:7":              true,  // recent
		"forecrew-env:unknown": true,  // untracked environment images start their clock
	} {
		if m.tracked(ref) != want {
			t.Errorf("tracked(%q) = %v, want %v", ref, !want, want)
		}
	}

	down, _ := fakeDocker(t, `image) echo down >&2; exit 1;;`)
	if _, err := newDockerCLI(down, "/work", nil).pruneImages(t.Context(), m, time.Hour, now); err == nil {
		t.Error("a failing listing was not reported")
	}
}

func TestDockerClientEnvironment(t *testing.T) {
	d := newDockerCLI("docker", "/work", []string{"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///x", "ANTHROPIC_API_KEY=sk", "FORECREW_WORKER_TOKEN=t"})
	want := []string{"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///x"}
	if !slices.Equal(d.env, want) {
		t.Errorf("env = %v, want %v", d.env, want)
	}
}

func TestDockerRegistryCredentialsAreIsolated(t *testing.T) {
	// ps fails with the client's view of the registry settings.
	bin, _ := fakeDocker(t, `ps) echo "cfg=$DOCKER_CONFIG auth=$DOCKER_AUTH_CONFIG ctx=$DOCKER_CONTEXT host=$DOCKER_HOST" >&2; exit 1;;
context) echo unix:///run/ctx.sock;;`)
	environ := []string{"PATH=/bin", "HOME=/h", "DOCKER_CONFIG=/h/.docker", "DOCKER_AUTH_CONFIG=secret-login", "DOCKER_CONTEXT=remote"}
	seen := func(registryAuth bool, env []string) string {
		w := New(Config{WorkDir: t.TempDir(), DockerBin: bin, Environ: env, RegistryAuth: registryAuth, DockerHost: "unix:///run/ctx.sock"})
		err := w.docker.removeJob(t.Context(), "j")
		if err == nil {
			t.Fatal("ps did not fail")
		}
		return err.Error() + "|" + w.cfg.WorkDir
	}

	got := seen(false, environ)
	work := got[strings.LastIndex(got, "|")+1:]
	for _, want := range []string{"cfg=" + filepath.Join(work, "docker-config"), "auth= ", "ctx= ", "host=unix:///run/ctx.sock"} {
		if !strings.Contains(got, want) {
			t.Errorf("isolated client saw %q, missing %q", got, want)
		}
	}
	if st, err := os.Stat(filepath.Join(work, "docker-config")); err != nil || !st.IsDir() {
		t.Errorf("config dir: %v", err)
	}
	// An explicit DOCKER_HOST wins over the context's.
	if got := seen(false, append(slices.Clone(environ), "DOCKER_HOST=tcp://127.0.0.1:2375")); !strings.Contains(got, "host=tcp://127.0.0.1:2375") {
		t.Errorf("DOCKER_HOST was replaced: %q", got)
	}
	// With registry_auth the operator's settings stay.
	got = seen(true, environ)
	for _, want := range []string{"cfg=/h/.docker", "auth=secret-login", "ctx=remote"} {
		if !strings.Contains(got, want) {
			t.Errorf("client with registry_auth saw %q, missing %q", got, want)
		}
	}

	d := newDockerCLI(bin, t.TempDir(), nil)
	if h := d.host(t.Context()); h != "unix:///run/ctx.sock" {
		t.Errorf("host = %q", h)
	}
}

func TestRootWorkerDoesNotAdvertiseDocker(t *testing.T) {
	bin, _ := fakeDocker(t, `version) echo 29.1.0;;`)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := hostIDs
	hostIDs = func() (int, int) { return 0, 0 }
	defer func() { hostIDs = old }()
	labels, dockerBin, info := detectLabels(t.Context(), &config.Worker{WorkDir: t.TempDir()}, os.Environ())
	if slices.Contains(labels, "docker") || dockerBin != "" || !strings.Contains(info, "root") {
		t.Errorf("labels = %v, bin = %q, info = %q", labels, dockerBin, info)
	}
}

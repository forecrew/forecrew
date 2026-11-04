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
	bin, log := fakeDocker(t, `image) if [ "$2" = ls ]; then printf 'forecrew-env:old\nforecrew-env:old-base\nforecrew-env:fresh\nforecrew-env:unknown\n'; fi;;`)
	d := newDockerCLI(bin, "/work", nil)
	m := imageMarkers(filepath.Join(t.TempDir(), "env-images"))
	now := time.Now()
	m.touch("old", now.Add(-20*24*time.Hour))
	m.touch("fresh", now.Add(-time.Hour))

	removed, err := d.pruneImages(t.Context(), m, 14*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"forecrew-env:old", "forecrew-env:old-base"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v (calls %q)", removed, want, calls(t, log))
	}
	// An image without a marker starts its clock now.
	if _, err := os.Stat(filepath.Join(string(m), "unknown")); err != nil {
		t.Errorf("unknown image was not marked: %v", err)
	}

	failing, _ := fakeDocker(t, `image) if [ "$2" = ls ]; then echo forecrew-env:old; else echo busy >&2; exit 1; fi;;`)
	if _, err := newDockerCLI(failing, "/work", nil).pruneImages(t.Context(), m, time.Hour, now); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v, want the failed removal", err)
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

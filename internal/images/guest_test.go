package images

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStart runs the embedded forecrew-start with env as its bootstrap file
// and a fake worker that prints its arguments, config and environment.
func runStart(t *testing.T, envFile, labelsFile string) (stdout string, config string, envLeft bool, err error) {
	t.Helper()
	sh, lookErr := exec.LookPath("sh")
	if lookErr != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script, readErr := guestFS.ReadFile("guest/forecrew-start")
	if readErr != nil {
		t.Fatal(readErr)
	}
	start := filepath.Join(dir, "forecrew-start")
	worker := filepath.Join(dir, "worker")
	bootstrap := filepath.Join(dir, "run", "bootstrap.env")
	for path, data := range map[string]string{
		start:  string(script),
		worker: "#!/bin/sh\necho \"args: $*\"\ncat \"$3\"\nenv | grep '^FORECREW_' | sort\n",
	} {
		if e := os.WriteFile(path, []byte(data), 0o700); e != nil { // #nosec G306 -- test scripts
			t.Fatal(e)
		}
	}
	if e := os.MkdirAll(filepath.Dir(bootstrap), 0o700); e != nil {
		t.Fatal(e)
	}
	if envFile != "" {
		if e := os.WriteFile(bootstrap, []byte(envFile), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.Command(sh, start) // #nosec G204 -- test script
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "FORECREW_BOOTSTRAP_ENV=" + bootstrap, "FORECREW_WORKER=" + worker,
		"FORECREW_WORK_DIR=" + filepath.Join(dir, "work"), "FORECREW_LABELS_FILE=" + labelsFile, "FORECREW_SECRET_OF_THE_PARENT=1"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	_, statErr := os.Stat(bootstrap)
	if runErr != nil {
		runErr = &startError{runErr, errb.String()}
	}
	return out.String(), "", statErr == nil, runErr
}

type startError struct {
	err    error
	stderr string
}

func (e *startError) Error() string { return e.err.Error() + ": " + e.stderr }

func TestStartScript(t *testing.T) {
	labels := filepath.Join(t.TempDir(), "labels")
	if err := os.WriteFile(labels, []byte("docker\n# comment\n\nrunning\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, left, err := runStart(t, "# delivered\nFORECREW_SERVER_URL=https://forecrew.example.com\nFORECREW_BOOTSTRAP_TOKEN=fcb_abc-DEF_123\nFORECREW_WORKER_LABELS=gpu:l4\nFORECREW_WORKER_MAX_JOBS=2\nFORECREW_WORKER_NAME=box-1\n", labels)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"args: run --config ", "server_url: https://forecrew.example.com", "name: box-1", "max_jobs: 2",
		"  - gpu:l4", "  - docker", "  - running", "FORECREW_BOOTSTRAP_TOKEN=fcb_abc-DEF_123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	// The token file is gone, and nothing else of the caller's environment reaches the worker.
	if left || strings.Contains(out, "SECRET_OF_THE_PARENT") || strings.Contains(out, "FORECREW_WORKER_TOKEN") {
		t.Errorf("env file left %v, or the environment leaked:\n%s", left, out)
	}

	out, _, _, err = runStart(t, "FORECREW_SERVER_URL=http://localhost:8080\nFORECREW_WORKER_TOKEN=fcw_x\n", filepath.Join(t.TempDir(), "none"))
	if err != nil || !strings.Contains(out, "FORECREW_WORKER_TOKEN=fcw_x") || strings.Contains(out, "labels:") {
		t.Errorf("worker token: %v\n%s", err, out)
	}

	for name, in := range map[string]string{
		"missing file":      "",
		"no server":         "FORECREW_BOOTSTRAP_TOKEN=fcb_x\n",
		"no token":          "FORECREW_SERVER_URL=https://x.example.com\n",
		"both tokens":       "FORECREW_SERVER_URL=https://x.example.com\nFORECREW_BOOTSTRAP_TOKEN=a\nFORECREW_WORKER_TOKEN=b\n",
		"shell characters":  "FORECREW_SERVER_URL=https://x.example.com;reboot\nFORECREW_WORKER_TOKEN=b\n",
		"yaml injection":    "FORECREW_SERVER_URL=https://x.example.com\nFORECREW_WORKER_TOKEN=b\nFORECREW_WORKER_NAME=a b\n",
		"unknown setting":   "FORECREW_SERVER_URL=https://x.example.com\nFORECREW_WORKER_TOKEN=b\nPATH=/evil\n",
		"line without sign": "FORECREW_SERVER_URL\n",
	} {
		if _, _, _, err := runStart(t, in, filepath.Join(t.TempDir(), "none")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

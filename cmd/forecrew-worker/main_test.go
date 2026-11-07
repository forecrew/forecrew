package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain removes the GIT_* variables the test process inherited, so that no
// test can reach the repo it was started from.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

// hermeticEnviron is the environment the commands run with: PATH, an empty
// home and no git config, plus extra entries.
func hermeticEnviron(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
	}, extra...)
}

func TestRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "forecrew-worker v1.2.3\n"},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "forecrew-worker v1.2.3\n"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "usage: forecrew-worker"},
		{name: "no command", args: nil, wantCode: 2, wantStderr: "usage: forecrew-worker"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
		{name: "run help", args: []string{"run", "-h"}, wantCode: 0, wantStderr: "-config"},
		{name: "bad flag", args: []string{"run", "--nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "run without config", args: []string{"run", "--config", missing}, wantCode: 1, wantStderr: "load config"},
		{name: "check without config", args: []string{"check", "--config", missing}, wantCode: 1, wantStderr: "load config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, &stdout, &stderr, nil, "v1.2.3")
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunAndCheckWithConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fcw_token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "worker.yaml")
	yaml := "server_url: " + srv.URL + "\nname: w\nwork_dir: " + filepath.Join(dir, "work") + "\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := hermeticEnviron(t, "FORECREW_WORKER_TOKEN=fcw_token", "GIT_DIR=/nonexistent") // GIT_DIR must be ignored

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"check", "--config", cfgPath}, &stdout, &stderr, environ, "v1"); code != 0 {
		t.Fatalf("check code = %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "token accepted") || strings.Contains(stdout.String()+stderr.String(), "fcw_token") {
		t.Errorf("check output = %s", stdout.String())
	}
	stdout.Reset()
	if code := run(t.Context(), []string{"status", "--config", cfgPath}, &stdout, &stderr, environ, "v1"); code != 0 || !strings.Contains(stdout.String(), "auto_install_agents: on") {
		t.Fatalf("status code = %d\n%s\n%s", code, stdout.String(), stderr.String())
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	stderr.Reset()
	if code := run(ctx, []string{"run", "--config", cfgPath}, &stdout, &stderr, environ, "v1"); code != 0 {
		t.Fatalf("run code = %d\n%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	bad := hermeticEnviron(t, "FORECREW_WORKER_TOKEN=revoked")
	if code := run(t.Context(), []string{"check", "--config", cfgPath}, &stdout, &stderr, bad, "v1"); code != 1 {
		t.Errorf("check with a bad token: code = %d", code)
	}
	stderr.Reset()
	if code := run(t.Context(), []string{"run", "--config", cfgPath}, &stdout, &stderr, bad, "v1"); code != 1 || !strings.Contains(stderr.String(), "revoked") {
		t.Errorf("run with a bad token: code = %d, stderr = %s", code, stderr.String())
	}
}

func TestGetenv(t *testing.T) {
	env := getenv([]string{"A=1", "AB=2", "B=x=y"})
	if env("A") != "1" || env("AB") != "2" || env("B") != "x=y" || env("C") != "" {
		t.Error("getenv returned wrong values")
	}
}

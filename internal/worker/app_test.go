package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

func TestAgentEnv(t *testing.T) {
	got := AgentEnv([]string{
		"PATH=/bin", "FORECREW_WORKER_TOKEN=fcw_x", "HOME=/h", "FORECREW_OTHER=1", "MY_FORECREW_X=2",
		"GIT_DIR=/x", "GIT_TRACE=1", "GIT_SSH_COMMAND=evil", "GITHUB_USER=u",
	})
	if want := []string{"PATH=/bin", "HOME=/h", "MY_FORECREW_X=2", "GITHUB_USER=u"}; !slices.Equal(got, want) {
		t.Errorf("AgentEnv = %v, want %v", got, want)
	}
}

// fakeTools puts a fake claude CLI in front of PATH and returns its dump
// directory. The fake records what the agent process sees.
func fakeTools(t *testing.T) (dump string) {
	t.Helper()
	isolateGit(t)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	bin, dump := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.0.0 (Fake Claude)"; exit 0; fi
env > '` + dump + `/env.txt'
git config --list --show-origin > '` + dump + `/gitconfig.txt' 2>&1
git remote -v > '` + dump + `/remotes.txt' 2>&1
cat .git > '` + dump + `/gitfile.txt' 2>&1
echo "work by the agent" > from-agent.txt
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","num_turns":1}'
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(config.EnvWorkerToken, testWorkerTok)
	return dump
}

// TestAgentNeverSeesTheTokens runs a job through the production wiring with a
// fake claude CLI and checks what the agent process could read.
func TestAgentNeverSeesTheTokens(t *testing.T) {
	dump := fakeTools(t)
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	cfg := &config.Worker{ServerURL: e.api.srv.URL, Name: "w", WorkDir: e.workDir, MaxJobs: 1, Token: testWorkerTok}
	// The operator's environment holds the worker token and hostile GIT_*
	// settings; neither may reach the agent.
	environ := append(hermeticEnv(), "FORECREW_WORKER_TOKEN="+testWorkerTok, "GIT_DIR=/nonexistent", "GIT_TRACE=1", "GIT_COMMON_DIR=/nonexistent")
	w := newFromConfig(t.Context(), cfg, environ, "v-test", e.cfg.Logger, false)
	w.cfg.CloneURL, w.cfg.HeartbeatInterval, w.cfg.FlushInterval = e.cfg.CloneURL, e.cfg.HeartbeatInterval, e.cfg.FlushInterval
	e.w = w
	if !slices.Contains(w.cfg.Labels, "runtime:claude-code") {
		t.Fatalf("labels = %v, want runtime:claude-code from the fake CLI", w.cfg.Labels)
	}

	s := e.spec(func(s *domain.JobSpec) { s.Runtime = "claude-code" })
	c := e.doComplete(s)
	if c.Status != StatusSucceeded || c.HeadSHA == "" {
		t.Fatalf("completion = %+v (logs: %s)", c, e.logs.String())
	}
	if got := runGit(t, e.remote, "show", "agent/task-1:from-agent.txt"); got != "work by the agent" {
		t.Errorf("pushed file = %q", got)
	}

	env, err := os.ReadFile(filepath.Join(dump, "env.txt")) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "ANTHROPIC_API_KEY="+testAPIKey) {
		t.Error("the agent did not get its own credentials")
	}
	if strings.Contains(string(env), "GIT_") {
		t.Errorf("the agent inherited GIT_* variables:\n%s", env)
	}
	for _, f := range []string{"env.txt", "gitconfig.txt", "remotes.txt", "gitfile.txt"} {
		b, err := os.ReadFile(filepath.Join(dump, f)) //nolint:gosec // test path
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{testRepoToken, authHeader(testRepoToken), testWorkerTok, "FORECREW_WORKER_TOKEN", "extraHeader", "x-access-token"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s shows the agent %q:\n%s", f, secret, b)
			}
		}
	}
	scanTree(t, e.workDir, testRepoToken, authHeader(testRepoToken), testWorkerTok)
	e.noSecrets()
}

func TestCheck(t *testing.T) {
	fakeTools(t)
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		if token != "Bearer good" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "revoked")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	check := func(url, tok string) (string, error) {
		var out bytes.Buffer
		err := Check(t.Context(), &config.Worker{ServerURL: url, Labels: []string{"Go"}, Token: tok}, hermeticEnv(), "v9", &out)
		return out.String(), err
	}
	out, err := check(srv.URL, "good")
	if err != nil {
		t.Fatalf("Check = %v\n%s", err, out)
	}
	for _, want := range []string{"forecrew-worker v9", "labels: ", "go", "runtime:claude-code", "git version", "runtime claude-code: 2.0.0", "token accepted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "good") && strings.Contains(out, "Bearer") {
		t.Error("output shows the token")
	}

	out, err = check(srv.URL, "bad")
	if !errors.Is(err, ErrCheckFailed) || !strings.Contains(out, "rejected") {
		t.Errorf("revoked token: %v\n%s", err, out)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	if out, err = check(url, "good"); !errors.Is(err, ErrCheckFailed) || !strings.Contains(out, "server "+url+": FAILED") {
		t.Errorf("unreachable server: %v\n%s", err, out)
	}
}

func TestCheckWithoutGit(t *testing.T) {
	isolateGit(t)
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	err := Check(t.Context(), &config.Worker{ServerURL: srv.URL, Token: "t"}, hermeticEnv(), "v", &out)
	if !errors.Is(err, ErrCheckFailed) || !strings.Contains(out.String(), "git: FAILED") || !strings.Contains(out.String(), "not available") {
		t.Errorf("Check = %v\n%s", err, out.String())
	}
}

func TestRunFunction(t *testing.T) {
	fakeTools(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cfg := &config.Worker{ServerURL: srv.URL, Name: "w", WorkDir: t.TempDir(), MaxJobs: 1, Token: "t"}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := Run(ctx, cfg, hermeticEnv(), "v1", logger); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !strings.Contains(logs.String(), "forecrew-worker started") || !strings.Contains(logs.String(), "forecrew-worker stopped") {
		t.Errorf("logs = %s", logs.String())
	}
	if strings.Contains(logs.String(), `"t"`) {
		t.Error("logs show the token")
	}

	if strings.Contains(logs.String(), "max_jobs is above 1") {
		t.Error("warned about max_jobs 1")
	}
	var logs2 lockedBuffer
	logger = slog.New(slog.NewJSONHandler(&logs2, nil))
	cfg.MaxJobs = 2
	ctx2, cancel2 := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel2()
	if err := Run(ctx2, cfg, hermeticEnv(), "v1", logger); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !strings.Contains(logs2.String(), "max_jobs is above 1") {
		t.Errorf("no warning for max_jobs 2: %s", logs2.String())
	}
	cfg.MaxJobs = 1

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "revoked")
	}))
	defer srv2.Close()
	cfg.ServerURL = srv2.URL
	if err := Run(t.Context(), cfg, hermeticEnv(), "v1", logger); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Run with a revoked token = %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := Run(t.Context(), cfg, hermeticEnv(), "v1", logger); err == nil || !strings.Contains(err.Error(), "git is required") {
		t.Errorf("Run without git = %v", err)
	}
}

package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// The tests run the test binary itself as the fake CLI (the helper-process
// pattern): TestMain turns into the fake when FORECREW_FAKE_CLI is set.
//
//	FAKE_STREAM  file printed to stdout
//	FAKE_STDERR  text printed to stderr
//	FAKE_EXIT    exit code
//	FAKE_HANG    after printing, start a child process and block
//	FAKE_BGCHILD start a background child holding stdout, then exit normally
//	FAKE_PIDFILE where the child's pid is written
//	FAKE_RECORD  where the args, stdin and API_KEY are recorded as JSON
//	FAKE_VERSION what --version prints
//	FAKE_RETRY_STREAM, FAKE_RETRY_RECORD  FAKE_STREAM and FAKE_RECORD of a run
//	             that continues a session
//	FAKE_OPENCODE_LOAD where the configuration opencode would load is written
func TestMain(m *testing.M) {
	if os.Getenv("FORECREW_FAKE_CLI") == "1" {
		fakeCLI()
		return
	}
	os.Exit(m.Run())
}

type fakeRecord struct {
	Args   []string `json:"args"`
	Stdin  string   `json:"stdin"`
	APIKey string   `json:"api_key"`
	Dir    string   `json:"dir"`
	// ConfigDir is CLAUDE_CONFIG_DIR and ConfigEntries the number of entries
	// in it, as seen by the CLI.
	ConfigDir     string `json:"config_dir"`
	ConfigEntries int    `json:"config_entries"`
	// CodexHome is CODEX_HOME and CodexEntries the number of entries in it.
	CodexHome    string `json:"codex_home"`
	CodexEntries int    `json:"codex_entries"`
	// JobMCPToken is the job MCP token variable as seen by the CLI.
	JobMCPToken string `json:"job_mcp_token"`
	// Env is the whole environment as seen by the CLI.
	Env map[string]string `json:"env"`
}

func environMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func fakeCLI() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprint(os.Stdout, cmp.Or(os.Getenv("FAKE_VERSION"), "9.9.9 (Fake CLI)")+"\nsecond line\n")
		return
	}
	if out := os.Getenv("FAKE_OPENCODE_LOAD"); out != "" {
		dir, _ := os.Getwd()
		b, _ := json.Marshal(opencodeLoad(environMap(), dir))
		_ = os.WriteFile(out, b, 0o600)
	}
	recordTo, streamFrom := os.Getenv("FAKE_RECORD"), os.Getenv("FAKE_STREAM")
	if slices.Contains(args, "--session") {
		recordTo, streamFrom = cmp.Or(os.Getenv("FAKE_RETRY_RECORD"), recordTo), cmp.Or(os.Getenv("FAKE_RETRY_STREAM"), streamFrom)
	}
	if rec := recordTo; rec != "" {
		stdin, _ := readAll(os.Stdin)
		dir, _ := os.Getwd()
		cfgDir := os.Getenv("CLAUDE_CONFIG_DIR")
		entries, _ := os.ReadDir(cfgDir)
		codexHome := os.Getenv("CODEX_HOME")
		codexEntries, _ := os.ReadDir(codexHome)
		b, _ := json.Marshal(fakeRecord{
			Args: args, Stdin: stdin, APIKey: os.Getenv("API_KEY"), Dir: dir,
			ConfigDir: cfgDir, ConfigEntries: len(entries), JobMCPToken: os.Getenv(JobMCPTokenEnv),
			CodexHome: codexHome, CodexEntries: len(codexEntries), Env: environMap(),
		})
		_ = os.WriteFile(rec, b, 0o600)
	}
	if f := streamFrom; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(99)
		}
		_, _ = os.Stdout.Write(b)
	}
	if s := os.Getenv("FAKE_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	if os.Getenv("FAKE_BGCHILD") != "" {
		// A background child (a dev server) that keeps stdout and stderr open
		// after the CLI exits. FAKE_BGCHILD=setsid also leaves the process group; chatty does too and keeps writing.
		child := exec.Command("sleep", "300")
		if os.Getenv("FAKE_BGCHILD") == "chatty" {
			// Leaves the group and never stops writing.
			child = exec.Command("sh", "-c", "while :; do echo noise; sleep 0.02; done")
		}
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: os.Getenv("FAKE_BGCHILD") != "1"}
		_ = child.Start()
		_ = os.WriteFile(os.Getenv("FAKE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}
	if os.Getenv("FAKE_HANG") != "" {
		child := exec.Command("sleep", "300")
		_ = child.Start()
		_ = os.WriteFile(os.Getenv("FAKE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		time.Sleep(5 * time.Minute)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

func readAll(f *os.File) (string, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out), nil
		}
	}
}

// fakeEnv is the base environment of a fake CLI run.
func fakeEnv(kv ...string) []string {
	return append(append(os.Environ(), "FORECREW_FAKE_CLI=1"), kv...)
}

func stream(name string) string {
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return p
}

// collect runs a and returns the emitted events.
func collect(t *testing.T, a Adapter, spec RunSpec) ([]domain.JobEvent, Result, error) {
	t.Helper()
	var events []domain.JobEvent
	if spec.WorkDir == "" {
		spec.WorkDir = t.TempDir()
	}
	res, err := a.Run(context.Background(), spec, func(e domain.JobEvent) { events = append(events, e) })
	return events, res, err
}

func types(events []domain.JobEvent) []domain.JobEventType {
	out := make([]domain.JobEventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func find(events []domain.JobEvent, t domain.JobEventType) []domain.JobEvent {
	var out []domain.JobEvent
	for _, e := range events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r fakeRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// childStarted reports whether the fake child wrote its pid. The file can
// exist for a moment before the pid is in it, so an empty file is not enough.
func childStarted(pidfile string) bool {
	b, err := os.ReadFile(pidfile)
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// cancelWhenStarted returns a context that is cancelled once the fake CLI has
// written its child's pid file, so a run is never cancelled before the child
// it must kill exists. It gives up waiting, and cancels, after a minute.
func cancelWhenStarted(t *testing.T, pidfile string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		defer cancel()
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			if childStarted(pidfile) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ctx
}

// runToTimeout runs a hanging fake CLI under a job timeout and checks that
// the run ends with ErrTimeout and that the child is gone. The timeout runs
// from the start of the run, and a race-built fake CLI can take longer than
// it to start under load; a run that was killed before its child existed
// proves nothing, so it is repeated with twice the timeout until the child
// was seen. newAdapter gets the pid file path. It returns the last adapter.
func runToTimeout(t *testing.T, spec RunSpec, newAdapter func(pidfile string) Adapter) Adapter {
	t.Helper()
	timeout := 500 * time.Millisecond
	for range 6 {
		pidfile := filepath.Join(t.TempDir(), "pid")
		a := newAdapter(pidfile)
		spec.Timeout = timeout
		start := time.Now()
		_, _, err := collect(t, a, spec)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
		if d := time.Since(start); d > timeout+10*time.Second {
			t.Errorf("took %s", d)
		}
		if childStarted(pidfile) {
			assertChildGone(t, pidfile)
			return a
		}
		timeout *= 2
	}
	t.Fatal("fake CLI never started its child before the timeout")
	return nil
}

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
}

func fakeCLI() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprint(os.Stdout, "9.9.9 (Fake CLI)\nsecond line\n")
		return
	}
	if rec := os.Getenv("FAKE_RECORD"); rec != "" {
		stdin, _ := readAll(os.Stdin)
		dir, _ := os.Getwd()
		cfgDir := os.Getenv("CLAUDE_CONFIG_DIR")
		entries, _ := os.ReadDir(cfgDir)
		b, _ := json.Marshal(fakeRecord{
			Args: args, Stdin: stdin, APIKey: os.Getenv("API_KEY"), Dir: dir,
			ConfigDir: cfgDir, ConfigEntries: len(entries),
		})
		_ = os.WriteFile(rec, b, 0o600)
	}
	if f := os.Getenv("FAKE_STREAM"); f != "" {
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
		// after the CLI exits. FAKE_BGCHILD=setsid also leaves the process group.
		child := exec.Command("sleep", "300")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: os.Getenv("FAKE_BGCHILD") == "setsid"}
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

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

func newFakeCodex(env ...string) Adapter {
	return NewCodex(Config{Binary: os.Args[0], Env: fakeEnv(env...)})
}

func codexSpec() RunSpec {
	return RunSpec{
		Model: "gpt-5", Prompt: "do the work", OutputSchema: json.RawMessage(testSchema),
		AllowedTools: []string{"Read", "Edit"}, Timeout: time.Minute,
		Env: map[string]string{"CODEX_API_KEY": "sk-test-codexsecretvalue123456"},
	}
}

func TestCodexSuccess(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeCodex("FAKE_STREAM="+stream("codex_success.jsonl"), "FAKE_RECORD="+rec)
	spec := codexSpec()
	spec.WorkDir = t.TempDir()
	events, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"codex did it"}` {
		t.Errorf("output = %s", res.Output)
	}
	if res.Turns != 1 || res.CostUSD != 0 {
		t.Errorf("turns/cost = %d/%v", res.Turns, res.CostUSD)
	}
	if want := (domain.Usage{InputTokens: 400, CacheReadTokens: 600, CacheWriteTokens: 7, OutputTokens: 80}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	wantTypes := []domain.JobEventType{
		domain.EventStatus,
		domain.EventToolCall, domain.EventToolResult, // ls
		domain.EventToolCall,                         // file_change
		domain.EventToolCall, domain.EventToolResult, // declined
		domain.EventToolCall, domain.EventToolResult, // mcp, error
		domain.EventToolCall, domain.EventToolResult, // mcp, result
		domain.EventToolCall, // web_search
		domain.EventMessage, domain.EventUsage,
	}
	if got := types(events); !slices.Equal(got, wantTypes) {
		t.Errorf("event types = %v, want %v", got, wantTypes)
	}
	results := find(events, domain.EventToolResult)
	if !strings.Contains(string(results[0].Data), `"is_error":false`) || !strings.Contains(string(results[0].Data), "README.md") {
		t.Errorf("result 0 = %s", results[0].Data)
	}
	if !strings.Contains(string(results[1].Data), `"is_error":true`) {
		t.Errorf("result 1 = %s", results[1].Data)
	}
	if d := string(results[2].Data); !strings.Contains(d, `"name":"gh.search"`) || !strings.Contains(d, `"is_error":true`) || !strings.Contains(d, "tool exploded") {
		t.Errorf("result 2 = %s", d)
	}
	if d := string(results[3].Data); !strings.Contains(d, `"is_error":false`) || !strings.Contains(d, "found it") {
		t.Errorf("result 3 = %s", d)
	}

	r := readRecord(t, rec)
	wantArgs := []string{
		"exec", "--json", "--skip-git-repo-check", "--ephemeral", "-c", `approval_policy="never"`, "--sandbox", "workspace-write",
		"--cd", spec.WorkDir, "--model", "gpt-5", "-",
	}
	if !slices.Equal(r.Args, wantArgs) {
		t.Errorf("args = %q", r.Args)
	}
	if r.Stdin != "do the work" || r.Dir != spec.WorkDir {
		t.Errorf("record = %+v", r)
	}
}

func TestCodexSandbox(t *testing.T) {
	tests := []struct {
		tools []string
		want  string
	}{
		{nil, "read-only"},
		{[]string{"Read", "Grep", "Bash(go test:*)"}, "read-only"},
		{[]string{"Write"}, "workspace-write"},
		{[]string{"Read", "Edit(src/**)"}, "workspace-write"},
	}
	for _, tt := range tests {
		args := codexArgs(RunSpec{AllowedTools: tt.tools})
		if i := slices.Index(args, "--sandbox"); i < 0 || args[i+1] != tt.want {
			t.Errorf("tools %v: args = %q, want sandbox %s", tt.tools, args, tt.want)
		}
		if slices.Contains(args, "--cd") || slices.Contains(args, "--model") {
			t.Errorf("empty spec should not set --cd or --model: %q", args)
		}
	}
}

func TestCodexEvidenceDir(t *testing.T) {
	args := codexArgs(RunSpec{EvidenceDir: "/scratch"})
	if i := slices.Index(args, "--add-dir"); i < 0 || args[i+1] != "/scratch" {
		t.Errorf("args = %q", args)
	}
}

func TestCodexMalformedLine(t *testing.T) {
	a := newFakeCodex("FAKE_STREAM=" + stream("codex_malformed.jsonl"))
	events, res, err := collect(t, a, codexSpec())
	if err != nil {
		t.Fatal(err)
	}
	if logs := find(events, domain.EventLog); len(logs) != 1 || !strings.Contains(logs[0].Text, "unparseable") {
		t.Errorf("logs = %+v", logs)
	}
	if string(res.Output) != `{"summary":"ok"}` {
		t.Errorf("output = %s", res.Output)
	}
}

func TestCodexFailures(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		env     []string
		is      error
		contain string
	}{
		{"missing final output", "codex_no_output.jsonl", nil, ErrInvalidOutput, "no JSON block"},
		{"schema invalid", "codex_schema_invalid.jsonl", nil, ErrInvalidOutput, "summary"},
		{"failed turn", "codex_failed.jsonl", []string{"FAKE_EXIT=1"}, ErrRunFailed, "model overloaded"},
		{"non-zero exit", "codex_no_turn.jsonl", []string{"FAKE_EXIT=2", "FAKE_STDERR=bad flag"}, ErrRunFailed, "bad flag"},
		{"no turn", "codex_no_turn.jsonl", nil, ErrRunFailed, "without a result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFakeCodex(append([]string{"FAKE_STREAM=" + stream(tt.stream)}, tt.env...)...)
			spec := codexSpec()
			spec.Env = map[string]string{"CODEX_API_KEY": "hunter2-token-value"}
			events, _, err := collect(t, a, spec)
			if !errors.Is(err, tt.is) || !strings.Contains(err.Error(), tt.contain) {
				t.Fatalf("err = %v, want %v containing %q", err, tt.is, tt.contain)
			}
			for _, e := range events {
				if strings.Contains(e.Text, "hunter2") {
					t.Errorf("event leaks secret: %+v", e)
				}
			}
		})
	}
}

func TestCodexTimeoutAndDetect(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	a := newFakeCodex("FAKE_HANG=1", "FAKE_PIDFILE="+pidfile)
	spec := codexSpec()
	spec.Timeout = 500 * time.Millisecond
	if _, _, err := collect(t, a, spec); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	assertChildGone(t, pidfile)

	if v, err := a.Detect(context.Background()); err != nil || v != "9.9.9 (Fake CLI)" {
		t.Errorf("Detect = %q, %v", v, err)
	}
	if a.Name() != "codex" {
		t.Errorf("Name = %q", a.Name())
	}
	missing := NewCodex(Config{Binary: "/nonexistent/codex"})
	if _, err := missing.Detect(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Detect err = %v", err)
	}
}

func TestCodexCancellation(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	a := newFakeCodex("FAKE_HANG=1", "FAKE_PIDFILE="+pidfile)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for range 200 {
			if _, err := os.Stat(pidfile); err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		cancel()
	}()
	spec := codexSpec()
	spec.WorkDir = t.TempDir()
	if _, err := a.Run(ctx, spec, func(domain.JobEvent) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertChildGone(t, pidfile)
}

func TestCodexUsageIsRunningTotal(t *testing.T) {
	a := newFakeCodex("FAKE_STREAM=" + stream("codex_running_total.jsonl"))
	_, res, err := collect(t, a, codexSpec())
	if err != nil {
		t.Fatal(err)
	}
	if want := (domain.Usage{InputTokens: 150, CacheReadTokens: 100, OutputTokens: 30}); res.Usage != want || res.Turns != 2 {
		t.Errorf("usage = %+v turns %d, want %+v", res.Usage, res.Turns, want)
	}
}

func TestCodexIsolatesHome(t *testing.T) {
	// The worker account's own config and login must not be what the CLI sees.
	t.Setenv("CODEX_HOME", "/home/worker/.codex")
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeCodex("CODEX_HOME=/home/worker/.codex", "FAKE_STREAM="+stream("codex_success.jsonl"), "FAKE_RECORD="+rec)
	spec := codexSpec()
	spec.Env["API_KEY"] = "sk-test-codexsecretvalue123456"
	if _, _, err := collect(t, a, spec); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, rec)
	if r.CodexHome == "" || r.CodexHome == "/home/worker/.codex" || r.CodexEntries != 0 {
		t.Errorf("codex home = %q with %d entries, want a fresh empty dir", r.CodexHome, r.CodexEntries)
	}
	if _, err := os.Stat(r.CodexHome); !os.IsNotExist(err) {
		t.Errorf("codex home %s was not removed: %v", r.CodexHome, err)
	}
	if r.APIKey == "" {
		t.Error("credentials must still reach the CLI through the env")
	}
}

func TestCodexNeedsItsKey(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeCodex("FAKE_STREAM="+stream("codex_success.jsonl"), "FAKE_RECORD="+rec)
	spec := codexSpec()
	delete(spec.Env, "CODEX_API_KEY")
	_, _, err := collect(t, a, spec)
	if !errors.Is(err, ErrRunFailed) || !strings.Contains(err.Error(), "CODEX_API_KEY") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(rec); err == nil {
		t.Fatal("the CLI ran without a key")
	}
	// The worker's own environment is as good as the job's.
	a = newFakeCodex("CODEX_API_KEY=sk-from-the-worker", "FAKE_STREAM="+stream("codex_success.jsonl"))
	if _, _, err := collect(t, a, spec); err != nil {
		t.Fatal(err)
	}
}

func TestCodexEndpoint(t *testing.T) {
	spec := codexSpec()
	spec.Endpoint = &domain.Endpoint{BaseURL: "https://proxy.example.com/v1", KeyEnv: "CODEX_API_KEY"}
	args := codexArgs(spec)
	if i := slices.Index(args, `openai_base_url="https://proxy.example.com/v1"`); i < 1 || args[i-1] != "-c" {
		t.Fatalf("args = %q", args)
	}
	if slices.ContainsFunc(codexArgs(codexSpec()), func(a string) bool { return strings.Contains(a, "openai_base_url") }) {
		t.Error("base URL set without an endpoint")
	}
}

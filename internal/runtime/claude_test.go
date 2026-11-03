package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

func newFakeClaude(env ...string) Adapter {
	return NewClaudeCode(Config{Binary: os.Args[0], Env: fakeEnv(env...)})
}

func claudeSpec() RunSpec {
	return RunSpec{
		Model: "claude-sonnet-4-5", Prompt: "do the work", OutputSchema: json.RawMessage(testSchema),
		AllowedTools: []string{"Read", "Bash(go test:*)"}, MaxTurns: 7, Timeout: time.Minute,
		Env: map[string]string{"API_KEY": "sk-ant-test-SECRETSECRET"},
	}
}

func TestClaudeSuccess(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeClaude("FAKE_STREAM="+stream("claude_success.jsonl"), "FAKE_RECORD="+rec)
	spec := claudeSpec()
	spec.WorkDir = t.TempDir()
	events, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"listed files"}` {
		t.Errorf("output = %s", res.Output)
	}
	if res.Turns != 2 || res.CostUSD != 0.0123 {
		t.Errorf("turns/cost = %d/%v", res.Turns, res.CostUSD)
	}
	wantUsage := domain.Usage{InputTokens: 120, OutputTokens: 55, CacheReadTokens: 400, CacheWriteTokens: 30}
	if res.Usage != wantUsage {
		t.Errorf("usage = %+v", res.Usage)
	}
	if !strings.HasPrefix(res.Text, "All done.") {
		t.Errorf("text = %q", res.Text)
	}
	wantTypes := []domain.JobEventType{
		domain.EventStatus, domain.EventMessage, domain.EventToolCall, domain.EventToolResult,
		domain.EventMessage, domain.EventUsage,
	}
	if got := types(events); !slices.Equal(got, wantTypes) {
		t.Errorf("event types = %v, want %v", got, wantTypes)
	}
	call := find(events, domain.EventToolCall)[0]
	if call.Text != "Bash" || !strings.Contains(string(call.Data), `"command":"ls"`) {
		t.Errorf("tool call = %+v data %s", call, call.Data)
	}
	result := find(events, domain.EventToolResult)[0]
	if !strings.Contains(string(result.Data), `"name":"Bash"`) || !strings.Contains(string(result.Data), "main.go") {
		t.Errorf("tool result data = %s", result.Data)
	}
	for _, e := range events {
		if e.Time.IsZero() {
			t.Errorf("event %s has no time", e.Type)
		}
	}

	assertClaudeInvocation(t, readRecord(t, rec), spec)
}

func assertClaudeInvocation(t *testing.T, r fakeRecord, spec RunSpec) {
	t.Helper()
	if r.Stdin != "do the work" || r.APIKey != "sk-ant-test-SECRETSECRET" || r.Dir != spec.WorkDir {
		t.Errorf("record = %+v", r)
	}
	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--no-session-persistence",
		"--permission-mode", "dontAsk", "--setting-sources", "user",
		"--tools", "Read,Bash", "--disallowedTools", "mcp__*",
		"--model", "claude-sonnet-4-5", "--max-turns", "7", "--allowedTools", "Read", "Bash(go test:*)",
	}
	if !slices.Equal(r.Args, wantArgs) {
		t.Errorf("args = %q", r.Args)
	}
	for _, a := range r.Args {
		if strings.Contains(a, "sk-ant") || strings.Contains(a, "do the work") {
			t.Errorf("secret or prompt in argv: %q", a)
		}
	}
}

func TestClaudeDefaults(t *testing.T) {
	args := claudeArgs(RunSpec{})
	want := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--no-session-persistence",
		"--permission-mode", "dontAsk", "--setting-sources", "user",
		"--tools", "Read,Grep,Glob", "--disallowedTools", "mcp__*", "--allowedTools", "Read", "Grep", "Glob",
	}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q", args)
	}
}

func TestClaudeToolResultShapes(t *testing.T) {
	a := newFakeClaude("FAKE_STREAM=" + stream("claude_tool_blocks.jsonl"))
	events, _, err := collect(t, a, claudeSpec())
	if err != nil {
		t.Fatal(err)
	}
	results := find(events, domain.EventToolResult)
	if len(results) != 2 {
		t.Fatalf("got %d tool results", len(results))
	}
	if !strings.Contains(string(results[0].Data), `"output":"line one\nline two"`) || !strings.Contains(string(results[0].Data), `"is_error":true`) {
		t.Errorf("block content = %s", results[0].Data)
	}
	if !strings.Contains(string(results[1].Data), `odd`) {
		t.Errorf("object content = %s", results[1].Data)
	}
}

func TestClaudeMalformedLine(t *testing.T) {
	a := newFakeClaude("FAKE_STREAM=" + stream("claude_malformed.jsonl"))
	events, res, err := collect(t, a, claudeSpec())
	if err != nil {
		t.Fatal(err)
	}
	logs := find(events, domain.EventLog)
	if len(logs) != 1 || !strings.Contains(logs[0].Text, "unparseable output line") {
		t.Errorf("logs = %+v", logs)
	}
	if string(res.Output) != `{"summary":"fine"}` {
		t.Errorf("output = %s", res.Output)
	}
}

func TestClaudeFailures(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		env     []string
		is      error
		contain string
	}{
		{"missing final output", "claude_no_output.jsonl", nil, ErrInvalidOutput, "no JSON block"},
		{"schema invalid", "claude_schema_invalid.jsonl", nil, ErrInvalidOutput, "summary"},
		{"max turns", "claude_max_turns.jsonl", []string{"FAKE_EXIT=1"}, ErrRunFailed, "Reached maximum number of turns"},
		{"non-zero exit", "claude_no_result.jsonl", []string{"FAKE_EXIT=3", "FAKE_STDERR=boom happened"}, ErrRunFailed, "boom happened"},
		{"no result event", "claude_no_result.jsonl", nil, ErrRunFailed, "without a result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFakeClaude(append([]string{"FAKE_STREAM=" + stream(tt.stream)}, tt.env...)...)
			events, _, err := collect(t, a, claudeSpec())
			if !errors.Is(err, tt.is) {
				t.Fatalf("err = %v, want %v", err, tt.is)
			}
			if !strings.Contains(err.Error(), tt.contain) {
				t.Errorf("err %q does not contain %q", err, tt.contain)
			}
			errs := find(events, domain.EventError)
			if len(errs) != 1 || !strings.Contains(errs[0].Text, tt.contain) {
				t.Errorf("error events = %+v", errs)
			}
		})
	}
}

func TestClaudeKeepsUsageOnFailure(t *testing.T) {
	a := newFakeClaude("FAKE_STREAM="+stream("claude_schema_invalid.jsonl"), "FAKE_EXIT=0")
	_, res, err := collect(t, a, claudeSpec())
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 10 || res.CostUSD != 0.002 {
		t.Errorf("result = %+v", res)
	}
}

func TestClaudeRedaction(t *testing.T) {
	spec := claudeSpec()
	spec.Env = map[string]string{"API_KEY": "hunter2-token-value"}
	spec.OutputSchema = nil
	a := newFakeClaude("FAKE_STREAM=" + stream("claude_secret.jsonl"))
	events, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	all := res.Text
	for _, e := range events {
		all += e.Text + string(e.Data)
		if e.Data != nil && !json.Valid(e.Data) {
			t.Errorf("invalid data after redaction: %s", e.Data)
		}
	}
	for _, secret := range []string{"hunter2-token-value", "sk-ant-test-SECRETSECRET", "ghs_abcdefghijklmnop"} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked: %s", secret, all)
		}
	}
	if !strings.Contains(all, "[REDACTED]") {
		t.Error("nothing was redacted")
	}
}

func TestClaudeRedactsErrors(t *testing.T) {
	spec := claudeSpec()
	spec.Env = map[string]string{"API_KEY": "hunter2-token-value"}
	a := newFakeClaude("FAKE_EXIT=2", "FAKE_STDERR=auth failed for hunter2-token-value")
	events, _, err := collect(t, a, spec)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error leaks secret: %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Text, "hunter2") {
			t.Errorf("event leaks secret: %+v", e)
		}
	}

	// Invalid output messages are redacted too.
	spec.OutputSchema = json.RawMessage(`{"type":"object","properties":{"summary":{"const":"x"}},"required":["summary"]}`)
	a = newFakeClaude("FAKE_STREAM=" + stream("claude_secret.jsonl"))
	events, _, err = collect(t, a, spec)
	var ioe *InvalidOutputError
	if !errors.As(err, &ioe) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ioe.Message, "hunter2") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("validator message leaks secret: %s", ioe.Message)
	}
	for _, e := range find(events, domain.EventError) {
		if strings.Contains(e.Text, "hunter2") {
			t.Errorf("error event leaks secret: %q", e.Text)
		}
	}
}

func TestClaudeTimeoutKillsProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	a := newFakeClaude("FAKE_HANG=1", "FAKE_PIDFILE="+pidfile)
	spec := claudeSpec()
	spec.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, _, err := collect(t, a, spec)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
	assertChildGone(t, pidfile)
}

func TestClaudeCancellationKillsProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	a := newFakeClaude("FAKE_HANG=1", "FAKE_PIDFILE="+pidfile)
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
	spec := claudeSpec()
	spec.WorkDir = t.TempDir()
	_, err := a.Run(ctx, spec, func(domain.JobEvent) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertChildGone(t, pidfile)
}

// assertChildGone checks that the child the fake CLI started was killed with
// the rest of the process group.
func assertChildGone(t *testing.T, pidfile string) {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("fake CLI never started its child: %v", err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 only checks existence; a zombie still counts until reaped,
		// and init reaps it quickly.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("child process %d survived the run", pid)
}

func TestClaudeNotInstalled(t *testing.T) {
	a := NewClaudeCode(Config{Binary: "/nonexistent/claude-binary"})
	if _, err := a.Detect(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Detect err = %v", err)
	}
	_, _, err := collect(t, a, claudeSpec())
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Run err = %v", err)
	}
}

func TestClaudeDetect(t *testing.T) {
	a := newFakeClaude()
	got, err := a.Detect(context.Background())
	if err != nil || got != "9.9.9 (Fake CLI)" {
		t.Errorf("Detect = %q, %v", got, err)
	}
	if a.Name() != "claude-code" {
		t.Errorf("Name = %q", a.Name())
	}
}

func TestDetectFailure(t *testing.T) {
	a := NewClaudeCode(Config{Binary: "false"})
	_, err := a.Detect(context.Background())
	if err == nil || errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want a plain failure", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Setenv("FORECREW_FAKE_CLI", "1")
	t.Setenv("FAKE_STREAM", stream("claude_success.jsonl"))
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := NewClaudeCode(Config{Binary: os.Args[0], Now: func() time.Time { return fixed }})
	events, _, err := collect(t, a, claudeSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !events[0].Time.Equal(fixed) {
		t.Errorf("time = %v", events[0].Time)
	}
	if got := (Config{}).binary("claude"); got != "claude" {
		t.Errorf("default binary = %q", got)
	}
	if (Config{}).now().IsZero() {
		t.Error("default now is zero")
	}
}

func TestBaseToolNames(t *testing.T) {
	got := baseToolNames([]string{"Read", "Bash(git log *)", "Bash(go test:*)", " Edit(src/**)", "(odd)"})
	if want := []string{"Read", "Bash", "Edit"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClaudeBackgroundChildDoesNotBlock(t *testing.T) {
	for _, mode := range []string{"1", "setsid"} {
		t.Run(mode, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "pid")
			a := newFakeClaude("FAKE_STREAM="+stream("claude_success.jsonl"), "FAKE_BGCHILD="+mode, "FAKE_PIDFILE="+pidfile)
			spec := claudeSpec()
			spec.Timeout = 30 * time.Second
			start := time.Now()
			_, res, err := collect(t, a, spec)
			if err != nil {
				t.Fatalf("a background child turned success into %v", err)
			}
			if string(res.Output) != `{"summary":"listed files"}` {
				t.Errorf("output = %s", res.Output)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("took %s", d)
			}
			if mode == "1" {
				assertChildGone(t, pidfile) // killed with the group
				return
			}
			// It left the group, so it is ours to clean up.
			b, _ := os.ReadFile(pidfile)
			var pid int
			if _, err := fmt.Sscan(string(b), &pid); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
	}
}

func TestClaudeRedactsOutput(t *testing.T) {
	spec := claudeSpec()
	spec.Env = map[string]string{"API_KEY": "hunter2-token-value"}
	spec.OutputSchema = json.RawMessage(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`)
	a := newFakeClaude("FAKE_STREAM=" + stream("claude_secret.jsonl"))
	_, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Output), "hunter2") || !strings.Contains(string(res.Output), "[REDACTED]") {
		t.Errorf("output = %s", res.Output)
	}
}

func TestClaudeIsolatesConfigDir(t *testing.T) {
	// The worker account's own config must not be what the CLI sees.
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/worker/.claude")
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeClaude("FAKE_STREAM="+stream("claude_success.jsonl"), "FAKE_RECORD="+rec)
	if _, _, err := collect(t, a, claudeSpec()); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, rec)
	if r.ConfigDir == "" || r.ConfigDir == "/home/worker/.claude" || r.ConfigEntries != 0 {
		t.Errorf("config dir = %q with %d entries, want a fresh empty dir", r.ConfigDir, r.ConfigEntries)
	}
	if _, err := os.Stat(r.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("config dir %s was not removed: %v", r.ConfigDir, err)
	}
	if r.APIKey == "" {
		t.Error("credentials must still reach the CLI through the env")
	}
}

func TestClaudeSlowConsumerKeepsEndOfStream(t *testing.T) {
	// 20 lines of 1.5 KB plus a result: more than a pipe read, and a consumer
	// slower than the drain grace after the CLI exits.
	var b strings.Builder
	b.WriteString(`{"type":"system","subtype":"init"}` + "\n")
	for range 20 {
		fmt.Fprintf(&b, `{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`+"\n", strings.Repeat("x", 1500))
	}
	b.WriteString("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1," +
		"\"result\":\"```json\\n{\\\"summary\\\":\\\"end\\\"}\\n```\",\"total_cost_usd\":0,\"usage\":{}}\n")
	path := filepath.Join(t.TempDir(), "slow.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newFakeClaude("FAKE_STREAM=" + path)
	spec := claudeSpec()
	spec.WorkDir = t.TempDir()
	var n int
	res, err := a.Run(context.Background(), spec, func(domain.JobEvent) {
		n++
		time.Sleep(100 * time.Millisecond)
	})
	if err != nil {
		t.Fatalf("slow consumer lost the end of the stream: %v", err)
	}
	if string(res.Output) != `{"summary":"end"}` || n < 21 {
		t.Errorf("output = %s after %d events", res.Output, n)
	}
}

func TestClaudeChattyChildDrainIsCapped(t *testing.T) {
	// A child that left the process group and never stops writing must not
	// keep the run open after the CLI is gone.
	pidfile := filepath.Join(t.TempDir(), "pid")
	a := newFakeClaude("FAKE_STREAM="+stream("claude_success.jsonl"), "FAKE_BGCHILD=chatty", "FAKE_PIDFILE="+pidfile)
	spec := claudeSpec()
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, res, err := collect(t, a, spec)
		done <- outcome{res, err}
	}()
	defer func() {
		b, _ := os.ReadFile(pidfile)
		var pid int
		if _, err := fmt.Sscan(string(b), &pid); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	select {
	case o := <-done:
		if o.err != nil || string(o.res.Output) != `{"summary":"listed files"}` {
			t.Fatalf("result = %s, %v", o.res.Output, o.err)
		}
		if d := time.Since(start); d > drainCap+3*time.Second {
			t.Errorf("took %s, want about %s", d, drainCap)
		}
	case <-time.After(drainCap + 10*time.Second):
		t.Fatal("run still open: a chatty child kept the output drain going")
	}
}

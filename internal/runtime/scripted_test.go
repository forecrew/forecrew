package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

func writeScript(t *testing.T, sc any) string {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScriptedSuccess(t *testing.T) {
	path := writeScript(t, Script{
		Events: []ScriptEvent{
			{Type: domain.EventMessage, Text: "working"},
			{Type: domain.EventToolCall, Text: "Edit", Data: json.RawMessage(`{"name":"Edit"}`)},
		},
		Edits:  []ScriptEdit{{Path: "src/a.txt", Content: "hello"}, {Path: "old.txt", Delete: true}, {Path: "none.txt", Delete: true}},
		Text:   "finished",
		Output: json.RawMessage(`{"summary":"scripted"}`),
		Turns:  2, Usage: domain.Usage{InputTokens: 3, OutputTokens: 4}, CostUSD: 0.5,
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewScripted(path, Config{})
	if a.Name() != "scripted" {
		t.Errorf("Name = %q", a.Name())
	}
	if v, err := a.Detect(context.Background()); err != nil || v == "" {
		t.Errorf("Detect = %q, %v", v, err)
	}
	spec := RunSpec{WorkDir: dir, OutputSchema: json.RawMessage(testSchema), Timeout: time.Minute}
	events, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"scripted"}` || res.Turns != 2 || res.CostUSD != 0.5 || res.Usage.OutputTokens != 4 {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Text, "finished") {
		t.Errorf("text = %q", res.Text)
	}
	if got := types(events); len(got) != 3 || got[2] != domain.EventUsage {
		t.Errorf("types = %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src", "a.txt")); err != nil || string(b) != "hello" {
		t.Errorf("edit not applied: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt still exists: %v", err)
	}
}

func TestScriptedFailures(t *testing.T) {
	tests := []struct {
		name   string
		script Script
		spec   RunSpec
		is     error
	}{
		{"error", Script{Error: "boom hunter2-token-value"}, RunSpec{}, ErrRunFailed},
		{"no output", Script{Text: "no json"}, RunSpec{OutputSchema: json.RawMessage(testSchema)}, ErrInvalidOutput},
		{"invalid output", Script{Output: json.RawMessage(`{"summary":1}`)}, RunSpec{OutputSchema: json.RawMessage(testSchema)}, ErrInvalidOutput},
		{"edit escapes", Script{Edits: []ScriptEdit{{Path: "../evil", Content: "x"}}}, RunSpec{}, errEditOutside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.spec.Env = map[string]string{"K": "hunter2-token-value"}
			_, _, err := collect(t, NewScripted(writeScript(t, tt.script), Config{}), tt.spec)
			if !errors.Is(err, tt.is) {
				t.Fatalf("err = %v, want %v", err, tt.is)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks secret: %v", err)
			}
		})
	}
}

func TestScriptedBadScript(t *testing.T) {
	if _, _, err := collect(t, NewScripted(filepath.Join(t.TempDir(), "missing.json"), Config{}), RunSpec{}); err == nil {
		t.Error("missing script: want error")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collect(t, NewScripted(p, Config{}), RunSpec{}); err == nil {
		t.Error("bad script: want error")
	}
}

func TestScriptedTimeoutAndCancel(t *testing.T) {
	path := writeScript(t, Script{Events: []ScriptEvent{{Type: domain.EventLog, Text: "slow", Delay: domain.Duration(time.Minute)}}})
	a := NewScripted(path, Config{})
	_, _, err := collect(t, a, RunSpec{Timeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("timeout err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = a.Run(ctx, RunSpec{WorkDir: t.TempDir()}, func(domain.JobEvent) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel err = %v", err)
	}
	// Canceled between events (no delay) is caught too.
	path = writeScript(t, Script{Events: []ScriptEvent{{Type: domain.EventLog, Text: "a"}}})
	_, err = NewScripted(path, Config{}).Run(ctx, RunSpec{WorkDir: t.TempDir()}, func(domain.JobEvent) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel err = %v", err)
	}
}

func TestScriptedRedactsOutput(t *testing.T) {
	path := writeScript(t, Script{Output: json.RawMessage(`{"summary":"key hunter2-token-value"}`)})
	spec := RunSpec{OutputSchema: json.RawMessage(testSchema), Env: map[string]string{"K": "hunter2-token-value"}}
	_, res, err := collect(t, NewScripted(path, Config{}), spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"summary":"key [REDACTED]"}` {
		t.Errorf("output = %s", res.Output)
	}
}

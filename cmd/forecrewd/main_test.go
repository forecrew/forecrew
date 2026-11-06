package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	invalid := filepath.Join(t.TempDir(), "forecrew.yaml")
	if err := os.WriteFile(invalid, []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"--version"}, wantCode: 0, wantStdout: "forecrewd v1.2.3\n"},
		{name: "ignore stored settings is a flag", args: []string{"--ignore-stored-settings", "--version"}, wantCode: 0, wantStdout: "forecrewd v1.2.3\n"},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "-config"},
		{name: "bad flag", args: []string{"--nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "missing config", args: []string{"--config", missing}, wantCode: 1, wantStderr: "load config"},
		{name: "invalid config", args: []string{"--config", invalid}, wantCode: 1, wantStderr: "invalid config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, &stdout, &stderr, func(string) string { return "" }, "v1.2.3")
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
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

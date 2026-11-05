package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// fakePath returns a PATH-like directory holding executable files named bins.
func fakePath(t *testing.T, bins ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, b := range bins {
		name := b
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test executable
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectLabels(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		goos       string
		bins       []string
		want       []string
	}{
		{name: "nothing found", goos: "linux", want: []string{"linux"}},
		{
			name: "both runtimes", configured: []string{"Go", " node "}, goos: "linux", bins: []string{"claude", "codex"},
			want: []string{"go", "linux", "node", "runtime:claude-code", "runtime:codex"},
		},
		{name: "opencode", goos: "linux", bins: []string{"opencode"}, want: []string{"linux", "runtime:opencode"}},
		{name: "claude only", goos: "windows", bins: []string{"claude"}, want: []string{"runtime:claude-code", "windows"}},
		{name: "macos has both names", configured: []string{"go", "GO"}, goos: "darwin", bins: []string{"codex"}, want: []string{"darwin", "go", "macos", "runtime:codex"}},
		{name: "unrelated binary", goos: "linux", bins: []string{"python"}, want: []string{"linux"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", fakePath(t, tt.bins...))
			if got := DetectLabels(tt.configured, tt.goos, exec.LookPath); !slices.Equal(got, tt.want) {
				t.Errorf("DetectLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

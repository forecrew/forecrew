// Package worker implements the forecrew-worker daemon: it polls forecrewd
// for jobs, runs each one in an isolated git worktree with a runtime adapter,
// pushes the agent branch and reports back (docs/contracts/worker-api.md).
package worker

import (
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// runtimeCLIs maps the runtime CLIs the worker looks for in PATH to the
// adapter they belong to.
var runtimeCLIs = []struct{ binary, adapter string }{
	{"claude", runtime.ClaudeCode},
	{"codex", runtime.Codex},
}

// osLabels returns the labels for an operating system (a GOOS value). macOS
// gets both darwin (conventions.md) and macos (the name used by repo
// profiles), so jobs written either way match.
func osLabels(goos string) []string {
	if goos == "darwin" {
		return []string{"darwin", "macos"}
	}
	return []string{goos}
}

// DetectLabels returns every label of the worker, normalized: the configured
// ones, the os label for goos and runtime:<adapter> for each runtime CLI that
// lookPath finds (exec.LookPath in production).
func DetectLabels(configured []string, goos string, lookPath func(string) (string, error)) []string {
	labels := append([]string(nil), configured...)
	labels = append(labels, osLabels(goos)...)
	for _, cli := range runtimeCLIs {
		if _, err := lookPath(cli.binary); err == nil {
			labels = append(labels, domain.RuntimeLabel(cli.adapter))
		}
	}
	return domain.NormalizeLabels(labels)
}

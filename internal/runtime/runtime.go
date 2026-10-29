// Package runtime defines the adapter between the worker and an agent
// runtime CLI (Claude Code headless, Codex CLI). internal/testkit has a
// scripted fake.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// Sentinel errors.
var (
	// ErrNotInstalled means the runtime CLI was not found on the machine.
	ErrNotInstalled = errors.New("runtime: not installed")
	// ErrInvalidOutput means the final output did not match the output schema.
	ErrInvalidOutput = errors.New("runtime: output does not match schema")
)

// RunSpec is one runtime invocation, built by the worker from a job spec.
type RunSpec struct {
	WorkDir string // repo checkout the agent works in
	Model   string
	Prompt  string
	// OutputSchema is an inline JSON Schema (no $ref) the final output must
	// match; empty means free text.
	OutputSchema json.RawMessage
	AllowedTools []string
	MaxTurns     int
	Timeout      time.Duration
	// Env is added to the runtime's environment (credentials); never logged.
	Env map[string]string
}

// Result is the outcome of a run.
type Result struct {
	// Output is the structured output, valid against RunSpec.OutputSchema.
	Output  json.RawMessage
	Text    string // final assistant message
	Turns   int
	Usage   domain.Usage
	CostUSD float64
}

// Adapter runs one kind of runtime CLI.
type Adapter interface {
	// Name is the adapter name used in config: claude-code or codex.
	Name() string
	// Detect returns the CLI version, or ErrNotInstalled.
	Detect(ctx context.Context) (string, error)
	// Run executes spec and calls emit with each normalized event (Time,
	// Type, Text and Data set) as it happens. emit is called from one
	// goroutine. Run stops when ctx is done or spec.Timeout passes.
	Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error)
}

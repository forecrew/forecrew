// Package runtime defines the adapter between the worker and an agent
// runtime CLI (Claude Code headless, Codex CLI), plus a scripted adapter that
// replays a script file for end-to-end tests. internal/testkit has an
// in-memory fake for unit tests.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	// JobMCP is the job MCP gateway the agent may call; nil means none. Its
	// token is Env[JobMCPTokenEnv], which the runtime's MCP configuration
	// refers to, so it never appears in argv or a file.
	JobMCP *JobMCP
}

// JobMCPTokenEnv is the environment variable holding the job MCP token.
const JobMCPTokenEnv = "FORECREW_JOB_MCP_TOKEN" // #nosec G101 -- a variable name, not a secret

// JobMCPServer is the server name of the gateway in the runtimes' MCP
// configuration (jobmcp.ServerName): Claude Code names its tools
// mcp__forecrew__<tool>, the role's allowed tools.
const JobMCPServer = "forecrew"

// JobMCPPath is where forecrewd serves the job MCP gateway; a worker reaches
// it under its own server URL.
const JobMCPPath = "/job-mcp"

// jobMCPToolTimeout bounds one gateway call: above ask_human's 30 minutes.
const jobMCPToolTimeout = 35 * time.Minute

// JobMCP is the job MCP gateway of a run.
type JobMCP struct {
	URL string
}

// JobMCPTool returns the runtime tool name of a gateway tool, the form
// role tools map to in a job's allowed tools: mcp__forecrew__<tool>.
func JobMCPTool(tool string) string { return "mcp__" + JobMCPServer + "__" + tool }

// JobMCPToolOf returns the gateway tool of a runtime tool name, or false
// when it is not one.
func JobMCPToolOf(runtimeTool string) (string, bool) {
	name, ok := strings.CutPrefix(runtimeTool, "mcp__"+JobMCPServer+"__")
	return name, ok && name != ""
}

// jobMCPTools returns the gateway tools among the allowed tools, by their
// gateway name.
func jobMCPTools(allowed []string) []string {
	var out []string
	for _, t := range allowed {
		if name, ok := JobMCPToolOf(t); ok {
			out = append(out, name)
		}
	}
	return out
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

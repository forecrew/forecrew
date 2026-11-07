// Package runtime defines the adapter between the worker and an agent
// runtime CLI (Claude Code headless, Codex CLI, opencode), plus a scripted adapter that
// replays a script file for end-to-end tests. internal/testkit has an
// in-memory fake for unit tests.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"slices"
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
	// EvidenceDir is a scratch directory outside the checkout the agent may
	// write evidence files to (also in Env[EvidenceDirEnv]); empty means none.
	EvidenceDir string
	// Endpoint is the custom model endpoint of the runtime; nil means the
	// vendor's own.
	Endpoint *domain.Endpoint
	// OrganizationModel says the run is on an organization's model
	// connection: the CLI gets no key, token or secret of the worker's own.
	OrganizationModel bool
	// MCPServers are the stdio MCP servers of the job's toolbox, started by
	// the CLI next to the gateway. Their secrets are the variables of Env
	// they name in CredentialEnv, which the CLI's configuration refers to.
	MCPServers []domain.MCPServer
	// Container, when set, runs the CLI inside the job's toolbox container
	// instead of on the host (see Container).
	Container Container
}

// Container runs the processes of a job inside its toolbox container.
// WorkDir and EvidenceDir are the same paths in the container as on the host.
type Container interface {
	// Wrap returns the process that runs p in the container. p.Env is the
	// whole environment of p: the container gets exactly that, never the
	// worker's own.
	Wrap(p Process) Process
}

// ContainerHome is the HOME of a toolbox container: an empty directory that
// lives and dies with the container, so each run has its own and the worker
// account's ~/.claude or ~/.codex never reaches it.
const ContainerHome = "/home/agent"

// KeyEnv returns the variable an adapter's CLI reads its API key from, which
// a worker may hold in its own environment (docs/contracts/config.md,
// credential_env), or "" for an unknown adapter.
func KeyEnv(adapter string) string {
	switch adapter {
	case ClaudeCode:
		return claudeKeyEnv
	case Codex:
		return codexKeyEnv
	}
	return ""
}

// binary returns the CLI to run: the adapter's own name inside a container
// (the operator's path is the host's), otherwise the configured one.
func (s RunSpec) binary(cfg Config, def string) string {
	if s.Container != nil {
		return def
	}
	return cfg.binary(def)
}

// baseEnv returns the environment before the adapter's additions: the
// worker's plus Env on the host, only Env in a container.
func (s RunSpec) baseEnv(cfg Config) []string {
	if s.Container == nil {
		base := startEnv(cfg.Env, nil)
		if s.organization() {
			base = withoutWorkerKeys(base)
		}
		return startEnv(base, s.Env)
	}
	env := make([]string, 0, len(s.Env))
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, k+"="+s.Env[k])
	}
	return env
}

// organization reports whether the run is on an organization's model
// connection.
func (s RunSpec) organization() bool {
	return s.OrganizationModel || (s.Endpoint != nil && s.Endpoint.Organization)
}

// workerKeep lists the worker's variables a run on an organization's model
// connection keeps: what a CLI and the agent's shell need to start and find
// their tools. Everything else of the worker is dropped, because a variable
// of the worker's must not send the organization's key or code elsewhere
// (OPENAI_BASE_URL, OLLAMA_HOST, CLAUDE_CODE_USE_BEDROCK, ANTHROPIC_VERTEX_*,
// *_ENDPOINT...) nor give the agent a credential of the worker's.
var workerKeep = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "LANG": true, "LANGUAGE": true,
	"TZ": true, "TMPDIR": true, "TERM": true, "PWD": true,
	// Windows
	"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true, "TEMP": true, "TMP": true,
	"USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMDATA": true, "PROGRAMFILES": true,
	"PROGRAMFILES(X86)": true, "SYSTEMDRIVE": true, "HOMEDRIVE": true, "HOMEPATH": true, "USERNAME": true,
}

// workerKeepPrefixes are kept like workerKeep: locale, certificates and the
// XDG directories (opencode sets its own over them).
var workerKeepPrefixes = []string{"LC_", "SSL_CERT_", "XDG_"}

// proxyNames are kept only without credentials in their URL.
var proxyNames = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true}

// keptForOrganization reports whether the worker's variable kv stays in a run
// on an organization's model connection.
func keptForOrganization(kv string) bool {
	name, value, _ := strings.Cut(kv, "=")
	n := strings.ToUpper(name)
	switch {
	case workerKeep[n]:
		return true
	case proxyNames[n]:
		u, err := url.Parse(value)
		return n == "NO_PROXY" || (err == nil && u.User == nil && !strings.Contains(value, "@"))
	}
	return slices.ContainsFunc(workerKeepPrefixes, func(p string) bool { return strings.HasPrefix(n, p) })
}

// withoutWorkerKeys keeps of env only what keptForOrganization allows.
func withoutWorkerKeys(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return !keptForOrganization(kv) })
}

// EvidenceDirEnv is the environment variable holding the scratch directory.
const EvidenceDirEnv = "FORECREW_EVIDENCE_DIR"

// EvidencePrefix marks an evidence_attach path as relative to the scratch
// directory instead of the work tree.
const EvidencePrefix = "evidence:"

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

// stdioStartupTimeout is how long codex waits for a toolbox server to start:
// a browser takes longer than its default 10 s.
const stdioStartupTimeout = time.Minute

// JobMCP is the job MCP gateway of a run.
type JobMCP struct {
	URL string
}

// JobMCPTool returns the runtime tool name of a gateway tool, the form
// role tools map to in a job's allowed tools: mcp__forecrew__<tool>.
func JobMCPTool(tool string) string { return "mcp__" + JobMCPServer + "__" + tool }

// MCPServerTool returns the allowed-tools rule that lets the agent use every
// tool of the MCP server of a toolbox: mcp__<server>.
func MCPServerTool(server string) string { return "mcp__" + server }

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
	// Name is the adapter name used in config: claude-code, codex or opencode.
	Name() string
	// Detect returns the CLI version, or ErrNotInstalled.
	Detect(ctx context.Context) (string, error)
	// Run executes spec and calls emit with each normalized event (Time,
	// Type, Text and Data set) as it happens. emit is called from one
	// goroutine. Run stops when ctx is done or spec.Timeout passes.
	Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error)
}

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// ClaudeCode is the adapter name of the Claude Code CLI.
const ClaudeCode = "claude-code"

// claudeKeyEnv is the variable claude takes its API key from.
const claudeKeyEnv = "ANTHROPIC_API_KEY" // #nosec G101 -- a variable name, not a secret

// readOnlyClaudeTools is the tool set when RunSpec.AllowedTools is empty.
var readOnlyClaudeTools = []string{"Read", "Grep", "Glob"}

// claudeCode runs Claude Code headless (`claude -p`).
type claudeCode struct{ cfg Config }

// NewClaudeCode returns the claude-code adapter.
func NewClaudeCode(cfg Config) Adapter { return &claudeCode{cfg: cfg} }

func (c *claudeCode) Name() string { return ClaudeCode }

func (c *claudeCode) Detect(ctx context.Context) (string, error) {
	return detectVersion(ctx, c.cfg, "claude")
}

// claudeArgs builds the argument list. The prompt goes through stdin, so it
// never shows in the process list.
//
// AllowedTools is a hard limit: --tools restricts the built-in tools to the
// base names, --allowedTools pre-approves the rules (for example
// Bash(git log *)), and --permission-mode dontAsk denies everything else
// instead of prompting. Only user settings are loaded, so the checked-out
// repo cannot add allow rules or servers (Run also isolates the user config
// dir). MCP tools are disabled, except, with a job MCP gateway, the
// gateway's: --strict-mcp-config loads only the server given here, and its
// allowed tools (mcp__forecrew__<tool>) are pre-approved like the others.
// --allowedTools is last because it takes a variable number of values.
func claudeArgs(spec RunSpec) []string {
	tools := spec.AllowedTools
	if len(tools) == 0 {
		tools = readOnlyClaudeTools
	}
	var builtin []string
	for _, t := range tools {
		if !strings.HasPrefix(t, "mcp__") {
			builtin = append(builtin, t)
		}
	}
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--no-session-persistence",
		"--permission-mode", "dontAsk", "--setting-sources", "user",
		"--tools", strings.Join(baseToolNames(builtin), ","),
	}
	if spec.JobMCP != nil || len(spec.MCPServers) > 0 {
		args = append(args, "--mcp-config", claudeMCPConfig(spec), "--strict-mcp-config")
	} else {
		args = append(args, "--disallowedTools", "mcp__*")
	}
	if spec.EvidenceDir != "" {
		args = append(args, "--add-dir", spec.EvidenceDir)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprint(spec.MaxTurns))
	}
	return append(args, append([]string{"--allowedTools"}, tools...)...)
}

// claudeMCPConfig is the --mcp-config of the job MCP gateway and the stdio
// servers of the toolbox. The gateway's header and the credentials of a
// server refer to environment variables, which Claude Code expands, so no
// secret is in argv.
func claudeMCPConfig(spec RunSpec) string {
	servers := map[string]any{}
	if spec.JobMCP != nil {
		servers[JobMCPServer] = map[string]any{
			"type": "http", "url": spec.JobMCP.URL,
			"headers": map[string]string{"Authorization": "Bearer ${" + JobMCPTokenEnv + "}"},
			"timeout": jobMCPToolTimeout.Milliseconds(),
		}
	}
	for _, s := range spec.MCPServers {
		env := maps.Clone(s.Env)
		if env == nil {
			env = map[string]string{}
		}
		for _, name := range s.CredentialEnv {
			env[name] = "${" + name + "}"
		}
		server := map[string]any{"type": "stdio", "command": s.Command, "args": s.Args}
		if s.Args == nil {
			server["args"] = []string{}
		}
		if len(env) > 0 {
			server["env"] = env
		}
		servers[s.Name] = server
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers}) // strings and numbers always marshal
	return string(b)
}

// baseToolNames returns the tool names of permission rules such as
// "Bash(git log *)", without duplicates.
func baseToolNames(rules []string) []string {
	var names []string
	for _, r := range rules {
		name, _, _ := strings.Cut(r, "(")
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func (c *claudeCode) Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error) {
	// Each run gets an empty config dir, so the worker account's own
	// ~/.claude (allow rules, hooks, settings) cannot widen the permissions
	// the job was given. Credentials come from the API key in spec.Env.
	configDir := ContainerHome // empty and private to the container
	if spec.Container == nil {
		var err error
		if configDir, err = os.MkdirTemp("", "forecrew-claude-"); err != nil {
			return Result{}, fmt.Errorf("create claude config dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(configDir) }()
	}
	r := cliRun{
		binary: spec.binary(c.cfg, "claude"), args: claudeArgs(spec), stdin: spec.Prompt,
		env: []string{"CLAUDE_CONFIG_DIR=" + configDir},
	}
	if spec.Endpoint != nil {
		r.env = append(r.env, "ANTHROPIC_BASE_URL="+spec.Endpoint.BaseURL)
	}
	return runCLI(ctx, c.cfg, spec, r, &claudeParser{tools: map[string]string{}}, emit)
}

// claudeParser reads the stream-json output of `claude -p --verbose`.
type claudeParser struct {
	tools  map[string]string // tool_use id -> tool name
	result *claudeResult
	text   string // last assistant text, used when there is no result event
	turns  int
}

type claudeMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []claudeBlock `json:"content"`
	} `json:"message"`
	claudeResult
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeResult struct {
	Subtype   string   `json:"subtype"`
	IsError   bool     `json:"is_error"`
	Result    string   `json:"result"`
	NumTurns  int      `json:"num_turns"`
	CostUSD   float64  `json:"total_cost_usd"`
	RawErrors []string `json:"errors"`
	Usage     struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (p *claudeParser) line(raw []byte, e *emitter) {
	var m claudeMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		e.event(domain.EventLog, "unparseable output line: "+truncate(string(raw), 200), nil)
		return
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			e.event(domain.EventStatus, "session started", nil)
		}
	case "assistant":
		p.turns++
		for _, b := range m.Message.Content {
			switch b.Type {
			case "text":
				p.text = b.Text
				e.event(domain.EventMessage, b.Text, nil)
			case "tool_use":
				p.tools[b.ID] = b.Name
				e.event(domain.EventToolCall, b.Name, map[string]any{"name": b.Name, "input": b.Input})
			}
		}
	case "user":
		for _, b := range m.Message.Content {
			if b.Type == "tool_result" {
				e.event(domain.EventToolResult, p.tools[b.ToolUseID], map[string]any{
					"name": p.tools[b.ToolUseID], "output": truncate(toolOutput(b.Content), maxToolOutput), "is_error": b.IsError,
				})
			}
		}
	case "result":
		res := m.claudeResult
		p.result = &res
	}
}

// toolOutput flattens a tool_result content: a string or a list of blocks.
func toolOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (p *claudeParser) done() outcome {
	if p.result == nil {
		return outcome{text: p.text, turns: p.turns, incomplete: true}
	}
	r := p.result
	out := outcome{
		text:  r.Result,
		turns: r.NumTurns,
		usage: domain.Usage{
			InputTokens: r.Usage.Input, OutputTokens: r.Usage.Output,
			CacheReadTokens: r.Usage.CacheRead, CacheWriteTokens: r.Usage.CacheWrite,
		},
		costUSD: r.CostUSD,
	}
	if r.IsError {
		out.failure = "claude: " + r.Subtype
		if len(r.RawErrors) > 0 {
			out.failure += ": " + strings.Join(r.RawErrors, "; ")
		} else if r.Result != "" {
			out.failure += ": " + r.Result
		}
	}
	return out
}

// detectVersion runs `<binary> --version` and returns its first line.
func detectVersion(ctx context.Context, cfg Config, def string) (string, error) {
	binary := cfg.binary(def)
	cmd := exec.CommandContext(ctx, binary, "--version") //nolint:gosec // the binary is operator config
	cmd.Env = startEnv(cfg.Env, nil)
	out, err := cmd.Output()
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%s: %w", binary, ErrNotInstalled)
		}
		return "", fmt.Errorf("detect %s: %w", binary, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

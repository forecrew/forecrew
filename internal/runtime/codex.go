package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// Codex is the adapter name of the Codex CLI.
const Codex = "codex"

// editingTools are the allowed tools that make a run need a writable sandbox.
var editingTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// codexCLI runs `codex exec` non-interactively.
type codexCLI struct{ cfg Config }

// NewCodex returns the codex adapter.
func NewCodex(cfg Config) Adapter { return &codexCLI{cfg: cfg} }

func (c *codexCLI) Name() string { return Codex }

func (c *codexCLI) Detect(ctx context.Context) (string, error) {
	return detectVersion(ctx, c.cfg, "codex")
}

// codexArgs builds the argument list. The prompt goes through stdin ("-").
// Codex has no tool allow list or turn limit: allowed tools only choose the
// sandbox (writable when the job may edit files, read-only otherwise).
func codexArgs(spec RunSpec) []string {
	sandbox := "read-only"
	for _, t := range spec.AllowedTools {
		name, _, _ := strings.Cut(t, "(")
		if slices.Contains(editingTools, name) {
			sandbox = "workspace-write"
			break
		}
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ephemeral", "-c", `approval_policy="never"`, "--sandbox", sandbox}
	if spec.WorkDir != "" {
		args = append(args, "--cd", spec.WorkDir)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.JobMCP != nil {
		args = append(args, codexMCPConfig(spec.JobMCP.URL, jobMCPTools(spec.AllowedTools))...)
	}
	return append(args, "-")
}

// codexMCPConfig returns the -c overrides that add the job MCP gateway as a
// streamable HTTP server: the token comes from its environment variable
// (bearer_token_env_var), only the allowed gateway tools are enabled, they
// run without approval and may take longer than ask_human's 30 minutes.
func codexMCPConfig(url string, tools []string) []string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = strconv.Quote(t)
	}
	server := "mcp_servers." + JobMCPServer + "."
	return []string{
		"-c", server + "url=" + strconv.Quote(url),
		"-c", server + "bearer_token_env_var=" + strconv.Quote(JobMCPTokenEnv),
		"-c", server + "enabled_tools=[" + strings.Join(quoted, ",") + "]",
		"-c", server + `default_tools_approval_mode="approve"`,
		"-c", server + "tool_timeout_sec=" + strconv.Itoa(int(jobMCPToolTimeout.Seconds())),
	}
}

func (c *codexCLI) Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error) {
	r := cliRun{binary: c.cfg.binary("codex"), args: codexArgs(spec), stdin: spec.Prompt}
	return runCLI(ctx, c.cfg, spec, r, &codexParser{started: map[string]bool{}}, emit)
}

// codexParser reads the JSONL events of `codex exec --json`.
type codexParser struct {
	started map[string]bool // item ids whose start was reported
	text    string
	turns   int
	usage   domain.Usage
	failure string
}

type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		Input      int64 `json:"input_tokens"`
		CacheRead  int64 `json:"cached_input_tokens"`
		CacheWrite int64 `json:"cache_write_input_tokens"`
		Output     int64 `json:"output_tokens"`
	} `json:"usage"`
	Item codexItem `json:"item"`
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Message          string          `json:"message"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Changes          json.RawMessage `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Query            string          `json:"query"`
	Status           string          `json:"status"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *codexParser) line(raw []byte, e *emitter) {
	var ev codexEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		e.event(domain.EventLog, "unparseable output line: "+truncate(string(raw), 200), nil)
		return
	}
	switch ev.Type {
	case "thread.started":
		e.event(domain.EventStatus, "session started", nil)
	case "turn.started":
		p.turns++
	case "turn.completed":
		// The usage of turn.completed is a running total for the thread.
		p.usage = domain.Usage{
			InputTokens:      ev.Usage.Input - ev.Usage.CacheRead,
			CacheReadTokens:  ev.Usage.CacheRead,
			CacheWriteTokens: ev.Usage.CacheWrite,
			OutputTokens:     ev.Usage.Output,
		}
	case "turn.failed":
		p.failure = cmp.Or(ev.Error.Message, "turn failed")
	case "error":
		// Non-fatal errors (reconnects) are also reported this way; a failed
		// turn or a non-zero exit decides whether the run failed.
		e.event(domain.EventError, ev.Message, nil)
	case "item.started", "item.updated", "item.completed":
		p.item(ev.Type == "item.completed", ev.Item, e)
	}
}

// item reports one thread item. A call is reported once, when first seen.
func (p *codexParser) item(completed bool, it codexItem, e *emitter) {
	switch it.Type {
	case "agent_message":
		if completed {
			p.text = it.Text
			e.event(domain.EventMessage, it.Text, nil)
		}
	case "command_execution":
		p.call(it, "shell", map[string]any{"command": it.Command}, e)
		if completed {
			failed := it.failed() || (it.ExitCode != nil && *it.ExitCode != 0)
			e.event(domain.EventToolResult, "shell", map[string]any{
				"name": "shell", "output": truncate(it.AggregatedOutput, maxToolOutput), "is_error": failed,
			})
		}
	case "mcp_tool_call":
		name := it.Server + "." + it.Tool
		p.call(it, name, map[string]any{"input": it.Arguments}, e)
		if completed {
			out := string(it.Result)
			if it.Error != nil {
				out = it.Error.Message
			}
			e.event(domain.EventToolResult, name, map[string]any{
				"name": name, "output": truncate(out, maxToolOutput), "is_error": it.failed() || it.Error != nil,
			})
		}
	case "file_change":
		p.call(it, "file_change", map[string]any{"input": it.Changes}, e)
	case "web_search":
		p.call(it, "web_search", map[string]any{"input": map[string]string{"query": it.Query}}, e)
	case "error":
		if completed {
			e.event(domain.EventError, it.Message, nil)
		}
	}
}

// failed reports a call that did not succeed: it failed or was declined by
// the approval policy or sandbox.
func (it codexItem) failed() bool { return it.Status == "failed" || it.Status == "declined" }

func (p *codexParser) call(it codexItem, name string, data map[string]any, e *emitter) {
	if p.started[it.ID] {
		return
	}
	p.started[it.ID] = true
	data["name"] = name
	e.event(domain.EventToolCall, name, data)
}

func (p *codexParser) done() outcome {
	return outcome{
		text: p.text, turns: p.turns, usage: p.usage, failure: p.failure,
		incomplete: p.turns == 0,
	}
}

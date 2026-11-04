package runtime

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

const (
	testJobToken = "fcj_test-job-token-SECRET-0123456789abcdefghijkl"
	testGateway  = "https://forecrew.example.com/job-mcp"
)

// withJobMCP adds the gateway to a spec, with ask_human and report_progress
// allowed, the way the worker does.
func withJobMCP(spec RunSpec) RunSpec {
	spec.JobMCP = &JobMCP{URL: testGateway}
	spec.AllowedTools = append(spec.AllowedTools, JobMCPTool("ask_human"), JobMCPTool("report_progress"))
	spec.Env[JobMCPTokenEnv] = testJobToken
	return spec
}

// noToken fails when the token shows in argv or in the events.
func noToken(t *testing.T, r fakeRecord, events []domain.JobEvent) {
	t.Helper()
	if r.JobMCPToken != testJobToken {
		t.Errorf("the CLI got token %q", r.JobMCPToken)
	}
	for _, a := range r.Args {
		if strings.Contains(a, testJobToken) {
			t.Errorf("token in argv: %q", a)
		}
	}
	b, _ := json.Marshal(events)
	if strings.Contains(string(b), testJobToken) {
		t.Errorf("token in events: %s", b)
	}
}

func TestClaudeJobMCP(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeClaude("FAKE_STREAM="+stream("claude_success.jsonl"), "FAKE_RECORD="+rec)
	spec := withJobMCP(claudeSpec())
	spec.WorkDir = t.TempDir()
	events, _, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, rec)
	noToken(t, r, events)
	i := slices.Index(r.Args, "--mcp-config")
	if i < 0 || r.Args[i+2] != "--strict-mcp-config" || slices.Contains(r.Args, "--disallowedTools") {
		t.Fatalf("args = %q", r.Args)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Timeout int64             `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.Args[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	s := cfg.MCPServers["forecrew"]
	if s.Type != "http" || s.URL != testGateway || s.Headers["Authorization"] != "Bearer ${FORECREW_JOB_MCP_TOKEN}" || s.Timeout < 30*60*1000 {
		t.Fatalf("mcp config = %+v", cfg)
	}
	// The gateway tools are pre-approved but are not built-in tools.
	tools := r.Args[slices.Index(r.Args, "--tools")+1]
	allowed := r.Args[slices.Index(r.Args, "--allowedTools")+1:]
	if strings.Contains(tools, "mcp__") || !slices.Contains(allowed, "mcp__forecrew__ask_human") {
		t.Fatalf("tools %q, allowed %q", tools, allowed)
	}
}

func TestCodexJobMCP(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec.json")
	a := newFakeCodex("FAKE_STREAM="+stream("codex_success.jsonl"), "FAKE_RECORD="+rec)
	spec := withJobMCP(codexSpec())
	spec.WorkDir = t.TempDir()
	events, _, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, rec)
	noToken(t, r, events)
	for _, want := range []string{
		`mcp_servers.forecrew.url="https://forecrew.example.com/job-mcp"`,
		`mcp_servers.forecrew.bearer_token_env_var="FORECREW_JOB_MCP_TOKEN"`,
		`mcp_servers.forecrew.enabled_tools=["ask_human","report_progress"]`,
		`mcp_servers.forecrew.default_tools_approval_mode="approve"`,
		`mcp_servers.forecrew.tool_timeout_sec=2100`,
	} {
		if i := slices.Index(r.Args, want); i < 1 || r.Args[i-1] != "-c" {
			t.Errorf("missing -c %s in %q", want, r.Args)
		}
	}
	if r.Args[len(r.Args)-1] != "-" {
		t.Errorf("prompt is not read from stdin: %q", r.Args)
	}
	if slices.ContainsFunc(codexArgs(codexSpec()), func(a string) bool { return strings.Contains(a, "mcp_servers") }) {
		t.Error("gateway configured without job_mcp")
	}
}

func TestJobMCPToolNames(t *testing.T) {
	if name, ok := JobMCPToolOf(JobMCPTool("ask_human")); !ok || name != "ask_human" {
		t.Fatalf("JobMCPToolOf = %q %v", name, ok)
	}
	for _, bad := range []string{"Read", "mcp__forecrew__", "mcp__other__x"} {
		if _, ok := JobMCPToolOf(bad); ok {
			t.Errorf("%q taken for a gateway tool", bad)
		}
	}
}

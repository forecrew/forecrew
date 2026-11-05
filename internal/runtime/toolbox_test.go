package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

// fakeBox is a Container that runs what it wraps as the fake CLI on the host
// and remembers the process it was given.
type fakeBox struct {
	got   Process
	extra []string
	// run is the program run instead of the wrapped one; empty means the
	// test binary as the fake CLI.
	run string
}

func (b *fakeBox) Wrap(p Process) Process {
	b.got = p
	out := p
	out.Env = append(slices.Clone(p.Env), b.extra...)
	if b.run == "" {
		out.Path, out.Env = os.Args[0], append(out.Env, "FORECREW_FAKE_CLI=1")
	} else {
		out.Path = b.run
	}
	return out
}

var stdioServers = []domain.MCPServer{
	{Name: "playwright", Command: "playwright-mcp", Args: []string{"--headless"}, Env: map[string]string{"MODE": "ci"}},
	{Name: "scanner", Command: "scan", CredentialEnv: []string{"SCAN_KEY"}},
}

func TestContainerEnvAndHome(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    func(env ...string) Adapter
		spec RunSpec
		home string
		key  string
	}{
		{"claude", func(env ...string) Adapter { return newFakeClaude(env...) }, claudeSpec(), "CLAUDE_CONFIG_DIR=" + ContainerHome, "API_KEY"},
		{"codex", func(env ...string) Adapter { return newFakeCodex(env...) }, codexSpec(), "CODEX_HOME=" + ContainerHome, "CODEX_API_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec.json")
			// The adapter's Env is the worker's: a variable of it, and the
			// operator's binary, must not reach the container.
			a := tc.a("WORKER_PRIVATE=1")
			box := &fakeBox{extra: []string{"FAKE_STREAM=" + stream(tc.name + "_success.jsonl"), "FAKE_RECORD=" + rec}}
			spec := tc.spec
			spec.WorkDir, spec.Container = t.TempDir(), box
			if _, _, err := collect(t, a, spec); err != nil {
				t.Fatal(err)
			}
			// The binary is the adapter's own name: Config.Binary is a host path.
			if box.got.Path == os.Args[0] || (box.got.Path != "claude" && box.got.Path != "codex") {
				t.Errorf("wrapped path = %q", box.got.Path)
			}
			if box.got.Dir != spec.WorkDir {
				t.Errorf("wrapped dir = %q, want %q", box.got.Dir, spec.WorkDir)
			}
			if !slices.Contains(box.got.Env, tc.home) {
				t.Errorf("home not set in %q", box.got.Env)
			}
			for _, kv := range box.got.Env {
				if strings.HasPrefix(kv, "WORKER_PRIVATE=") || strings.HasPrefix(kv, "PATH=") {
					t.Errorf("worker variable %q reached the container", kv)
				}
			}
			if !slices.ContainsFunc(box.got.Env, func(kv string) bool { return strings.HasPrefix(kv, tc.key+"=") }) {
				t.Errorf("credential %s missing in %q", tc.key, box.got.Env)
			}
		})
	}
}

func TestContainerSpecHelpers(t *testing.T) {
	spec := RunSpec{Env: map[string]string{"B": "2", "A": "1"}}
	cfg := Config{Binary: "/opt/claude", Env: []string{"W=1"}}
	if got := spec.baseEnv(cfg); len(got) != 3 || got[0] != "W=1" {
		t.Errorf("host env = %q", got)
	}
	spec.Container = &fakeBox{}
	if got := spec.baseEnv(cfg); !slices.Equal(got, []string{"A=1", "B=2"}) {
		t.Errorf("container env = %q", got)
	}
	if got := spec.binary(cfg, "claude"); got != "claude" {
		t.Errorf("container binary = %q", got)
	}
	spec.Container = nil
	if got := spec.binary(cfg, "claude"); got != "/opt/claude" {
		t.Errorf("host binary = %q", got)
	}
}

func TestClaudeToolboxServers(t *testing.T) {
	spec := claudeSpec()
	spec.MCPServers = stdioServers
	spec.AllowedTools = append(spec.AllowedTools, MCPServerTool("playwright"))
	args := claudeArgs(spec)
	i := slices.Index(args, "--mcp-config")
	if i < 0 || args[i+2] != "--strict-mcp-config" || slices.Contains(args, "--disallowedTools") {
		t.Fatalf("args = %q", args)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	pw, sc := cfg.MCPServers["playwright"], cfg.MCPServers["scanner"]
	if pw.Type != "stdio" || pw.Command != "playwright-mcp" || !slices.Equal(pw.Args, []string{"--headless"}) || pw.Env["MODE"] != "ci" {
		t.Errorf("playwright = %+v", pw)
	}
	// A credential is a reference the CLI expands, never the value; no args
	// is an empty list, not null.
	if sc.Env["SCAN_KEY"] != "${SCAN_KEY}" || sc.Args == nil || len(cfg.MCPServers) != 2 {
		t.Errorf("scanner = %+v, servers %v", sc, cfg.MCPServers)
	}
	tools := args[slices.Index(args, "--tools")+1]
	if allowed := args[slices.Index(args, "--allowedTools")+1:]; strings.Contains(tools, "mcp__") || !slices.Contains(allowed, "mcp__playwright") {
		t.Errorf("tools %q, allowed %q", tools, allowed)
	}

	// With the gateway both are configured, the gateway first.
	spec.JobMCP = &JobMCP{URL: testGateway}
	args = claudeArgs(spec)
	if err := json.Unmarshal([]byte(args[slices.Index(args, "--mcp-config")+1]), &cfg); err != nil || len(cfg.MCPServers) != 3 {
		t.Errorf("servers with the gateway: %v, %v", cfg.MCPServers, err)
	}
}

func TestCodexToolboxServers(t *testing.T) {
	spec := codexSpec()
	spec.MCPServers = stdioServers
	args := codexArgs(spec)
	for _, want := range []string{
		`mcp_servers.playwright.command="playwright-mcp"`,
		`mcp_servers.playwright.args=["--headless"]`,
		`mcp_servers.playwright.env={MODE="ci"}`,
		`mcp_servers.playwright.default_tools_approval_mode="approve"`,
		`mcp_servers.playwright.startup_timeout_sec=60`,
		`mcp_servers.scanner.args=[]`,
		`mcp_servers.scanner.env_vars=["SCAN_KEY"]`,
	} {
		if i := slices.Index(args, want); i < 1 || args[i-1] != "-c" {
			t.Errorf("missing -c %s in %q", want, args)
		}
	}
	if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "bearer_token") }) {
		t.Error("gateway configured without job_mcp")
	}
}

func TestScriptedCommands(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "script.json")
	body := `{"commands":[{"argv":["sh","-c","echo found $LEAK_KEY; echo oops >&2; exit 1"]},{"argv":["true"]}],"text":"done"}`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewScripted(script, Config{})
	box := &fakeBox{run: "/bin/sh", extra: []string{"PATH=" + os.Getenv("PATH")}}
	// The wrapped "sh" is run as /bin/sh: its args are the command's.
	spec := RunSpec{WorkDir: dir, Container: box, Env: map[string]string{"LEAK_KEY": "sekret-value-12345"}}
	events, res, err := collect(t, a, spec)
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	for _, e := range events {
		if e.Type == domain.EventLog {
			logs = append(logs, e.Text)
		}
	}
	want := []string{"$ sh -c echo found $LEAK_KEY; echo oops >&2; exit 1", "found [redacted]", "oops", "exit code 1", "$ true", "exit code 0"}
	if res.Text != "done" || len(logs) != len(want) {
		t.Fatalf("logs = %q, text %q", logs, res.Text)
	}
	for _, w := range want {
		if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, strings.TrimSuffix(w, "[redacted]")) }) {
			t.Errorf("missing log %q in %q", w, logs)
		}
	}
	for _, l := range logs {
		if strings.Contains(l, "sekret-value") {
			t.Errorf("secret in log %q", l)
		}
	}
}

func TestScriptedCommandErrors(t *testing.T) {
	write := func(body string) Adapter {
		p := filepath.Join(t.TempDir(), "script.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return NewScripted(p, Config{})
	}
	_, _, err := collect(t, write(`{"commands":[{"argv":["true"]}]}`), RunSpec{})
	if !errors.Is(err, errNoContainer) {
		t.Errorf("without a container: %v", err)
	}
	_, _, err = collect(t, write(`{"commands":[{"argv":[]}]}`), RunSpec{Container: &fakeBox{run: "/bin/sh"}})
	if err == nil || !strings.Contains(err.Error(), "no argv") {
		t.Errorf("empty argv: %v", err)
	}
	_, _, err = collect(t, write(`{"commands":[{"argv":["true"]}]}`), RunSpec{Container: &fakeBox{run: "/nonexistent/docker"}})
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("missing docker: %v", err)
	}
}

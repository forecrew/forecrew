//go:build docker && browser && !windows

package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// TestBrowserToolboxServerIsListed is the smoke test of the browser toolbox:
// the agent CLI inside the container, given the toolbox's MCP server, lists
// it as connected. Building the image downloads Chromium, which takes
// minutes and a few hundred MB, so it has its own tag:
// go test -tags 'docker browser' -run Browser ./internal/worker.
func TestBrowserToolboxServerIsListed(t *testing.T) {
	box := packToolbox(t, "browser")
	if len(box.MCPServers) != 1 {
		t.Fatalf("browser toolbox servers = %+v", box.MCPServers)
	}
	srv := box.MCPServers[0]
	conf, _ := json.Marshal(map[string]any{"type": "stdio", "command": srv.Command, "args": srv.Args})
	// `claude mcp list` starts every configured server and reports whether it
	// answered; the server is added the way the run's --mcp-config adds it.
	script := "claude mcp add-json --scope user " + srv.Name + " '" + string(conf) + "' && claude mcp list"
	e := toolboxWorker(t, runtime.Script{Commands: []runtime.ScriptCommand{{Argv: []string{"sh", "-c", script}}}})
	s := e.spec(func(s *domain.JobSpec) {
		s.Toolbox, s.Timeout, s.WritePaths = &box, domain.Duration(10*time.Minute), nil
	})
	c := e.doComplete(s)
	logs := strings.Join(logsOf(e, s.JobID), "\n")
	if c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v\nlogs:\n%s", c, logs)
	}
	if !strings.Contains(logs, srv.Name+": ") || !strings.Contains(logs, "Connected") {
		t.Errorf("the server is not listed as connected:\n%s", logs)
	}
}

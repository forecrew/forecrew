package councils

import "github.com/forecrew/forecrew/internal/roles"

// The shipped roles list job MCP gateway tools, which internal/jobmcp
// registers; this package cannot import it, so its tests register the names.
func init() {
	for name, tool := range map[string]string{"kb.search": "mcp__forecrew__kb_search", "notes.write": "mcp__forecrew__notes_write", "kb.propose": "mcp__forecrew__kb_propose", "request_access": "mcp__forecrew__request_access"} {
		if !roles.HasTool(name) {
			roles.RegisterTool(name, tool)
		}
	}
}

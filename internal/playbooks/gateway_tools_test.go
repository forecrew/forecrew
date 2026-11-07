package playbooks_test

import "github.com/forecrew/forecrew/internal/roles"

// The shipped roles list job MCP gateway tools, which internal/jobmcp
// registers; this package cannot import it, so its tests register the names.
func init() {
	for name, tool := range map[string]string{"config.read": "mcp__forecrew__config_read", "config.plan": "mcp__forecrew__config_plan", "models.suggest": "mcp__forecrew__models_suggest", "ask_human": "mcp__forecrew__ask_human", "kb.search": "mcp__forecrew__kb_search", "notes.write": "mcp__forecrew__notes_write", "kb.propose": "mcp__forecrew__kb_propose", "request_access": "mcp__forecrew__request_access"} {
		if !roles.HasTool(name) {
			roles.RegisterTool(name, tool)
		}
	}
}

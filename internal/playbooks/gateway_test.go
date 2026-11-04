package playbooks_test

import "github.com/forecrew/forecrew/internal/roles"

// The shipped roles list job MCP role tools. The gateway registers them when
// internal/jobmcp is linked, which these tests do not do, so they are
// registered here.
func init() { roles.RegisterTool("assumption.record", "mcp__forecrew__assumption_record") }

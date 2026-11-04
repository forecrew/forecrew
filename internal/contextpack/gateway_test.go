package contextpack

import "github.com/forecrew/forecrew/internal/roles"

// The shipped roles list job MCP role tools. The gateway registers them
// when internal/jobmcp is linked, which these tests cannot do (jobmcp
// imports contextpack), so they are registered here.
func init() { roles.RegisterTool("assumption.record", "mcp__forecrew__assumption_record") }

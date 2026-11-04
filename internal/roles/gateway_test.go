package roles

// The shipped roles list job MCP role tools. The gateway registers them
// when internal/jobmcp is linked, which this package's tests cannot do
// (jobmcp imports roles), so they are registered here.
func init() { RegisterTool("assumption.record", "mcp__forecrew__assumption_record") }

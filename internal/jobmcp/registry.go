// Package jobmcp is the job MCP gateway (docs/contracts/job-mcp.md): an MCP
// endpoint at /job-mcp that an agent reaches while its job runs, with a
// token bound to one job and one lease. Tools are registered with Register,
// so other packages add theirs without changing the gateway.
package jobmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/roles"
)

// ServerName is the name of the gateway in the runtimes' MCP configuration;
// Claude Code names its tools mcp__<ServerName>__<tool>.
const ServerName = "forecrew"

// RuntimeTool returns the runtime tool name of a gateway tool, the form
// role tools map to in a job's allowed tools: mcp__forecrew__<tool>.
func RuntimeTool(tool string) string { return "mcp__" + ServerName + "__" + tool }

// ToolOf returns the gateway tool of a runtime tool name, or false when it
// is not one.
func ToolOf(runtimeTool string) (string, bool) {
	return strings.CutPrefix(runtimeTool, "mcp__"+ServerName+"__")
}

// Handler runs a tool call. in is the call's arguments, already valid
// against the tool's input schema. It returns the structured output, or a
// *ToolError the agent can act on; any other error is an internal error.
type Handler func(ctx context.Context, c *Call, in json.RawMessage) (Result, error)

// Result is what a tool returns: Data is the structuredContent (a JSON
// object matching the output schema), Text the text content, which defaults
// to Data as JSON. Text from the repo, a provider, the web, a note or
// another agent must be wrapped with contextpack.WrapUntrusted.
type Result struct {
	Data any
	Text string
}

// Tool is a gateway tool.
type Tool struct {
	// Name is the wire name, e.g. ask_human (^[a-z][a-z0-9_]*$).
	Name string
	// RoleTool is the name a role lists in tools to see it, e.g.
	// evidence.attach.
	RoleTool    string
	Description string
	// Input and Output are self-contained JSON Schemas (no $ref); see
	// ContractSchema.
	Input, Output json.RawMessage
	// MaxCalls is how many times one job attempt may call the tool; 0 is
	// no limit beyond the gateway's rate.
	MaxCalls int
	Handler  Handler
}

var (
	registry = map[string]Tool{}
	toolName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Register adds a tool to the gateway and makes its role tool valid in role
// files (roles.RegisterTool). Call it from an init function; a malformed or
// duplicate tool panics.
func Register(t Tool) {
	switch {
	case !toolName.MatchString(t.Name):
		panic("jobmcp: invalid tool name " + t.Name)
	case t.RoleTool == "" || t.Handler == nil || len(t.Input) == 0 || len(t.Output) == 0 || t.Description == "":
		panic("jobmcp: tool " + t.Name + " is incomplete")
	}
	if _, ok := registry[t.Name]; ok {
		panic("jobmcp: tool " + t.Name + " registered twice")
	}
	var in map[string]any
	if err := json.Unmarshal(t.Input, &in); err != nil || in["type"] != "object" {
		panic("jobmcp: input schema of " + t.Name + " must be a JSON object schema")
	}
	registry[t.Name] = t
	roles.RegisterTool(t.RoleTool, RuntimeTool(t.Name))
}

// Tools returns the registered tools, by name.
func Tools() []Tool {
	return slices.SortedFunc(maps.Values(registry), func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
}

// offered returns the registered tools a job sees: those whose runtime tool
// is among its allowed tools, which come from its role's tools.
func offered(allowedTools []string) []Tool {
	var out []Tool
	for _, t := range Tools() {
		if slices.Contains(allowedTools, RuntimeTool(t.Name)) {
			out = append(out, t)
		}
	}
	return out
}

// contractFile is the contract schema of the gateway's tools.
const contractFile = "job-mcp.schema.json"

// ContractSchema returns the input and output schema of a tool from
// docs/contracts/job-mcp.schema.json ($defs/<tool>_input and _output),
// with every $ref inlined so each stands alone. It panics when the contract
// has no such tool: tools are registered at startup.
func ContractSchema(tool string) (input, output json.RawMessage) {
	data, err := fs.ReadFile(forecrew.Files, "docs/contracts/"+contractFile)
	if err != nil {
		panic("jobmcp: " + err.Error())
	}
	var doc struct {
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		panic("jobmcp: " + contractFile + ": " + err.Error())
	}
	schema := func(def string) json.RawMessage {
		d, ok := doc.Defs[def]
		if !ok {
			panic(fmt.Sprintf("jobmcp: %s has no $defs/%s", contractFile, def))
		}
		b, err := json.Marshal(inline(d, doc.Defs))
		if err != nil {
			panic("jobmcp: " + err.Error())
		}
		return b
	}
	return schema(tool + "_input"), schema(tool + "_output")
}

// inline returns v with every {"$ref": "#/$defs/x", ...} replaced by a copy
// of x, the sibling keys (a description) taking precedence.
func inline(v any, defs map[string]any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		if ref, ok := x["$ref"].(string); ok {
			name, _ := strings.CutPrefix(ref, "#/$defs/")
			def, ok := defs[name]
			if !ok {
				panic("jobmcp: unknown $ref " + ref)
			}
			maps.Copy(out, inline(def, defs).(map[string]any))
		}
		for k, val := range x {
			if k != "$ref" {
				out[k] = inline(val, defs)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = inline(val, defs)
		}
		return out
	}
	return v
}

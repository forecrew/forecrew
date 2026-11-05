package mcp

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestToolAccessCoversEveryTool fails when an MCP tool has no access entry
// (docs/contracts/permissions.md), or an entry names no tool.
func TestToolAccessCoversEveryTool(t *testing.T) {
	access := ToolAccess()
	for _, tool := range Tools() {
		a, ok := access[tool]
		if !ok {
			t.Errorf("tool %s has no entry in ToolAccess", tool)
			continue
		}
		if err := CheckAccess(a); err != nil {
			t.Errorf("tool %s: %v", tool, err)
		}
	}
	for _, tool := range slices.Sorted(maps.Keys(access)) {
		if !slices.Contains(Tools(), tool) {
			t.Errorf("ToolAccess lists %s, which is not a tool", tool)
		}
	}
}

func TestCheckAccess(t *testing.T) {
	ok := []Access{{}, {Public: true}, {Gate: true}, {Permission: "goal.read"}, {Permission: "token.manage", Own: true}, {Target: true}}
	for _, a := range ok {
		if err := CheckAccess(a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Access{{Public: true, Own: true}, {Gate: true, Permission: "goal.read"}, {Own: true}, {Permission: "goal.delete"}, {Target: true, Permission: "goal.read"}, {Public: true, Target: true}}
	for _, a := range bad {
		if err := CheckAccess(a); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

// TestToolsAreDocumented keeps the tool table of the permissions contract and
// docs/mcp.md in step with Tools.
func TestToolsAreDocumented(t *testing.T) {
	for _, file := range []string{"../../docs/contracts/permissions.md", "../../docs/mcp.md", "../../docs/api.md"} {
		doc, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range Tools() {
			if !strings.Contains(string(doc), "`"+tool+"`") {
				t.Errorf("%s lacks %s", file, tool)
			}
		}
	}
}

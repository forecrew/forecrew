package mcp

import (
	"maps"
	"slices"
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
	ok := []Access{{}, {Public: true}, {Gate: true}, {Permission: "goal.read"}, {Permission: "token.manage", Own: true}}
	for _, a := range ok {
		if err := CheckAccess(a); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Access{{Public: true, Own: true}, {Gate: true, Permission: "goal.read"}, {Own: true}, {Permission: "goal.delete"}}
	for _, a := range bad {
		if err := CheckAccess(a); err == nil {
			t.Errorf("%+v accepted", a)
		}
	}
}

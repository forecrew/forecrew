package jobmcp

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/runtime"
)

// TestEveryPackToolMapsToOpencode: this package registers the gateway tools,
// so here a role has every tool it can list. A role on opencode must get
// exactly what its tools say, and no rule may wait for an answer.
func TestEveryPackToolMapsToOpencode(t *testing.T) {
	defs, err := roles.Load(forecrew.Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		d := defs[name]
		tools := d.AllowedTools()
		rules, err := runtime.OpencodeRules(tools, "/scratch")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		b, err := json.Marshal(rules)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"ask"`) {
			t.Errorf("%s: rules ask: %s", name, b)
		}
		_, edit := rules["edit"]
		if want := slices.Contains(tools, "Edit"); edit != want {
			t.Errorf("%s: edit rule %v, role edits %v", name, edit, want)
		}
		if len(d.WritePaths) == 0 && edit {
			t.Errorf("%s: read-only role can edit", name)
		}
		if shell := rules["bash"]; shell != "allow" && slices.Contains(tools, "Bash") {
			t.Errorf("%s: bash = %v", name, shell)
		}
		for _, tool := range tools {
			if gateway, ok := runtime.JobMCPToolOf(tool); ok && rules[runtime.JobMCPServer+"_"+gateway] != "allow" {
				t.Errorf("%s: gateway tool %s is not allowed: %s", name, gateway, b)
			}
		}
	}
}

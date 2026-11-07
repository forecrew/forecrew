package roles

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestReadOnly(t *testing.T) {
	if !(Role{}).ReadOnly() {
		t.Error("role without write paths is not read-only")
	}
	if (Role{WritePaths: []string{"**", "!.github/**"}}).ReadOnly() {
		t.Error("role with write paths is read-only")
	}
}

func TestWithoutWrites(t *testing.T) {
	allowed := Definition{Tools: []string{"repo.read", "repo.write", "git.read", "git.commit", "run.tests"}}.AllowedTools()
	r := Role{Name: "developer", WritePaths: []string{"**"}, AllowedTools: allowed}
	got := r.WithoutWrites()
	want := Definition{Tools: []string{"repo.read", "git.read", "run.tests"}}.AllowedTools()
	if !got.ReadOnly() || !slices.Equal(got.AllowedTools, want) || got.Name != "developer" {
		t.Errorf("WithoutWrites = %+v, want read-only with %v", got, want)
	}
	if !slices.Equal(r.AllowedTools, allowed) {
		t.Errorf("WithoutWrites changed the original: %v", r.AllowedTools)
	}
}

func TestWithWritePaths(t *testing.T) {
	allowed := Definition{Tools: []string{"repo.read", "git.read"}}.AllowedTools()
	r := Role{Name: "architect", AllowedTools: allowed}
	got := r.WithWritePaths([]string{"docs/adr/**"})
	if got.ReadOnly() || !slices.Equal(got.WritePaths, []string{"docs/adr/**"}) || !slices.Contains(got.AllowedTools, "Edit") || !slices.Contains(got.AllowedTools, "Write") {
		t.Errorf("WithWritePaths = %+v", got)
	}
	if !r.ReadOnly() || !slices.Equal(r.AllowedTools, allowed) {
		t.Errorf("WithWritePaths changed the original: %+v", r)
	}
	// A role that can edit already keeps its tools once.
	dev := Role{AllowedTools: Definition{Tools: []string{"repo.write"}}.AllowedTools()}
	if got := dev.WithWritePaths([]string{"a/**"}); len(got.AllowedTools) != 2 {
		t.Errorf("tools = %v", got.AllowedTools)
	}
	if got := r.WithWritePaths(nil); !got.ReadOnly() || !slices.Equal(got.AllowedTools, allowed) {
		t.Errorf("no paths: %+v", got)
	}
}

func TestPathAllowed(t *testing.T) {
	dev := []string{"**", "!.github/**"}
	for _, tt := range []struct {
		patterns []string
		file     string
		want     bool
	}{
		{dev, "a/b.go", true},
		{dev, ".github/ci.yml", false},
		{nil, "a.go", false},
		{[]string{"!a.go"}, "b.go", false},
		{[]string{"[a"}, "a", false},
		{[]string{"**", "!knowledge/**"}, "Knowledge/Vision/x.md", false},
		{[]string{"**", "!Docs/**"}, "docs/a.md", false},
		{[]string{"Docs/**"}, "docs/a.md", false}, // includes keep their case
	} {
		if got := PathAllowed(tt.patterns, tt.file); got != tt.want {
			t.Errorf("PathAllowed(%v, %q) = %v, want %v", tt.patterns, tt.file, got, tt.want)
		}
	}
}

func TestProposalReviewer(t *testing.T) {
	r := Role{Name: "architect", WritePaths: []string{"docs/**"}, Instructions: "Plan.", OutputSchema: json.RawMessage(`{"plan":1}`),
		AllowedTools:     append(append([]string{"Read"}, tools["kb.search"]...), append(tools["notes.write"], tools["kb.propose"]...)...),
		DefinitionOfDone: []string{"a plan"}, EscalateWhen: []string{"unclear"}}
	got := r.ProposalReviewer(json.RawMessage(`{"verdict":1}`))
	if !got.ReadOnly() || string(got.OutputSchema) != `{"verdict":1}` || got.DefinitionOfDone != nil || got.EscalateWhen != nil {
		t.Errorf("role = %+v", got)
	}
	if !strings.Contains(got.Instructions, "architect") || strings.Contains(got.Instructions, "Plan.") {
		t.Errorf("instructions = %q", got.Instructions)
	}
	for _, name := range tools["notes.write"] {
		if slices.Contains(got.AllowedTools, name) {
			t.Errorf("tools = %v", got.AllowedTools)
		}
	}
	if !slices.Contains(got.AllowedTools, "Read") || len(r.AllowedTools) == len(got.AllowedTools) {
		t.Errorf("tools = %v", got.AllowedTools)
	}
}

func TestForTask(t *testing.T) {
	r := Role{Name: "knowledge-curator", WritePaths: []string{"knowledge/**"}, AllowedTools: []string{"Read", "Write", "mcp__forecrew__kb_propose"}, DefinitionOfDone: []string{"committed"}, EscalateWhen: []string{"stuck"}, Instructions: "seed"}
	got := r.ForTask(Task{Role: "knowledge-curator", Name: "retro", Instructions: "retro", OutputSchema: json.RawMessage(`{"type":"object"}`)})
	if got.Instructions != "retro" || string(got.OutputSchema) != `{"type":"object"}` || !got.ReadOnly() || slices.Contains(got.AllowedTools, "Write") || !slices.Contains(got.AllowedTools, "mcp__forecrew__kb_propose") ||
		len(got.DefinitionOfDone) != 0 || len(got.EscalateWhen) != 0 {
		t.Errorf("ForTask = %+v", got)
	}
	if r.Instructions != "seed" || r.ReadOnly() || len(r.DefinitionOfDone) != 1 {
		t.Errorf("ForTask changed the role: %+v", r)
	}
}

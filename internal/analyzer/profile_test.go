package analyzer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestProfileSchemaMatchesContract(t *testing.T) {
	contract, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "profile.schema.json"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	if !bytes.Equal(contract, []byte(profileSchema)) {
		t.Error("internal/analyzer/profile.schema.json differs from docs/contracts/profile.schema.json; copy the contract over it")
	}
}

func TestDraftProfileGolden(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			facts, err := Local{}.Analyze(context.Background(), filepath.Join("..", "..", "testdata", name))
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			data, err := marshalProfile(DraftProfile(facts))
			if err != nil {
				t.Fatalf("marshal profile: %v", err)
			}
			if err := Validate(data); err != nil {
				t.Fatalf("draft does not validate: %v\n%s", err, data)
			}
			checkGolden(t, filepath.Join("..", "..", "testdata", "golden", name+".profile.yaml"), data)
		})
	}
}

func TestDraftProfileContent(t *testing.T) {
	facts := Facts{
		Name:       "shop",
		Languages:  []Language{{Name: "Go"}, {Name: "TypeScript"}},
		CIFiles:    []string{".github/workflows/ci.yml"},
		CIRunners:  []string{"macos-14", "self-hosted", "ubuntu-latest", "windows-2025", "ubuntu-24.04"},
		AgentFiles: []string{"AGENTS.md", "CLAUDE.md"},
		Services:   []Service{{File: "compose.yml", Name: "db", Image: "postgres:18"}},
		Components: []Component{
			{Path: "api", Stack: "go", Manifests: []string{"api/Dockerfile", "api/go.mod"}, Build: "go build ./...", Test: "go test ./..."},
			{Path: "web", Stack: "node", Manifests: []string{"web/package.json"}},
		},
	}
	p := DraftProfile(facts)
	if p.Version != 1 || p.Name != "shop" {
		t.Errorf("version, name = %d, %q", p.Version, p.Name)
	}
	if p.Summary != "shop has 2 component(s). Languages: Go, TypeScript." {
		t.Errorf("summary = %q", p.Summary)
	}
	if want := []string{"linux", "macos", "windows"}; !slices.Equal(p.Platforms, want) {
		t.Errorf("platforms = %v, want %v", p.Platforms, want)
	}
	if got := p.Components[0]; !slices.Equal(got.Needs, []string{"docker", "go"}) || !slices.Equal(got.Platforms, p.Platforms) {
		t.Errorf("api component = %+v", got)
	}
	if got := p.Components[1].Needs; !slices.Equal(got, []string{"node"}) {
		t.Errorf("web needs = %v, want [node]", got)
	}
	if p.AgentInstructions != "Follow the instructions in AGENTS.md, CLAUDE.md." {
		t.Errorf("agent instructions = %q", p.AgentInstructions)
	}
	if p.CI.Runners[1] != "self-hosted" || len(p.Services) != 1 || p.Services[0].Image != "postgres:18" {
		t.Errorf("ci or services = %+v, %+v", p.CI, p.Services)
	}
	if want := []string{"What are the build and test commands of web?", "Which CI checks must pass before a merge?"}; !slices.Equal(p.OpenQuestions, want) {
		t.Errorf("open questions = %v, want %v", p.OpenQuestions, want)
	}
}

func TestDraftProfileWithoutCI(t *testing.T) {
	p := DraftProfile(Facts{Name: "lib"})
	if p.Summary != "lib has 0 component(s). Languages: none detected." {
		t.Errorf("summary = %q", p.Summary)
	}
	if p.CI.Workflows != nil || p.AgentInstructions != "" || p.Platforms != nil {
		t.Errorf("empty facts gave ci %+v, instructions %q, platforms %v", p.CI, p.AgentInstructions, p.Platforms)
	}
	want := []string{
		"Which commands build and test this project? No build file was found.",
		"Which checks must pass before a merge? The repo has no CI workflow.",
		"Which operating systems do jobs need?",
		"Do agents need repo-specific instructions?",
	}
	if !slices.Equal(p.OpenQuestions, want) {
		t.Errorf("open questions = %v, want %v", p.OpenQuestions, want)
	}
	if err := Validate(mustMarshal(t, p)); err != nil {
		t.Errorf("empty draft does not validate: %v", err)
	}
}

func TestOSLabels(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"self-hosted"}, nil},
		{[]string{"Ubuntu-Latest", "ubuntu-24.04", "macos-14", "windows-2025", "linux-arm"}, []string{"linux", "macos", "windows"}},
	}
	for _, tt := range tests {
		if got := osLabels(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("osLabels(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	const minimal = "version: 1\nname: shop\nsummary: A shop.\n"
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"minimal", minimal, false},
		{"full", minimal + `
languages: [Go]
platforms: [linux, macos]
components:
  - path: api
    stack: go
    manifests: [api/go.mod]
    build: go build ./...
    test: go test ./...
    platforms: [linux]
    needs: [go, linux]
services:
  - name: db
    image: postgres:18
    file: docker-compose.yml
ci:
  workflows: [.github/workflows/ci.yml]
  runners: [ubuntu-latest]
  gating_checks: [test]
agent_instructions: Run tests.
suggested_roles:
  - role: developer
    write_paths: [api/]
    tools: [go]
    tier: strong
open_questions: ["Which checks gate merges?"]
`, false},
		{"empty document", "", true},
		{"not yaml", "version: [", true},
		{"root is a list", "- version: 1\n", true},
		{"wrong version", "version: 2\nname: shop\nsummary: x\n", true},
		{"missing name", "version: 1\nsummary: x\n", true},
		{"empty name", "version: 1\nname: \"\"\nsummary: x\n", true},
		{"missing summary", "version: 1\nname: shop\n", true},
		{"unknown top-level key", minimal + "owner: me\n", true},
		{"component missing stack", minimal + "components:\n  - path: api\n", true},
		{"component unknown key", minimal + "components:\n  - path: api\n    stack: go\n    cmd: x\n", true},
		{"platform not an os", minimal + "platforms: [ubuntu-latest]\n", true},
		{"need not a label", minimal + "components:\n  - path: api\n    stack: go\n    needs: [cobol]\n", true},
		{"duplicate platforms", minimal + "platforms: [linux, linux]\n", true},
		{"bad tier", minimal + "suggested_roles:\n  - role: dev\n    tier: cheap\n", true},
		{"role without name", minimal + "suggested_roles:\n  - tools: [go]\n", true},
		{"service without name", minimal + "services:\n  - image: redis\n", true},
		{"ci unknown key", minimal + "ci:\n  jobs: 3\n", true},
		{"languages not strings", minimal + "languages: [1, 2]\n", true},
		{"open question empty", minimal + "open_questions: [\"\"]\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate([]byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("error %v does not wrap ErrInvalidProfile", err)
			}
		})
	}
}

func TestCompileProfileSchemaBrokenDocument(t *testing.T) {
	saved := profileSchema
	defer func() { profileSchema = saved }()
	profileSchema = "{"
	if err := Validate([]byte(minimalProfile)); err == nil || errors.Is(err, ErrInvalidProfile) {
		t.Errorf("Validate with a broken schema = %v, want a schema error", err)
	}
}

const minimalProfile = "version: 1\nname: shop\nsummary: x\n"

// marshalProfile renders a profile as YAML.
func marshalProfile(p Profile) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mustMarshal(t *testing.T, p Profile) []byte {
	t.Helper()
	data, err := marshalProfile(p)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return data
}

package analyzer

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
)

func TestComposeServices(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"docker-compose.yml":  "services:\n  web:\n    build: .\n  db:\n    image: postgres:18\n",
		"deploy/compose.yaml": "services:\n  queue:\n    image: nats:2\n",
		"broken/compose.yml":  "services: [\n",
		"notes.yml":           "services:\n  ignored:\n    image: x\n",
	})
	files := []string{"broken/compose.yml", "deploy/compose.yaml", "docker-compose.yml", "notes.yml"}
	got, err := composeServices(root, files)
	if err != nil {
		t.Fatalf("composeServices: %v", err)
	}
	want := []Service{
		{File: "deploy/compose.yaml", Name: "queue", Image: "nats:2"},
		{File: "docker-compose.yml", Name: "db", Image: "postgres:18"},
		{File: "docker-compose.yml", Name: "web", Image: ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("composeServices = %+v, want %+v", got, want)
	}
}

func TestComposeServicesMissingFile(t *testing.T) {
	_, err := composeServices(t.TempDir(), []string{"docker-compose.yml"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestIsAgentFile(t *testing.T) {
	tests := map[string]bool{
		"CLAUDE.md":                       true,
		"services/api/AGENTS.md":          true,
		".cursor/rules":                   true,
		".cursor/rules/style.mdc":         true,
		".github/copilot-instructions.md": true,
		".cursor/other.mdc":               false,
		"docs/AGENTS.txt":                 false,
	}
	for rel, want := range tests {
		if got := isAgentFile(rel); got != want {
			t.Errorf("isAgentFile(%q) = %v, want %v", rel, got, want)
		}
	}
}

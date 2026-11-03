package roles

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
)

func TestLoadPacks(t *testing.T) {
	defs, err := Load(forecrew.Files)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]struct {
		tier     config.Tier
		readOnly bool
	}{
		"onboarding-analyst":   {config.TierStrong, false},
		"product-owner":        {config.TierBest, true},
		"architect":            {config.TierBest, true},
		"developer":            {config.TierStrong, false},
		"reviewer":             {config.TierStrong, true},
		"test-architect":       {config.TierBest, false},
		"environment-engineer": {config.TierStrong, false},
	}
	if len(defs) != len(want) {
		t.Fatalf("got %d roles, want %d", len(defs), len(want))
	}
	for name, w := range want {
		d, ok := defs[name]
		if !ok {
			t.Errorf("role %s missing", name)
			continue
		}
		if d.Tier != w.tier || (len(d.WritePaths) == 0) != w.readOnly {
			t.Errorf("%s: tier %s read-only %v, want %s %v", name, d.Tier, len(d.WritePaths) == 0, w.tier, w.readOnly)
		}
		for _, phrase := range []string{"assumption", "one fenced `json` block"} {
			if !strings.Contains(d.Instructions, phrase) {
				t.Errorf("%s: prompt does not mention %q", name, phrase)
			}
		}
		if d.Timeout <= 0 || len(d.OutputSchema) == 0 || len(d.DefinitionOfDone) == 0 {
			t.Errorf("%s: incomplete definition %+v", name, d)
		}
	}
	if got := defs["onboarding-analyst"].WritePaths; !slices.Equal(got, []string{".forecrew/**"}) {
		t.Errorf("onboarding-analyst write_paths = %v", got)
	}
	if got := defs["developer"].WritePaths; !slices.Contains(got, "!.github/workflows/**") {
		t.Errorf("developer may write .github/workflows: %v", got)
	}
}

func TestAllowedTools(t *testing.T) {
	d := Definition{Tools: []string{"repo.write", "repo.read", "repo.read"}}
	want := []string{"Edit", "Glob", "Grep", "Read", "Write"}
	if got := d.AllowedTools(); !slices.Equal(got, want) {
		t.Errorf("AllowedTools = %v, want %v", got, want)
	}
}

const validRole = `role: dev
model: strong
prompt: roles/dev.md
tools: [repo.read, repo.write]
write_paths: ["**"]
output_schema: schemas/out.json
definition_of_done: [tests pass]
escalate_when: [stuck]
max_turns: 10
timeout: 5m
`

func packFS(role string) fstest.MapFS {
	return fstest.MapFS{
		"packs/roles/dev.yaml":   {Data: []byte(role)},
		"packs/roles/dev.md":     {Data: []byte("You are a developer.\n")},
		"packs/schemas/out.json": {Data: []byte(`{"type":"object"}`)},
	}
}

func TestLoadValid(t *testing.T) {
	defs, err := Load(packFS(validRole))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := defs["dev"]
	if d.Instructions != "You are a developer." || d.Timeout != 5*time.Minute || d.MaxTurns != 10 ||
		string(d.OutputSchema) != `{"type":"object"}` || !slices.Equal(d.EscalateWhen, []string{"stuck"}) {
		t.Errorf("unexpected definition %+v", d)
	}
}

func TestLoadPoolAndToolbox(t *testing.T) {
	fsys := packFS(validRole + "pool: control\ntoolbox: security\ncontext: {}\n")
	fsys["packs/toolboxes/security.yaml"] = &fstest.MapFile{Data: []byte("name: security\nimage: ghcr.io/x/security:1\nneeds: [Linux]\n")}
	defs, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := defs["dev"]
	if d.Pool != "control" || d.Toolbox == nil || d.Toolbox.Image != "ghcr.io/x/security:1" || !slices.Equal(d.Toolbox.Needs, []string{"linux"}) {
		t.Fatalf("definition = %+v", d)
	}
	defs, err = Load(packFS(validRole))
	if err != nil || defs["dev"].Pool != "build" || defs["dev"].Toolbox != nil {
		t.Fatalf("defaults = %+v, %v", defs["dev"], err)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name string
		edit func(fstest.MapFS)
		want string
	}{
		{"unknown tool", setRole(strings.Replace(validRole, "repo.read,", "repo.read, repo.delete,", 1)), `unknown tool "repo.delete"`},
		{"no tools", setRole(strings.Replace(validRole, "tools: [repo.read, repo.write]", "tools: []", 1)), "tools must not be empty"},
		{"name mismatch", setRole(strings.Replace(validRole, "role: dev", "role: developer", 1)), `must match the file name "dev"`},
		{"bad name", func(f fstest.MapFS) {
			f["packs/roles/Dev.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(validRole, "role: dev", "role: Dev", 1))}
		}, `role "Dev" must match`},
		{"bad tier", setRole(strings.Replace(validRole, "model: strong", "model: huge", 1)), `model must be fast, strong or best, got "huge"`},
		{"unknown key", setRole(validRole + "colour: red\n"), "field colour not found"},
		{"missing write_paths", setRole(strings.Replace(validRole, "write_paths: [\"**\"]\n", "", 1)), "write_paths must be set"},
		{"read-only with write tool", setRole(strings.Replace(validRole, `write_paths: ["**"]`, "write_paths: []", 1)), `tool "repo.write" needs write_paths`},
		{"write paths without repo.write", setRole(strings.Replace(validRole, "tools: [repo.read, repo.write]", "tools: [repo.read]", 1)), "tools lack repo.write"},
		{"only excludes", setRole(strings.Replace(validRole, `["**"]`, `["!.github/**"]`, 1)), "no include pattern"},
		{"absolute path", setRole(strings.Replace(validRole, `["**"]`, `["/etc/**"]`, 1)), `"/etc/**" must be a glob relative`},
		{"parent path", setRole(strings.Replace(validRole, `["**"]`, `["../**"]`, 1)), `"../**" must be a glob relative`},
		{"no prompt", setRole(strings.Replace(validRole, "prompt: roles/dev.md\n", "", 1)), "prompt must be set"},
		{"missing prompt file", func(f fstest.MapFS) { delete(f, "packs/roles/dev.md") }, "prompt: open packs/roles/dev.md"},
		{"empty prompt", func(f fstest.MapFS) { f["packs/roles/dev.md"] = &fstest.MapFile{Data: []byte(" \n")} }, "roles/dev.md is empty"},
		{"no schema", setRole(strings.Replace(validRole, "output_schema: schemas/out.json\n", "", 1)), "output_schema must be set"},
		{"bad schema", func(f fstest.MapFS) { f["packs/schemas/out.json"] = &fstest.MapFile{Data: []byte(`{"type":1}`)} }, "output_schema: invalid schema"},
		{"no done", setRole(strings.Replace(validRole, "definition_of_done: [tests pass]\n", "", 1)), "definition_of_done must not be empty"},
		{"negative turns", setRole(strings.Replace(validRole, "max_turns: 10", "max_turns: -1", 1)), "max_turns must not be negative"},
		{"no timeout", setRole(strings.Replace(validRole, "timeout: 5m\n", "", 1)), "timeout must be a positive duration"},
		{"bad yaml", setRole("role: [dev"), "packs/roles/dev.yaml: yaml"},
		{"no files", func(f fstest.MapFS) { delete(f, "packs/roles/dev.yaml") }, "no role files"},
		{"bad pool", setRole(validRole + "pool: Control Room\n"), `pool "Control Room" is not a valid pool name`},
		{"unknown toolbox", setRole(validRole + "toolbox: security\n"), `toolbox "security": no packs/toolboxes/security.yaml`},
		{"context set", setRole(validRole + "context: {sections: [adr]}\n"), "context is reserved"},
		{"bad toolbox file", func(f fstest.MapFS) {
			f["packs/toolboxes/security.yaml"] = &fstest.MapFile{Data: []byte("name: security\n")}
		}, "packs/toolboxes/security.yaml: invalid file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := packFS(validRole)
			tt.edit(fsys)
			_, err := Load(fsys)
			if !errors.Is(err, ErrInvalidRole) {
				t.Fatalf("Load error = %v, want ErrInvalidRole", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	role := strings.NewReplacer("model: strong", "model: huge", "repo.read,", "repo.read, shell,").Replace(validRole)
	_, err := Load(packFS(role))
	for _, want := range []string{"packs/roles/dev.yaml", "model must be", `unknown tool "shell"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load error = %v, want it to contain %q", err, want)
		}
	}
}

func setRole(data string) func(fstest.MapFS) {
	return func(f fstest.MapFS) { f["packs/roles/dev.yaml"] = &fstest.MapFile{Data: []byte(data)} }
}

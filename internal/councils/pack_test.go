package councils

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
)

// lookup returns a RoleLookup over the role packs of fsys.
func lookup(t *testing.T, fsys fs.FS) RoleLookup {
	t.Helper()
	defs, err := roles.Load(fsys)
	if err != nil {
		t.Fatalf("roles.Load: %v", err)
	}
	return func(name string) (roles.Definition, error) {
		d, ok := defs[name]
		if !ok {
			return roles.Definition{}, fmt.Errorf("%q: %w", name, roles.ErrUnknownRole)
		}
		return d, nil
	}
}

func shipped(t *testing.T) *Set {
	t.Helper()
	s, err := Load(forecrew.Files, lookup(t, forecrew.Files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestLoadShipped(t *testing.T) {
	s := shipped(t)
	want := []domain.CouncilKind{domain.CouncilIncident, domain.CouncilOnboarding, domain.CouncilProduct, domain.CouncilTechnical, domain.CouncilTestDesign}
	if got := s.Kinds(); !slices.Equal(got, want) {
		t.Fatalf("Kinds = %v, want %v", got, want)
	}
	members := map[domain.CouncilKind][]string{
		domain.CouncilOnboarding: {"architect", "environment-engineer", "test-architect", DevilsAdvocate},
		domain.CouncilProduct:    {"product-owner", "market-researcher", "ux", DevilsAdvocate},
		domain.CouncilTechnical:  {"architect", "developer", "security-reviewer", "environment-engineer", DevilsAdvocate},
		domain.CouncilTestDesign: {"test-architect", "environment-engineer", "developer", DevilsAdvocate},
		domain.CouncilIncident:   {"developer", "environment-engineer", "test-architect", DevilsAdvocate},
	}
	for kind, roleNames := range members {
		p, err := s.Pack(kind)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range p.Members {
			got = append(got, m.Role)
			if len(m.Runtimes) != 2 {
				t.Errorf("%s/%s: runtimes %v, want both adapters in order", kind, m.Role, m.Runtimes)
			}
		}
		if !slices.Equal(got, roleNames) {
			t.Errorf("%s members = %v, want %v", kind, got, roleNames)
		}
		if p.Facilitator != "facilitator" || p.Budget.Tokens <= 0 || p.Budget.Time <= 0 || p.MaxRounds < 1 {
			t.Errorf("%s: %+v", kind, p)
		}
	}
	tech, _ := s.Pack(domain.CouncilTechnical)
	if tech.MaxRounds != 2 || tech.Budget != (Budget{Tokens: 800000, Time: 90 * time.Minute}) {
		t.Errorf("technical = %+v", tech)
	}
	if _, err := s.Pack(domain.CouncilAdhoc); !errors.Is(err, ErrInvalidTurn) {
		t.Errorf("Pack(adhoc) error = %v", err)
	}
}

const validPack = `kind: technical
question: How should we build it?
facilitator: chair
max_rounds: 2
budget: {tokens: 1000, time: 10m}
members:
  - {role: dev, priority: cost, focus: effort, runtimes: [codex]}
  - {role: arch, priority: simplicity, focus: few parts, runtimes: [claude-code, codex]}
  - {role: devils-advocate, priority: dissent, focus: the other side, runtimes: [codex]}
`

// councilFS is a minimal pack tree: roles chair (facilitator), dev, arch
// and devils-advocate, the shipped schemas and prompts, and one council.
func councilFS(t *testing.T, pack string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{"packs/councils/technical.yaml": {Data: []byte(pack)}}
	for _, glob := range []string{"packs/schemas/*.json", "packs/councils/prompts/*.md"} {
		names, err := fs.Glob(forecrew.Files, glob)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s: %v", glob, err)
		}
		for _, name := range names {
			data, err := fs.ReadFile(forecrew.Files, name)
			if err != nil {
				t.Fatal(err)
			}
			fsys[name] = &fstest.MapFile{Data: data}
		}
	}
	for name, schema := range map[string]string{"chair": "council-brief", "dev": "council-proposal", "arch": "task-breakdown", DevilsAdvocate: "council-proposal"} {
		role := fmt.Sprintf("role: %s\nmodel: strong\nprompt: roles/%s.md\ntools: [repo.read]\nwrite_paths: []\n"+
			"output_schema: schemas/%s.json\ndefinition_of_done: [done]\ntimeout: 5m\n", name, name, schema)
		fsys["packs/roles/"+name+".yaml"] = &fstest.MapFile{Data: []byte(role)}
		fsys["packs/roles/"+name+".md"] = &fstest.MapFile{Data: []byte("You are " + name + ".\n")}
	}
	return fsys
}

func loadFS(t *testing.T, fsys fstest.MapFS) (*Set, error) {
	t.Helper()
	return Load(fsys, lookup(t, fsys))
}

func TestLoadValid(t *testing.T) {
	s, err := loadFS(t, councilFS(t, validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := s.Pack(domain.CouncilTechnical)
	want := Seat{Role: "arch", Priority: "simplicity", Focus: "few parts", Runtimes: []string{"claude-code", "codex"}}
	if len(p.Members) != 3 || p.Members[1].Role != want.Role || !slices.Equal(p.Members[1].Runtimes, want.Runtimes) ||
		p.Budget.Time != 10*time.Minute || p.Question != "How should we build it?" {
		t.Errorf("pack = %+v", p)
	}
	for _, step := range Steps() {
		if sch, err := s.Schema(step); err != nil || len(sch) == 0 {
			t.Errorf("Schema(%s) = %s, %v", step, sch, err)
		}
	}
	if _, err := s.Schema("vote"); !errors.Is(err, ErrInvalidTurn) {
		t.Errorf("Schema(vote) error = %v", err)
	}
}

func TestLoadInvalid(t *testing.T) {
	replace := func(old, new string) func(fstest.MapFS) {
		return func(f fstest.MapFS) {
			f["packs/councils/technical.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(validPack, old, new, 1))}
		}
	}
	tests := []struct {
		name string
		edit func(fstest.MapFS)
		want string
	}{
		{"kind mismatch", replace("kind: technical", "kind: product"), `kind "product" must match the file name "technical"`},
		{"adhoc", func(f fstest.MapFS) {
			f["packs/councils/adhoc.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(validPack, "kind: technical", "kind: adhoc", 1))}
		}, `kind "adhoc" is not a standard council kind`},
		{"unknown kind", replace("kind: technical", "kind: tea"), `kind "tea" is not a standard council kind`},
		{"no question", replace("question: How should we build it?\n", ""), "question must be set"},
		{"too many rounds", replace("max_rounds: 2", "max_rounds: 3"), "max_rounds must be from 0 to 2, got 3"},
		{"negative rounds", replace("max_rounds: 2", "max_rounds: -1"), "max_rounds must be from 0 to 2"},
		{"no budget", replace("budget: {tokens: 1000, time: 10m}\n", ""), "budget needs positive tokens and time"},
		{"no facilitator", replace("facilitator: chair\n", ""), "facilitator must be set"},
		{"facilitator writes no brief", replace("facilitator: chair", "facilitator: arch"), `facilitator "arch" must be a role whose output_schema is schemas/council-brief.json`},
		{"facilitator as member", replace("role: dev,", "role: chair,"), "the facilitator cannot be a member"},
		{"unknown role", replace("role: dev,", "role: ghost,"), `role: "ghost": unknown role`},
		{"too few members", replace("  - {role: dev, priority: cost, focus: effort, runtimes: [codex]}\n", ""), "at least 3 members, got 2"},
		{"bad priority", replace("priority: cost", "priority: Low Cost"), `priority "Low Cost" must match`},
		{"shared priority", replace("priority: cost", "priority: simplicity"), `priority "simplicity" is held by two members`},
		{"no focus", replace("focus: effort, ", ""), "focus must be set"},
		{"unknown runtime", replace("runtimes: [codex]}", "runtimes: [gemini]}"), `runtime "gemini" must be one of claude-code, codex, opencode`},
		{"runtime twice", replace("runtimes: [claude-code, codex]", "runtimes: [claude-code, claude-code]"), `runtime "claude-code" is listed twice`},
		{"no devil's advocate", replace("role: devils-advocate,", "role: dev,"), "exactly one devils-advocate, got 0"},
		{"one vendor", replace("runtimes: [claude-code, codex]", "runtimes: [codex, claude-code]"), "at least two runtimes"},
		{"same role same runtime", func(f fstest.MapFS) {
			f["packs/councils/technical.yaml"] = &fstest.MapFile{Data: []byte(validPack +
				"  - {role: dev, priority: speed, focus: fast, runtimes: [codex, claude-code]}\n")}
		}, "role dev sits twice"},
		{"unknown key", replace("max_rounds: 2", "max_rounds: 2\nquorum: 3"), "field quorum not found"},
		{"bad yaml", replace("kind: technical", "kind: [technical"), "packs/councils/technical.yaml: yaml"},
		{"no packs", func(f fstest.MapFS) { delete(f, "packs/councils/technical.yaml") }, "no council packs"},
		{"missing prompt", func(f fstest.MapFS) { delete(f, "packs/councils/prompts/critique.md") }, "no prompt packs/councils/prompts/critique.md"},
		{"bad prompt", func(f fstest.MapFS) {
			f["packs/councils/prompts/check.md"] = &fstest.MapFile{Data: []byte("{{ .Step")}
		}, "prompts: template"},
		{"missing schema", func(f fstest.MapFS) { delete(f, "packs/schemas/council-critique.json") }, "step critique: invalid schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := councilFS(t, validPack)
			tt.edit(fsys)
			_, err := loadFS(t, fsys)
			if !errors.Is(err, ErrInvalidPack) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want ErrInvalidPack containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadSameRoleOnTwoRuntimes(t *testing.T) {
	pack := strings.Replace(validPack, "runtimes: [codex]}", "runtimes: [codex]}\n  - {role: dev, priority: speed, focus: fast, runtimes: [claude-code]}", 1)
	s, err := loadFS(t, councilFS(t, pack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p, _ := s.Pack(domain.CouncilTechnical); len(p.Members) != 4 {
		t.Errorf("members = %+v", p.Members)
	}
	pack = strings.Replace(validPack, "runtimes: [codex]}", "runtimes: [codex]}\n  - {role: dev, priority: speed, focus: fast}", 1)
	if _, err := loadFS(t, councilFS(t, pack)); err == nil || !strings.Contains(err.Error(), "role dev sits twice") {
		t.Errorf("second seat without runtimes: %v", err)
	}
}

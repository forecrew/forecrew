package knowledge_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/knowledge"
)

const targetPolicy = `version: 1
providers:
  wiki: {kind: confluence, instance: acme.atlassian.net}
sections:
  architecture: {paths: ["docs/**"], owner: architect}
  vision: {paths: ["docs/vision/**"], protected: true, owner: architect, access: {propose: [product-owner]}}
  conventions: {paths: [CONTRIBUTING.md], owner: codeowners}
  runbooks: {provider: wiki, paths: ["space:ENG"], owner: developer}
  lessons: {owner: knowledge-curator, access: {propose: [knowledge-curator, developer], read: [developer], write: [developer]}}
  generated: {}
`

func TestTarget(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte(targetPolicy))
	must(t, err)
	tests := []struct {
		name, section, file, role string
		refused                   bool
		owners                    []string
		protected, repo           bool
	}{
		{name: "role-owned", section: "architecture", file: "docs/adr/1.md", role: "developer", owners: []string{"architect"}, repo: true},
		{name: "codeowners", section: "conventions", file: "CONTRIBUTING.md", role: "reviewer", owners: []string{"codeowners"}, repo: true},
		{name: "provider section takes any location", section: "runbooks", file: "space:ENG/page:1", role: "developer", owners: []string{"developer"}},
		{name: "protected is human whatever its owner", section: "vision", file: "docs/vision/a.md", role: "product-owner", owners: []string{"architect", "human"}, protected: true, repo: true},
		{name: "a human proposes anywhere", section: "vision", file: "docs/vision/a.md", owners: []string{"architect", "human"}, protected: true, repo: true},
		{name: "not allowed", section: "lessons", file: "knowledge/lessons/a.md", role: "reviewer", refused: true},
		{name: "a path of a protected section under another name", section: "architecture", file: "docs/vision/a.md", role: "developer", refused: true},
		{name: "outside the section", section: "architecture", file: "src/main.go", role: "developer", refused: true},
		{name: "not a text entry", section: "architecture", file: "docs/run.sh", role: "developer", refused: true},
		{name: "the policy file", section: "architecture", file: knowledge.PolicyFile, role: "developer", refused: true},
		{name: "reserved", section: "generated", file: "x", role: "developer", refused: true},
		{name: "notes", section: "notes", file: "x", role: "developer", refused: true},
		{name: "unknown section", section: "market", file: "x", role: "developer", refused: true},
		{name: "traversal", section: "architecture", file: "docs/../.github/a.md", role: "developer", refused: true},
		{name: "unclean", section: "architecture", file: "docs//a.md", role: "developer", refused: true},
		{name: "backtick", section: "architecture", file: "docs/`a.md", role: "developer", refused: true},
		{name: "newline", section: "runbooks", file: "a\nb", role: "developer", refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.Target(tt.section, tt.file, tt.role)
			if tt.refused {
				if !errors.Is(err, knowledge.ErrRefused) {
					t.Fatalf("err = %v, got %+v", err, got)
				}
				return
			}
			must(t, err)
			if !slices.Equal(got.Owners, tt.owners) {
				t.Errorf("owners = %v, want %v", got.Owners, tt.owners)
			}
			if got.Protected != tt.protected || got.Repo != tt.repo || got.Sections[0] != tt.section {
				t.Errorf("target = %+v", got)
			}
		})
	}
}

// A policy cannot take the protection off itself: a section that claims the
// file, unprotected and open to writes, changes nothing.
func TestThePolicyFileStaysProtected(t *testing.T) {
	for _, paths := range []string{`[".forecrew/knowledge-policy.yaml"]`, `[".forecrew/**"]`, `["**"]`} {
		p, err := knowledge.ParsePolicy([]byte("version: 1\nsections:\n  policy: {paths: " + paths +
			", protected: false, owner: developer, access: {propose: ['*'], write: [developer]}}\n"))
		must(t, err)
		if _, err := p.Target("policy", knowledge.PolicyFile, "developer"); !errors.Is(err, knowledge.ErrRefused) {
			t.Errorf("paths %s: err = %v", paths, err)
		}
		if _, err := p.Target("policy", knowledge.PolicyFile, ""); !errors.Is(err, knowledge.ErrRefused) {
			t.Errorf("paths %s, a human: err = %v", paths, err)
		}
		if m := knowledge.RepoMapping(p); func() bool { _, ok := m.SectionOf(knowledge.PolicyFile, 0); return ok }() {
			t.Errorf("paths %s: the policy file is an entry", paths)
		}
	}
}

func TestDefaultPolicySections(t *testing.T) {
	p := knowledge.DefaultPolicy(nil, []string{"knowledge/testing/a.md"})
	for name, owner := range map[string]string{
		"architecture": "architect", "conventions": "reviewer", "testing": "test-architect", "runbooks": "developer",
		"lessons": "knowledge-curator", "vision": knowledge.OwnerHuman, "security": knowledge.OwnerHuman,
	} {
		s, ok := p.Sections[name]
		if !ok || s.Owner != owner {
			t.Errorf("%s = %+v", name, s)
		}
		if !slices.Equal(s.Access.Read, []string{"*"}) || !slices.Equal(s.Access.Propose, []string{"*"}) {
			t.Errorf("%s access = %+v", name, s.Access)
		}
		if s.Protected != (name == "vision" || name == "security") {
			t.Errorf("%s protected = %v", name, s.Protected)
		}
	}
	// A section without a location can be proposed to and is still for the
	// curator to seed; one with files is not.
	if !p.Sections["architecture"].Implicit() || p.Sections["testing"].Implicit() {
		t.Errorf("implicit: %+v", p.Sections)
	}
	missing := knowledge.MissingSections(p)
	if !slices.Contains(missing, "architecture") || slices.Contains(missing, "testing") {
		t.Errorf("missing = %v", missing)
	}
	if _, err := p.Target("lessons", "knowledge/lessons/a.md", "developer"); err != nil {
		t.Errorf("Target: %v", err)
	}
}

func TestHas(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte(targetPolicy))
	must(t, err)
	for _, tt := range []struct {
		section, level, role string
		want                 bool
	}{
		{"lessons", "read", "developer", true},
		{"lessons", "read", "ux", false},
		{"lessons", "propose", "developer", true},
		{"lessons", "propose", "ux", false},
		{"lessons", "write", "developer", true},
		{"architecture", "write", "developer", false},
		{"architecture", "propose", "anyone", true},
	} {
		if got := p.Has(tt.section, tt.level, tt.role); got != tt.want {
			t.Errorf("Has(%s, %s, %s) = %v", tt.section, tt.level, tt.role, got)
		}
	}
}

const broadPolicy = `version: 1
sections:
  everything: {paths: ["**"], owner: developer}
  vision: {owner: human, protected: true, access: {propose: [product-owner]}}
`

// A section of ** does not reach what forecrewd keeps, in any case.
func TestBroadSectionStopsAtForecrewdsPaths(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte(broadPolicy))
	must(t, err)
	for _, file := range []string{"generated/components/api.md", "generated/x.md", "Generated/x.md", ".FORECREW/knowledge-policy.yaml", ".Git/x.md"} {
		if _, err := p.Target("everything", file, "developer"); !errors.Is(err, knowledge.ErrRefused) {
			t.Errorf("%s: err = %v", file, err)
		}
		if _, err := p.Target("everything", file, ""); !errors.Is(err, knowledge.ErrRefused) {
			t.Errorf("%s, a human: err = %v", file, err)
		}
		if _, ok := knowledge.RepoMapping(p).SectionOf(file, 0); ok {
			t.Errorf("%s is an entry", file)
		}
	}
	if _, err := p.Target("everything", "docs/generated/x.md", "developer"); err != nil {
		t.Errorf("a folder of that name elsewhere: %v", err)
	}
}

// knowledge/Vision and knowledge/vision are one folder on a case-insensitive
// checkout, so the other spelling does not get around the protection.
func TestProtectionIgnoresCase(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte(broadPolicy))
	must(t, err)
	for _, file := range []string{"knowledge/Vision/a.md", "KNOWLEDGE/VISION/a.md", "knowledge/vision/a.md"} {
		if _, err := p.Target("everything", file, "developer"); !errors.Is(err, knowledge.ErrRefused) {
			t.Errorf("%s: a role the protected section does not allow: %v", file, err)
		}
		got, err := p.Target("everything", file, "")
		must(t, err)
		if !got.Protected || !slices.Contains(got.Owners, knowledge.OwnerHuman) || !slices.Contains(got.Sections, "vision") {
			t.Errorf("%s: target = %+v", file, got)
		}
	}
	// Only protected sections fold: others keep git's exact paths.
	open, err := knowledge.ParsePolicy([]byte("version: 1\nsections:\n  everything: {paths: ['**']}\n  lessons: {access: {propose: [architect]}}\n"))
	must(t, err)
	if got, err := open.Target("everything", "knowledge/Lessons/a.md", "developer"); err != nil || len(got.Sections) != 1 {
		t.Errorf("target = %+v, %v", got, err)
	}
}

const rulesPolicy = `version: 1
sections:
  runbooks: {paths: ["ops/**", "!ops/private/**"], owner: developer, access: {write: [developer]}}
  vision: {paths: ["knowledge/vision/**"], protected: true, owner: human}
  architecture: {paths: ["docs/adr/**"], owner: architect}
  lessons: {owner: knowledge-curator, access: {write: [knowledge-curator]}}
  notes: {}
`

// PathRules: every role loses the sections' files except the ones it holds
// access.write on; the curator loses the protected sections only.
func TestPathRules(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte(rulesPolicy))
	must(t, err)
	tests := []struct {
		role, name string
		curator    bool
		granted    []string
		denied     []string
	}{
		{"architect", "a role without grants", false, nil, []string{".forecrew/knowledge-policy.yaml", "docs/adr/**", "knowledge/**", "knowledge/lessons/**", "knowledge/vision/**", "ops/**"}},
		{"developer", "a grant outside knowledge/", false, []string{"ops/**"}, []string{".forecrew/knowledge-policy.yaml", "docs/adr/**", "knowledge/**", "knowledge/lessons/**", "knowledge/vision/**"}},
		{"knowledge-curator", "a grant under knowledge/ lifts the catch-all", false, []string{"knowledge/lessons/**"}, []string{".forecrew/knowledge-policy.yaml", "docs/adr/**", "knowledge/vision/**", "ops/**"}},
		{"knowledge-curator", "the curator", true, []string{"knowledge/lessons/**"}, []string{".forecrew/knowledge-policy.yaml", "knowledge/vision/**"}},
		{"reviewer", "the curator is a flag, not a name", true, nil, []string{".forecrew/knowledge-policy.yaml", "knowledge/vision/**"}},
	}
	for _, tt := range tests {
		got := p.PathRules(tt.role, tt.curator)
		if !slices.Equal(got.Granted, tt.granted) || !slices.Equal(got.Denied, tt.denied) {
			t.Errorf("%s: PathRules = %+v, want granted %v denied %v", tt.name, got, tt.granted, tt.denied)
		}
	}
}

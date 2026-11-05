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
			if !slices.Equal(got.Owners, tt.owners) && !(tt.protected && slices.Contains(got.Owners, knowledge.OwnerHuman)) {
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

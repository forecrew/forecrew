package knowledge

import (
	"path"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
)

// MaxEntryBytes is the largest repo file that is indexed.
const MaxEntryBytes = 256 << 10

// Default sections kept human-owned and protected when a repo has no policy
// file (knowledge.md).
var defaultProtected = []string{"vision", "security"}

// textExts are the extensions indexed when a section's glob has a wildcard;
// a glob that names one file indexes it whatever it is called (CODEOWNERS).
var textExts = []string{".md", ".mdx", ".markdown", ".txt"}

// DefaultPolicy is the policy of a repo without .forecrew/knowledge-policy.yaml:
// the repo provider; the sections the repo already keeps (ADR folders are
// architecture, the contributing guide conventions, CODEOWNERS owners, as
// the analyzer finds them); vision and security, protected and owned by a
// human; and a section for every other folder under knowledge/. files are
// the paths of the repo.
func DefaultPolicy(sources []analyzer.KnowledgeSource, files []string) Policy {
	p := Policy{Version: 1, Providers: map[string]Provider{}, Sections: map[string]Section{}}
	add := func(name string, s Section, glob string) {
		cur := p.Sections[name]
		cur.Paths = append(cur.Paths, glob)
		cur.Protected = cur.Protected || s.Protected
		p.Sections[name] = cur
	}
	for _, src := range sources {
		glob := src.Path
		if src.Kind == "adr" {
			glob += "/**"
		}
		add(src.Section, Section{}, glob)
	}
	for _, name := range defaultProtected {
		add(name, Section{Protected: true}, "knowledge/"+name+"/**")
	}
	for _, f := range files {
		rest, ok := strings.CutPrefix(f, "knowledge/")
		if !ok {
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested && domain.ValidName(name) && !Reserved(name) {
			if _, have := p.Sections[name]; !have {
				add(name, Section{}, "knowledge/"+name+"/**")
			}
		}
	}
	p.applyDefaults()
	return p
}

// Mapping tells which repo files are entries of which section.
type Mapping struct {
	sections []mapped
}

type mapped struct {
	name  string
	globs []string
}

// RepoMapping returns the mapping of the sections of p that the repo
// provider serves (provider kind repo, not reserved).
func RepoMapping(p Policy) Mapping {
	var m Mapping
	for name, s := range p.Sections {
		if Reserved(name) || p.Providers[s.Provider].Kind != domain.ProviderKindRepo {
			continue
		}
		m.sections = append(m.sections, mapped{name, s.Paths})
	}
	slices.SortFunc(m.sections, func(a, b mapped) int { return strings.Compare(a.name, b.name) })
	return m
}

// Globs returns every glob of the mapping, sorted.
func (m Mapping) Globs() []string {
	var out []string
	for _, s := range m.sections {
		out = append(out, s.globs...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SectionOf returns the section file belongs to: the first section, by
// name, one of whose globs matches and no ! glob excludes. size is the file
// size, 0 when unknown.
func (m Mapping) SectionOf(file string, size int) (string, bool) {
	for _, s := range m.sections {
		if Indexable(s.globs, file, size) {
			return s.name, true
		}
	}
	return "", false
}

// Indexable reports whether file is an entry under the globs: a glob
// matches it, no ! glob does, it is small enough, and it is a text file
// unless a glob names it exactly. The policy file and git internals are
// never entries.
func Indexable(globs []string, file string, size int) bool {
	if size > MaxEntryBytes || file == PolicyFile || file == ".git" || strings.HasPrefix(file, ".git/") {
		return false
	}
	exact, wild := false, false
	for _, g := range globs {
		if neg, ok := strings.CutPrefix(g, "!"); ok {
			if matched, _ := doublestar.Match(neg, file); matched {
				return false
			}
			continue
		}
		if matched, _ := doublestar.Match(g, file); matched {
			if strings.ContainsAny(g, "*?[{") {
				wild = true
			} else {
				exact = true
			}
		}
	}
	return exact || (wild && slices.Contains(textExts, strings.ToLower(path.Ext(file))))
}

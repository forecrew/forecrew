package knowledge

import (
	"maps"
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

// defaultOwners are the roles that own a section when a repo has no policy
// file. Each of these sections exists there, so agents can propose to it,
// even when the repo has no location for it yet.
var defaultOwners = map[string]string{
	"architecture": "architect",
	"conventions":  "reviewer",
	"testing":      "test-architect",
	"runbooks":     "developer",
	"lessons":      "knowledge-curator",
}

// textExts are the extensions indexed when a section's glob has a wildcard;
// a glob that names one file indexes it whatever it is called (CODEOWNERS).
var textExts = []string{".md", ".mdx", ".markdown", ".txt"}

// DefaultPolicy is the policy of a repo without .forecrew/knowledge-policy.yaml:
// the repo provider; the sections the repo already keeps (ADR folders are
// architecture, the contributing guide conventions, CODEOWNERS owners, as
// the analyzer finds them); vision and security, protected and owned by a
// human; the sections of defaultOwners with their owners; and a section for
// every other folder under knowledge/. files are the paths of the repo.
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
	for name, owner := range defaultOwners {
		s, have := p.Sections[name]
		if !have {
			s = Section{Paths: []string{"knowledge/" + name + "/**"}, implicit: true}
		}
		s.Owner = owner
		p.Sections[name] = s
	}
	p.applyDefaults()
	return p
}

// foldedMatch reports whether file is an entry under the globs when case
// does not count, as on a case-insensitive checkout.
func foldedMatch(globs []string, file string) bool {
	folded := make([]string, len(globs))
	for i, g := range globs {
		folded[i] = strings.ToLower(g)
	}
	return Indexable(folded, strings.ToLower(file), 0)
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

// keptOut reports whether file is one no section can hold. Case does not
// count: the same names collide on a case-insensitive checkout.
func keptOut(file string) bool {
	lower := strings.ToLower(file)
	return lower == PolicyFile || lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.HasPrefix(lower, GeneratedPrefix)
}

// Indexable reports whether file is an entry under the globs: a glob
// matches it, no ! glob does, it is small enough, and it is a text file
// unless a glob names it exactly. The policy file, git internals and paths
// under GeneratedPrefix, which forecrewd keeps, are never entries.
func Indexable(globs []string, file string, size int) bool {
	if size > MaxEntryBytes || keptOut(file) {
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

// PathRules are the repo globs a writing role's job gets besides its own
// write paths: Granted are added as includes, Denied as "!" excludes (which
// ignore case, roles.PathAllowed).
type PathRules struct {
	Granted []string
	Denied  []string
	// Open are the Denied globs of sections that are not protected and that
	// the role has no write grant on. A job that was given exactly such a glob
	// as an extra write path (DeniedFor) may write it; a protected section's
	// globs are never open.
	Open []string
}

// DeniedFor returns Denied without the Open globs that extra, the write paths
// a playbook gave one job, names exactly: the playbook chose to let this job
// write what proposals normally carry (the architect's ADR).
func (r PathRules) DeniedFor(extra []string) []string {
	return slices.DeleteFunc(slices.Clone(r.Denied), func(g string) bool {
		return slices.Contains(r.Open, g) && slices.Contains(extra, g)
	})
}

// PathRules says what role may do to curated knowledge in the repo.
// Curated knowledge changes only through proposals, so every role is denied
// the policy file, knowledge/** and the globs of every repo section, except
// the sections the role holds access.write on, which it may change. The
// curator, who writes the fallback knowledge/ folder, is denied the policy
// file and the protected sections only. The globs a granted section leaves out
// with "!" stay excluded. A grant under knowledge/ takes the
// knowledge/** exclusion away, since an exclusion beats an include; the
// files there that no section maps are then writable.
func (p Policy) PathRules(role string, curator bool) PathRules {
	r := PathRules{Denied: []string{PolicyFile}}
	if !curator {
		r.Denied = append(r.Denied, "knowledge/**")
	}
	for _, name := range slices.Sorted(maps.Keys(p.Sections)) {
		s := p.Sections[name]
		if Reserved(name) || p.Providers[s.Provider].Kind != domain.ProviderKindRepo {
			continue
		}
		granted := !s.Protected && p.Has(name, "write", role)
		for _, g := range s.Paths {
			switch {
			case strings.HasPrefix(g, "!"):
				// What a section leaves out is not the grant's to write.
				if granted {
					r.Denied = append(r.Denied, strings.TrimPrefix(g, "!"))
				}
			case s.Protected:
				r.Denied = append(r.Denied, g)
			case granted:
				r.Granted = append(r.Granted, g)
			case !curator:
				r.Denied = append(r.Denied, g)
				r.Open = append(r.Open, g)
			}
		}
	}
	if slices.ContainsFunc(r.Granted, func(g string) bool { return strings.HasPrefix(strings.ToLower(g), "knowledge/") }) {
		r.Denied = slices.DeleteFunc(r.Denied, func(d string) bool { return d == "knowledge/**" })
	}
	slices.Sort(r.Granted)
	slices.Sort(r.Denied)
	slices.Sort(r.Open)
	r.Granted, r.Denied, r.Open = slices.Compact(r.Granted), slices.Compact(r.Denied), slices.Compact(r.Open)
	return r
}

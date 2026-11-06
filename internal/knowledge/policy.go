// Package knowledge holds the knowledge base contracts
// (docs/contracts/knowledge.md): the knowledge policy of a project, the
// frontmatter of repo entries and the provider interface. The index, the
// providers and the proposal flow (T36, T37) build on it.
package knowledge

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/schema"
)

// PolicyFile is where a project keeps its policy.
const PolicyFile = ".forecrew/knowledge-policy.yaml"

// ErrInvalid is wrapped by every error about a policy or an entry.
var ErrInvalid = schema.ErrInvalid

// Owners of a section besides a role.
const (
	OwnerHuman      = "human"      // a human approves proposals (the default)
	OwnerCodeowners = "codeowners" // the CODEOWNERS reviewers of the paths approve
)

// RepoProvider is the name of the project repo's provider, there whether
// the policy declares it or not.
const RepoProvider = "repo"

// Everyone in an access list means every role.
const Everyone = "*"

// Policy is a .forecrew/knowledge-policy.yaml file
// (docs/contracts/knowledge-policy.schema.json), with its defaults applied
// by ParsePolicy.
type Policy struct {
	Version   int                 `json:"version"`
	Providers map[string]Provider `json:"providers"`
	Sections  map[string]Section  `json:"sections"`
}

// Provider is a declared knowledge provider. It never holds secrets:
// Credentials names a credential set configured on forecrewd.
type Provider struct {
	Kind        string         `json:"kind"`
	Instance    string         `json:"instance,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	Credentials string         `json:"credentials,omitempty"`
}

// Section maps a knowledge section to a provider and says who may do what.
type Section struct {
	Provider  string   `json:"provider"`
	Paths     []string `json:"paths"`
	Owner     string   `json:"owner"` // role, OwnerHuman or OwnerCodeowners
	Protected bool     `json:"protected"`
	Access    Access   `json:"access"`

	implicit bool // from defaultOwners: the repo gives it no location
}

// Implicit reports whether the section only exists because the default
// policy names it: the repo has no location for it yet.
func (s Section) Implicit() bool { return s.implicit }

// Access lists the roles (or Everyone) that may read a section, propose
// changes to it, and write it without a proposal.
type Access struct {
	Read    []string `json:"read"`
	Propose []string `json:"propose"`
	Write   []string `json:"write"`
}

// ParsePolicy validates a knowledge policy against its schema and the rules
// a schema cannot express, and returns it with the defaults applied: the
// repo provider, provider repo, repo paths knowledge/<section>/**, owner
// human, read and propose for everyone. Every problem wraps ErrInvalid.
func ParsePolicy(data []byte) (Policy, error) {
	raw, inst, err := schema.DecodeYAML(data)
	if err != nil {
		return Policy{}, fmt.Errorf("%s: %w", PolicyFile, err)
	}
	if err := schema.CheckContract("knowledge-policy.schema.json", inst); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", PolicyFile, err)
	}
	var p Policy
	if err := schema.Strict(raw, &p); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", PolicyFile, err)
	}
	p.applyDefaults()
	if problems := p.check(); len(problems) > 0 {
		return Policy{}, fmt.Errorf("%s: %w: %s", PolicyFile, ErrInvalid, strings.Join(problems, "; "))
	}
	return p, nil
}

func (p *Policy) applyDefaults() {
	if p.Providers == nil {
		p.Providers = map[string]Provider{}
	}
	if _, ok := p.Providers[RepoProvider]; !ok {
		p.Providers[RepoProvider] = Provider{Kind: domain.ProviderKindRepo}
	}
	if p.Sections == nil {
		p.Sections = map[string]Section{}
	}
	for name, s := range p.Sections {
		if Reserved(name) {
			s.Access = Access{Read: orEveryone(s.Access.Read)}
			p.Sections[name] = s
			continue
		}
		s.Provider = cmp.Or(s.Provider, RepoProvider)
		if len(s.Paths) == 0 && p.Providers[s.Provider].Kind == domain.ProviderKindRepo {
			s.Paths = []string{"knowledge/" + name + "/**"}
		}
		s.Owner = cmp.Or(s.Owner, OwnerHuman)
		s.Access.Read = orEveryone(s.Access.Read)
		s.Access.Propose = orEveryone(s.Access.Propose)
		if s.Access.Write == nil {
			s.Access.Write = []string{}
		}
		p.Sections[name] = s
	}
}

// check returns the problems the schema cannot catch, sorted.
func (p Policy) check() []string {
	var out []string
	for name, prov := range p.Providers {
		out = append(out, checkProvider(name, prov)...)
	}
	for name, s := range p.Sections {
		if !Reserved(name) {
			out = append(out, p.checkSection(name, s)...)
		}
	}
	slices.Sort(out)
	return out
}

func checkProvider(name string, prov Provider) []string {
	var out []string
	if name == domain.ProviderInternal {
		out = append(out, "providers.internal: the name is reserved for agent notes")
	}
	for k := range prov.Config {
		if domain.SecretLikeName(k) {
			out = append(out, fmt.Sprintf("providers.%s.config.%s looks like a secret; name credentials instead", name, k))
		}
	}
	if prov.Kind != domain.ProviderKindRepo && prov.Instance == "" {
		out = append(out, fmt.Sprintf("providers.%s: a %s provider needs its instance", name, prov.Kind))
	}
	return out
}

func (p Policy) checkSection(name string, s Section) []string {
	prov, ok := p.Providers[s.Provider]
	if !ok {
		return []string{fmt.Sprintf("sections.%s: provider %q is not declared", name, s.Provider)}
	}
	var out []string
	if len(s.Paths) == 0 {
		out = append(out, fmt.Sprintf("sections.%s: a %s provider needs paths", name, prov.Kind))
	}
	for _, g := range s.Paths {
		if prov.Kind == domain.ProviderKindRepo && !domain.RelativePath(strings.TrimPrefix(g, "!")) {
			out = append(out, fmt.Sprintf("sections.%s.paths: %q must be relative to the repo root", name, g))
		}
	}
	if s.Protected && len(s.Access.Write) > 0 {
		out = append(out, fmt.Sprintf("sections.%s: a protected section cannot be written without a proposal", name))
	}
	return out
}

// ErrRefused means the policy does not allow a proposal or an access level.
var ErrRefused = errors.New("refused by the knowledge policy")

// Target is what the policy says about a change to one entry.
type Target struct {
	Provider string
	// Repo is true when the entry lives in the project repo.
	Repo bool
	// Sections are the sections the entry belongs to, the named one first:
	// globs of several sections can match one file.
	Sections []string
	// Protected is true when any of them is.
	Protected bool
	// Owners are the distinct approvers, sorted: OwnerHuman for a protected
	// section whatever its owner says.
	Owners []string
}

// Target checks a proposal to file of section by role ("" is a human, who
// may propose anywhere) and returns who approves it. It wraps ErrRefused for
// a reserved or unknown section, the policy file, whatever it says about
// itself, a path under GeneratedPrefix, a repo path outside the section, and
// a section of the path that the role may not propose to. A protected section
// also claims a path that differs from its globs only in case: the two are
// one file on a case-insensitive checkout.
func (p Policy) Target(section, file, role string) (Target, error) {
	s, ok := p.Sections[section]
	if Reserved(section) || !ok {
		return Target{}, fmt.Errorf("section %q takes no proposals: %w", section, ErrRefused)
	}
	if strings.ContainsFunc(file, func(r rune) bool { return unicode.IsControl(r) || r == '`' }) {
		return Target{}, fmt.Errorf("path %q has control characters or backticks: %w", file, ErrRefused)
	}
	t := Target{Provider: s.Provider, Repo: p.Providers[s.Provider].Kind == domain.ProviderKindRepo, Sections: []string{section}}
	if t.Repo {
		if err := checkRepoPath(section, s, file); err != nil {
			return Target{}, err
		}
		t.Sections = append(t.Sections, p.alsoHolding(section, file)...)
	}
	for _, name := range t.Sections {
		o := p.Sections[name]
		if role != "" && !allows(o.Access.Propose, role) {
			return Target{}, fmt.Errorf("role %s may not propose to section %s: %w", role, name, ErrRefused)
		}
		owner := o.Owner
		if o.Protected {
			t.Protected, owner = true, OwnerHuman
		}
		t.Owners = append(t.Owners, owner)
	}
	slices.Sort(t.Owners)
	t.Owners = slices.Compact(t.Owners)
	return t, nil
}

// checkRepoPath refuses a repo path no proposal to section s may touch.
func checkRepoPath(section string, s Section, file string) error {
	switch {
	case strings.EqualFold(file, PolicyFile):
		return fmt.Errorf("%s changes only through request_access and a human: %w", PolicyFile, ErrRefused)
	case strings.HasPrefix(strings.ToLower(file), GeneratedPrefix):
		return fmt.Errorf("path %q is under %s, which forecrewd keeps: %w", file, GeneratedPrefix, ErrRefused)
	case !domain.RelativePath(file) || path.Clean(file) != file:
		return fmt.Errorf("path %q must be a clean path relative to the repo root: %w", file, ErrRefused)
	case !Indexable(s.Paths, file, 0):
		return fmt.Errorf("path %q is not an entry of section %s: %w", file, section, ErrRefused)
	}
	return nil
}

// alsoHolding returns the other repo sections file belongs to, sorted.
func (p Policy) alsoHolding(section, file string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(p.Sections)) {
		o := p.Sections[name]
		if name == section || Reserved(name) || p.Providers[o.Provider].Kind != domain.ProviderKindRepo {
			continue
		}
		if Indexable(o.Paths, file, 0) || (o.Protected && foldedMatch(o.Paths, file)) {
			out = append(out, name)
		}
	}
	return out
}

// EntryPath returns the repo path of a new entry called name in section: the
// first folder of the section's globs (dir/** or dir/*) that holds it as an
// entry. False for a section that is unknown, outside the repo or without such
// a folder.
func (p Policy) EntryPath(section, name string) (string, bool) {
	s, ok := p.Sections[section]
	if !ok || Reserved(section) || p.Providers[s.Provider].Kind != domain.ProviderKindRepo {
		return "", false
	}
	for _, g := range s.Paths {
		dir, ok := strings.CutSuffix(g, "/**")
		if !ok {
			dir, ok = strings.CutSuffix(g, "/*")
		}
		if !ok || strings.HasPrefix(dir, "!") || strings.ContainsAny(dir, "*?[{") {
			continue
		}
		if file := dir + "/" + name; Indexable(s.Paths, file, 0) && p.checkEntry(section, file) {
			return file, true
		}
	}
	return "", false
}

// checkEntry reports whether file is a path a proposal to section may name.
func (p Policy) checkEntry(section, file string) bool {
	return checkRepoPath(section, p.Sections[section], file) == nil
}

// Has reports whether role holds level (read, propose or write) on section.
func (p Policy) Has(section, level, role string) bool {
	s := p.Sections[section]
	switch level {
	case "read":
		return CanRead(p, section, role)
	case "propose":
		return allows(s.Access.Propose, role)
	}
	return allows(s.Access.Write, role)
}

func allows(roles []string, role string) bool {
	return slices.Contains(roles, Everyone) || slices.Contains(roles, role)
}

// Reserved reports whether section is kept by forecrewd (generated, notes):
// never proposed to and never owned.
func Reserved(section string) bool {
	return section == domain.SectionGenerated || section == domain.SectionNotes
}

func orEveryone(roles []string) []string {
	if roles == nil {
		return []string{Everyone}
	}
	return roles
}

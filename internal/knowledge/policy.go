// Package knowledge holds the knowledge base contracts
// (docs/contracts/knowledge.md): the knowledge policy of a project, the
// frontmatter of repo entries and the provider interface. The index, the
// providers and the proposal flow (T36, T37) build on it.
package knowledge

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

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
}

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

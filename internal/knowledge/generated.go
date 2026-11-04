package knowledge

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// GeneratedPrefix starts the path of every generated entry.
const GeneratedPrefix = "generated/"

// GeneratedAuthor is the author_role of generated entries.
const GeneratedAuthor = "forecrewd"

// BuildGenerated turns a snapshot into the generated section: one entry per
// component (path, stack, build, test), one with the package list and one
// with the direct dependencies. They are marked generated, read-only and
// untrusted (the text comes from the repo). repo is owner/name, sha the
// commit they describe.
func BuildGenerated(account, project uuid.UUID, repo, sha string, snap Snapshot, now time.Time) []domain.KnowledgeEntry {
	mk := func(path, title, body string) domain.KnowledgeEntry {
		return domain.KnowledgeEntry{
			AccountID: account, ProjectID: project, Provider: RepoProvider, Path: GeneratedPrefix + path, Section: domain.SectionGenerated,
			Title: title, Body: body, Version: sha, Generated: true,
			SourceURL:  "https://github.com/" + repo + "/tree/" + sha,
			Provenance: domain.Provenance{AuthorRole: GeneratedAuthor, Source: "https://github.com/" + repo + "/commit/" + sha, Created: now, LastVerified: now},
		}
	}
	var out []domain.KnowledgeEntry
	for _, c := range snap.Facts.Components {
		var b strings.Builder
		fmt.Fprintf(&b, "- Path: `%s`\n- Stack: %s\n", c.Path, c.Stack)
		if len(c.Manifests) > 0 {
			fmt.Fprintf(&b, "- Manifests: %s\n", strings.Join(c.Manifests, ", "))
		}
		fmt.Fprintf(&b, "- Build: %s\n- Test: %s\n", command(c.Build), command(c.Test))
		slug := strings.Trim(strings.ReplaceAll(c.Path, "/", "-"), ".-")
		if slug == "" {
			slug = "root"
		}
		out = append(out, mk("components/"+slug, "Component "+c.Path+" ("+c.Stack+")", b.String()))
	}
	var pk strings.Builder
	for _, p := range snap.Packages {
		fmt.Fprintf(&pk, "- `%s`\n", p)
	}
	if pk.Len() == 0 {
		pk.WriteString("No packages found.\n")
	}
	out = append(out, mk("packages", "Packages", pk.String()))
	out = append(out, mk("dependencies", "Direct dependencies", dependencyText(snap.Dependencies)))
	return out
}

func command(c string) string {
	if c == "" {
		return "unknown"
	}
	return "`" + c + "`"
}

func dependencyText(deps []Dependency) string {
	if len(deps) == 0 {
		return "No direct dependencies found in go.mod or package.json.\n"
	}
	byComponent := map[string][]Dependency{}
	var order []string
	for _, d := range deps {
		if _, ok := byComponent[d.Component]; !ok {
			order = append(order, d.Component)
		}
		byComponent[d.Component] = append(byComponent[d.Component], d)
	}
	slices.Sort(order)
	var b strings.Builder
	for _, c := range order {
		fmt.Fprintf(&b, "## %s\n\n", c)
		for _, d := range byComponent[c] {
			fmt.Fprintf(&b, "- %s %s (%s)\n", d.Name, d.Version, d.Kind)
		}
		b.WriteString("\n")
	}
	return b.String()
}

package verification

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/forecrew/forecrew/internal/domain"
)

// Marker is the hidden text that identifies the PR comment of the report, so
// that a rebuild finds and updates it.
const Marker = "<!-- forecrew:verification -->"

// Bounds on what the render lists. Test names, versions and environment names
// come from the repo, so the size of the render does not depend on them.
const (
	maxRows      = 100   // rows of a table or items of a list
	maxTests     = 5     // tests named per criterion
	maxCellText  = 120   // runes of a name in a table cell
	maxRenderLen = 60000 // bytes; a GitHub comment holds 65536 characters
)

// text makes s safe to show in a Markdown table cell or list item: one line,
// no control characters, no table, HTML or code-span breaks and no mention.
func text(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r) && !unicode.IsSpace(r):
			return -1
		case r == '`':
			return '\''
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxCellText {
		s = string(r[:maxCellText-1]) + "…"
	}
	return s
}

// code shows s as inline code: its characters are not Markdown, and an @name
// in it mentions nobody.
func code(s string) string {
	s = text(s)
	if s == "" {
		return "-"
	}
	return "`" + strings.ReplaceAll(s, "|", "\\|") + "`"
}

// plain shows s as plain text in a table cell.
func plain(s string) string {
	s = text(s)
	s = strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;", "@", "@\u200b", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "#", "\\#").Replace(s)
	if s == "" {
		return "-"
	}
	return s
}

func envName(name string) string {
	if name == "" {
		return "host"
	}
	return name
}

func cellText(cell map[string]string) string {
	if len(cell) == 0 {
		return "-"
	}
	return code(CellKey(cell))
}

// Render writes the report as Markdown, for the PR comment, the check run
// summary and the stored artifact. It starts with the Marker.
func Render(r Report) string {
	r = r.normalize()
	var b strings.Builder
	b.WriteString(Marker + "\n")
	fmt.Fprintf(&b, "## Verification of %s: %s\n\n%s.\n", code(r.SHA[:min(len(r.SHA), 12)]), r.Status, r.Summary())
	list(&b, "Blocking the merge", r.Blocking, func(s string) string { return "- " + plain(s) })
	renderResults(&b, r)
	renderCriteria(&b, r)
	if len(r.NewTests) > 0 {
		b.WriteString("\n### New tests on the base branch\n\n| Test | Failed on base |\n|---|---|\n")
		for _, id := range capped(slices.Sorted(maps.Keys(r.NewTests))) {
			fmt.Fprintf(&b, "| %s | %s |\n", code(id), yesNo(r.NewTests[id]))
		}
		more(&b, len(r.NewTests))
	}
	list(&b, "Flaky tests", flakyLines(r.Flaky), func(s string) string { return "- " + s })
	renderEnvironments(&b, r)
	out := b.String()
	if len(out) > maxRenderLen {
		out = strings.ToValidUTF8(out[:maxRenderLen], "") + "\n\n(cut: the report is too long to show here)\n"
	}
	return out
}

func renderResults(b *strings.Builder, r Report) {
	if len(r.Results) == 0 {
		return
	}
	b.WriteString("\n### Results\n\n| Environment | Cell | Suite | Required | Status | Passed | Failed | Skipped | Errored | Attempts | Job |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, x := range capped(r.Results) {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %d | %d | %d | %d | %d | %s |\n", plain(envName(x.Environment)), cellText(x.Cell), plain(x.Suite),
			yesNo(x.Required), plain(x.Status), x.Counts.Passed, x.Counts.Failed, x.Counts.Skipped, x.Counts.Errored, x.Attempts, plain(x.Job))
	}
	more(b, len(r.Results))
}

func renderCriteria(b *strings.Builder, r Report) {
	if len(r.Criteria) == 0 {
		return
	}
	b.WriteString("\n### Acceptance criteria\n\n| Criterion | Status | Tests |\n|---|---|---|\n")
	for _, id := range capped(r.sortedCriteria()) {
		c := r.Criteria[id]
		fmt.Fprintf(b, "| %s | %s | %s |\n", plain(id), plain(c.Status), criterionTests(c))
	}
	more(b, len(r.Criteria))
}

func renderEnvironments(b *strings.Builder, r Report) {
	if len(r.Environments) == 0 {
		return
	}
	b.WriteString("\n### Environments\n\n| Environment | Tool | Version |\n|---|---|---|\n")
	rows := 0
	for _, name := range slices.Sorted(maps.Keys(r.Environments)) {
		v := r.Environments[name].Versions
		if len(v) == 0 {
			fmt.Fprintf(b, "| %s | - | - |\n", plain(envName(name)))
			rows++
		}
		for _, tool := range slices.Sorted(maps.Keys(v)) {
			if rows++; rows <= maxRows {
				fmt.Fprintf(b, "| %s | %s | %s |\n", plain(envName(name)), plain(tool), code(v[tool]))
			}
		}
	}
	more(b, rows)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func capped[T any](s []T) []T { return s[:min(len(s), maxRows)] }

func more(b *strings.Builder, n int) {
	if n > maxRows {
		fmt.Fprintf(b, "\n%d more not shown.\n", n-maxRows)
	}
}

// list writes a titled list unless it is empty.
func list(b *strings.Builder, title string, items []string, line func(string) string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s\n\n", title)
	for _, it := range capped(items) {
		b.WriteString(line(it) + "\n")
	}
	more(b, len(items))
}

func flakyLines(flaky []Flaky) []string {
	out := make([]string, len(flaky))
	for i, f := range flaky {
		envs := make([]string, len(f.Environments))
		for j, e := range f.Environments {
			envs[j] = plain(envName(e))
		}
		out[i] = fmt.Sprintf("%s failed, then passed in %s", code(f.Test), strings.Join(envs, ", "))
	}
	return out
}

// criterionTests lists the tests of a criterion with their results.
func criterionTests(c Criterion) string {
	if len(c.Tests) == 0 {
		if c.Untestable != "" {
			return "untestable: " + plain(c.Untestable)
		}
		return "none"
	}
	ids := slices.Sorted(maps.Keys(c.Tests))
	var parts []string
	for _, id := range ids[:min(len(ids), maxTests)] {
		parts = append(parts, code(id)+" "+plain(c.Tests[id]))
	}
	if len(ids) > maxTests {
		parts = append(parts, strconv.Itoa(len(ids)-maxTests)+" more")
	}
	return strings.Join(parts, "<br>")
}

// Title is the title of the check run.
func (r Report) Title() string {
	switch r.Status {
	case domain.VerificationPassed:
		return "Verified"
	case domain.VerificationFailed:
		return "Verification failed"
	}
	return "Verification incomplete"
}

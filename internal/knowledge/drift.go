package knowledge

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/github"
)

// Drift is what a push changed that an entry may still describe the old way
// (docs/knowledge.md, Drift). The rule is deliberately simple:
//
//   - Paths are the repo paths the push modified, removed or renamed (the
//     old and the new path of a rename). Added files are not drift.
//   - Symbols are the exported Go identifiers (func, method, type, const,
//     var) and the TypeScript exports (function, class, const, let, var,
//     interface, type, enum) that appear on a changed line of a .go, .ts or
//     .tsx file, at least minSymbol characters long.
//
// An entry drifts when its body mentions a path as a whole path or a symbol
// as a whole word.
type Drift struct {
	Paths   []string
	Symbols []string
}

// minSymbol keeps short, common names (Run, New, Get) from flagging every
// entry.
const minSymbol = 5

var symbolRes = []*regexp.Regexp{
	regexp.MustCompile(`^[+-]\s*func\s+(?:\([^)]*\)\s*)?([A-Z]\w*)`),
	regexp.MustCompile(`^[+-]\s*(?:type|const|var)\s+([A-Z]\w*)`),
	regexp.MustCompile(`^[+-]\s*export\s+(?:default\s+)?(?:declare\s+)?(?:async\s+)?(?:abstract\s+)?(?:function\*?|class|const|let|var|interface|type|enum)\s+([A-Za-z_$][\w$]*)`),
}

// DriftOf extracts the drift of a comparison.
func DriftOf(cmp github.Comparison) Drift {
	var d Drift
	for _, f := range cmp.Files {
		if f.Status == github.FileAdded {
			continue
		}
		d.Paths = append(d.Paths, f.Path)
		if f.PreviousPath != "" {
			d.Paths = append(d.Paths, f.PreviousPath)
		}
		switch path.Ext(f.Path) {
		case ".go", ".ts", ".tsx":
			for line := range strings.SplitSeq(f.Patch, "\n") {
				for _, re := range symbolRes {
					if m := re.FindStringSubmatch(line); m != nil && len(m[1]) >= minSymbol {
						d.Symbols = append(d.Symbols, m[1])
					}
				}
			}
		}
	}
	slices.Sort(d.Paths)
	d.Paths = slices.Compact(d.Paths)
	slices.Sort(d.Symbols)
	d.Symbols = slices.Compact(d.Symbols)
	return d
}

// Mentions returns the paths and symbols of d that body mentions.
func (d Drift) Mentions(body string) (paths, symbols []string) {
	for _, p := range d.Paths {
		if mentionsPath(body, p) {
			paths = append(paths, p)
		}
	}
	for _, s := range d.Symbols {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(s) + `\b`).MatchString(body) {
			symbols = append(symbols, s)
		}
	}
	return paths, symbols
}

func mentionsPath(body, p string) bool {
	for from := 0; ; {
		i := strings.Index(body[from:], p)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(p)
		if (start == 0 || !pathChar(body[start-1], true)) && (end == len(body) || !pathChar(body[end], false)) {
			return true
		}
		from = start + 1
	}
}

// pathChar reports whether c continues a path next to a mention; a dot
// before it does, a dot after it is a full stop.
func pathChar(c byte, before bool) bool {
	return c == '_' || c == '-' || c == '/' || (before && c == '.') || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// DriftReason is the reverify_reason of an entry: the commit and what it
// changed that the entry mentions.
func DriftReason(sha string, paths, symbols []string) string {
	const shown = 5
	items := append(slices.Clone(paths), symbols...)
	more := ""
	if len(items) > shown {
		more = fmt.Sprintf(" and %d more", len(items)-shown)
		items = items[:shown]
	}
	return "commit " + sha[:min(len(sha), 12)] + " changed " + strings.Join(items, ", ") + more
}

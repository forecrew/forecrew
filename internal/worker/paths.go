package worker

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// writable reports whether file (relative to the repo root, "/" separated)
// may be changed under patterns: it matches at least one include pattern and
// no "!" exclude pattern. Empty patterns allow nothing.
func writable(patterns []string, file string) bool {
	ok := false
	for _, p := range patterns {
		if ex, isEx := strings.CutPrefix(p, "!"); isEx {
			if m, _ := doublestar.Match(ex, file); m {
				return false
			}
			continue
		}
		if m, _ := doublestar.Match(p, file); m {
			ok = true
		}
	}
	return ok
}

// outsideWritePaths returns the files that patterns do not allow.
func outsideWritePaths(patterns, files []string) []string {
	var out []string
	for _, f := range files {
		if !writable(patterns, f) {
			out = append(out, f)
		}
	}
	return out
}

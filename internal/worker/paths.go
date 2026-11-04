package worker

import "github.com/forecrew/forecrew/internal/roles"

// outsideWritePaths returns the files that patterns do not allow.
func outsideWritePaths(patterns, files []string) []string {
	var out []string
	for _, f := range files {
		if !roles.PathAllowed(patterns, f) {
			out = append(out, f)
		}
	}
	return out
}

// lockedPaths returns the files that match the locked globs.
func lockedPaths(locked, files []string) []string {
	var out []string
	for _, f := range files {
		if roles.PathAllowed(locked, f) {
			out = append(out, f)
		}
	}
	return out
}

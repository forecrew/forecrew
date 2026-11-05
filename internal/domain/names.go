package domain

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether s is a valid name for an environment, toolbox,
// pool, suite or matrix axis: 1 to 64 lowercase letters, digits, '.', '_'
// or '-', starting with a letter or digit.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// secretSuffixes are the name endings SecretLikeName refuses.
var secretSuffixes = []string{"TOKEN", "SECRET", "PASSWORD", "KEY"}

// SecretLikeName reports whether an environment variable name looks like it
// holds a secret: it is, or ends in _ followed by, TOKEN, SECRET, PASSWORD or
// KEY, ignoring case (GITHUB_TOKEN, AWS_SECRET, DB_PASSWORD, API_KEY).
// Files committed to a repo (environments, matrix, toolboxes) and non-secret
// env maps of job specs refuse such names.
func SecretLikeName(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	return slices.ContainsFunc(secretSuffixes, func(s string) bool {
		return n == s || strings.HasSuffix(n, "_"+s)
	})
}

// SecretPatterns matches well-known credential shapes: API keys, provider and
// Forecrew tokens, private keys and URLs with a password. Logs are scrubbed
// with it, and values that must never hold a secret are refused with it.
var SecretPatterns = regexp.MustCompile(strings.Join([]string{
	`sk-ant-[A-Za-z0-9_-]{8,}`,
	`sk-[A-Za-z0-9_-]{20,}`,
	`gh[pousr]_[A-Za-z0-9]{20,}`,
	`github_pat_[A-Za-z0-9_]{20,}`,
	`fc[wmpb]_[A-Za-z0-9_-]{20,}`,
	`A[KS]IA[A-Z0-9]{16}`,
	`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
	`://[^/\s:@]+:[^/\s@]+@`,
}, "|"))

// RelativePath reports whether p is a slash-separated path (or glob)
// relative to the repo root that does not leave it.
func RelativePath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, `\`) &&
		!slices.Contains(strings.Split(p, "/"), "..")
}

var artifactSegment = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// ValidArtifactName reports whether name can name a job artifact: a relative
// path of at most 255 bytes whose segments use letters, digits, '.', '_' and
// '-' and do not start with '.'.
func ValidArtifactName(name string) bool {
	if name == "" || len(name) > 255 || path.Clean(name) != name {
		return false
	}
	for seg := range strings.SplitSeq(name, "/") {
		if !artifactSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// problems collects validation messages.
type problems []string

func (p *problems) addf(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

// err returns nil without problems, or every problem joined, wrapping
// ErrInvalidSpec.
func (p problems) err(what string) error { return p.errOf(ErrInvalidSpec, what) }

// errOf is err with another sentinel.
func (p problems) errOf(sentinel error, what string) error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: %s", sentinel, what, strings.Join(p, "; "))
}

// checkText adds a problem when s is empty or only blanks.
func (p *problems) checkText(field, s string) {
	if strings.TrimSpace(s) == "" {
		p.addf("%s must be set", field)
	}
}

// checkEnum adds a problem when v is not one of all.
func checkEnum[S ~string](p *problems, field string, v S, all []S) {
	if !slices.Contains(all, v) {
		p.addf("%s must be one of %v, got %q", field, all, v)
	}
}

// checkEnv adds a problem per empty or secret-looking name in env.
func (p *problems) checkEnv(field string, env map[string]string) {
	for _, k := range sortedKeys(env) {
		switch {
		case strings.TrimSpace(k) == "":
			p.addf("%s: empty variable name", field)
		case SecretLikeName(k):
			p.addf("%s.%s: looks like a secret; secrets are never allowed here", field, k)
		}
	}
}

// checkPaths adds a problem per path that is not relative to the repo root.
func (p *problems) checkPaths(field string, paths []string) {
	for _, g := range paths {
		if !RelativePath(g) {
			p.addf("%s: %q must be relative to the repo root", field, g)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

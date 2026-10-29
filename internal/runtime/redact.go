package runtime

import (
	"regexp"
	"slices"
	"strings"
)

const redacted = "[REDACTED]"

// minSecretLen is the shortest env value treated as a secret; shorter values
// would mangle ordinary text.
const minSecretLen = 6

// secretPatterns match well-known credential shapes, so a secret is hidden
// even when it did not come from RunSpec.Env.
var secretPatterns = regexp.MustCompile(strings.Join([]string{
	`sk-ant-[A-Za-z0-9_-]{8,}`,
	`sk-[A-Za-z0-9_-]{20,}`,
	`gh[pousr]_[A-Za-z0-9]{20,}`,
	`github_pat_[A-Za-z0-9_]{20,}`,
	`fc[wm]_[A-Za-z0-9_-]{20,}`,
	`(?i)bearer\s+[A-Za-z0-9._~+/=-]{16,}`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
}, "|"))

// Redactor hides secrets in text: the values of RunSpec.Env and well-known
// credential shapes. The zero value is not usable; build it with NewRedactor.
type Redactor struct {
	replacer *strings.Replacer
}

// NewRedactor returns a Redactor for the given env values (credentials).
func NewRedactor(env map[string]string) *Redactor {
	var secrets []string
	for _, v := range env {
		if len(v) >= minSecretLen {
			secrets = append(secrets, v)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		pairs = append(pairs, s, redacted)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// String returns s with secrets replaced by "[REDACTED]".
func (r *Redactor) String(s string) string {
	return secretPatterns.ReplaceAllString(r.replacer.Replace(s), redacted)
}

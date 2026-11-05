package runtime

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

const redacted = "[REDACTED]"

// minSecretLen is the shortest env value treated as a secret; shorter values
// would mangle ordinary text.
const minSecretLen = 6

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
		// The same secret as it appears inside a JSON string.
		if b, err := json.Marshal(s); err == nil && len(b) > 2 && string(b[1:len(b)-1]) != s {
			pairs = append(pairs, string(b[1:len(b)-1]), redacted)
		}
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// String returns s with secrets replaced by "[REDACTED]".
func (r *Redactor) String(s string) string {
	return domain.SecretPatterns.ReplaceAllString(r.replacer.Replace(s), redacted)
}

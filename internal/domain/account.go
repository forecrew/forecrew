package domain

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"uuid"
)

// Account is an organization (docs/contracts/identity.md): it owns
// projects, workers, tokens, memberships and teams. Storage and Go code keep
// the name account (account_id); the API, UI, CLI and docs say
// organization.
type Account struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`                 // ValidSlug, unique; in URLs such as /login?org=<slug>
	CreatedBy string    `json:"created_by,omitempty"` // Actor.String(); empty for the organization created at install
	// MaxTokenTTL caps the personal tokens of users under SSO enforcement
	// (docs/contracts/identity.md, Tokens); zero is no cap.
	MaxTokenTTL Duration  `json:"max_token_ttl,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
}

// DefaultSSOTokenTTL is the MaxTokenTTL an organization without one gets
// when it turns SSO enforcement on; an Owner may allow longer.
const DefaultSSOTokenTTL = 7 * 24 * time.Hour

var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSlug reports whether s is a URL-safe slug of an organization or
// team: 1 to 63 lowercase letters, digits and '-', starting and ending with
// a letter or digit.
func ValidSlug(s string) bool { return slugRE.MatchString(s) }

var nonSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// SlugCandidate returns the nth slug to try for base, a Slugify result:
// base itself, then base-2, base-3, ..., shortened to stay a ValidSlug. It
// is the rule of migration 00005.
func SlugCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	return strings.TrimRight(base[:min(len(base), 63-len(suffix))], "-") + suffix
}

// Slugify derives a slug from a name: lowercased, runs of other characters
// replaced by '-', trimmed to 63 characters. A name without letters or
// digits gives "org".
func Slugify(name string) string {
	s := strings.Trim(nonSlugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		return "org"
	}
	return s
}

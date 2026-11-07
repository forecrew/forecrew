package linear

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Ways to sign in.
const (
	// AuthAPIKey is a personal API key of the dedicated Linear user, sent as
	// it is in the Authorization header.
	AuthAPIKey = "api_key"
	// AuthOAuthToken is an OAuth access token, sent as a Bearer token.
	AuthOAuthToken = "oauth_token" //nolint:gosec // the name of a way to sign in, not a credential
)

// Config is tracker_connections.config of kind linear. It holds references to
// secrets, never a secret.
type Config struct {
	// Teams are the keys of the teams the connection reads (ENG).
	Teams []string `json:"teams"`
	// Auth is api_key (the default) or oauth_token.
	Auth string `json:"auth,omitempty"`
	// TokenRef is a store: reference to the key or token.
	TokenRef string `json:"token_ref"`
	// WebhookSecretRef is a store: reference to the signing secret of the
	// webhook; without it webhooks are off and polling does everything.
	WebhookSecretRef string `json:"webhook_secret_ref,omitempty"`
}

var (
	teamKeyRE  = regexp.MustCompile(`^[A-Z0-9]{1,15}$`)
	issueKeyRE = regexp.MustCompile(`^([A-Z0-9]{1,15})-[1-9][0-9]{0,9}$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("linear: %s: %w", fmt.Sprintf(format, args...), tracker.ErrInvalid)
}

// parseConfig reads a connection's config strictly: a key Linear does not
// have is a mistake to say, not to ignore.
func parseConfig(raw map[string]any) (Config, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return Config{}, fmt.Errorf("encode config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, invalid("config: %v", err)
	}
	if c.Auth == "" {
		c.Auth = AuthAPIKey
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if len(c.Teams) == 0 {
		return invalid("teams is required: the keys of the teams to read")
	}
	for _, t := range c.Teams {
		if !teamKeyRE.MatchString(t) {
			return invalid("team key %q is not a Linear team key (capital letters and digits)", t)
		}
	}
	if c.Auth != AuthAPIKey && c.Auth != AuthOAuthToken {
		return invalid("auth must be %s or %s", AuthAPIKey, AuthOAuthToken)
	}
	if _, ok := domain.StoreSecretName(c.TokenRef); !ok {
		return invalid("token_ref must name a value of the secret store: store:<name>")
	}
	if _, ok := domain.StoreSecretName(c.WebhookSecretRef); c.WebhookSecretRef != "" && !ok {
		return invalid("webhook_secret_ref must name a value of the secret store: store:<name>")
	}
	return nil
}

// ours reports whether an issue key is of a team of the connection.
func (c Config) ours(key string) bool {
	m := issueKeyRE.FindStringSubmatch(key)
	return m != nil && slices.Contains(c.Teams, m[1])
}

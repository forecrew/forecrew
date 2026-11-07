package jira

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Deployments of Jira.
const (
	Cloud      = "cloud"
	DataCenter = "data_center"
)

// Ways to sign in.
const (
	// AuthPAT is a personal access token of Jira Data Center, sent as a
	// Bearer token.
	AuthPAT = "pat"
	// AuthAPIToken is an API token of an Atlassian account on Jira Cloud,
	// with the account's email, over the site's URL.
	AuthAPIToken = "api_token"
	// AuthScopedToken is an API token with scopes on Jira Cloud, which
	// Atlassian serves only through api.atlassian.com.
	AuthScopedToken = "scoped_token"
)

// How a webhook proves it comes from Jira.
const (
	// SigningHMAC is the X-Hub-Signature of a webhook with a secret.
	SigningHMAC = "hmac"
	// SigningURL is the secret in the query of the webhook's URL, for a
	// sender that cannot sign, such as an automation rule.
	SigningURL = "url"
)

// Config is tracker_connections.config of kind jira. It holds references to
// secrets, never a secret.
type Config struct {
	// BaseURL is the site, with its context path: https://acme.atlassian.net,
	// https://jira.acme.local/jira.
	BaseURL string `json:"base_url"`
	// Projects are the keys of the projects the connection reads.
	Projects []string `json:"projects"`
	Auth     string   `json:"auth"`
	// Email is the Atlassian account of a Cloud token.
	Email string `json:"email,omitempty"`
	// TokenRef is a store: reference to the token.
	TokenRef string `json:"token_ref"`
	// CABundle is PEM certificates to trust besides the system's, for a
	// Jira behind a private CA. Certificates are public; a private key is
	// refused.
	CABundle string `json:"ca_bundle,omitempty"`
	// WebhookSecretRef is a store: reference to the secret of the webhook;
	// without it webhooks are off and polling does everything.
	WebhookSecretRef string `json:"webhook_secret_ref,omitempty"`
	// WebhookSigning is hmac (the default) or url.
	WebhookSigning string `json:"webhook_signing,omitempty"`
	// Deployment and CloudID are read from Jira when the connection is set
	// up (tracker.Preparer).
	Deployment string `json:"deployment,omitempty"`
	CloudID    string `json:"cloud_id,omitempty"`
}

// derived are the keys Prepare fills in.
var derived = []string{"deployment", "cloud_id"}

var (
	projectKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}$`)
	issueKeyRE   = regexp.MustCompile(`^([A-Z][A-Z0-9_]{0,254})-[1-9][0-9]{0,17}$`)
	emailRE      = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
	cloudIDRE    = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("jira: %s: %w", fmt.Sprintf(format, args...), tracker.ErrInvalid)
}

// parseConfig reads a connection's config strictly: a key Jira does not have
// is a mistake to say, not to ignore.
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
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.WebhookSigning == "" {
		c.WebhookSigning = SigningHMAC
	}
	return c, c.validate()
}

// validate checks what can be checked without asking Jira.
func (c Config) validate() error {
	u, err := url.Parse(c.BaseURL)
	switch {
	case c.BaseURL == "":
		return invalid("base_url is required")
	case err != nil:
		return invalid("base_url: %v", err)
	case u.Scheme != "https":
		return invalid("base_url must be https: TLS cannot be turned off")
	case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return invalid("base_url must be a plain https URL, with an optional context path")
	case len(c.Projects) == 0:
		return invalid("projects is required: the keys of the projects to read")
	}
	for _, p := range c.Projects {
		if !projectKeyRE.MatchString(p) {
			return invalid("project key %q is not a Jira key", p)
		}
	}
	if err := c.validateAuth(); err != nil {
		return err
	}
	if _, ok := domain.StoreSecretName(c.WebhookSecretRef); c.WebhookSecretRef != "" && !ok {
		return invalid("webhook_secret_ref must name a value of the secret store: store:<name>")
	}
	if c.WebhookSigning != SigningHMAC && c.WebhookSigning != SigningURL {
		return invalid("webhook_signing must be %s or %s", SigningHMAC, SigningURL)
	}
	if err := checkBundle(c.CABundle); err != nil {
		return err
	}
	if c.CloudID != "" && !cloudIDRE.MatchString(c.CloudID) {
		return invalid("cloud_id %q is not a cloud ID", c.CloudID)
	}
	return nil
}

// validateAuth checks the token and that it fits the deployment, once
// that is known.
func (c Config) validateAuth() error {
	switch c.Auth {
	case AuthPAT:
		if c.Email != "" {
			return invalid("a personal access token takes no email")
		}
	case AuthAPIToken, AuthScopedToken:
		if !emailRE.MatchString(c.Email) {
			return invalid("auth %s needs the email of the token's account", c.Auth)
		}
	default:
		return invalid("auth must be %s, %s or %s", AuthPAT, AuthAPIToken, AuthScopedToken)
	}
	if _, ok := domain.StoreSecretName(c.TokenRef); !ok {
		return invalid("token_ref must name a value of the secret store: store:<name>")
	}
	switch {
	case c.Deployment == Cloud && c.Auth == AuthPAT:
		return invalid("Jira Cloud takes an API token, not a personal access token")
	case c.Deployment == DataCenter && c.Auth != AuthPAT:
		return invalid("Jira Data Center takes a personal access token")
	case c.Deployment != "" && c.Deployment != Cloud && c.Deployment != DataCenter:
		return invalid("deployment %q is not %s or %s", c.Deployment, Cloud, DataCenter)
	}
	return nil
}

// checkBundle refuses anything in a CA bundle but certificates: a private
// key there would be a secret in the config.
func checkBundle(bundle string) error {
	rest := []byte(bundle)
	for len(bytes.TrimSpace(rest)) > 0 {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		switch {
		case b == nil:
			return invalid("ca_bundle must hold PEM certificates only")
		case b.Type != "CERTIFICATE":
			return invalid("ca_bundle holds a %s: only certificates belong there", b.Type)
		}
	}
	return nil
}

// ours reports whether an issue key is of a project of the connection.
func (c Config) ours(key string) bool {
	m := issueKeyRE.FindStringSubmatch(key)
	return m != nil && slices.Contains(c.Projects, m[1])
}

// toMap returns the config as tracker_connections.config holds it.
func (c Config) toMap() map[string]any {
	raw, err := json.Marshal(c)
	if err != nil {
		panic("jira: config does not encode: " + err.Error()) // plain data
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m) // what Marshal wrote
	return m
}

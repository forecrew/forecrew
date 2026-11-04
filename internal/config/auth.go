package config

import (
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// Defaults of auth (docs/contracts/config.md).
const (
	DefaultSessionIdle     = 7 * 24 * time.Hour
	DefaultSessionAbsolute = 30 * 24 * time.Hour
	DefaultTokenTTL        = 90 * 24 * time.Hour
)

// Auth configures sign-in, sessions and tokens (docs/contracts/identity.md).
type Auth struct {
	// SessionIdle ends a session that was not used for this long.
	SessionIdle time.Duration `yaml:"session_idle"`
	// SessionAbsolute ends a session this long after sign-in, used or not.
	SessionAbsolute time.Duration `yaml:"session_absolute"`
	// TokenDefaultTTL is the expiry of a personal or service account token
	// created without one.
	TokenDefaultTTL time.Duration `yaml:"token_default_ttl"`
	// TokenLogin lets POST /api/v1/session sign in with a token (legacy).
	TokenLogin bool `yaml:"token_login"`
	// Connections are the instance-level connections by name, usable by
	// every organization.
	Connections map[string]Connection `yaml:"connections"`
	// InstanceAdmins are the people who may recover access to any
	// organization: verified emails, or github:<user ID>. GitHub logins are
	// refused, because a login can be renamed and then taken by someone else.
	InstanceAdmins []string `yaml:"instance_admins"`
}

// Connection is an instance-level sign-in connection. The client secret
// is never in the file: ClientSecretRef names it (env:NAME or
// secret:name).
type Connection struct {
	Kind            domain.ConnectionKind `yaml:"kind"`
	Issuer          string                `yaml:"issuer"`
	ClientID        string                `yaml:"client_id"`
	ClientSecretRef string                `yaml:"client_secret_ref"`
	GroupClaim      string                `yaml:"group_claim"`
	// secretSet records at Parse whether an env:NAME secret is set; the
	// value itself is read where it is used, never kept in the config.
	secretSet bool
}

// Domain returns the connection called name as the domain value forecrewd
// stores (ConnectionRepo.UpsertInstance): instance-level and enabled.
func (c Connection) Domain(name string) domain.Connection {
	return domain.Connection{Kind: c.Kind, Name: name, Issuer: c.Issuer, ClientID: c.ClientID,
		ClientSecretRef: c.ClientSecretRef, GroupClaim: c.GroupClaim, Enabled: true}
}

// instanceAdminGitHub starts an instance admin named by GitHub user ID.
const instanceAdminGitHub = "github:"

// IsInstanceAdmin reports whether a person is an instance admin, by their
// verified email (ignoring case) or their GitHub user ID (the subject of
// their GitHub identity). Either may be empty.
func (a Auth) IsInstanceAdmin(verifiedEmail, githubUserID string) bool {
	for _, admin := range a.InstanceAdmins {
		if id, ok := strings.CutPrefix(admin, instanceAdminGitHub); ok {
			if githubUserID != "" && id == githubUserID {
				return true
			}
		} else if verifiedEmail != "" && strings.EqualFold(admin, verifiedEmail) {
			return true
		}
	}
	return false
}

// validInstanceAdmin reports whether s is an email or github:<user ID>.
func validInstanceAdmin(s string) bool {
	if id, ok := strings.CutPrefix(s, instanceAdminGitHub); ok {
		return id != "" && strings.Trim(id, "0123456789") == ""
	}
	local, host, ok := strings.Cut(s, "@")
	return ok && local != "" && host != "" && !strings.ContainsAny(s, " \t") && !strings.Contains(host, "@")
}

// readAuth records which env:NAME connection secrets are set.
func (c *Config) readAuth(getenv func(string) string) {
	for name, conn := range c.Auth.Connections {
		env, ok := strings.CutPrefix(conn.ClientSecretRef, domain.SecretRefEnv)
		conn.secretSet = ok && getenv(env) != ""
		c.Auth.Connections[name] = conn
	}
}

func (c *Config) applyAuthDefaults() {
	a := &c.Auth
	if a.SessionIdle == 0 {
		a.SessionIdle = DefaultSessionIdle
	}
	if a.SessionAbsolute == 0 {
		a.SessionAbsolute = DefaultSessionAbsolute
	}
	if a.TokenDefaultTTL == 0 {
		a.TokenDefaultTTL = DefaultTokenTTL
	}
}

// validateAuth checks durations, connections and instance admins. A
// connection's env:NAME secret must be set in the environment.
func (c *Config) validateAuth(p *problems) {
	a := c.Auth
	if a.SessionIdle < 0 || a.SessionAbsolute < 0 || a.TokenDefaultTTL < 0 {
		p.addf("auth: durations must be positive")
	}
	if a.SessionIdle > a.SessionAbsolute {
		p.addf("auth.session_idle must not be longer than auth.session_absolute")
	}
	for _, name := range sortedKeys(a.Connections) {
		conn := a.Connections[name]
		if err := conn.Domain(name).Validate(); err != nil {
			p.addf("auth.connections.%s: %v", name, err)
			continue
		}
		if env, ok := strings.CutPrefix(conn.ClientSecretRef, domain.SecretRefEnv); ok && !conn.secretSet {
			p.addf("auth.connections.%s: %s is not set", name, env)
		}
	}
	for _, admin := range a.InstanceAdmins {
		if !validInstanceAdmin(admin) {
			p.addf("auth.instance_admins: %q must be an email or github:<user ID>, not a GitHub login", admin)
		}
	}
}

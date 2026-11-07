package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"uuid"
)

// User is a person. Users are global: they join organizations through
// memberships (docs/contracts/identity.md).
type User struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email,omitempty"` // primary email; unique among verified ones, ignoring case
	EmailVerified bool      `json:"email_verified"`
	// InstanceAdmin users may open any organization's settings to recover
	// access; every such use is audited.
	InstanceAdmin bool      `json:"instance_admin"`
	DisabledAt    time.Time `json:"disabled_at,omitzero"` // a disabled user cannot sign in or use tokens
	LastSeenAt    time.Time `json:"last_seen_at,omitzero"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Identity links a user to an account at an identity provider through a
// connection. (connection, subject) is unique.
type Identity struct {
	ID           uuid.UUID `json:"id"`
	UserID       uuid.UUID `json:"user_id"`
	ConnectionID uuid.UUID `json:"connection_id"`
	Subject      string    `json:"subject"`         // the provider's stable user ID (OIDC sub, GitHub user ID)
	Login        string    `json:"login,omitempty"` // GitHub login, or the provider's preferred username
	Email        string    `json:"email,omitempty"`
	LastUsedAt   time.Time `json:"last_used_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}

// MembershipStatus is the state of a user in an organization.
type MembershipStatus string

// Membership states. Only active members get permissions.
const (
	MembershipActive    MembershipStatus = "active"
	MembershipInvited   MembershipStatus = "invited"
	MembershipSuspended MembershipStatus = "suspended"
)

// MembershipStatuses returns every membership state.
func MembershipStatuses() []MembershipStatus {
	return []MembershipStatus{MembershipActive, MembershipInvited, MembershipSuspended}
}

// Source says what created a membership, a team or a team member.
type Source string

// Sources. Manual is a person through the UI, API or CLI.
const (
	SourceManual     Source = "manual"
	SourceInvitation Source = "invitation" // memberships only
	SourceJIT        Source = "jit"        // memberships only: just-in-time at first sign-in
	SourceIDP        Source = "idp"        // teams and team members: group mapping at sign-in
	SourceSCIM       Source = "scim"
)

// MembershipSources returns the sources of a membership.
func MembershipSources() []Source {
	return []Source{SourceManual, SourceInvitation, SourceJIT, SourceSCIM}
}

// TeamSources returns the sources of a team and of a team member.
func TeamSources() []Source { return []Source{SourceManual, SourceIDP, SourceSCIM} }

// Membership puts a user in an organization. (account, user) is unique.
type Membership struct {
	ID         uuid.UUID        `json:"id"`
	AccountID  uuid.UUID        `json:"account_id"`
	UserID     uuid.UUID        `json:"user_id"`
	Status     MembershipStatus `json:"status"`
	Source     Source           `json:"source"`
	ExternalID string           `json:"external_id,omitempty"` // SCIM externalId of the user in this organization
	// SCIMSuspended says SCIM suspended the membership, so SCIM may let it
	// back in; an Owner's suspension never carries it.
	SCIMSuspended bool      `json:"scim_suspended,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Invitation asks someone, by email or GitHub login, to join an
// organization with roles. Only the SHA-256 of its token is stored.
type Invitation struct {
	ID          uuid.UUID `json:"id"`
	AccountID   uuid.UUID `json:"account_id"`
	Email       string    `json:"email,omitempty"` // exactly one of Email and GitHubLogin
	GitHubLogin string    `json:"github_login,omitempty"`
	// GitHubUserID is the stable ID of GitHubLogin when the invitation was
	// created; it is what acceptance compares, because a login can be renamed
	// and then taken by someone else.
	GitHubUserID int64     `json:"github_user_id,omitempty"`
	Grants       []Grant   `json:"roles"` // roles given on acceptance
	TokenHash    []byte    `json:"-"`
	ExpiresAt    time.Time `json:"expires_at"`
	InvitedBy    string    `json:"invited_by"`           // Actor.String()
	AcceptedBy   uuid.UUID `json:"accepted_by,omitzero"` // the user who accepted
	AcceptedAt   time.Time `json:"accepted_at,omitzero"`
	RevokedAt    time.Time `json:"revoked_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}

// Pending reports whether the invitation can still be accepted at now.
func (i Invitation) Pending(now time.Time) bool {
	return i.AcceptedAt.IsZero() && i.RevokedAt.IsZero() && now.Before(i.ExpiresAt)
}

// Validate checks that exactly one of Email and GitHubLogin is set, that
// the token hash is a SHA-256, that it expires, and that every grant names a
// role.
func (i Invitation) Validate() error {
	var p problems
	if (i.Email == "") == (i.GitHubLogin == "") {
		p.addf("exactly one of email and github_login must be set")
	}
	if i.Email != "" && !strings.Contains(i.Email, "@") {
		p.addf("email %q is not an email address", i.Email)
	}
	if len(i.TokenHash) != 32 {
		p.addf("token hash must be a SHA-256")
	}
	if i.ExpiresAt.IsZero() {
		p.addf("expires_at must be set")
	}
	for _, g := range i.Grants {
		if g.RoleID == uuid.Nil() {
			p.addf("every role must have an ID")
		}
	}
	return p.errOf(ErrInvalidRecord, "invitation")
}

// Team groups members of an organization. Slug is unique per organization.
type Team struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"` // ValidSlug
	Source     Source    `json:"source"`
	ExternalID string    `json:"external_id,omitempty"` // SCIM group externalId; unique per organization
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TeamMember puts a member of the organization in a team. A source idp
// member is added and removed by group mapping at sign-in, a scim one by
// SCIM; manual members are never touched by either.
type TeamMember struct {
	TeamID    uuid.UUID `json:"team_id"`
	UserID    uuid.UUID `json:"user_id"`
	AccountID uuid.UUID `json:"account_id"`
	Source    Source    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// ServiceAccount is a non-human member for CI and scripts: it holds tokens
// and role bindings and never signs in interactively. Name is unique per
// organization.
type ServiceAccount struct {
	ID          uuid.UUID `json:"id"`
	AccountID   uuid.UUID `json:"account_id"`
	Name        string    `json:"name"` // ValidName
	Description string    `json:"description,omitempty"`
	CreatedBy   string    `json:"created_by"` // Actor.String()
	DisabledAt  time.Time `json:"disabled_at,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
}

// ConnectionKind is the kind of identity provider of a connection.
type ConnectionKind string

// Connection kinds. Every one is OpenID Connect or, for GitHub, the GitHub
// App's user authorization; forecrewd never parses SAML.
const (
	ConnectionGitHub    ConnectionKind = "github"
	ConnectionGoogle    ConnectionKind = "google"
	ConnectionMicrosoft ConnectionKind = "microsoft"
	ConnectionOIDC      ConnectionKind = "oidc"
)

// ConnectionKinds returns every connection kind.
func ConnectionKinds() []ConnectionKind {
	return []ConnectionKind{ConnectionGitHub, ConnectionGoogle, ConnectionMicrosoft, ConnectionOIDC}
}

// GoogleIssuer is the issuer of Google sign-in.
const GoogleIssuer = "https://accounts.google.com"

// JIT configures just-in-time membership at a first sign-in (enterprise).
type JIT struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// DefaultRoles are organization-scope roles of the connection's
	// organization given to the new member.
	DefaultRoles []uuid.UUID `json:"default_roles" yaml:"default_roles"`
}

// Connection is a way to sign in. AccountID zero is an instance-level
// connection from config (forecrew.yaml auth.connections), usable by every
// organization; the others belong to one organization. The client secret is
// never stored: ClientSecretRef names where forecrewd reads it.
type Connection struct {
	ID              uuid.UUID      `json:"id"`
	AccountID       uuid.UUID      `json:"account_id,omitzero"`
	Kind            ConnectionKind `json:"kind"`
	Name            string         `json:"name"`             // ValidName, unique per organization (or among instance connections)
	Issuer          string         `json:"issuer,omitempty"` // OIDC issuer URL; empty for github
	ClientID        string         `json:"client_id"`
	ClientSecretRef string         `json:"client_secret_ref"` // ValidSecretRef; ValidOrganizationSecretRef with an AccountID
	JIT             JIT            `json:"jit"`
	GroupClaim      string         `json:"group_claim,omitempty"` // ID token claim with the user's groups
	Enabled         bool           `json:"enabled"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

var (
	envRefRE    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	secretRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
)

// Secret reference prefixes.
const (
	SecretRefEnv  = "env:"    // an environment variable of forecrewd
	SecretRefFile = "secret:" // a file under /run/secrets
	// SecretRefStore names a value of the secret store (internal/secrets),
	// in the organization of what refers to it.
	SecretRefStore = "store:"
	// StagedSecretPrefix starts the names where values entered for a plan
	// wait for its confirmation. They belong to internal/secrets: no
	// reference names one, so no plan reads what another plan staged.
	StagedSecretPrefix = "staged/"
)

// The namespace of an organization's connections. Whoever manages such a
// connection controls its issuer, and forecrewd sends the client secret
// there, so these references can never name forecrewd's own secrets.
const (
	OrganizationSecretEnv  = SecretRefEnv + "FORECREW_CONN_"
	OrganizationSecretFile = SecretRefFile + "conn-"
)

// ValidSecretRef reports whether ref names a secret instead of holding one:
// "env:NAME" with an environment variable name (uppercase letters, digits
// and '_'), "secret:name" with a lowercase secret name, or "store:name", a
// value of the secret store. Any other string, a secret value in particular,
// is refused.
func ValidSecretRef(ref string) bool {
	if _, ok := StoreSecretName(ref); ok {
		return true
	}
	return validHostSecretRef(ref)
}

// validHostSecretRef reports whether ref is one a host resolves on its own:
// "env:NAME" or "secret:name". A fleet agent has no way to the secret store.
func validHostSecretRef(ref string) bool {
	if name, ok := strings.CutPrefix(ref, SecretRefEnv); ok {
		return envRefRE.MatchString(name)
	}
	if name, ok := strings.CutPrefix(ref, SecretRefFile); ok {
		return validSecretName(name)
	}
	return false
}

// ValidSecretName reports whether name has the form of a secret's name.
func ValidSecretName(name string) bool { return validSecretName(name) }

func validSecretName(name string) bool {
	return secretRefRE.MatchString(name) && !strings.Contains(name, "..")
}

// StoreSecretName returns the name of a "store:name" reference, and false
// for anything else, the reserved names of staged values included.
func StoreSecretName(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, SecretRefStore)
	return name, ok && validSecretName(name) && !strings.HasPrefix(name, StagedSecretPrefix)
}

// ValidOrganizationSecretRef reports whether ref is a ValidSecretRef in the
// namespace of organizations' connections: "env:FORECREW_CONN_NAME",
// "secret:conn-name", or "store:name", which only reaches the organization's
// own values.
func ValidOrganizationSecretRef(ref string) bool {
	if !ValidSecretRef(ref) {
		return false
	}
	if _, ok := StoreSecretName(ref); ok {
		return true
	}
	for _, prefix := range []string{OrganizationSecretEnv, OrganizationSecretFile} {
		if name, ok := strings.CutPrefix(ref, prefix); ok && name != "" {
			return true
		}
	}
	return false
}

// Validate checks a connection: kind, name, client ID, a secret reference
// (never a value, and in the reserved namespace on an organization's own
// connection), an issuer that fits the kind, and JIT only on an
// organization's own connection.
func (c Connection) Validate() error {
	var p problems
	checkEnum(&p, "kind", c.Kind, ConnectionKinds())
	if !ValidName(c.Name) {
		p.addf("name %q must be a lowercase name", c.Name)
	}
	p.checkText("client_id", c.ClientID)
	switch {
	case !ValidSecretRef(c.ClientSecretRef):
		p.addf("client_secret_ref must be env:NAME, secret:name or store:name; the secret itself is never stored")
	case c.AccountID != uuid.Nil() && !ValidOrganizationSecretRef(c.ClientSecretRef):
		p.addf("client_secret_ref of an organization's connection must start with %s, %s or %s", OrganizationSecretEnv, OrganizationSecretFile, SecretRefStore)
	}
	switch c.Kind {
	case ConnectionGitHub:
		if c.Issuer != "" {
			p.addf("a github connection has no issuer")
		}
	case ConnectionGoogle:
		if c.Issuer != "" && c.Issuer != GoogleIssuer {
			p.addf("the issuer of a google connection is %s", GoogleIssuer)
		}
	default:
		if u, err := url.Parse(c.Issuer); err != nil || u.Scheme != "https" || u.Host == "" {
			p.addf("issuer must be an https URL")
		}
	}
	if c.AccountID == uuid.Nil() && (c.JIT.Enabled || len(c.JIT.DefaultRoles) > 0) {
		p.addf("jit is only for an organization's own connection")
	}
	return p.errOf(ErrInvalidRecord, fmt.Sprintf("connection %s", c.Name))
}

// GroupMapping maps a group of a connection's identity provider to a team
// of an organization.
type GroupMapping struct {
	ID            uuid.UUID `json:"id"`
	AccountID     uuid.UUID `json:"account_id"`
	ConnectionID  uuid.UUID `json:"connection_id"`
	ExternalGroup string    `json:"external_group"`
	TeamID        uuid.UUID `json:"team_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// VerifiedDomain is an email domain an organization proved it owns with a
// DNS TXT record (enterprise). A verified domain belongs to one
// organization; EnforceSSO needs it verified and a connection.
type VerifiedDomain struct {
	ID                uuid.UUID `json:"id"`
	AccountID         uuid.UUID `json:"account_id"`
	Domain            string    `json:"domain"`             // lowercase
	VerificationToken string    `json:"verification_token"` // the TXT record value; public, not a secret
	VerifiedAt        time.Time `json:"verified_at,omitzero"`
	EnforceSSO        bool      `json:"enforce_sso"`
	ConnectionID      uuid.UUID `json:"connection_id,omitzero"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Session is a signed-in browser. The cookie holds a random token; only
// its SHA-256 is stored.
type Session struct {
	ID            uuid.UUID `json:"id"`
	TokenHash     []byte    `json:"-"`
	UserID        uuid.UUID `json:"user_id"`
	AccountID     uuid.UUID `json:"account_id,omitzero"`    // current organization; zero before one is picked
	ConnectionID  uuid.UUID `json:"connection_id,omitzero"` // zero for a break-glass sign-in
	CreatedAt     time.Time `json:"created_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	IdleExpiresAt time.Time `json:"idle_expires_at"` // moved forward on use, never past ExpiresAt
	ExpiresAt     time.Time `json:"expires_at"`      // absolute
	UserAgent     string    `json:"user_agent,omitempty"`
	IP            string    `json:"ip,omitempty"`
	RevokedAt     time.Time `json:"revoked_at,omitzero"`
}

// Active reports whether the session may be used at now.
func (s Session) Active(now time.Time) bool {
	return s.RevokedAt.IsZero() && now.Before(s.IdleExpiresAt) && now.Before(s.ExpiresAt)
}

// OAuthClient is an MCP client registered with forecrewd's authorization
// server. Clients are instance-wide; a grant picks the organization.
type OAuthClient struct {
	ID           uuid.UUID `json:"id"`
	ClientID     string    `json:"client_id"` // public identifier, unique; a URL for a client ID metadata document
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	SecretHash   []byte    `json:"-"` // SHA-256 of the secret of a confidential client; nil for public clients
	LastUsedAt   time.Time `json:"last_used_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}

// OAuthGrantKind is the kind of an OAuth credential.
type OAuthGrantKind string

// OAuth credential kinds.
const (
	OAuthCode    OAuthGrantKind = "code"    // authorization code, single use
	OAuthAccess  OAuthGrantKind = "access"  // access token for one resource
	OAuthRefresh OAuthGrantKind = "refresh" // refresh token, single use, rotated
)

// OAuthGrantKinds returns every OAuth credential kind.
func OAuthGrantKinds() []OAuthGrantKind {
	return []OAuthGrantKind{OAuthCode, OAuthAccess, OAuthRefresh}
}

// OAuthGrant is one authorization code, access token or refresh token of
// a user for a client in one organization. Only the SHA-256 is stored. Every
// credential issued from one authorization shares FamilyID: reusing a used
// code or refresh token revokes the family, and the family is the
// "connected app" users see and revoke.
type OAuthGrant struct {
	ID            uuid.UUID      `json:"id"`
	Kind          OAuthGrantKind `json:"kind"`
	TokenHash     []byte         `json:"-"`
	FamilyID      uuid.UUID      `json:"family_id"`
	ClientID      uuid.UUID      `json:"client_id"` // OAuthClient.ID
	UserID        uuid.UUID      `json:"user_id"`
	AccountID     uuid.UUID      `json:"account_id"`
	Scopes        []Scope        `json:"scopes"`
	Resource      string         `json:"resource"`                 // the resource indicator, forecrewd's /mcp URL
	RedirectURI   string         `json:"redirect_uri,omitempty"`   // codes only
	CodeChallenge string         `json:"code_challenge,omitempty"` // codes only, PKCE S256
	ExpiresAt     time.Time      `json:"expires_at"`
	UsedAt        time.Time      `json:"used_at,omitzero"` // codes and refresh tokens
	RevokedAt     time.Time      `json:"revoked_at,omitzero"`
	CreatedAt     time.Time      `json:"created_at"`
}

// Usable reports whether the grant may be used at now: not revoked, not
// expired and, for a code or refresh token, not used yet.
func (g OAuthGrant) Usable(now time.Time) bool {
	return g.RevokedAt.IsZero() && g.UsedAt.IsZero() && now.Before(g.ExpiresAt)
}

// SCIMToken is a bearer token of an organization's SCIM endpoint
// (enterprise). Only its SHA-256 is stored.
type SCIMToken struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id"`
	Name       string    `json:"name"`
	TokenHash  []byte    `json:"-"`
	CreatedBy  string    `json:"created_by"` // Actor.String()
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	CreatedAt  time.Time `json:"created_at"`
}

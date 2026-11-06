package mcp

import (
	"context"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Tool names of the organization settings (docs/contracts/permissions.md).
const (
	ToolListMembers        = "list_members"
	ToolSetMemberSuspended = "set_member_suspended"
	ToolRemoveMember       = "remove_member"
	ToolUpdateOrganization = "update_organization"
	ToolListAudit          = "list_audit"
	ToolListConnections    = "list_connections"
	ToolCreateConnection   = "create_connection"
	ToolSetConnection      = "set_connection_enabled"
)

// OrgService manages the organization and its members. Every change obeys
// No escalation.
type OrgService interface {
	// ListMembers lists the members of the organization with their status,
	// the roles they hold directly and their teams.
	ListMembers(ctx context.Context, by domain.Actor) ([]MemberInfo, error)
	// SetMemberSuspended suspends a member or lets a suspended one back in.
	// Suspending ends their sessions and revokes their personal tokens in
	// the organization in the same transaction. The last Owner is a conflict.
	SetMemberSuspended(ctx context.Context, by domain.Actor, in SetMemberInput) (MemberInfo, error)
	// RemoveMember removes a member with their teams and roles, ending their
	// sessions and revoking their personal tokens in the organization in the
	// same transaction. The last Owner is a conflict.
	RemoveMember(ctx context.Context, by domain.Actor, in MemberRefInput) (MemberInfo, error)
	// UpdateOrganization renames the organization or changes its slug.
	UpdateOrganization(ctx context.Context, by domain.Actor, in UpdateOrganizationInput) (domain.Account, error)
	// ListAudit lists the audit log, newest first.
	ListAudit(ctx context.Context, by domain.Actor, in ListAuditInput) ([]AuditInfo, error)
	// ListConnections lists the ways to sign in: the organization's own and
	// the instance's, which come from the server file.
	ListConnections(ctx context.Context, by domain.Actor) ([]ConnectionInfo, error)
	// CreateConnection adds a sign-in connection of the organization, turned
	// off until a test sign-in has worked.
	CreateConnection(ctx context.Context, by domain.Actor, in CreateConnectionInput) (ConnectionInfo, error)
	// SetConnectionEnabled turns an organization's connection on, which needs
	// a test sign-in that worked, or off.
	SetConnectionEnabled(ctx context.Context, by domain.Actor, in SetConnectionInput) (ConnectionInfo, error)
}

// MemberRole is a role a member holds directly.
type MemberRole struct {
	Role    string           `json:"role"`
	Scope   domain.RoleScope `json:"scope"`
	Project string           `json:"project,omitempty"` // empty: the organization
}

// MemberInfo is a member of the organization with who they are.
type MemberInfo struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
	// Email is only shown with member.manage.
	Email    string                  `json:"email,omitempty"`
	Status   domain.MembershipStatus `json:"status"`
	Source   domain.Source           `json:"source"`
	Disabled bool                    `json:"disabled,omitempty"` // the user is disabled on the whole server
	// Roles are the roles bound to the member directly; the roles of their
	// teams are those of the teams.
	Roles      []MemberRole `json:"roles"`
	Teams      []string     `json:"teams"` // slugs
	LastSeenAt time.Time    `json:"last_seen_at,omitzero"`
	CreatedAt  time.Time    `json:"created_at"` // of the membership
}

// MemberRefInput names a member.
type MemberRefInput struct {
	User string `json:"user"` // UUID
}

// SetMemberInput is the input of set_member_suspended.
type SetMemberInput struct {
	User      string `json:"user"` // UUID
	Suspended bool   `json:"suspended"`
}

// UpdateOrganizationInput is the input of update_organization. Empty fields
// stay as they are.
type UpdateOrganizationInput struct {
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// ListAuditInput is the input of list_audit; every filter is optional.
type ListAuditInput struct {
	Actor       string `json:"actor,omitempty"`        // as in the log: user:<id>, service_account:<id>, github:<login>, ...
	Action      string `json:"action,omitempty"`       // for example member.removed
	SubjectType string `json:"subject_type,omitempty"` // for example team
	Subject     string `json:"subject,omitempty"`      // UUID of what was acted on
	Since       string `json:"since,omitempty"`        // RFC 3339, included
	Until       string `json:"until,omitempty"`        // RFC 3339, excluded
	Limit       int    `json:"limit,omitempty"`        // default 100, at most 500
}

// AuditInfo is an entry of the audit log with the name of its actor when it
// is a person or a service account of the organization. Data is what the
// action recorded; treat it as text.
type AuditInfo struct {
	domain.AuditEntry
	ActorName string `json:"actor_name,omitempty"`
}

// ConnectionInfo is a sign-in connection. An instance connection comes
// without its client ID and secret reference.
type ConnectionInfo struct {
	domain.Connection
	Instance bool `json:"instance"` // from the server file, not changed here
	// Tested says a test sign-in worked; an organization's connection can be
	// enabled only once it has.
	Tested bool `json:"tested"`
	// RedirectURL is what the identity provider must allow as redirect address.
	RedirectURL string `json:"redirect_url"`
	// TestURL starts a test sign-in in the browser; only for an organization's
	// connection.
	TestURL string `json:"test_url,omitempty"`
}

// CreateConnectionInput is the input of create_connection.
type CreateConnectionInput struct {
	Kind   domain.ConnectionKind `json:"kind"`             // github, google, microsoft or oidc
	Name   string                `json:"name"`             // lowercase, unique in the organization
	Issuer string                `json:"issuer,omitempty"` // https URL; empty for github and google
	// ClientID is the OAuth client ID registered at the provider.
	ClientID string `json:"client_id"`
	// ClientSecretRef names where forecrewd reads the client secret:
	// env:FORECREW_CONN_NAME or secret:conn-name. It is never the secret.
	ClientSecretRef string `json:"client_secret_ref"`
}

// SetConnectionInput is the input of set_connection_enabled.
type SetConnectionInput struct {
	Connection string `json:"connection"` // name or UUID
	Enabled    bool   `json:"enabled"`
}

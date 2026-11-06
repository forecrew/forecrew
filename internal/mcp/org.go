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

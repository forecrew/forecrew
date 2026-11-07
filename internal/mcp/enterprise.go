package mcp

import (
	"context"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Tool names of the enterprise features (docs/enterprise.md). The open-core
// build lists them too and answers unavailable.
const (
	ToolListDomains        = "list_domains"
	ToolAddDomain          = "add_domain"
	ToolVerifyDomain       = "verify_domain"
	ToolDeleteDomain       = "delete_domain"
	ToolSetSSOEnforcement  = "set_sso_enforcement"
	ToolSetMaxTokenTTL     = "set_max_token_ttl" //nolint:gosec // a tool name
	ToolSetProvisioning    = "set_connection_provisioning"
	ToolListGroupMappings  = "list_group_mappings"
	ToolAddGroupMapping    = "add_group_mapping"
	ToolRemoveGroupMapping = "remove_group_mapping"
	ToolListSCIMTokens     = "list_scim_tokens"  //nolint:gosec // a tool name
	ToolCreateSCIMToken    = "create_scim_token" //nolint:gosec // a tool name
	ToolRevokeSCIMToken    = "revoke_scim_token" //nolint:gosec // a tool name
	ToolCreateRole         = "create_role"
	ToolUpdateRole         = "update_role"
	ToolDeleteRole         = "delete_role"
)

// EnterpriseTools are the operations of the enterprise build, which ee/
// implements. Open core reaches them through this interface only;
// NoEnterprise answers for them in the open-core build.
type EnterpriseTools interface {
	// ListDomains lists the organization's email domains.
	ListDomains(ctx context.Context, by domain.Actor) ([]DomainInfo, error)
	// AddDomain adds a domain, pending until its DNS TXT record is verified.
	AddDomain(ctx context.Context, by domain.Actor, in DomainInput) (DomainInfo, error)
	// VerifyDomain looks the TXT record up and marks the domain verified. A
	// domain another organization verified is a conflict.
	VerifyDomain(ctx context.Context, by domain.Actor, in DomainInput) (DomainInfo, error)
	// DeleteDomain removes a domain, with its enforcement.
	DeleteDomain(ctx context.Context, by domain.Actor, in DomainInput) (DomainInfo, error)
	// SetSSOEnforcement turns SSO enforcement of a verified domain on, through
	// one connection, or off. Turning it on caps personal tokens at
	// domain.DefaultSSOTokenTTL when the organization has no cap.
	SetSSOEnforcement(ctx context.Context, by domain.Actor, in SetSSOEnforcementInput) (DomainInfo, error)
	// SetMaxTokenTTL sets how long the personal tokens of users under SSO
	// enforcement last. It needs org.manage.
	SetMaxTokenTTL(ctx context.Context, by domain.Actor, in SetMaxTokenTTLInput) (domain.Account, error)
	// SetConnectionProvisioning sets the just-in-time membership and the group
	// claim of an organization's connection, within No escalation.
	SetConnectionProvisioning(ctx context.Context, by domain.Actor, in SetProvisioningInput) (ProvisioningInfo, error)
	// ListGroupMappings lists the organization's group mappings.
	ListGroupMappings(ctx context.Context, by domain.Actor) ([]domain.GroupMapping, error)
	// AddGroupMapping maps a group of a connection to a team, within No
	// escalation.
	AddGroupMapping(ctx context.Context, by domain.Actor, in AddGroupMappingInput) (domain.GroupMapping, error)
	// RemoveGroupMapping removes a mapping. A team left without any takes
	// its members with source idp out.
	RemoveGroupMapping(ctx context.Context, by domain.Actor, in GroupMappingRefInput) (domain.GroupMapping, error)
	// ListSCIMTokens lists the organization's SCIM tokens, never their values.
	ListSCIMTokens(ctx context.Context, by domain.Actor) ([]domain.SCIMToken, error)
	// CreateSCIMToken creates a SCIM token, within No escalation. The value
	// is returned once.
	CreateSCIMToken(ctx context.Context, by domain.Actor, in CreateSCIMTokenInput) (CreateSCIMTokenOutput, error)
	// RevokeSCIMToken revokes a SCIM token.
	RevokeSCIMToken(ctx context.Context, by domain.Actor, in SCIMTokenRefInput) (domain.SCIMToken, error)
	// CreateRole creates a custom role from the catalog, which needs every
	// permission it holds.
	CreateRole(ctx context.Context, by domain.Actor, in CreateRoleInput) (domain.Role, error)
	// UpdateRole changes a custom role, which needs every permission it held
	// and will hold: the change reaches everyone bound to it.
	UpdateRole(ctx context.Context, by domain.Actor, in UpdateRoleInput) (domain.Role, error)
	// DeleteRole deletes a custom role. A role in use is a conflict: bound,
	// given by a pending invitation, or a JIT default of a connection.
	DeleteRole(ctx context.Context, by domain.Actor, in RoleRefInput) (domain.Role, error)
}

// DomainInfo is an email domain with the DNS record that proves it.
type DomainInfo struct {
	domain.VerifiedDomain
	RecordName  string `json:"record_name"`  // the TXT record to create
	RecordValue string `json:"record_value"` // its value
}

// DomainInput names an email domain.
type DomainInput struct {
	Domain string `json:"domain"` // for example acme.com, or the UUID of one added already
}

// SetSSOEnforcementInput is the input of set_sso_enforcement.
type SetSSOEnforcementInput struct {
	Domain     string `json:"domain"`
	Enforce    bool   `json:"enforce"`
	Connection string `json:"connection,omitempty"` // name or UUID; required to enforce
}

// SetMaxTokenTTLInput is the input of set_max_token_ttl.
type SetMaxTokenTTLInput struct {
	MaxTokenTTL string `json:"max_token_ttl"` // a duration such as 168h; empty removes the cap
}

// SetProvisioningInput is the input of set_connection_provisioning. A field
// left out stays as it is.
type SetProvisioningInput struct {
	Connection   string    `json:"connection"` // name or UUID of an organization's connection
	JITEnabled   *bool     `json:"jit_enabled,omitempty"`
	DefaultRoles *[]string `json:"default_roles,omitempty"` // names of roles
	GroupClaim   *string   `json:"group_claim,omitempty"`
}

// ProvisioningInfo is what a connection provisions.
type ProvisioningInfo struct {
	ConnectionID uuid.UUID  `json:"connection_id"`
	Name         string     `json:"name"`
	JIT          domain.JIT `json:"jit"`
	GroupClaim   string     `json:"group_claim,omitempty"`
}

// AddGroupMappingInput is the input of add_group_mapping.
type AddGroupMappingInput struct {
	Connection string `json:"connection"` // name or UUID
	Group      string `json:"group"`      // the group as the provider names it in the claim
	Team       string `json:"team"`       // slug or UUID
}

// GroupMappingRefInput names a group mapping.
type GroupMappingRefInput struct {
	Mapping string `json:"mapping"` // UUID
}

// CreateSCIMTokenInput is the input of create_scim_token.
type CreateSCIMTokenInput struct {
	Name string `json:"name"`
}

// CreateSCIMTokenOutput carries the value of a new SCIM token, shown once,
// and the base URL to give the identity provider.
type CreateSCIMTokenOutput struct {
	Token  domain.SCIMToken `json:"token"`
	Secret string           `json:"secret"`
	URL    string           `json:"url"`
}

// SCIMTokenRefInput names a SCIM token.
type SCIMTokenRefInput struct {
	Token string `json:"token"` // name or UUID
}

// CreateRoleInput is the input of create_role.
type CreateRoleInput struct {
	Name        string              `json:"name"`
	Scope       domain.RoleScope    `json:"scope"` // organization or project
	Description string              `json:"description,omitempty"`
	Permissions []domain.Permission `json:"permissions"`
}

// UpdateRoleInput is the input of update_role. A field left out stays as it
// is.
type UpdateRoleInput struct {
	Role        string               `json:"role"` // name or UUID
	Name        string               `json:"name,omitempty"`
	Description *string              `json:"description,omitempty"`
	Permissions *[]domain.Permission `json:"permissions,omitempty"`
}

// RoleRefInput names a custom role.
type RoleRefInput struct {
	Role string `json:"role"` // name or UUID
}

// errNoEnterprise answers the enterprise tools in the open-core build.
var errNoEnterprise = fmt.Errorf("this needs the enterprise build: %w", ErrUnavailable)

// NoEnterprise is EnterpriseTools of the open-core build: every call answers
// unavailable.
type NoEnterprise struct{}

// ListDomains implements EnterpriseTools.
func (NoEnterprise) ListDomains(context.Context, domain.Actor) ([]DomainInfo, error) {
	return nil, errNoEnterprise
}

// AddDomain implements EnterpriseTools.
func (NoEnterprise) AddDomain(context.Context, domain.Actor, DomainInput) (DomainInfo, error) {
	return DomainInfo{}, errNoEnterprise
}

// VerifyDomain implements EnterpriseTools.
func (NoEnterprise) VerifyDomain(context.Context, domain.Actor, DomainInput) (DomainInfo, error) {
	return DomainInfo{}, errNoEnterprise
}

// DeleteDomain implements EnterpriseTools.
func (NoEnterprise) DeleteDomain(context.Context, domain.Actor, DomainInput) (DomainInfo, error) {
	return DomainInfo{}, errNoEnterprise
}

// SetSSOEnforcement implements EnterpriseTools.
func (NoEnterprise) SetSSOEnforcement(context.Context, domain.Actor, SetSSOEnforcementInput) (DomainInfo, error) {
	return DomainInfo{}, errNoEnterprise
}

// SetMaxTokenTTL implements EnterpriseTools.
func (NoEnterprise) SetMaxTokenTTL(context.Context, domain.Actor, SetMaxTokenTTLInput) (domain.Account, error) {
	return domain.Account{}, errNoEnterprise
}

// SetConnectionProvisioning implements EnterpriseTools.
func (NoEnterprise) SetConnectionProvisioning(context.Context, domain.Actor, SetProvisioningInput) (ProvisioningInfo, error) {
	return ProvisioningInfo{}, errNoEnterprise
}

// ListGroupMappings implements EnterpriseTools.
func (NoEnterprise) ListGroupMappings(context.Context, domain.Actor) ([]domain.GroupMapping, error) {
	return nil, errNoEnterprise
}

// AddGroupMapping implements EnterpriseTools.
func (NoEnterprise) AddGroupMapping(context.Context, domain.Actor, AddGroupMappingInput) (domain.GroupMapping, error) {
	return domain.GroupMapping{}, errNoEnterprise
}

// RemoveGroupMapping implements EnterpriseTools.
func (NoEnterprise) RemoveGroupMapping(context.Context, domain.Actor, GroupMappingRefInput) (domain.GroupMapping, error) {
	return domain.GroupMapping{}, errNoEnterprise
}

// ListSCIMTokens implements EnterpriseTools.
func (NoEnterprise) ListSCIMTokens(context.Context, domain.Actor) ([]domain.SCIMToken, error) {
	return nil, errNoEnterprise
}

// CreateSCIMToken implements EnterpriseTools.
func (NoEnterprise) CreateSCIMToken(context.Context, domain.Actor, CreateSCIMTokenInput) (CreateSCIMTokenOutput, error) {
	return CreateSCIMTokenOutput{}, errNoEnterprise
}

// RevokeSCIMToken implements EnterpriseTools.
func (NoEnterprise) RevokeSCIMToken(context.Context, domain.Actor, SCIMTokenRefInput) (domain.SCIMToken, error) {
	return domain.SCIMToken{}, errNoEnterprise
}

// CreateRole implements EnterpriseTools.
func (NoEnterprise) CreateRole(context.Context, domain.Actor, CreateRoleInput) (domain.Role, error) {
	return domain.Role{}, errNoEnterprise
}

// UpdateRole implements EnterpriseTools.
func (NoEnterprise) UpdateRole(context.Context, domain.Actor, UpdateRoleInput) (domain.Role, error) {
	return domain.Role{}, errNoEnterprise
}

// DeleteRole implements EnterpriseTools.
func (NoEnterprise) DeleteRole(context.Context, domain.Actor, RoleRefInput) (domain.Role, error) {
	return domain.Role{}, errNoEnterprise
}

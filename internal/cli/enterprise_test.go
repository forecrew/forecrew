package cli

import (
	"context"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestEnterpriseCommands(t *testing.T) {
	e := newRemoteEnv(t)
	d := mcp.DomainInfo{VerifiedDomain: domain.VerifiedDomain{Domain: "acme.com"}, RecordName: "_forecrew-challenge.acme.com", RecordValue: "forecrew-verification=x"}
	one := func(context.Context, domain.Actor, mcp.DomainInput) (mcp.DomainInfo, error) { return d, nil }
	e.svc.ListDomainsFunc = func(context.Context, domain.Actor, any) ([]mcp.DomainInfo, error) { return []mcp.DomainInfo{d}, nil }
	e.svc.AddDomainFunc, e.svc.VerifyDomainFunc, e.svc.DeleteDomainFunc = one, one, one
	e.svc.SetSSOEnforcementFunc = func(context.Context, domain.Actor, mcp.SetSSOEnforcementInput) (mcp.DomainInfo, error) { return d, nil }
	e.svc.SetMaxTokenTTLFunc = func(context.Context, domain.Actor, mcp.SetMaxTokenTTLInput) (domain.Account, error) {
		return domain.Account{Slug: "acme"}, nil
	}
	e.svc.SetConnectionProvisioningFunc = func(context.Context, domain.Actor, mcp.SetProvisioningInput) (mcp.ProvisioningInfo, error) {
		return mcp.ProvisioningInfo{Name: "okta", JIT: domain.JIT{Enabled: true}, GroupClaim: "groups"}, nil
	}
	m := domain.GroupMapping{ID: domain.NewID(), ExternalGroup: "devs"}
	e.svc.ListGroupMappingsFunc = func(context.Context, domain.Actor, any) ([]domain.GroupMapping, error) {
		return []domain.GroupMapping{m}, nil
	}
	e.svc.AddGroupMappingFunc = func(context.Context, domain.Actor, mcp.AddGroupMappingInput) (domain.GroupMapping, error) {
		return m, nil
	}
	e.svc.RemoveGroupMappingFunc = func(context.Context, domain.Actor, mcp.GroupMappingRefInput) (domain.GroupMapping, error) {
		return m, nil
	}
	tok := domain.SCIMToken{Name: "okta"}
	e.svc.ListSCIMTokensFunc = func(context.Context, domain.Actor, any) ([]domain.SCIMToken, error) {
		return []domain.SCIMToken{tok}, nil
	}
	e.svc.CreateSCIMTokenFunc = func(context.Context, domain.Actor, mcp.CreateSCIMTokenInput) (mcp.CreateSCIMTokenOutput, error) {
		return mcp.CreateSCIMTokenOutput{Token: tok, Secret: "fcscim_x", URL: "https://f/scim/v2/acme"}, nil
	}
	e.svc.RevokeSCIMTokenFunc = func(context.Context, domain.Actor, mcp.SCIMTokenRefInput) (domain.SCIMToken, error) { return tok, nil }
	role := domain.Role{Name: "auditor"}
	e.svc.CreateRoleFunc = func(context.Context, domain.Actor, mcp.CreateRoleInput) (domain.Role, error) { return role, nil }
	e.svc.UpdateRoleFunc = func(context.Context, domain.Actor, mcp.UpdateRoleInput) (domain.Role, error) { return role, nil }
	e.svc.DeleteRoleFunc = func(context.Context, domain.Actor, mcp.RoleRefInput) (domain.Role, error) { return role, nil }
	on, claim, roles, text := true, "groups", []string{"member"}, "reads"
	perms := []domain.Permission{domain.PermAuditRead}
	for _, tt := range []struct {
		args []string
		want string
		tool string
		in   any
	}{
		{[]string{"domains"}, "acme.com", mcp.ToolListDomains, nil},
		{[]string{"domains", "add", "acme.com"}, "_forecrew-challenge.acme.com TXT", mcp.ToolAddDomain, mcp.DomainInput{Domain: "acme.com"}},
		{[]string{"domains", "verify", "acme.com"}, "domain acme.com verified", mcp.ToolVerifyDomain, mcp.DomainInput{Domain: "acme.com"}},
		{[]string{"domains", "delete", "acme.com"}, "domain acme.com removed", mcp.ToolDeleteDomain, mcp.DomainInput{Domain: "acme.com"}},
		{[]string{"domains", "enforce", "acme.com", "okta"}, "now requires okta", mcp.ToolSetSSOEnforcement, mcp.SetSSOEnforcementInput{Domain: "acme.com", Enforce: true, Connection: "okta"}},
		{[]string{"domains", "unenforce", "acme.com"}, "requires no connection", mcp.ToolSetSSOEnforcement, mcp.SetSSOEnforcementInput{Domain: "acme.com"}},
		{[]string{"domains", "token-ttl", "24h"}, "token cap of acme updated", mcp.ToolSetMaxTokenTTL, mcp.SetMaxTokenTTLInput{MaxTokenTTL: "24h"}},
		{[]string{"group-mappings"}, "devs", mcp.ToolListGroupMappings, nil},
		{[]string{"group-mappings", "add", "okta", "devs", "core"}, "group devs mapped", mcp.ToolAddGroupMapping, mcp.AddGroupMappingInput{Connection: "okta", Group: "devs", Team: "core"}},
		{[]string{"group-mappings", "remove", "m"}, "mapping of devs removed", mcp.ToolRemoveGroupMapping, mcp.GroupMappingRefInput{Mapping: "m"}},
		{[]string{"scim-tokens"}, "okta", mcp.ToolListSCIMTokens, nil},
		{[]string{"scim-tokens", "create", "okta"}, "fcscim_x", mcp.ToolCreateSCIMToken, mcp.CreateSCIMTokenInput{Name: "okta"}},
		{[]string{"scim-tokens", "revoke", "okta"}, "SCIM token okta revoked", mcp.ToolRevokeSCIMToken, mcp.SCIMTokenRefInput{Token: "okta"}},
		{[]string{"roles", "create", "auditor", "--scope", "organization", "--description", "reads", "--permission", "audit.read"}, "role auditor created", mcp.ToolCreateRole,
			mcp.CreateRoleInput{Name: "auditor", Scope: domain.RoleScopeOrganization, Description: "reads", Permissions: perms}},
		{[]string{"roles", "delete", "auditor"}, "role auditor deleted", mcp.ToolDeleteRole, mcp.RoleRefInput{Role: "auditor"}},
	} {
		e.check(t, tt.args, tt.want, tt.tool, tt.in)
	}
	// Pointer inputs compare by address, so these look at the output alone.
	if out, _, code := e.run(t, "provisioning", "okta", "--jit", "--default-role", "member", "--group-claim", "groups"); code != 0 || out == "" {
		t.Errorf("provisioning = %d %q", code, out)
	}
	calls := e.svc.Calls()
	in, ok := calls[len(calls)-1].Input.(mcp.SetProvisioningInput)
	if !ok || in.Connection != "okta" || *in.JITEnabled != on || *in.GroupClaim != claim || (*in.DefaultRoles)[0] != roles[0] {
		t.Errorf("provisioning input = %+v", calls[len(calls)-1].Input)
	}
	if _, _, code := e.run(t, "roles", "update", "auditor", "--name", "reader", "--description", "reads", "--permission", "audit.read"); code != 0 {
		t.Errorf("roles update = %d", code)
	}
	calls = e.svc.Calls()
	up, ok := calls[len(calls)-1].Input.(mcp.UpdateRoleInput)
	if !ok || up.Role != "auditor" || up.Name != "reader" || *up.Description != text || (*up.Permissions)[0] != perms[0] {
		t.Errorf("roles update input = %+v", calls[len(calls)-1].Input)
	}
}

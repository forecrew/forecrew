package cli

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// customRoleCommands manage custom roles (enterprise).
func customRoleCommands(r *remote) []*cobra.Command {
	var scope, description string
	var permissions []string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a custom role from the permission catalog",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.CreateRoleInput{Name: args[0], Scope: domain.RoleScope(scope), Description: description, Permissions: asPermissions(permissions)}
			var x domain.Role
			if err := r.call(cmd.Context(), http.MethodPost, "/roles", in, &x); err != nil {
				return err
			}
			cmd.Printf("role %s created\n", x.Name)
			return nil
		},
	}
	create.Flags().StringVar(&scope, "scope", string(domain.RoleScopeProject), "organization or project")
	create.Flags().StringVar(&description, "description", "", "what the role is for")
	create.Flags().StringSliceVar(&permissions, "permission", nil, "a permission of the catalog; repeat or separate with commas")
	var name, newDescription string
	var newPermissions []string
	update := &cobra.Command{
		Use:   "update <role>",
		Short: "Change a custom role; it reaches everyone bound to it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.UpdateRoleInput{Name: name}
			if cmd.Flags().Changed("description") {
				in.Description = &newDescription
			}
			if cmd.Flags().Changed("permission") {
				list := asPermissions(newPermissions)
				in.Permissions = &list
			}
			var x domain.Role
			if err := r.call(cmd.Context(), http.MethodPatch, "/roles/"+url.PathEscape(args[0]), in, &x); err != nil {
				return err
			}
			cmd.Printf("role %s updated\n", x.Name)
			return nil
		},
	}
	update.Flags().StringVar(&name, "name", "", "new name")
	update.Flags().StringVar(&newDescription, "description", "", "new description")
	update.Flags().StringSliceVar(&newPermissions, "permission", nil, "the new permissions; repeat or separate with commas")
	remove := &cobra.Command{
		Use:   "delete <role>",
		Short: "Delete a custom role that nothing uses",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var x domain.Role
			if err := r.call(cmd.Context(), http.MethodDelete, "/roles/"+url.PathEscape(args[0]), nil, &x); err != nil {
				return err
			}
			cmd.Printf("role %s deleted\n", x.Name)
			return nil
		},
	}
	return []*cobra.Command{create, update, remove}
}

func asPermissions(in []string) []domain.Permission {
	out := make([]domain.Permission, len(in))
	for i, p := range in {
		out[i] = domain.Permission(p)
	}
	return out
}

// domainCommand is a subcommand on one domain that answers with it.
func domainCommand(r *remote, use, short, method, suffix, done string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <domain>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d mcp.DomainInfo
			if err := r.call(cmd.Context(), method, "/domains/"+url.PathEscape(args[0])+suffix, nil, &d); err != nil {
				return err
			}
			cmd.Printf("domain %s %s\n", d.Domain, done)
			return nil
		},
	}
}

// domainsCommand lists the email domains; its subcommands manage them and SSO
// enforcement (enterprise).
func domainsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "List the email domains of the organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []mcp.DomainInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/domains", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"DOMAIN", "VERIFIED", "ENFORCED", "TXT RECORD", "VALUE"}}
			for _, d := range items {
				rows = append(rows, []string{d.Domain, strconv.FormatBool(!d.VerifiedAt.IsZero()), strconv.FormatBool(d.EnforceSSO), d.RecordName, d.RecordValue})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add <domain>",
		Short: "Add a domain and show the DNS TXT record that proves it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d mcp.DomainInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/domains", mcp.DomainInput{Domain: args[0]}, &d); err != nil {
				return err
			}
			cmd.Printf("domain %s added; create this DNS record, then run domains verify:\n%s TXT %q\n", d.Domain, d.RecordName, d.RecordValue)
			return nil
		},
	}
	enforce := &cobra.Command{
		Use:   "enforce <domain> <connection>",
		Short: "Require a connection for everyone in a verified domain",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d mcp.DomainInfo
			in := mcp.SetSSOEnforcementInput{Enforce: true, Connection: args[1]}
			if err := r.call(cmd.Context(), http.MethodPut, "/domains/"+url.PathEscape(args[0])+"/enforcement", in, &d); err != nil {
				return err
			}
			cmd.Printf("domain %s now requires %s\n", d.Domain, args[1])
			return nil
		},
	}
	unenforce := &cobra.Command{
		Use:   "unenforce <domain>",
		Short: "Stop requiring a connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d mcp.DomainInfo
			if err := r.call(cmd.Context(), http.MethodPut, "/domains/"+url.PathEscape(args[0])+"/enforcement", mcp.SetSSOEnforcementInput{}, &d); err != nil {
				return err
			}
			cmd.Printf("domain %s requires no connection\n", d.Domain)
			return nil
		},
	}
	tokenTTL := &cobra.Command{
		Use:   "token-ttl <duration>",
		Short: "Set how long the personal tokens of users under SSO enforcement last; \"\" removes the cap",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var a domain.Account
			if err := r.call(cmd.Context(), http.MethodPut, "/organization/max-token-ttl", mcp.SetMaxTokenTTLInput{MaxTokenTTL: args[0]}, &a); err != nil {
				return err
			}
			cmd.Printf("token cap of %s updated\n", a.Slug)
			return nil
		},
	}
	cmd.AddCommand(add, enforce, unenforce, tokenTTL,
		domainCommand(r, "verify", "Look up the TXT record and mark the domain verified", http.MethodPost, "/verify", "verified"),
		domainCommand(r, "delete", "Remove a domain with its SSO enforcement", http.MethodDelete, "", "removed"))
	return cmd
}

// provisioningCommand sets what a connection provisions (enterprise).
func provisioningCommand(r *remote) *cobra.Command {
	var jit bool
	var defaults []string
	var claim string
	cmd := &cobra.Command{
		Use:   "provisioning <connection>",
		Short: "Set the just-in-time membership and the group claim of an organization's connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var in mcp.SetProvisioningInput
			if cmd.Flags().Changed("jit") {
				in.JITEnabled = &jit
			}
			if cmd.Flags().Changed("default-role") {
				in.DefaultRoles = &defaults
			}
			if cmd.Flags().Changed("group-claim") {
				in.GroupClaim = &claim
			}
			var p mcp.ProvisioningInfo
			if err := r.call(cmd.Context(), http.MethodPut, "/connections/"+url.PathEscape(args[0])+"/provisioning", in, &p); err != nil {
				return err
			}
			cmd.Printf("connection %s: jit %t, group claim %q\n", p.Name, p.JIT.Enabled, p.GroupClaim)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jit, "jit", false, "create the membership at a first sign-in")
	cmd.Flags().StringSliceVar(&defaults, "default-role", nil, "an organization role new members get; repeat or separate with commas")
	cmd.Flags().StringVar(&claim, "group-claim", "", "ID token claim with the user's groups")
	return cmd
}

// groupMappingsCommand lists the group mappings; its subcommands manage them
// (enterprise).
func groupMappingsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group-mappings",
		Short: "List the identity provider groups mapped to teams",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.GroupMapping
			if err := r.call(cmd.Context(), http.MethodGet, "/group-mappings", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"ID", "CONNECTION", "GROUP", "TEAM"}}
			for _, m := range items {
				rows = append(rows, []string{m.ID.String(), m.ConnectionID.String(), m.ExternalGroup, m.TeamID.String()})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add <connection> <group> <team>",
		Short: "Map a group to a team",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var m domain.GroupMapping
			in := mcp.AddGroupMappingInput{Connection: args[0], Group: args[1], Team: args[2]}
			if err := r.call(cmd.Context(), http.MethodPost, "/group-mappings", in, &m); err != nil {
				return err
			}
			cmd.Printf("group %s mapped\n", m.ExternalGroup)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove <mapping>",
		Short: "Remove a mapping (by UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var m domain.GroupMapping
			if err := r.call(cmd.Context(), http.MethodDelete, "/group-mappings/"+url.PathEscape(args[0]), nil, &m); err != nil {
				return err
			}
			cmd.Printf("mapping of %s removed\n", m.ExternalGroup)
			return nil
		},
	}
	cmd.AddCommand(add, remove)
	return cmd
}

// scimTokensCommand lists the SCIM tokens; its subcommands manage them
// (enterprise).
func scimTokensCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scim-tokens",
		Short: "List the organization's SCIM tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.SCIMToken
			if err := r.call(cmd.Context(), http.MethodGet, "/scim-tokens", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"NAME", "REVOKED"}}
			for _, t := range items {
				rows = append(rows, []string{t.Name, strconv.FormatBool(!t.RevokedAt.IsZero())})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a SCIM token; its value is shown once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.CreateSCIMTokenOutput
			if err := r.call(cmd.Context(), http.MethodPost, "/scim-tokens", mcp.CreateSCIMTokenInput{Name: args[0]}, &out); err != nil {
				return err
			}
			cmd.Println(strings.Join([]string{"SCIM URL: " + out.URL, "token (shown once): " + out.Secret}, "\n"))
			return nil
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <token>",
		Short: "Revoke a SCIM token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t domain.SCIMToken
			if err := r.call(cmd.Context(), http.MethodPost, "/scim-tokens/"+url.PathEscape(args[0])+"/revoke", nil, &t); err != nil {
				return err
			}
			cmd.Printf("SCIM token %s revoked\n", t.Name)
			return nil
		},
	}
	cmd.AddCommand(create, revoke)
	return cmd
}

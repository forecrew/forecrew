package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// membersCommand lists the members of the organization; its subcommands
// suspend, resume and remove one. A member is named by user UUID, email or
// name.
func membersCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List the members of the organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []mcp.MemberInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/members", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"NAME", "EMAIL", "STATUS", "ROLES", "TEAMS", "USER"}}
			for _, m := range items {
				rows = append(rows, []string{safeText(m.Name), dash(safeText(m.Email)), string(m.Status), dash(memberRoles(m)), dash(strings.Join(m.Teams, ",")), m.UserID.String()})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	act := func(use, short, verb string, run func(cmd *cobra.Command, m mcp.MemberInfo) (mcp.MemberInfo, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <user>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				m, err := r.member(cmd, args[0])
				if err != nil {
					return err
				}
				if m, err = run(cmd, m); err != nil {
					return err
				}
				cmd.Printf("%s %s\n", safeText(m.Name), verb)
				return nil
			},
		}
	}
	set := func(suspend bool) func(*cobra.Command, mcp.MemberInfo) (mcp.MemberInfo, error) {
		return func(cmd *cobra.Command, m mcp.MemberInfo) (mcp.MemberInfo, error) {
			var out mcp.MemberInfo
			path := "/members/" + m.UserID.String() + "/resume"
			if suspend {
				path = "/members/" + m.UserID.String() + "/suspend"
			}
			return out, r.call(cmd.Context(), http.MethodPost, path, nil, &out)
		}
	}
	cmd.AddCommand(
		act("suspend", "Suspend a member; their sessions end and their personal tokens are revoked", "is suspended", set(true)),
		act("resume", "Let a suspended member back in", "is active again", set(false)),
		act("remove", "Remove a member with their teams and roles; their sessions end and their personal tokens are revoked", "was removed",
			func(cmd *cobra.Command, m mcp.MemberInfo) (mcp.MemberInfo, error) {
				var out mcp.MemberInfo
				return out, r.call(cmd.Context(), http.MethodDelete, "/members/"+url.PathEscape(m.UserID.String()), nil, &out)
			}),
	)
	return cmd
}

func memberRoles(m mcp.MemberInfo) string {
	roles := make([]string, len(m.Roles))
	for i, x := range m.Roles {
		roles[i] = x.Role
		if x.Project != "" {
			roles[i] += "@" + x.Project
		}
	}
	return strings.Join(roles, ",")
}

// member finds the member ref names: a user UUID, an email or a name.
func (r *remote) member(cmd *cobra.Command, ref string) (mcp.MemberInfo, error) {
	var items []mcp.MemberInfo
	if err := r.call(cmd.Context(), http.MethodGet, "/members", nil, &items); err != nil {
		return mcp.MemberInfo{}, err
	}
	id, err := uuid.Parse(ref)
	isID := err == nil
	var found []mcp.MemberInfo
	for _, m := range items {
		if (isID && m.UserID == id) || (!isID && (strings.EqualFold(m.Email, ref) || strings.EqualFold(m.Name, ref))) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return mcp.MemberInfo{}, fmt.Errorf("no member %q: give a user UUID, an email or a name (forecrew members)", ref)
	}
	return mcp.MemberInfo{}, fmt.Errorf("%d members are called %q: give the user UUID (forecrew members)", len(found), ref)
}

// orgUpdateCommand renames the organization or changes its slug.
func orgUpdateCommand(r *remote) *cobra.Command {
	var in mcp.UpdateOrganizationInput
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Rename the organization or change its slug",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var a domain.Account
			if err := r.call(cmd.Context(), http.MethodPatch, "/organization", in, &a); err != nil {
				return err
			}
			cmd.Printf("organization %s updated (slug %s)\n", safeText(a.Name), a.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&in.Name, "name", "", "new name")
	cmd.Flags().StringVar(&in.Slug, "slug", "", "new slug; links that carry the old one stop working")
	return cmd
}

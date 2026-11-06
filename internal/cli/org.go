package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// auditCommand shows the audit log, newest first.
func auditCommand(r *remote) *cobra.Command {
	var in mcp.ListAuditInput
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit log of the organization, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for k, v := range map[string]string{"actor": in.Actor, "action": in.Action, "subject_type": in.SubjectType, "subject": in.Subject, "since": in.Since, "until": in.Until} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if in.Limit > 0 {
				q.Set("limit", strconv.Itoa(in.Limit))
			}
			var items []mcp.AuditInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/audit?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"TIME", "ACTOR", "ACTION", "SUBJECT"}}
			for _, e := range items {
				actor := e.Actor
				if e.ActorName != "" {
					actor = e.ActorName
				}
				rows = append(rows, []string{e.CreatedAt.UTC().Format(time.RFC3339), safeText(actor), safeText(e.Action), safeText(e.SubjectType) + " " + e.SubjectID.String()})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&in.Actor, "actor", "", "only this actor, for example user:<id>")
	cmd.Flags().StringVar(&in.Action, "action", "", "only this action")
	cmd.Flags().StringVar(&in.SubjectType, "subject-type", "", "only this kind of subject")
	cmd.Flags().StringVar(&in.Subject, "subject", "", "only this subject (UUID)")
	cmd.Flags().StringVar(&in.Since, "since", "", "from this RFC 3339 time")
	cmd.Flags().StringVar(&in.Until, "until", "", "before this RFC 3339 time")
	cmd.Flags().IntVar(&in.Limit, "limit", 0, "at most this many entries; default 100, at most 500")
	return cmd
}

// connectionsCommand lists the ways to sign in; its subcommands add one and
// turn one on or off.
func connectionsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connections",
		Short: "List the ways to sign in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []mcp.ConnectionInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/connections", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"NAME", "KIND", "ISSUER", "STATE", "TESTED", "SCOPE"}}
			for _, c := range items {
				state, scope := "off", "organization"
				if c.Enabled {
					state = "on"
				}
				if c.Instance {
					scope = "server file"
				}
				rows = append(rows, []string{safeText(c.Name), string(c.Kind), dash(safeText(c.Issuer)), state, map[bool]string{true: "yes", false: "no"}[c.Tested], scope})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	var in mcp.CreateConnectionInput
	var kind string
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a sign-in connection; it starts off, and needs a test sign-in before it can be turned on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name, in.Kind = args[0], domain.ConnectionKind(kind)
			var c mcp.ConnectionInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/connections", in, &c); err != nil {
				return err
			}
			cmd.Printf("connection %s added, off\nAllow this redirect address at the provider: %s\nTest it by signing in at: %s\nThen: forecrew connections enable %s\n", c.Name, c.RedirectURL, c.TestURL, c.Name)
			return nil
		},
	}
	add.Flags().StringVar(&kind, "kind", "oidc", "github, google, microsoft or oidc")
	add.Flags().StringVar(&in.Issuer, "issuer", "", "https URL of the OpenID Connect issuer; none for github and google")
	add.Flags().StringVar(&in.ClientID, "client-id", "", "OAuth client ID registered at the provider")
	add.Flags().StringVar(&in.ClientSecretRef, "secret-ref", "", "where forecrewd reads the client secret: env:FORECREW_CONN_NAME or secret:conn-name; never the secret")
	set := func(use, short, verb string) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <connection>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var c mcp.ConnectionInfo
				if err := r.call(cmd.Context(), http.MethodPost, "/connections/"+url.PathEscape(args[0])+"/"+use, nil, &c); err != nil {
					return err
				}
				cmd.Printf("connection %s is %s\n", safeText(c.Name), verb)
				return nil
			},
		}
	}
	cmd.AddCommand(add, set("enable", "Turn a connection on; it needs a test sign-in that worked", "on"), set("disable", "Turn a connection off", "off"))
	return cmd
}

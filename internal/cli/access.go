package cli

import (
	"cmp"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// rolesCommand lists the roles of the organization.
func rolesCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roles",
		Short: "List the roles of the organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.Role
			if err := r.call(cmd.Context(), http.MethodGet, "/roles", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"NAME", "SCOPE", "KIND", "PERMISSIONS"}}
			for _, x := range items {
				kind := "custom"
				if x.BuiltIn {
					kind = "built-in"
				}
				rows = append(rows, []string{x.Name, string(x.Scope), kind, strconv.Itoa(len(x.Permissions))})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.AddCommand(customRoleCommands(r)...)
	return cmd
}

// teamsCommand lists the teams; its subcommands manage them. A team named
// like a subcommand is reached by UUID.
func teamsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "List the teams of the organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.Team
			if err := r.call(cmd.Context(), http.MethodGet, "/teams", nil, &items); err != nil {
				return err
			}
			printTeams(cmd.OutOrStdout(), items)
			return nil
		},
	}
	var createSlug string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t domain.Team
			if err := r.call(cmd.Context(), http.MethodPost, "/teams", mcp.CreateTeamInput{Name: args[0], Slug: createSlug}, &t); err != nil {
				return err
			}
			cmd.Printf("team %s created\n", t.Slug)
			return nil
		},
	}
	create.Flags().StringVar(&createSlug, "slug", "", "URL name; derived from the name when left out")
	var name, slug string
	update := &cobra.Command{
		Use:   "update <team>",
		Short: "Rename a team or change its slug",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t domain.Team
			if err := r.call(cmd.Context(), http.MethodPatch, "/teams/"+url.PathEscape(args[0]), mcp.UpdateTeamInput{Name: name, Slug: slug}, &t); err != nil {
				return err
			}
			cmd.Printf("team %s updated\n", t.Slug)
			return nil
		},
	}
	update.Flags().StringVar(&name, "name", "", "new name")
	update.Flags().StringVar(&slug, "slug", "", "new slug")
	remove := &cobra.Command{
		Use:   "delete <team>",
		Short: "Delete a team; its members lose the roles they had through it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t domain.Team
			if err := r.call(cmd.Context(), http.MethodDelete, "/teams/"+url.PathEscape(args[0]), nil, &t); err != nil {
				return err
			}
			cmd.Printf("team %s deleted\n", t.Slug)
			return nil
		},
	}
	members := &cobra.Command{
		Use:   "members <team>",
		Short: "List the members of a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var items []domain.TeamMember
			if err := r.call(cmd.Context(), http.MethodGet, "/teams/"+url.PathEscape(args[0])+"/members", nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"USER", "SOURCE", "ADDED"}}
			for _, m := range items {
				rows = append(rows, []string{m.UserID.String(), string(m.Source), m.CreatedAt.UTC().Format(time.RFC3339)})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add-member <team> <user>",
		Short: "Add a member of the organization (by user UUID) to a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var m domain.TeamMember
			if err := r.call(cmd.Context(), http.MethodPost, "/teams/"+url.PathEscape(args[0])+"/members", map[string]string{"user": args[1]}, &m); err != nil {
				return err
			}
			cmd.Printf("%s is in team %s\n", m.UserID, args[0])
			return nil
		},
	}
	drop := &cobra.Command{
		Use:   "remove-member <team> <user>",
		Short: "Remove a member (by user UUID) from a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var m domain.TeamMember
			if err := r.call(cmd.Context(), http.MethodDelete, "/teams/"+url.PathEscape(args[0])+"/members/"+url.PathEscape(args[1]), nil, &m); err != nil {
				return err
			}
			cmd.Printf("%s left team %s\n", m.UserID, args[0])
			return nil
		},
	}
	cmd.AddCommand(create, update, remove, members, add, drop)
	return cmd
}

// bindingsCommand lists role bindings; its subcommands give and remove roles.
func bindingsCommand(r *remote) *cobra.Command {
	var project, kind, subject string
	cmd := &cobra.Command{
		Use:   "bindings",
		Short: "List who holds which role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for k, v := range map[string]string{"project": project, "subject_kind": kind, "subject": subject} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var items []mcp.RoleBindingInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/role-bindings?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			printBindings(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "only the bindings on this project")
	cmd.Flags().StringVar(&kind, "subject-kind", "", "user, team or service_account")
	cmd.Flags().StringVar(&subject, "subject", "", "only this subject; needs --subject-kind")

	var on string
	add := &cobra.Command{
		Use:   "add <user|team|service_account> <subject> <role>",
		Short: "Give a role, on the organization or with --project on one project",
		Long:  "Give a role to a user (UUID), a team (slug or UUID) or a service account (name or UUID). You need every permission of the role at that scope.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectKind(args[0]), Subject: args[1], Role: args[2], Project: on}
			var b mcp.RoleBindingInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/role-bindings", in, &b); err != nil {
				return err
			}
			cmd.Printf("binding %s: %s on %s\n", b.ID, b.Role, cmp.Or(b.Project, "the organization"))
			return nil
		},
	}
	add.Flags().StringVar(&on, "project", "", "bind on this project")
	remove := &cobra.Command{
		Use:   "remove <binding>",
		Short: "Remove a role binding by UUID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var b mcp.RoleBindingInfo
			if err := r.call(cmd.Context(), http.MethodDelete, "/role-bindings/"+url.PathEscape(args[0]), nil, &b); err != nil {
				return err
			}
			cmd.Printf("binding %s removed\n", b.ID)
			return nil
		},
	}
	cmd.AddCommand(add, remove)
	return cmd
}

// serviceAccountsCommand lists the service accounts; its subcommands manage
// them.
func serviceAccountsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service-accounts",
		Short: "List the service accounts of the organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.ServiceAccount
			if err := r.call(cmd.Context(), http.MethodGet, "/service-accounts", nil, &items); err != nil {
				return err
			}
			printServiceAccounts(cmd.OutOrStdout(), items)
			return nil
		},
	}
	var description string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a service account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var a domain.ServiceAccount
			in := mcp.CreateServiceAccountInput{Name: args[0], Description: description}
			if err := r.call(cmd.Context(), http.MethodPost, "/service-accounts", in, &a); err != nil {
				return err
			}
			cmd.Printf("service account %s created\n", a.Name)
			return nil
		},
	}
	create.Flags().StringVar(&description, "description", "", "what it is for")
	var name, newDescription string
	update := &cobra.Command{
		Use:   "update <service-account>",
		Short: "Rename a service account or change its description",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.UpdateServiceAccountInput{Name: name}
			if cmd.Flags().Changed("description") {
				in.Description = &newDescription
			}
			var a domain.ServiceAccount
			if err := r.call(cmd.Context(), http.MethodPatch, "/service-accounts/"+url.PathEscape(args[0]), in, &a); err != nil {
				return err
			}
			cmd.Printf("service account %s updated\n", a.Name)
			return nil
		},
	}
	update.Flags().StringVar(&name, "name", "", "new name")
	update.Flags().StringVar(&newDescription, "description", "", "new description")
	set := func(verb, short string) *cobra.Command {
		return &cobra.Command{
			Use:   verb + " <service-account>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var a domain.ServiceAccount
				if err := r.call(cmd.Context(), http.MethodPost, "/service-accounts/"+url.PathEscape(args[0])+"/"+verb, struct{}{}, &a); err != nil {
					return err
				}
				cmd.Printf("service account %s: %sd\n", a.Name, verb)
				return nil
			},
		}
	}
	cmd.AddCommand(create, update, set("disable", "Disable a service account"), set("enable", "Enable a service account"))
	return cmd
}

func printTeams(w io.Writer, items []domain.Team) {
	rows := [][]string{{"SLUG", "NAME", "SOURCE", "ID"}}
	for _, t := range items {
		rows = append(rows, []string{t.Slug, safeText(t.Name), string(t.Source), t.ID.String()})
	}
	table(w, rows)
}

func printBindings(w io.Writer, items []mcp.RoleBindingInfo) {
	rows := [][]string{{"ID", "SUBJECT", "ROLE", "ON"}}
	for _, b := range items {
		rows = append(rows, []string{b.ID.String(), string(b.SubjectKind) + ":" + b.SubjectID.String(), b.Role, cmp.Or(b.Project, "organization")})
	}
	table(w, rows)
}

func printServiceAccounts(w io.Writer, items []domain.ServiceAccount) {
	rows := [][]string{{"NAME", "STATUS", "ID", "DESCRIPTION"}}
	for _, a := range items {
		status := "enabled"
		if !a.DisabledAt.IsZero() {
			status = "disabled"
		}
		rows = append(rows, []string{a.Name, status, a.ID.String(), dash(safeText(a.Description))})
	}
	table(w, rows)
}

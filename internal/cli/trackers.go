package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// withConnection adds ?connection= to path when one is given.
func withConnection(path, connection string) string {
	if connection == "" {
		return path
	}
	return path + "?" + url.Values{"connection": {connection}}.Encode()
}

func trackerCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{Use: "tracker", Short: "Manage tracker connections, routes and people"}
	cmd.AddCommand(
		listCommand(r, "connections", "List the tracker connections", "/tracker-connections", printTrackerConnections),
		connectTrackerCommand(r), disconnectTrackerCommand(r), discardTrackerWritesCommand(r), testTrackerCommand(r),
		trackerRoutesCommand(r), addRouteCommand(r), deleteRouteCommand(r),
		trackerPeopleCommand(r), mapPersonCommand(r), unmapPersonCommand(r),
	)
	return cmd
}

// jiraFlags are the settings of a Jira connection (docs/trackers/jira.md) and
// the ones Linear shares with it (docs/trackers/linear.md).
type jiraFlags struct {
	baseURL, auth, email, tokenRef, caFile, hookRef, signing string
	projects, teams                                          []string
}

func (j jiraFlags) add(cmd *cobra.Command) *jiraFlags {
	f := cmd.Flags()
	f.StringVar(&j.baseURL, "base-url", "", "jira: the site, with its context path")
	f.StringSliceVar(&j.projects, "jira-project", nil, "jira: a project key (repeatable)")
	f.StringSliceVar(&j.teams, "team", nil, "linear: a team key (repeatable)")
	f.StringVar(&j.auth, "auth", "", "jira: pat (Data Center), api_token or scoped_token (Cloud); linear: api_key (default) or oauth_token")
	f.StringVar(&j.email, "email", "", "jira: the Atlassian account of a Cloud token")
	f.StringVar(&j.tokenRef, "token-ref", "", "jira, linear: store:<name> of the token")
	f.StringVar(&j.caFile, "ca-file", "", "jira: PEM certificates of a private CA to trust")
	f.StringVar(&j.hookRef, "webhook-secret-ref", "", "jira, linear: store:<name> of the webhook secret; without it, polling only")
	f.StringVar(&j.signing, "webhook-signing", "", "jira: hmac (default) or url")
	return &j
}

// config sets the Jira settings that were given.
func (j *jiraFlags) config(cfg map[string]any) error {
	for k, v := range map[string]string{"base_url": j.baseURL, "auth": j.auth, "email": j.email, "token_ref": j.tokenRef,
		"webhook_secret_ref": j.hookRef, "webhook_signing": j.signing} {
		if v != "" {
			cfg[k] = v
		}
	}
	if len(j.projects) > 0 {
		cfg["projects"] = j.projects
	}
	if len(j.teams) > 0 {
		cfg["teams"] = j.teams
	}
	if j.caFile != "" {
		pem, err := os.ReadFile(j.caFile)
		if err != nil {
			return fmt.Errorf("read the CA file: %w", err)
		}
		cfg["ca_bundle"] = string(pem)
	}
	return nil
}

func connectTrackerCommand(r *remote) *cobra.Command {
	var kind string
	var in mcp.CreateTrackerConnectionInput
	var repos []string
	var bot, owner, field string
	var project int
	var closed []string
	var jira *jiraFlags
	cmd := &cobra.Command{
		Use:   "connect <name>",
		Short: "Connect a tracker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name, in.Kind, in.Config = args[0], kind, map[string]any{}
			if len(repos) > 0 {
				in.Config["repos"] = repos
			}
			if bot != "" {
				in.Config["bot_login"] = bot
			}
			if owner != "" {
				in.Config["owner"] = owner
			}
			if project != 0 {
				in.Config["project"] = project
			}
			if field != "" {
				in.Config["field"] = field
			}
			if len(closed) > 0 {
				in.Config["closed"] = closed
			}
			if err := jira.config(in.Config); err != nil {
				return err
			}
			var c domain.TrackerConnection
			if err := r.call(cmd.Context(), http.MethodPost, "/tracker-connections", in, &c); err != nil {
				return err
			}
			printTrackerConnections(cmd.OutOrStdout(), []domain.TrackerConnection{c})
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "kind", "github", "the tracker's adapter: github, github_project, jira or linear")
	f.StringSliceVar(&repos, "repo", nil, "github: a repo of a project (repeatable)")
	f.StringVar(&bot, "bot-login", "", "github: the App's user, whose events are Forecrew's own")
	f.StringVar(&owner, "owner", "", "github_project: the organization that owns the board")
	f.IntVar(&project, "project", 0, "github_project: the board's number")
	f.StringVar(&field, "field", "", "github_project: the single-select field of the columns (default Status)")
	f.StringSliceVar(&closed, "closed", nil, "github_project: a column that counts as finished (default Done; repeatable)")
	f.StringVar(&in.Triggers.Assignee, "assignee", "", "hand over by assigning to this tracker user")
	f.StringVar(&in.Triggers.Label, "label", "", "hand over by adding this label")
	f.StringVar(&in.Triggers.State, "state", "", "hand over by moving to this state or column")
	jira = jiraFlags{}.add(cmd)
	return cmd
}

func testTrackerCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "test <connection>",
		Short: "Test a tracker connection: address, certificate, login, projects and transitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.TrackerConnectionTest
			if err := r.call(cmd.Context(), http.MethodPost, "/tracker-connections/"+url.PathEscape(args[0])+"/test", nil, &out); err != nil {
				return err
			}
			rows := [][]string{{"CHECK", "RESULT", "DETAIL"}}
			for _, c := range out.Checks {
				result := "ok"
				if !c.OK {
					result = "FAILED"
				}
				rows = append(rows, []string{c.Name, result, dash(safeText(c.Detail))}) // the tracker's words
			}
			table(cmd.OutOrStdout(), rows)
			if !out.OK {
				return fmt.Errorf("connection %s failed its test", out.Connection)
			}
			return nil
		},
	}
}

func disconnectTrackerCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect <connection>",
		Short: "Delete a tracker connection with its routes and people",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out domain.TrackerConnection
			if err := r.call(cmd.Context(), http.MethodDelete, "/tracker-connections/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "connection deleted")
			return nil
		},
	}
}

func discardTrackerWritesCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "discard-writes <connection>",
		Short: "Drop the unsent writes of tickets that are over, so the connection can be deleted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.DiscardedWrites
			if err := r.call(cmd.Context(), http.MethodPost, "/tracker-connections/"+url.PathEscape(args[0])+"/discard-writes", nil, &out); err != nil {
				return err
			}
			for _, w := range out.Writes {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d attempts\t%s\n", w.Key, w.Kind, w.Attempts, w.LastError)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d writes discarded\n", len(out.Writes))
			return nil
		},
	}
}

func trackerRoutesCommand(r *remote) *cobra.Command {
	var connection string
	cmd := &cobra.Command{
		Use:   "routes",
		Short: "List the routes in the order they are tried",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []mcp.TrackerRouteInfo
			if err := r.call(cmd.Context(), http.MethodGet, withConnection("/tracker-routes", connection), nil, &items); err != nil {
				return err
			}
			printTrackerRoutes(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&connection, "connection", "", "only this connection")
	return cmd
}

// parseStates reads logical=tracker pairs.
func parseStates(pairs []string) (map[domain.TicketState]string, error) {
	out := map[domain.TicketState]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("--state-map %q is not logical=state", p)
		}
		out[domain.TicketState(k)] = v
	}
	return out, nil
}

func addRouteCommand(r *remote) *cobra.Command {
	var in mcp.CreateTrackerRouteInput
	var kind string
	var states []string
	cmd := &cobra.Command{
		Use:   "route-add <connection> <project>",
		Short: "Send the tickets a route matches to a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if in.States, err = parseStates(states); err != nil {
				return err
			}
			in.Connection, in.Project, in.GoalKind = args[0], args[1], domain.GoalKind(kind)
			var out mcp.TrackerRouteInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/tracker-routes", in, &out); err != nil {
				return err
			}
			printTrackerRoutes(cmd.OutOrStdout(), []mcp.TrackerRouteInfo{out})
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Match.Project, "match-project", "", "the tracker's project")
	f.StringSliceVar(&in.Match.Types, "match-type", nil, "ticket types (repeatable)")
	f.StringSliceVar(&in.Match.Labels, "match-label", nil, "ticket labels (repeatable)")
	f.StringSliceVar(&in.Match.Components, "match-component", nil, "ticket components (repeatable)")
	f.StringVar(&kind, "goal-kind", "", "feature or bug; default by the ticket's type")
	f.StringVar(&in.Playbook, "playbook", "", "goal, goal-v2 or bugfix; default by the goal's kind and the project")
	f.IntVar(&in.Position, "position", 0, "where it goes in the order; default last")
	f.StringSliceVar(&states, "state-map", nil, "logical=state, for example done=closed (repeatable)")
	return cmd
}

func deleteRouteCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "route-rm <route>",
		Short: "Delete a route by UUID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out domain.TrackerRoute
			if err := r.call(cmd.Context(), http.MethodDelete, "/tracker-routes/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "route deleted")
			return nil
		},
	}
}

func trackerPeopleCommand(r *remote) *cobra.Command {
	var connection string
	cmd := &cobra.Command{
		Use:   "people",
		Short: "List who is who on the trackers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []domain.TrackerIdentity
			if err := r.call(cmd.Context(), http.MethodGet, withConnection("/tracker-identities", connection), nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"ID", "EXTERNAL ID", "NAME", "USER"}}
			for _, i := range items {
				rows = append(rows, []string{i.ID.String(), i.ExternalID, dash(i.ExternalName), i.UserID.String()})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&connection, "connection", "", "only this connection")
	return cmd
}

func mapPersonCommand(r *remote) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "map <connection> <external-id> <user>",
		Short: "Map a person on a tracker to a Forecrew user",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.MapTrackerIdentityInput{Connection: args[0], ExternalID: args[1], ExternalName: name, User: args[2]}
			var i domain.TrackerIdentity
			if err := r.call(cmd.Context(), http.MethodPost, "/tracker-identities", in, &i); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s mapped: %s\n", i.ExternalID, i.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name for people to read")
	return cmd
}

func unmapPersonCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "unmap <identity>",
		Short: "Remove a mapping by UUID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out domain.TrackerIdentity
			if err := r.call(cmd.Context(), http.MethodDelete, "/tracker-identities/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "mapping removed")
			return nil
		},
	}
}

func ticketsCommand(r *remote) *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "tickets",
		Short: "List the tickets waiting in the Inbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/tickets/waiting"
			if project != "" {
				path += "?" + url.Values{"project": {project}}.Encode()
			}
			var items []mcp.TicketInfo
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"ID", "CONNECTION", "KEY", "PROJECT", "WHY", "GATE", "HANDED BY"}}
			for _, t := range items {
				rows = append(rows, []string{t.ID.String(), t.Connection, t.Key, dash(t.Project), string(t.Reason), dash(t.GateRef), dash(t.HandedBy)})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "only this project")
	var into string
	accept := &cobra.Command{
		Use:   "accept <ticket>",
		Short: "Accept a ticket: its goal starts as you",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t mcp.TicketInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/tickets/"+url.PathEscape(args[0])+"/accept", mcp.AcceptTicketInput{Project: into}, &t); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is %s %s\n", t.Key, t.Status, t.GoalRef)
			return nil
		},
	}
	accept.Flags().StringVar(&into, "project", "", "the project, when no route gave one")
	var reason string
	dismiss := &cobra.Command{
		Use:   "dismiss <ticket>",
		Short: "Turn down a ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t mcp.TicketInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/tickets/"+url.PathEscape(args[0])+"/dismiss", mcp.DismissTicketInput{Reason: reason}, &t); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s dismissed\n", t.Key)
			return nil
		},
	}
	dismiss.Flags().StringVar(&reason, "reason", "", "why, for the record")
	cmd.AddCommand(accept, dismiss)
	return cmd
}

func printTrackerConnections(w io.Writer, items []domain.TrackerConnection) {
	rows := [][]string{{"NAME", "KIND", "ASSIGNEE", "LABEL", "STATE"}}
	for _, c := range items {
		rows = append(rows, []string{c.Name, c.Kind, dash(c.Triggers.Assignee), dash(c.Triggers.Label), dash(c.Triggers.State)})
	}
	table(w, rows)
}

func printTrackerRoutes(w io.Writer, items []mcp.TrackerRouteInfo) {
	rows := [][]string{{"ID", "CONNECTION", "#", "PROJECT", "MATCH PROJECT", "TYPES", "LABELS", "KIND", "WARNING"}}
	for _, r := range items {
		rows = append(rows, []string{r.ID.String(), r.Connection, strconv.Itoa(r.Position), r.Project, dash(r.Match.Project),
			dash(strings.Join(r.Match.Types, ",")), dash(strings.Join(r.Match.Labels, ",")), dash(string(r.GoalKind)), dash(safeText(r.Warning))})
	}
	table(w, rows)
}

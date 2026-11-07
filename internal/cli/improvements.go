package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/mcp"
)

// improvementsCommand lists a project's improvement proposals; its subcommand
// decides one. A project named decide cannot be listed by name; use its repo.
func improvementsCommand(r *remote) *cobra.Command {
	var status, category string
	cmd := &cobra.Command{
		Use:   "proposals <project>",
		Short: "List the improvements Forecrew proposes for a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}}
			for k, v := range map[string]string{"status": status, "category": category} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var items []mcp.ProposalInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/proposals?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			printImprovements(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "proposed, accepted, rejected, snoozed, done or superseded")
	cmd.Flags().StringVar(&category, "category", "", "observability, testing, ci, security, docs, knowledge, tooling, workers, roles or cost")

	var reason, until string
	decide := &cobra.Command{
		Use:   "decide <proposal> <accept|reject|snooze|done>",
		Short: "Decide an improvement proposal (PROP-3 or UUID)",
		Long:  "Accept a proposal to start its action, reject it with --reason, or snooze it until a date with --until (RFC 3339). done marks the setup step of an accepted proposal as carried out.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ProposalInfo
			body := map[string]string{"decision": args[1], "reason": reason, "until": until}
			if err := r.call(cmd.Context(), http.MethodPost, "/proposals/"+url.PathEscape(args[0])+"/decide", body, &p); err != nil {
				return err
			}
			cmd.Printf("%s is %s\n", p.Ref, p.Status)
			return nil
		},
	}
	decide.Flags().StringVar(&reason, "reason", "", "why; required to reject")
	decide.Flags().StringVar(&until, "until", "", "RFC 3339 time; required to snooze")
	cmd.AddCommand(decide)
	return cmd
}

func printImprovements(w io.Writer, items []mcp.ProposalInfo) {
	rows := [][]string{{"PROPOSAL", "STATUS", "CATEGORY", "EFFORT", "RISK", "OUTCOME", "TITLE"}}
	for _, p := range items {
		outcome := "-"
		if p.Outcome != nil {
			outcome = fmt.Sprintf("%g -> %g", p.Outcome.Before, p.Outcome.After)
		}
		rows = append(rows, []string{p.Ref, string(p.Status), string(p.Category), string(p.Effort), string(p.Risk), outcome, strings.Join(strings.Fields(p.Title), " ")})
	}
	table(w, rows)
}

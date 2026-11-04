package cli

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/mcp"
)

// assumptionsCommand lists a project's assumptions; its subcommand resolves
// one. A project named resolve cannot be listed by name; use its repo.
func assumptionsCommand(r *remote) *cobra.Command {
	var goal, task, status, impact, verifyBy string
	cmd := &cobra.Command{
		Use:   "assumptions <project>",
		Short: "List the assumptions of a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}}
			for k, v := range map[string]string{"goal": goal, "task": task, "status": status, "impact": impact, "verify_by": verifyBy} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var items []mcp.AssumptionInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/assumptions?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			printAssumptions(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "only the assumptions of a goal (GOAL-3)")
	cmd.Flags().StringVar(&task, "task", "", "only what a task rests on (TASK-12)")
	cmd.Flags().StringVar(&status, "status", "", "unverified, verified, invalidated or accepted")
	cmd.Flags().StringVar(&impact, "impact", "", "low, medium or high")
	cmd.Flags().StringVar(&verifyBy, "verify-by", "", "test, ask-human or research")

	var evidence string
	resolve := &cobra.Command{
		Use:   "resolve <assumption> <verified|invalidated|accepted>",
		Short: "Verify, invalidate or accept an assumption (A-42 or UUID)",
		Long:  "Verify, invalidate or accept an assumption. Verified and invalidated need --evidence. Invalidating flags what depends on it: unmerged tasks go back with the evidence as feedback, merged work gets a draft goal to re-evaluate it.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var a mcp.AssumptionInfo
			body := map[string]string{"status": args[1], "evidence": evidence}
			if err := r.call(cmd.Context(), http.MethodPost, "/assumptions/"+url.PathEscape(args[0])+"/resolve", body, &a); err != nil {
				return err
			}
			cmd.Printf("%s is %s\n", a.Ref, a.Status)
			return nil
		},
	}
	resolve.Flags().StringVar(&evidence, "evidence", "", "how you checked it; required for verified and invalidated")
	cmd.AddCommand(resolve)
	return cmd
}

// decisionsCommand lists a project's decisions.
func decisionsCommand(r *remote) *cobra.Command {
	var goal, council string
	cmd := &cobra.Command{
		Use:   "decisions <project>",
		Short: "List the decisions recorded on a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}}
			if goal != "" {
				q.Set("goal", goal)
			}
			if council != "" {
				q.Set("council", council)
			}
			var items []mcp.DecisionInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/decisions?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			printDecisions(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "only the decisions of a goal (GOAL-3)")
	cmd.Flags().StringVar(&council, "council", "", "only the decisions of a council (its UUID)")
	return cmd
}

func printAssumptions(w io.Writer, items []mcp.AssumptionInfo) {
	rows := [][]string{{"ASSUMPTION", "STATUS", "IMPACT", "CONFIDENCE", "VERIFY BY", "BY", "SOURCE", "STATEMENT"}}
	for _, a := range items {
		rows = append(rows, []string{a.Ref, string(a.Status), string(a.Impact), string(a.Confidence), string(a.VerifyBy), a.MadeBy, dash(a.Source), a.Statement})
	}
	table(w, rows)
}

func printDecisions(w io.Writer, items []mcp.DecisionInfo) {
	rows := [][]string{{"DECISION", "BY", "CHOICE", "QUESTION", "ASSUMPTIONS"}}
	for _, d := range items {
		rows = append(rows, []string{d.Ref, d.DecidedBy, d.Choice, strings.Join(strings.Fields(d.Question), " "), dash(strings.Join(d.Assumptions, ","))})
	}
	table(w, rows)
}

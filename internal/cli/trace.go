package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/mcp"
)

// traceCommand shows why something exists.
func traceCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "trace <ref>",
		Short: "Show why something exists: goal, councils, specs, decisions, tasks, changes and merge (GOAL-3, TASK-5, A-4, owner/app#12, ...)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t mcp.Trace
			if err := r.call(cmd.Context(), http.MethodGet, "/trace?ref="+url.QueryEscape(args[0]), nil, &t); err != nil {
				return err
			}
			printTrace(cmd.OutOrStdout(), t)
			return nil
		},
	}
}

// printTrace lists the nodes in order and then what follows from what.
// Titles come from agents, so they go through safeText.
func printTrace(w io.Writer, t mcp.Trace) {
	rows := [][]string{{"ID", "KIND", "STATUS", "TITLE"}}
	for _, n := range t.Nodes {
		rows = append(rows, []string{n.ID, n.Kind, dash(n.Status), safeText(n.Title)})
	}
	table(w, rows)
	for _, e := range t.Edges {
		fmt.Fprintf(w, "%s -> %s (%s)\n", e.From, e.To, e.Label)
	}
}

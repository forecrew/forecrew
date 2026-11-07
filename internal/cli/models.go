package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func modelsCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "Check model keys, test models and get suggestions"}
	cmd.AddCommand(modelsCheckCommand(r), modelsTestCommand(r), modelsResultsCommand(r), modelsSuggestCommand(r), modelsCatalogCommand(r))
	return cmd
}

func modelsCheckCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "check <connection>",
		Short: "Check the key of a model connection from a control worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.ModelCheckView
			if err := r.call(cmd.Context(), http.MethodPost, "/model-connections/"+url.PathEscape(args[0])+"/check", nil, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Key check of %s started as job %s. See forecrew models results.\n", out.Connection, out.JobID)
			return nil
		},
	}
}

// modelsTestCommand shows what a test costs and runs it once the person
// says yes, as config apply does.
func modelsTestCommand(r *remote) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "test <connection> <model>",
		Short: "Test what a model can do on a connection, after showing its estimated cost",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/model-connections/" + url.PathEscape(args[0]) + "/tests"
			var plan mcp.ModelsTestOutput
			if err := r.call(cmd.Context(), http.MethodPost, path, mcp.ModelsTestInput{Model: args[1]}, &plan); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			printEstimate(w, plan)
			if !yes {
				answer, err := confirmLine(cmd, "Run this test? Type yes to confirm:")
				if err != nil {
					return err
				}
				if answer != "yes" {
					fmt.Fprintln(w, "Not run.")
					return nil
				}
			}
			var out mcp.ModelsTestOutput
			if err := r.call(cmd.Context(), http.MethodPost, path, mcp.ModelsTestInput{Model: args[1], Run: true}, &out); err != nil {
				return err
			}
			fmt.Fprintf(w, "Test started as job %s. See forecrew models results.\n", out.Test.JobID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "run without asking")
	return cmd
}

func printEstimate(w io.Writer, p mcp.ModelsTestOutput) {
	dollars := func(v *float64) string {
		if v == nil {
			return "unknown"
		}
		return fmt.Sprintf("about $%.4f", *v)
	}
	fmt.Fprintf(w, "Test of %s on %s: at most %d turns and %ds.\n", p.Model, p.Connection, p.MaxTurns, p.TimeoutSeconds)
	fmt.Fprintf(w, "Typical cost: %s (%s).\n", dollars(p.Estimate.CostUSD), p.Estimate.Basis)
	fmt.Fprintf(w, "Worst-case cost: %s (%s).\n", dollars(p.Estimate.WorstCostUSD), p.Estimate.WorstBasis)
}

func modelsResultsCommand(r *remote) *cobra.Command {
	var connection string
	cmd := &cobra.Command{
		Use:   "results",
		Short: "Show the key checks and model tests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out []mcp.ModelCheckView
			if err := r.call(cmd.Context(), http.MethodGet, withConnection("/model-checks", connection), nil, &out); err != nil {
				return err
			}
			rows := [][]string{{"CONNECTION", "MODEL", "STATUS", "LEVEL", "DETAIL"}}
			for _, c := range out {
				level, detail := "", ""
				switch {
				case c.Key != nil:
					detail = c.Key.Detail
					if len(c.Key.Models) > 0 {
						detail = fmt.Sprintf("%d models", len(c.Key.Models))
					}
				case c.Test != nil:
					level, detail = c.Test.Level, abilities(c.Test)
				}
				if c.Stale {
					detail += " (agent changed, testing again)"
				}
				rows = append(rows, []string{c.Connection, dash(c.Model), c.Status, dash(level), dash(safeText(detail))})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&connection, "connection", "", "only this connection")
	return cmd
}

func modelsCatalogCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "List the providers and models of the model catalog",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out mcp.ModelsCatalog
			if err := r.call(cmd.Context(), http.MethodGet, "/models/catalog", nil, &out); err != nil {
				return err
			}
			rows := [][]string{{"PROVIDER", "TITLE", "AGENT", "WHERE", "KEY"}}
			for _, p := range out.Providers {
				rows = append(rows, []string{p.Name, p.Title, p.Adapter, p.Where(), p.KeyEnv})
			}
			table(cmd.OutOrStdout(), rows)
			fmt.Fprintln(cmd.OutOrStdout())
			rows = [][]string{{"MODEL", "MAKER", "TIER", "CONTEXT", "PRICE IN/OUT"}}
			for _, m := range out.Models {
				rows = append(rows, []string{m.ID, m.Maker, m.Tier, fmt.Sprint(m.Context), fmt.Sprintf("$%g / $%g", m.Price.Input, m.Price.Output)})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
}

// abilities lists what a test found, failed abilities in capitals.
func abilities(t *domain.SuitabilityResult) string {
	out := make([]string, 0, len(domain.Abilities()))
	for _, a := range domain.Abilities() {
		if !t.Abilities[a] {
			a = strings.ToUpper(a)
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

func modelsSuggestCommand(r *remote) *cobra.Command {
	var preset string
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "Suggest tested models for the roles, as a change plan you confirm",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out mcp.ModelsSuggestion
			if err := r.call(cmd.Context(), http.MethodPost, "/models/suggest", mcp.ModelsSuggestInput{Preset: preset, Source: domain.ConfigFromCLI}, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, n := range out.Notes {
				fmt.Fprintln(w, "Note: "+n)
			}
			if out.Plan == nil {
				fmt.Fprintln(w, "Nothing to change.")
				return nil
			}
			printPlan(w, *out.Plan)
			fmt.Fprintf(w, "Confirm it with: forecrew config apply %s\n", out.Plan.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&preset, "preset", "balanced", "balanced, low_cost or local_only")
	return cmd
}

package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
)

func machinesCommand(r *remote) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "machines",
		Short: "List the machines that exist, or those of one state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/machines"
			if state != "" {
				path += "?" + url.Values{"state": {state}}.Encode()
			}
			var items []mcp.MachineInfo
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &items); err != nil {
				return err
			}
			printMachines(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "only machines in this state (requested, booting, ready, busy, draining, destroyed, failed)")
	return cmd
}

func fleetCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "fleet",
		Short: "Show whether jobs have somewhere to run, and why any wait",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var o fleet.Overview
			if err := r.call(cmd.Context(), http.MethodGet, "/fleet", nil, &o); err != nil {
				return err
			}
			printOverview(cmd.OutOrStdout(), o)
			return nil
		},
	}
}

func printMachines(w io.Writer, items []mcp.MachineInfo) {
	rows := [][]string{{"NAME", "STATE", "LOCATION", "SHAPE", "JOB", "ERROR"}}
	for _, m := range items {
		rows = append(rows, []string{m.Name, string(m.State), m.Provider, m.Shape, dash(m.JobRef), dash(m.Error)})
	}
	table(w, rows)
}

func printShapes(w io.Writer, items []mcp.ShapeInfo) {
	rows := [][]string{{"NAME", "LOCATION", "OS", "SIZE", "POOL", "PRICE/MIN", "WARM", "MAX", "ENABLED"}}
	for _, s := range items {
		size := fmt.Sprintf("%d CPU, %d MB", s.CPU, s.MemoryMB)
		rows = append(rows, []string{s.Name, s.Provider, string(s.OS) + "/" + string(s.Arch), size, s.Pool,
			strconv.FormatFloat(s.PricePerMinute, 'f', -1, 64), strconv.Itoa(s.WarmMin), strconv.Itoa(s.Max), strconv.FormatBool(s.Enabled)})
	}
	table(w, rows)
}

func printProviders(w io.Writer, items []mcp.ProviderInfo) {
	rows := [][]string{{"NAME", "KIND", "MODE", "STATUS", "ONLINE", "OWNERSHIP"}}
	for _, p := range items {
		rows = append(rows, []string{p.Name, string(p.Kind), string(p.Mode), string(p.Status), strconv.FormatBool(p.Online), string(p.Ownership)})
	}
	table(w, rows)
}

func printOverview(w io.Writer, o fleet.Overview) {
	fmt.Fprintln(w, o.Headline)
	if o.Spend != nil {
		fmt.Fprintf(w, "Spend this month: %s %.2f\n", o.Spend.Currency, o.Spend.Amount)
	}
	if len(o.Locations) > 0 {
		rows := [][]string{{"LOCATION", "RUNS", "STATUS", "IN USE", "COST"}}
		for _, l := range o.Locations {
			use := strconv.Itoa(l.InUse)
			if l.Capacity > 0 {
				use += " of " + strconv.Itoa(l.Capacity)
			}
			rows = append(rows, []string{l.Name, dash(l.Runs), l.Status, use, l.Cost})
		}
		fmt.Fprintln(w)
		table(w, rows)
	}
	if len(o.Queue) > 0 {
		rows := [][]string{{"WAITING", "ROLE", "WHY"}}
		for _, q := range o.Queue {
			rows = append(rows, []string{q.Ref, dash(q.Role), q.Sentence})
		}
		fmt.Fprintln(w)
		table(w, rows)
	}
	if len(o.Running) > 0 {
		rows := [][]string{{"RUNNING", "ROLE", "MACHINE", "LOCATION", "MINUTES"}}
		for _, x := range o.Running {
			rows = append(rows, []string{x.Ref, dash(x.Role), x.Machine, x.Location, strconv.Itoa(x.Minutes)})
		}
		fmt.Fprintln(w)
		table(w, rows)
	}
	fmt.Fprintln(w, "\n"+strings.ReplaceAll(string(o.Routing), "_", " "))
}

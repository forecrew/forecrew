package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
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

// providerCommand connects locations. Providers are listed by `forecrew
// providers`.
func providerCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{Use: "provider", Short: "Connect and revoke the locations machines come from"}
	cmd.AddCommand(providerAddCommand(r), providerRevokeCommand(r), providerJoinCommand(r),
		listCommand(r, "joins", "List join codes and what their agents reported", "/join-codes", printJoinCodes),
		providerConfirmCommand(r))
	return cmd
}

func providerAddCommand(r *remote) *cobra.Command {
	var in mcp.CreateProviderInput
	var kind string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a location served by a fleet agent, for scripts and air-gapped sites; its token is shown once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name, in.Kind = args[0], domain.ProviderKind(kind)
			var out mcp.CreateProviderOutput
			if err := r.call(cmd.Context(), http.MethodPost, "/providers", in, &out); err != nil {
				return err
			}
			printSecret(cmd.OutOrStdout(), cmd.ErrOrStderr(), out.Token, "provider", out.Provider.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "what it runs on: microvm, proxmox, hyperv, tart or aws (required)")
	cmd.Flags().StringVar(&in.Org, "org", "", orgFlagHelp)
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

// orgFlagHelp explains --org of the commands that connect a location.
const orgFlagHelp = "organization slug; defaults to the server's organization when it has only one, otherwise instance-wide"

func providerRevokeCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name|id>",
		Short: "Revoke a location: its token stops working for good and it gets no more demand",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ProviderInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/providers/"+url.PathEscape(args[0])+"/revoke", nil, &p); err != nil {
				return err
			}
			cmd.Printf("provider %s revoked\n", p.Name)
			return nil
		},
	}
}

func providerJoinCommand(r *remote) *cobra.Command {
	var in mcp.CreateJoinCodeInput
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Draw a single-use join code for a fleet agent; no token is shown to anyone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out mcp.CreateJoinCodeOutput
			if err := r.call(cmd.Context(), http.MethodPost, "/join-codes", in, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Valid until %s. It is not stored and cannot be shown again. When the agent has reported, see `forecrew provider joins` and `forecrew provider confirm`.\n",
				out.JoinCode.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintln(cmd.OutOrStdout(), out.Code)
			return nil
		},
	}
	cmd.Flags().StringVar(&in.Org, "org", "", orgFlagHelp)
	cmd.Flags().BoolVar(&in.Cloud, "cloud", false, "make it last 7 days instead of 1 hour, for a kit another team runs")
	return cmd
}

func providerConfirmCommand(r *remote) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "confirm <join-id>",
		Short: "Confirm what an agent reported: it becomes a location and gets its token over its own connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ProviderInfo
			err := r.call(cmd.Context(), http.MethodPost, "/join-codes/"+url.PathEscape(args[0])+"/confirm", mcp.ConfirmJoinInput{Name: name}, &p)
			if err != nil {
				return err
			}
			cmd.Printf("provider %s created; its agent connects at its next call\n", p.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the new location (required)")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// printJoinCodes shows what agents said about themselves, which nobody checked.
func printJoinCodes(w io.Writer, items []mcp.JoinCodeInfo) {
	rows := [][]string{{"ID", "STATUS", "KIND", "HOST", "ACCOUNT", "REGION", "CPU", "MEMORY MB", "EXPIRES"}}
	for _, c := range items {
		f := c.Facts
		rows = append(rows, []string{c.ID.String(), c.Status, dash(safeText(string(c.Kind))), dash(safeText(f.Host)), dash(safeText(f.CloudAccount)), dash(safeText(f.Region)),
			strconv.Itoa(f.MaxCPU), strconv.Itoa(f.MaxMemoryMB), c.ExpiresAt.Format(time.RFC3339)})
	}
	table(w, rows)
}

func printMachines(w io.Writer, items []mcp.MachineInfo) {
	rows := [][]string{{"NAME", "STATE", "LOCATION", "SHAPE", "JOB", "ERROR"}}
	for _, m := range items {
		rows = append(rows, []string{m.Name, string(m.State), m.Provider, m.Shape, dash(m.JobRef), dash(safeText(m.Error))})
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

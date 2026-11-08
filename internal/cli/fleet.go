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

func usageCommand(r *remote) *cobra.Command {
	var month string
	var records bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show what the organization's machines cost in a month, by machine type",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if month != "" {
				q.Set("month", month)
			}
			if records {
				q.Set("records", "true")
			}
			path := "/usage"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var u fleet.UsageReport
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &u); err != nil {
				return err
			}
			printUsage(cmd.OutOrStdout(), u)
			return nil
		},
	}
	cmd.Flags().StringVar(&month, "month", "", "the month, YYYY-MM in UTC; by default the current one")
	cmd.Flags().BoolVar(&records, "records", false, "list every machine's usage record too")
	return cmd
}

func printUsage(w io.Writer, u fleet.UsageReport) {
	amount := func(t fleet.UsageTotals) string { return u.Currency + " " + strconv.FormatFloat(t.Cost, 'f', -1, 64) }
	minutes := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	fmt.Fprintf(w, "%s to %s\n\n", u.From.Format(time.DateOnly), u.To.Format(time.DateOnly))
	rows := [][]string{{"MACHINE TYPE", "OWNERSHIP", "MACHINES", "MINUTES", "COST"}}
	for _, s := range u.Shapes {
		rows = append(rows, []string{s.Shape, string(s.Ownership), strconv.Itoa(s.Machines), minutes(s.Minutes), amount(s.UsageTotals)})
	}
	rows = append(rows, []string{"total", "", strconv.Itoa(u.Total.Machines), minutes(u.Total.Minutes), amount(u.Total)})
	table(w, rows)
	if u.Running.Machines > 0 {
		fmt.Fprintf(w, "\nRunning now, billed when they end: %d machines, %s minutes, %s\n", u.Running.Machines, minutes(u.Running.Minutes), amount(u.Running))
	}
	if len(u.Records) > 0 {
		rows := [][]string{{"MACHINE", "OWNERSHIP", "STARTED", "ENDED", "MINUTES", "COST"}}
		for _, r := range u.Records {
			rows = append(rows, []string{r.MachineID.String(), string(r.Ownership), r.StartedAt.Format(time.RFC3339), r.EndedAt.Format(time.RFC3339),
				minutes(r.Minutes), strconv.FormatFloat(r.Cost, 'f', 6, 64)})
		}
		fmt.Fprintln(w)
		table(w, rows)
	}
}

// noticesCommand lists and resolves the instance's notices, for instance
// admins.
func noticesCommand(r *remote) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "notices",
		Short: "List what the instance's operator must look at: machines stopped for abuse",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/fleet/notices"
			if all {
				path += "?all=true"
			}
			var items []mcp.FleetNoticeInfo
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"ID", "KIND", "ORGANIZATION", "LOCATION", "MACHINE", "OPENED", "RESOLVED", "DETAIL"}}
			for _, n := range items {
				resolved := "-"
				if !n.ResolvedAt.IsZero() {
					resolved = n.ResolvedAt.Format(time.RFC3339)
				}
				rows = append(rows, []string{n.ID.String(), n.Kind, dash(n.Organization), n.Provider, n.MachineID.String(),
					n.CreatedAt.Format(time.RFC3339), resolved, dash(safeText(n.Detail))})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "resolved notices too")
	cmd.AddCommand(&cobra.Command{
		Use:   "resolve <id>",
		Short: "Close a notice once it is dealt with",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n mcp.FleetNoticeInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/fleet/notices/"+url.PathEscape(args[0])+"/resolve", nil, &n); err != nil {
				return err
			}
			cmd.Printf("notice %s resolved\n", n.ID)
			return nil
		},
	})
	return cmd
}

// providerCommand connects locations. Providers are listed by `forecrew
// providers`.
func providerCommand(r *remote) *cobra.Command {
	cmd := &cobra.Command{Use: "provider", Short: "Connect and revoke the locations machines come from"}
	cmd.AddCommand(providerAddCommand(r), providerRevokeCommand(r), providerJoinCommand(r), providerBuildImageCommand(r), providerImageBuildsCommand(r),
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
const orgFlagHelp = "organization slug, or instance for an instance-wide location; required on a server with several organizations"

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

// providerBuildImageCommand asks a location's fleet agent for the Windows image
// of its host. The ISO is a path on that host: only references are sent.
func providerBuildImageCommand(r *remote) *cobra.Command {
	var in mcp.StartImageBuildInput
	cmd := &cobra.Command{
		Use:   "build-image <name|id>",
		Short: "Ask a Hyper-V or Proxmox location to build its Windows image from your ISO, which stays on its host",
		Long: "Asks the fleet agent of a location to build the Windows image of its host with forecrew-fleet-agent build-image, about an hour. " +
			"--iso is a path on that host, in the directory its agent allows. The agent checks the checksum, the licence statement and the worker again. " +
			"The result is an unsigned manifest on the host: nothing is registered or signed. Follow it with `forecrew provider image-builds`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var b mcp.ImageBuildInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/providers/"+url.PathEscape(args[0])+"/image-builds", in, &b); err != nil {
				return err
			}
			cmd.Printf("image build %s requested on %s (%s); follow it with `forecrew provider image-builds %s`\n", b.Version, b.Provider, b.ID, b.Provider)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Version, "version", "", "version of the image, for example 2026.11.1 (required)")
	f.StringVar(&in.ISO, "iso", "", "absolute path of the Windows Server ISO on the location's host (required)")
	f.StringVar(&in.ISOChecksum, "iso-checksum", "", "sha256:<hex> of the ISO, from your licence portal (required)")
	f.StringVar(&in.ChocolateySHA256, "chocolatey-sha256", "", "SHA-256 of the Chocolatey install.ps1 you reviewed (required)")
	f.StringVar(&in.WindowsImageName, "windows-image-name", "", "the edition inside the ISO; default the template's")
	f.BoolVar(&in.LicenseConfirmed, "license-confirmed", false, "you hold a Windows Server licence that covers building and running this image (required)")
	for _, name := range []string{"version", "iso", "iso-checksum", "chocolatey-sha256"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func providerImageBuildsCommand(r *remote) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "image-builds <name|id>",
		Short: "Show the Windows image builds of a location, newest first, with their progress",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/providers/" + url.PathEscape(args[0]) + "/image-builds"
			if limit > 0 {
				path += "?" + url.Values{"limit": {strconv.Itoa(limit)}}.Encode()
			}
			var items []mcp.ImageBuildInfo
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &items); err != nil {
				return err
			}
			printImageBuilds(cmd.OutOrStdout(), items)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "how many builds to show (default 20)")
	return cmd
}

// printImageBuilds shows builds with the words their agents reported, which
// nobody checked.
func printImageBuilds(w io.Writer, items []mcp.ImageBuildInfo) {
	rows := [][]string{{"ID", "VERSION", "STATE", "PERCENT", "STEP", "WHY", "UNSIGNED MANIFEST"}}
	next := false
	for _, b := range items {
		rows = append(rows, []string{b.ID.String(), b.Version, string(b.State), strconv.Itoa(b.Percent), dash(safeText(b.Step)), dash(safeText(b.Error)), dash(safeText(b.Manifest))})
		next = next || b.Next != ""
	}
	table(w, rows)
	if next {
		fmt.Fprintln(w, "\nA manifest is unsigned and nothing is registered. Check it, sign it off the host with `forecrew image sign`, then `forecrew image add` it (docs/images.md).")
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
	var name, facts string
	cmd := &cobra.Command{
		Use:   "confirm <join-id>",
		Short: "Confirm what an agent reported: it becomes a location and gets its token over its own connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ProviderInfo
			err := r.call(cmd.Context(), http.MethodPost, "/join-codes/"+url.PathEscape(args[0])+"/confirm", mcp.ConfirmJoinInput{Name: name, FactsHash: facts}, &p)
			if err != nil {
				return err
			}
			cmd.Printf("provider %s created; its agent connects at its next call\n", p.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the new location (required)")
	cmd.Flags().StringVar(&facts, "facts", "", "the FACTS value `forecrew provider joins` showed for the report you read (required)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("facts")
	return cmd
}

// printJoinCodes shows what agents said about themselves, which nobody checked.
func printJoinCodes(w io.Writer, items []mcp.JoinCodeInfo) {
	rows := [][]string{{"ID", "STATUS", "KIND", "HOST", "ACCOUNT", "REGION", "CPU", "MEMORY MB", "EXPIRES", "FACTS"}}
	for _, c := range items {
		f := c.Facts
		rows = append(rows, []string{c.ID.String(), c.Status, dash(safeText(string(c.Kind))), dash(safeText(f.Host)), dash(safeText(f.CloudAccount)), dash(safeText(f.Region)),
			strconv.Itoa(f.MaxCPU), strconv.Itoa(f.MaxMemoryMB), c.ExpiresAt.Format(time.RFC3339), dash(c.FactsHash)})
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

package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// remote is the connection of the commands that call /api/v1.
type remote struct {
	env   Env
	url   string
	token string // --token; the environment variable when empty
}

// apiError is an error answer of /api/v1.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "unauthorized: check the token (--token or " + EnvToken + ")"
	}
	return e.Message
}

// call sends a request to /api/v1 and decodes the JSON answer into out.
func (r *remote) call(ctx context.Context, method, path string, body, out any) error {
	token := cmp.Or(r.token, r.env.Getenv(EnvToken))
	if token == "" {
		return errors.New("no token: set " + EnvToken + " or pass --token")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	u, err := url.Parse(r.url)
	if err != nil || u.Host == "" {
		return fmt.Errorf("bad --url %q", r.url)
	}
	if u.Scheme == "http" && !slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()) {
		return fmt.Errorf("refusing to send the token over plain http to %s: use an https URL", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.url, "/")+"/api/v1"+path, rd)
	if err != nil {
		return fmt.Errorf("bad --url: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := r.env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call forecrewd: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode, Message: fmt.Sprintf("forecrewd answered %s", resp.Status)}
		var body struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
			e.Code, e.Message = body.Error.Code, body.Error.Message
		}
		return e
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	return nil
}

// newRemote returns the connection that --url and --token configure.
func newRemote(env Env) *remote {
	url := env.Getenv(EnvURL)
	if url == "" {
		url = DefaultURL
	}
	return &remote{env: env, url: url}
}

// bind adds the connection flags to the root command.
func (r *remote) bind(root *cobra.Command) {
	root.PersistentFlags().StringVar(&r.url, "url", r.url, "base URL of forecrewd ("+EnvURL+")")
	root.PersistentFlags().StringVar(&r.token, "token", "", "API token; prefer "+EnvToken+", a flag shows in the process list")
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if cmd.Flag("token").Changed {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: --token is visible to other users in the process list; set "+EnvToken+" instead")
		}
	}
}

// commands returns the commands that call /api/v1.
func (r *remote) commands() []*cobra.Command {
	return []*cobra.Command{
		listCommand(r, "workers", "List workers", "/workers", printWorkers),
		listCommand(r, "projects", "List projects", "/projects", printProjects),
		tokensCommand(r),
		listCommand(r, "gates", "List the gates waiting for a decision", "/gates", printGates),
		decideCommand(r, "approve", "approve", "Approve a gate (GATE-7 or UUID); on a needs_you gate this retries"),
		decideCommand(r, "reject", "reject", "Reject a gate (GATE-7 or UUID); on a needs_you gate this stops the work"),
		changesCommand(r),
		jobsCommand(r),
	}
}

func listCommand[T any](r *remote, use, short, path string, print func(w io.Writer, items []T)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var items []T
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &items); err != nil {
				return err
			}
			print(cmd.OutOrStdout(), items)
			return nil
		},
	}
}

func decideCommand(r *remote, name, action, short string) *cobra.Command {
	var feedback string
	cmd := &cobra.Command{
		Use:   name + " <gate>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decide(cmd, r, args[0], action, feedback)
		},
	}
	cmd.Flags().StringVar(&feedback, "feedback", "", "note recorded with the decision")
	return cmd
}

func changesCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "changes <gate> <feedback>",
		Short: "Ask for changes on a gate",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return decide(cmd, r, args[0], "request-changes", strings.Join(args[1:], " "))
		},
	}
}

func decide(cmd *cobra.Command, r *remote, gate, action, feedback string) error {
	var body any
	if feedback != "" {
		body = map[string]string{"feedback": feedback}
	}
	var g domain.Gate
	if err := r.call(cmd.Context(), http.MethodPost, "/gates/"+url.PathEscape(gate)+"/"+action, body, &g); err != nil {
		return err
	}
	cmd.Printf("%s is %s\n", g.Ref(), g.Status)
	return nil
}

func jobsCommand(r *remote) *cobra.Command {
	jobs := &cobra.Command{Use: "jobs", Short: "Inspect jobs"}
	jobs.AddCommand(&cobra.Command{
		Use:   "show <job>",
		Short: "Show a job (JOB-41 or UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var j mcp.JobInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/jobs/"+url.PathEscape(args[0]), nil, &j); err != nil {
				return err
			}
			printJob(cmd.OutOrStdout(), j)
			return nil
		},
	})
	jobs.AddCommand(&cobra.Command{
		Use:   "cancel <job>",
		Short: "Cancel a queued or running job (JOB-41 or UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var j mcp.JobInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/jobs/"+url.PathEscape(args[0])+"/cancel", nil, &j); err != nil {
				return err
			}
			cmd.Printf("%s is %s\n", j.Ref, j.Status)
			return nil
		},
	})
	return jobs
}

// tokensCommand lists the tokens of the account; its subcommand revokes one.
func tokensCommand(r *remote) *cobra.Command {
	tokens := listCommand(r, "tokens", "List the API and MCP tokens", "/tokens", printTokens)
	tokens.AddCommand(&cobra.Command{
		Use:   "revoke <name|id>",
		Short: "Revoke a token; it stops working at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.RevokeTokenOutput
			if err := r.call(cmd.Context(), http.MethodPost, "/tokens/"+url.PathEscape(args[0])+"/revoke", nil, &out); err != nil {
				return err
			}
			cmd.Println(out.Message)
			return nil
		},
	})
	return tokens
}

func table(w io.Writer, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printWorkers(w io.Writer, items []mcp.WorkerInfo) {
	rows := [][]string{{"NAME", "POOL", "STATUS", "VERSION", "LABELS", "RUNNING"}}
	for _, x := range items {
		status := "offline"
		switch {
		case x.Revoked():
			status = "revoked"
		case x.Online:
			status = "online"
		}
		rows = append(rows, []string{x.Name, domain.OrDefaultPool(x.Pool), status, dash(x.Version), dash(strings.Join(x.Labels, ",")), dash(strings.Join(x.RunningJobs, ","))})
	}
	table(w, rows)
}

func printProjects(w io.Writer, items []domain.Project) {
	rows := [][]string{{"NAME", "REPO", "STATUS"}}
	for _, p := range items {
		rows = append(rows, []string{p.Name, p.Repo, string(p.Status)})
	}
	table(w, rows)
}

func printGates(w io.Writer, items []mcp.GateInfo) {
	rows := [][]string{{"GATE", "KIND", "PROJECT", "FOR", "TITLE", "LINK"}}
	for _, g := range items {
		subject := g.TaskRef
		if subject == "" {
			subject = g.GoalRef
		}
		rows = append(rows, []string{g.Ref, string(g.Kind), g.Project, dash(subject), g.Title, dash(g.URL)})
	}
	table(w, rows)
}

func printJob(w io.Writer, j mcp.JobInfo) {
	rows := [][]string{
		{"Job:", j.Ref},
		{"Role:", j.Role},
		{"Status:", string(j.Status)},
		{"Reason:", dash(j.Reason)},
		{"Worker:", dash(j.WorkerName)},
		{"Attempts:", fmt.Sprintf("%d of %d", j.Attempts, j.MaxAttempts)},
		{"Branch:", dash(j.Spec.Branch)},
		{"Head:", dash(j.HeadSHA)},
		{"Cost:", fmt.Sprintf("$%.2f", j.CostUSD)},
		{"Created:", j.CreatedAt.Format("2006-01-02 15:04:05 MST")},
	}
	if !j.FinishedAt.IsZero() {
		rows = append(rows, []string{"Finished:", j.FinishedAt.Format("2006-01-02 15:04:05 MST")})
	}
	table(w, rows)
}

func printTokens(w io.Writer, items []mcp.TokenInfo) {
	rows := [][]string{{"NAME", "ID", "STATUS", "LAST USED"}}
	for _, t := range items {
		status, lastUsed := "active", "-"
		switch {
		case !t.RevokedAt.IsZero():
			status = "revoked"
		case t.Current:
			status = "active (this one)"
		}
		if !t.LastUsedAt.IsZero() {
			lastUsed = t.LastUsedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, []string{t.Name, t.ID.String(), status, lastUsed})
	}
	table(w, rows)
}

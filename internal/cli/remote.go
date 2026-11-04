package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

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

// send sends a request to /api/v1 and returns the response, whatever its
// status; the caller closes the body.
func (r *remote) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	token := cmp.Or(r.token, r.env.Getenv(EnvToken))
	if token == "" {
		return nil, errors.New("no token: set " + EnvToken + " or pass --token")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	u, err := url.Parse(r.url)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad --url %q", r.url)
	}
	if u.Scheme == "http" && !slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()) {
		return nil, fmt.Errorf("refusing to send the token over plain http to %s: use an https URL", u.Host)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.url, "/")+"/api/v1"+path, rd)
	if err != nil {
		return nil, fmt.Errorf("bad --url: %w", err)
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
		return nil, fmt.Errorf("call forecrewd: %w", err)
	}
	return resp, nil
}

// failure turns an error answer into an *apiError.
func failure(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	e := &apiError{Status: resp.StatusCode, Message: fmt.Sprintf("forecrewd answered %s", resp.Status)}
	var body struct {
		Error struct{ Code, Message string }
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
		e.Code, e.Message = body.Error.Code, body.Error.Message
	}
	return e
}

// call sends a request to /api/v1 and decodes the JSON answer into out.
func (r *remote) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := r.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return failure(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	return nil
}

// download streams the answer of a GET to open's writer. open is called once
// the answer is known to be a success, so a failed request leaves no file.
func (r *remote) download(ctx context.Context, path string, open func() (io.WriteCloser, error)) error {
	resp, err := r.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return failure(resp)
	}
	w, err := open()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		_ = w.Close()
		return fmt.Errorf("download: %w", err)
	}
	return w.Close()
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
		artifactCommand(r),
		assumptionsCommand(r),
		decisionsCommand(r),
		knowledgeCommand(r),
		councilsCommand(r),
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
	jobs := &cobra.Command{Use: "jobs", Aliases: []string{"job"}, Short: "Inspect jobs"}
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
	jobs.AddCommand(&cobra.Command{
		Use:   "artifacts <job>",
		Short: "List the artifacts a job uploaded (JOB-41 or UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var items []domain.Artifact
			if err := r.call(cmd.Context(), http.MethodGet, "/jobs/"+url.PathEscape(args[0])+"/artifacts", nil, &items); err != nil {
				return err
			}
			printArtifacts(cmd.OutOrStdout(), items)
			return nil
		},
	})
	jobs.AddCommand(&cobra.Command{
		Use:   "tests <job>",
		Short: "Show the test runs of a command job and the tests that failed (JOB-41 or UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var items []mcp.TestRunInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/test-runs?job="+url.QueryEscape(args[0]), nil, &items); err != nil {
				return err
			}
			printTestRuns(cmd.OutOrStdout(), items)
			return nil
		},
	})
	return jobs
}

// councilsCommand starts and follows councils.
func councilsCommand(r *remote) *cobra.Command {
	councils := &cobra.Command{Use: "councils", Aliases: []string{"council"}, Short: "Run councils"}
	var kind string
	var roles, constraints []string
	start := &cobra.Command{
		Use:   "start <project> <question>",
		Short: "Start a council of its own",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var c mcp.CouncilInfo
			in := mcp.StartCouncilInput{Project: args[0], Question: args[1], Kind: kind, Roles: roles, Constraints: constraints}
			if err := r.call(cmd.Context(), http.MethodPost, "/councils", in, &c); err != nil {
				return err
			}
			cmd.Printf("%s started\n", c.Ref)
			return nil
		},
	}
	start.Flags().StringVar(&kind, "kind", "", "product, technical, test_design, incident or onboarding; default adhoc")
	start.Flags().StringArrayVar(&roles, "role", nil, "a member instead of the kind's (repeatable)")
	start.Flags().StringArrayVar(&constraints, "constraint", nil, "what every option must respect (repeatable)")
	councils.AddCommand(start)
	councils.AddCommand(&cobra.Command{
		Use:   "show <council>",
		Short: "Show a council (COUNCIL-2 or UUID)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d mcp.CouncilDetail
			if err := r.call(cmd.Context(), http.MethodGet, "/councils/"+url.PathEscape(args[0]), nil, &d); err != nil {
				return err
			}
			printCouncil(cmd.OutOrStdout(), d)
			return nil
		},
	})
	councils.AddCommand(&cobra.Command{
		Use:   "constrain <council> <constraint>",
		Short: "Add a constraint to a running council; its next round sees it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var c mcp.CouncilInfo
			if err := r.call(cmd.Context(), http.MethodPost, "/councils/"+url.PathEscape(args[0])+"/constraints", map[string]string{"constraint": args[1]}, &c); err != nil {
				return err
			}
			cmd.Printf("%s has %d constraints\n", c.Ref, len(c.Constraints))
			return nil
		},
	})
	return councils
}

// artifactCommand downloads artifacts.
func artifactCommand(r *remote) *cobra.Command {
	artifact := &cobra.Command{Use: "artifact", Short: "Download job artifacts"}
	var output string
	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Download an artifact by UUID (see: forecrew job artifacts)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			toStdout := output == "" || output == "-"
			created := false
			err := r.download(cmd.Context(), "/artifacts/"+url.PathEscape(args[0]), func() (io.WriteCloser, error) {
				if toStdout {
					return nopCloser{cmd.OutOrStdout()}, nil
				}
				f, err := os.Create(output) //nolint:gosec // the path is the one the user asked for
				created = err == nil
				return f, err
			})
			if err != nil && created {
				_ = os.Remove(output) // no partial file
			}
			return err
		},
	}
	get.Flags().StringVarP(&output, "output", "o", "", "file to write; standard output when empty or -")
	artifact.AddCommand(get)
	return artifact
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

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
	if t := j.Tests; t != nil {
		rows = append(rows, []string{"Tests:", fmt.Sprintf("%s: %d passed, %d failed, %d skipped, %d errored in %d runs", t.Status, t.Passed, t.Failed, t.Skipped, t.Errored, t.Runs)})
	}
	if !j.FinishedAt.IsZero() {
		rows = append(rows, []string{"Finished:", j.FinishedAt.Format("2006-01-02 15:04:05 MST")})
	}
	table(w, rows)
}

// safeText replaces control characters, escape sequences included, with
// spaces: test names and messages are written by the repo's tests and must
// not drive the user's terminal.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// printCouncil shows a council. Everything but its ref and status may come
// from agents, so it goes through safeText.
func printCouncil(w io.Writer, d mcp.CouncilDetail) {
	fmt.Fprintf(w, "%s (%s) is %s\n", d.Ref, d.Kind, d.Status)
	fmt.Fprintf(w, "Question: %s\n", safeText(d.Question))
	for _, c := range d.Constraints {
		fmt.Fprintf(w, "Constraint: %s\n", safeText(c))
	}
	rows := [][]string{{"MEMBER", "STAGE", "ROUND", "SOURCES"}}
	for _, k := range d.Contributions {
		bad := 0
		for _, s := range k.EvidenceCheck {
			if s.Status != domain.SourceOK {
				bad++
			}
		}
		rows = append(rows, []string{safeText(k.Member), string(k.Stage), strconv.Itoa(k.Round), fmt.Sprintf("%d, %d failed", len(k.EvidenceCheck), bad)})
	}
	if len(d.Contributions) > 0 {
		table(w, rows)
	}
	if b := d.Brief; b != nil {
		fmt.Fprintf(w, "Recommendation: %s\n", safeText(b.Recommendation))
		if b.BudgetExhausted {
			fmt.Fprintln(w, "The budget ran out before the debate finished.")
		}
		for _, x := range b.Dissent {
			fmt.Fprintf(w, "Dissent of %s: %s\n", safeText(x.Member), safeText(x.Position))
		}
	}
	if d.Gate != nil {
		fmt.Fprintf(w, "Gate: %s (%s)\n", d.Gate.Ref, d.Gate.Status)
	}
	if d.Decision != nil {
		fmt.Fprintf(w, "Decision: %s by %s: %s\n", d.Decision.Ref(), safeText(d.Decision.DecidedBy), safeText(d.Decision.Choice))
	}
}

func printTestRuns(w io.Writer, items []mcp.TestRunInfo) {
	rows := [][]string{{"SUITE", "ENVIRONMENT", "CELL", "STATUS", "PASSED", "FAILED", "SKIPPED", "ERRORED", "EXIT", "SHA"}}
	for _, r := range items {
		cell := make([]string, 0, len(r.Cell))
		for _, k := range slices.Sorted(maps.Keys(r.Cell)) {
			cell = append(cell, k+"="+r.Cell[k])
		}
		rows = append(rows, []string{dash(safeText(r.Suite)), dash(safeText(r.Environment)), dash(safeText(strings.Join(cell, ","))), string(r.Status),
			strconv.Itoa(r.Counts.Passed), strconv.Itoa(r.Counts.Failed), strconv.Itoa(r.Counts.Skipped), strconv.Itoa(r.Counts.Errored),
			strconv.Itoa(r.ExitCode), r.SHA[:min(12, len(r.SHA))]})
	}
	table(w, rows)
	for _, r := range items {
		for _, f := range r.Failures {
			msg, _, _ := strings.Cut(f.Message, "\n")
			_, _ = fmt.Fprintf(w, "%s %s %s: %s\n", strings.ToUpper(string(f.Status)), safeText(r.Suite), safeText(strings.TrimSpace(f.Classname+" "+f.Name)), safeText(msg))
		}
	}
}

func printArtifacts(w io.Writer, items []domain.Artifact) {
	rows := [][]string{{"ID", "NAME", "TYPE", "SIZE", "SHA256"}}
	for _, a := range items {
		rows = append(rows, []string{a.ID.String(), a.Name, dash(a.ContentType), strconv.FormatInt(a.Size, 10), a.SHA256[:min(12, len(a.SHA256))]})
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

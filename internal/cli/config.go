package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// printPlan shows a plan the way the web UI's card does.
func printPlan(w io.Writer, p mcp.ConfigPlanView) {
	if p.Title == "" && len(p.Changes) == 0 {
		fmt.Fprintln(w, "Nothing to change.")
		return
	}
	fmt.Fprintf(w, "Plan %s: %s (%s, applies %s)\n", p.ID, p.Title, p.Status, p.Applies)
	for _, c := range p.Changes {
		fmt.Fprintln(w, " ", c.Line)
	}
	fmt.Fprintf(w, "Affects: %s\nCost: %s\nUndo: %s\n", p.Affects, p.Cost, p.Undo)
	for _, s := range p.Steps {
		state := ""
		if s.Kind == domain.ConfigStepSecret {
			state = " (empty)"
			if s.Filled {
				state = " (saved)"
			}
		}
		fmt.Fprintf(w, "Step: %s: %s%s\n", s.Kind, s.Title, state)
	}
	for _, r := range p.LowersProtection {
		fmt.Fprintln(w, "Lowers protection:", r)
	}
}

// readInput reads a file, or standard input for "-".
func readInput(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	}
	return os.ReadFile(path) // #nosec G304 -- the operator names the file
}

// confirmLine asks a question on the terminal and returns the answer.
func confirmLine(cmd *cobra.Command, question string) (string, error) {
	cmd.Print(question + " ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// configCommands are the remote commands of `forecrew config`.
func (r *remote) configCommands() []*cobra.Command {
	return []*cobra.Command{r.configSchema(), r.configGet(), r.configPlan(), r.configShow(), r.configList(), r.configApply(), r.configRequest(),
		r.configRevert(), r.configCancel(), r.configExport(), r.configImport(), r.configSecret()}
}

func (r *remote) configSchema() *cobra.Command {
	return &cobra.Command{
		Use: "schema [kind]", Short: "Show the kinds of configuration resources and their schemas", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if len(args) == 1 {
				q.Set("kind", args[0])
			}
			var out []mcp.ConfigSchema
			if err := r.call(cmd.Context(), http.MethodGet, "/config/schema?"+q.Encode(), nil, &out); err != nil {
				return err
			}
			if len(args) == 0 {
				for _, s := range out {
					cmd.Printf("%s\t%s\t%s\n", s.Kind, s.Scope, s.Title)
				}
				return nil
			}
			for _, s := range out {
				cmd.Println(string(s.Schema))
			}
			return nil
		},
	}
}

func (r *remote) configGet() *cobra.Command {
	return &cobra.Command{
		Use: "get [kind] [name]", Short: "Read configuration resources", Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			for i, k := range []string{"kind", "name"} {
				if i < len(args) {
					q.Set(k, args[i])
				}
			}
			var out []mcp.ConfigResource
			if err := r.call(cmd.Context(), http.MethodGet, "/config/resources?"+q.Encode(), nil, &out); err != nil {
				return err
			}
			for _, c := range out {
				origin := ""
				if c.Origin != "" {
					origin = " (" + c.Origin + ")"
				}
				cmd.Printf("%s%s\n  %s\n", c.Ref(), origin, c.Spec)
			}
			return nil
		},
	}
}

func (r *remote) configPlan() *cobra.Command {
	var title string
	cmd := &cobra.Command{
		Use: "plan <file|->", Short: "Make a change plan from YAML: a list of kind, name and spec or delete", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readInput(cmd, args[0])
			if err != nil {
				return err
			}
			var list []map[string]any
			if err := yaml.Unmarshal(data, &list); err != nil {
				return fmt.Errorf("parse %s: %w", args[0], err)
			}
			raw, err := json.Marshal(list)
			if err != nil {
				return err
			}
			var changes []mcp.ConfigWanted
			if err := json.Unmarshal(raw, &changes); err != nil {
				return fmt.Errorf("parse %s: %w", args[0], err)
			}
			var p mcp.ConfigPlanView
			body := map[string]any{"title": title, "changes": changes, "source": domain.ConfigFromCLI}
			if err := r.call(cmd.Context(), http.MethodPost, "/config/plans", body, &p); err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), p)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "title of the plan")
	return cmd
}

func (r *remote) configShow() *cobra.Command {
	return &cobra.Command{
		Use: "show <plan>", Short: "Show a change plan", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodGet, "/config/plans/"+url.PathEscape(args[0]), nil, &p); err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

func (r *remote) configList() *cobra.Command {
	var inbox bool
	cmd := &cobra.Command{
		Use: "plans", Short: "List change plans", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out []mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodGet, fmt.Sprintf("/config/plans?inbox=%t", inbox), nil, &out); err != nil {
				return err
			}
			for _, p := range out {
				cmd.Printf("%s\t%s\t%s\t%s\n", p.ID, p.Status, p.Source, p.Title)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inbox, "inbox", false, "only the plans waiting for your confirmation")
	return cmd
}

// configApply shows the plan and asks: there is no flag to skip the question,
// so a script cannot confirm by passing a parameter. A plan the assistant or
// an MCP client wrote is refused by the server: those are confirmed in the
// web UI.
func (r *remote) configApply() *cobra.Command {
	return &cobra.Command{
		Use: "apply <plan>", Short: "Show a change plan and apply it after you confirm", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/config/plans/" + url.PathEscape(args[0])
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodGet, path, nil, &p); err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), p)
			body := map[string]string{}
			if len(p.LowersProtection) > 0 {
				word, err := confirmLine(cmd, "This lowers protection. Type "+p.ConfirmWord+" to confirm:")
				if err != nil {
					return err
				}
				body["word"] = word
			} else {
				answer, err := confirmLine(cmd, "Apply this plan? Type yes to confirm:")
				if err != nil {
					return err
				}
				if answer != "yes" {
					return errors.New("not applied")
				}
			}
			var out mcp.ConfigApplied
			if err := r.call(cmd.Context(), http.MethodPost, path+"/apply", body, &out); err != nil {
				return err
			}
			cmd.Printf("Applied %s\n", out.Plan.ID)
			if out.Plan.PRURL != "" {
				cmd.Println("Pull request:", out.Plan.PRURL)
			}
			for _, s := range out.ShowOnce {
				cmd.Printf("%s (shown once): %s\n", s.Label, s.Value)
			}
			return nil
		},
	}
}

func (r *remote) configRequest() *cobra.Command {
	return &cobra.Command{
		Use: "request-approval <plan>", Short: "Send a change plan to the Inbox of people who can confirm it", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var c mcp.ConfigConfirmation
			if err := r.call(cmd.Context(), http.MethodPost, "/config/plans/"+url.PathEscape(args[0])+"/request-approval", map[string]string{}, &c); err != nil {
				return err
			}
			cmd.Println(c.Message)
			return nil
		},
	}
}

func (r *remote) configRevert() *cobra.Command {
	return &cobra.Command{
		Use: "revert <plan>", Short: "Make a plan that puts back the values of an applied plan", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodPost, "/config/plans/"+url.PathEscape(args[0])+"/revert", map[string]string{}, &p); err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

func (r *remote) configCancel() *cobra.Command {
	return &cobra.Command{
		Use: "cancel <plan>", Short: "Drop a change plan", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodPost, "/config/plans/"+url.PathEscape(args[0])+"/cancel", map[string]string{}, &p); err != nil {
				return err
			}
			cmd.Printf("%s is %s\n", p.ID, p.Status)
			return nil
		},
	}
}

func (r *remote) configExport() *cobra.Command {
	return &cobra.Command{
		Use: "export", Short: "Print the configuration as YAML, without secrets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var e mcp.ConfigExport
			if err := r.call(cmd.Context(), http.MethodGet, "/config/export", nil, &e); err != nil {
				return err
			}
			cmd.Print(e.YAML)
			return nil
		},
	}
}

func (r *remote) configImport() *cobra.Command {
	return &cobra.Command{
		Use: "import <file|->", Short: "Make a change plan from exported YAML", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readInput(cmd, args[0])
			if err != nil {
				return err
			}
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodPost, "/config/import", map[string]string{"yaml": string(data)}, &p); err != nil {
				return err
			}
			printPlan(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

// configSecret reads the value from standard input, never from an argument,
// which would stay in the shell's history.
func (r *remote) configSecret() *cobra.Command {
	return &cobra.Command{
		Use: "secret <plan> <secret>", Short: "Save a secure field of a plan; the value comes from standard input", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			if err != nil {
				return err
			}
			value := strings.TrimRight(string(data), "\r\n")
			path := "/config/plans/" + url.PathEscape(args[0]) + "/secrets/" + url.PathEscape(args[1])
			var p mcp.ConfigPlanView
			if err := r.call(cmd.Context(), http.MethodPut, path, map[string]string{"value": value}, &p); err != nil {
				return err
			}
			cmd.Println("Saved.")
			return nil
		},
	}
}

// assistantCommand talks to the assistant.
func (r *remote) assistantCommand() *cobra.Command {
	var conversation, project string
	cmd := &cobra.Command{
		Use: "assistant <message>", Short: "Tell the configuration assistant what you want", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v mcp.AssistantView
			body := mcp.AssistantSendInput{Conversation: conversation, Message: args[0], Project: project}
			if err := r.call(cmd.Context(), http.MethodPost, "/assistant/messages", body, &v); err != nil {
				return err
			}
			cmd.Printf("Conversation %s: the assistant is working. Read the answer with: forecrew assistant show %s\n", v.ID, v.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "conversation to continue")
	cmd.Flags().StringVar(&project, "project", "", "project its jobs run in")
	cmd.AddCommand(&cobra.Command{
		Use: "show <conversation>", Short: "Read a conversation and its plans", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v mcp.AssistantView
			if err := r.call(cmd.Context(), http.MethodGet, "/assistant/conversations/"+url.PathEscape(args[0]), nil, &v); err != nil {
				return err
			}
			for _, m := range v.Messages {
				cmd.Printf("%s: %s\n\n", m.Author, m.Body)
			}
			for _, p := range v.Plans {
				printPlan(cmd.OutOrStdout(), p)
			}
			if v.Thinking {
				cmd.Println("Thinking...")
			}
			return nil
		},
	}, &cobra.Command{
		Use: "list", Short: "List your conversations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out []domain.AssistantConversation
			if err := r.call(cmd.Context(), http.MethodGet, "/assistant/conversations", nil, &out); err != nil {
				return err
			}
			for _, c := range out {
				cmd.Printf("%s\t%s\n", c.ID, c.Title)
			}
			return nil
		},
	})
	return cmd
}

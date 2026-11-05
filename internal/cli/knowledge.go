package cli

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/mcp"
)

// knowledgeCommand searches a project's knowledge base and deletes notes.
func knowledgeCommand(r *remote) *cobra.Command {
	kb := &cobra.Command{Use: "knowledge", Aliases: []string{"kb"}, Short: "Search the knowledge base"}
	var section, kind, provider string
	var limit int
	search := &cobra.Command{
		Use:   "search <project> <query>",
		Short: "Search a project's entries, agent notes and generated facts (words, \"phrases\"; put -exclusions after --)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}, "q": {strings.Join(args[1:], " ")}}
			for k, v := range map[string]string{"section": section, "kind": kind, "provider": provider} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var items []knowledge.Result
			if err := r.call(cmd.Context(), http.MethodGet, "/knowledge?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			printKnowledge(cmd.OutOrStdout(), items)
			return nil
		},
	}
	search.Flags().StringVar(&section, "section", "", "only this section, for example architecture")
	search.Flags().StringVar(&kind, "kind", "", "only this kind: entry, note or generated")
	search.Flags().StringVar(&provider, "provider", "", "only this provider, for example repo or internal")
	search.Flags().IntVar(&limit, "limit", 0, "maximum results (default 8, at most 50)")
	kb.AddCommand(search)
	kb.AddCommand(&cobra.Command{
		Use:   "delete-note <id>",
		Short: "Delete an agent note by UUID (see: forecrew knowledge search <project> <query> --kind note)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out mcp.DeleteNoteOutput
			if err := r.call(cmd.Context(), http.MethodDelete, "/knowledge/notes/"+url.PathEscape(args[0]), nil, &out); err != nil {
				return err
			}
			cmd.Println(out.Message)
			return nil
		},
	})
	kb.AddCommand(proposeCommand(r))
	return kb
}

// proposeCommand sends a human's change to a curated section to a PR.
func proposeCommand(r *remote) *cobra.Command {
	var bodyFile, reason, action string
	cmd := &cobra.Command{
		Use:   "propose <project> <section> <path>",
		Short: "Propose a change to a curated section; it goes straight to a pull request",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := mcp.ProposeKnowledgeInput{Project: args[0], Section: args[1], Path: args[2], Reason: reason, Action: domain.KnowledgeAction(action)}
			if bodyFile != "" {
				body, err := os.ReadFile(bodyFile) //nolint:gosec // the user names the file to send
				if err != nil {
					return err
				}
				in.Body = string(body)
			}
			var out mcp.ProposeKnowledgeOutput
			if err := r.call(cmd.Context(), http.MethodPost, "/knowledge/proposals", in, &out); err != nil {
				return err
			}
			cmd.Printf("%s: %s\n", out.Proposal, out.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "file with the full new Markdown, without frontmatter; leave it out to retire")
	cmd.Flags().StringVar(&reason, "reason", "", "why the knowledge base should change")
	cmd.Flags().StringVar(&action, "action", "", "add, edit or retire (default edit when the entry exists, else add)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// printKnowledge prints a result table. FLAGS has untrusted for text that
// agents or repo files wrote, and stale for entries a push may have made
// out of date.
func printKnowledge(w io.Writer, items []knowledge.Result) {
	rows := [][]string{{"ID", "KIND", "SECTION", "PATH", "TITLE", "FLAGS"}}
	for _, it := range items {
		var flags []string
		if it.Untrusted {
			flags = append(flags, "untrusted")
		}
		if it.NeedsReverify {
			flags = append(flags, "stale")
		}
		rows = append(rows, []string{it.ID.String(), string(it.Kind), it.Section, it.Path, it.Title, dash(strings.Join(flags, ","))})
	}
	table(w, rows)
}

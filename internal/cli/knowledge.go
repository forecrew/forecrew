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
	kb.AddCommand(listCommandOfKnowledge(r))
	kb.AddCommand(proposeCommand(r))
	kb.AddCommand(proposalsCommand(r))
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

// listCommandOfKnowledge lists the sections of a project and their entries
// without a search.
func listCommandOfKnowledge(r *remote) *cobra.Command {
	var section, kind, provider, goal string
	var limit int
	cmd := &cobra.Command{
		Use:   "list <project>",
		Short: "List a project's knowledge sections with their provider and location, and the entries of a section",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}}
			for k, v := range map[string]string{"section": section, "kind": kind, "provider": provider, "goal": goal} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var out mcp.KnowledgeListing
			if err := r.call(cmd.Context(), http.MethodGet, "/knowledge/entries?"+q.Encode(), nil, &out); err != nil {
				return err
			}
			rows := [][]string{{"SECTION", "PROVIDER", "LOCATION", "OWNER", "PROTECTED"}}
			for _, s := range out.Sections {
				rows = append(rows, []string{s.Name, dash(s.Provider), safeText(dash(strings.Join(s.Paths, ","))), dash(s.Owner), strconv.FormatBool(s.Protected)})
			}
			table(cmd.OutOrStdout(), rows)
			if len(out.Entries) > 0 {
				items := make([]knowledge.Result, len(out.Entries))
				for i, e := range out.Entries {
					items[i] = e.Result
				}
				printKnowledge(cmd.OutOrStdout(), items)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&section, "section", "", "list the entries of this section")
	cmd.Flags().StringVar(&kind, "kind", "", "only this kind: entry, note or generated")
	cmd.Flags().StringVar(&provider, "provider", "", "only this provider, for example repo or internal")
	cmd.Flags().StringVar(&goal, "goal", "", "with --kind note: the project's notes and the notes of a goal (GOAL-3)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum entries (default 50, at most 200)")
	return cmd
}

// proposalsCommand lists the proposed changes to a project's knowledge.
func proposalsCommand(r *remote) *cobra.Command {
	var goal, status string
	cmd := &cobra.Command{
		Use:   "proposals <project>",
		Short: "List the changes proposed to a project's knowledge, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"project": {args[0]}}
			for k, v := range map[string]string{"goal": goal, "status": status} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var items []mcp.KnowledgeProposalInfo
			if err := r.call(cmd.Context(), http.MethodGet, "/knowledge/proposals?"+q.Encode(), nil, &items); err != nil {
				return err
			}
			rows := [][]string{{"REF", "STATUS", "ACTION", "SECTION", "PATH", "BY", "REVIEW"}}
			for _, it := range items {
				review := it.URL
				if it.Gate != nil {
					review = it.Gate.Ref
				}
				rows = append(rows, []string{it.Ref, string(it.Status), string(it.Action), it.Section, safeText(it.Path), safeText(it.ProposedBy), dash(review)})
			}
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&goal, "goal", "", "only the proposals of a goal (GOAL-3)")
	cmd.Flags().StringVar(&status, "status", "", "queued, open, accepted or rejected")
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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
)

// commandRE matches the commands "approve", "changes" and "status".
var commandRE = regexp.MustCompile(`(?i)@forecrew\s+(approve|changes|status)\b`)

// parseCommand finds the first @forecrew command in a comment. For changes,
// feedback is the rest of the comment.
func parseCommand(body string) (cmd, feedback string, ok bool) {
	loc := commandRE.FindStringSubmatchIndex(body)
	if loc == nil {
		return "", "", false
	}
	cmd = strings.ToLower(body[loc[2]:loc[3]])
	return cmd, strings.TrimSpace(body[loc[1]:]), true
}

// comment runs the @forecrew command in a new issue or PR comment. Without an
// Allowed function nobody may command.
func (e *engine) comment(ctx context.Context, ev github.Event) error {
	if ev.Action != "created" {
		return nil
	}
	cmd, feedback, ok := parseCommand(ev.Body)
	if !ok {
		return nil
	}
	return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
		if e.Allowed == nil || !e.Allowed(ev.Actor) {
			return e.auditIgnored(ctx, p, ev, cmd)
		}
		return e.runCommand(ctx, p, ev, cmd, feedback)
	})
}

// auditIgnored records a command from somebody outside the allowlist.
func (e *engine) auditIgnored(ctx context.Context, p domain.Project, ev github.Event, cmd string) error {
	data, err := json.Marshal(map[string]any{"repo": ev.Repo, "number": ev.Number, "command": cmd})
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	err = e.Store.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: p.AccountID, ProjectID: p.ID, Actor: domain.Actor{Kind: domain.ActorGitHub, Name: ev.Actor}.String(),
		Action: "command.ignored", SubjectType: "project", SubjectID: p.ID, Data: data, CreatedAt: e.Now(),
	})
	if err != nil {
		return fmt.Errorf("audit ignored command: %w", err)
	}
	e.Log.Warn("command from a user outside the allowlist", "account_id", p.AccountID, "project_id", p.ID, "login", ev.Actor, "command", cmd)
	return nil
}

// runCommand applies the command of an allowed user to the oldest pending
// gate about the issue or PR and replies with the outcome.
func (e *engine) runCommand(ctx context.Context, p domain.Project, ev github.Event, cmd, feedback string) error {
	pending, err := e.Store.Gates().ListPendingBySubject(ctx, p.AccountID, ev.Repo, ev.Number)
	if err != nil {
		return fmt.Errorf("list gates of %s#%d: %w", ev.Repo, ev.Number, err)
	}
	if cmd == "status" {
		return e.reply(ctx, ev, statusReply(pending))
	}
	if len(pending) == 0 {
		return e.reply(ctx, ev, "Nothing is waiting for a decision here.")
	}
	g := pending[0]
	decision := domain.GateApproved
	tool := mcp.ToolApprove
	if cmd == "changes" {
		decision, tool = domain.GateChangesRequested, mcp.ToolRequestChanges
		if feedback == "" {
			return e.reply(ctx, ev, "Add what should change after the changes command.")
		}
	}
	if _, err := mcp.GateDecision(tool, g.Kind, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: feedback}); err != nil {
		if errors.Is(err, mcp.ErrInvalidInput) {
			return e.reply(ctx, ev, fmt.Sprintf("%s cannot be sent back for changes: approve it to retry or reject it to stop.", g.Ref()))
		}
		return err
	}
	by := domain.Actor{AccountID: p.AccountID, Kind: domain.ActorGitHub, Name: ev.Actor}
	if err := e.resolve(ctx, by, g, decision, feedback); err != nil {
		return err
	}
	return e.reply(ctx, ev, fmt.Sprintf("%s: %s.", g.Ref(), strings.ReplaceAll(string(decision), "_", " ")))
}

func statusReply(pending []domain.Gate) string {
	if len(pending) == 0 {
		return "Nothing is waiting for a decision here."
	}
	lines := make([]string, len(pending))
	for i, g := range pending {
		lines[i] = fmt.Sprintf("- %s (%s): %s", g.Ref(), strings.ReplaceAll(string(g.Kind), "_", " "), g.Title)
	}
	return "Waiting for a decision:\n" + strings.Join(lines, "\n")
}

func (e *engine) reply(ctx context.Context, ev github.Event, text string) error {
	if _, err := e.Tracker.CreateComment(ctx, ev.Repo, ev.Number, text); err != nil {
		return fmt.Errorf("reply on %s#%d: %w", ev.Repo, ev.Number, err)
	}
	return nil
}

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

// CommandAccess returns what each @forecrew command needs
// (docs/contracts/permissions.md), by command. It is the list of commands:
// a command without an entry does not parse.
func CommandAccess() map[string]mcp.Access {
	return map[string]mcp.Access{
		"approve": {Gate: true},
		"changes": {Gate: true},
		"status":  {Permission: domain.PermGateRead},
	}
}

// commandRE matches the commands of CommandAccess and the assumption ("A-3")
// a command may name.
var commandRE = regexp.MustCompile(`(?i)@forecrew\s+(` + strings.Join(slices.Sorted(maps.Keys(CommandAccess())), "|") + `)\b(?:[ \t]+(A-[0-9]+)\b)?`)

// parseCommand finds the first @forecrew command in a comment. assumption is
// the assumption it names ("A-3"), if any. For changes, feedback is the rest
// of the comment.
func parseCommand(body string) (cmd, assumption, feedback string, ok bool) {
	loc := commandRE.FindStringSubmatchIndex(body)
	if loc == nil {
		return "", "", "", false
	}
	cmd = strings.ToLower(body[loc[2]:loc[3]])
	if loc[4] >= 0 {
		assumption = strings.ToUpper(body[loc[4]:loc[5]])
	}
	return cmd, assumption, strings.TrimSpace(body[loc[1]:]), true
}

// comment runs the @forecrew command in a new issue or PR comment.
func (e *engine) comment(ctx context.Context, ev github.Event) error {
	if ev.Action != "created" {
		return nil
	}
	cmd, assumption, feedback, ok := parseCommand(ev.Body)
	if !ok {
		return nil
	}
	return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
		by, enforced, err := e.commander(ctx, p, ev)
		if err != nil {
			return err
		}
		switch {
		case by.Kind == domain.ActorUser:
			return e.runCommand(ctx, p, ev, by, cmd, assumption, feedback)
		case enforced:
			if err := e.auditIgnored(ctx, p, ev, cmd); err != nil {
				return err
			}
			return e.reply(ctx, ev, "Only a member of this Forecrew organization with a linked GitHub account can do that.")
		case e.Allowed != nil && e.Allowed(ev.Actor):
			return e.runCommand(ctx, p, ev, by, cmd, assumption, feedback)
		}
		return e.auditIgnored(ctx, p, ev, cmd)
	})
}

// commander returns who a comment acts as: the one member linked to the
// GitHub account of its author, or otherwise github:<login>. enforced is
// false in an organization nobody has a role in yet, as on an instance from
// before organizations, where the allowlist of the config still decides;
// from its first role binding on only permissions do.
func (e *engine) commander(ctx context.Context, p domain.Project, ev github.Event) (by domain.Actor, enforced bool, err error) {
	by = domain.Actor{AccountID: p.AccountID, Kind: domain.ActorGitHub, Name: ev.Actor}
	users, err := e.Store.Directory().GitHubMembers(ctx, p.AccountID, ev.ActorID, ev.Actor)
	if err != nil {
		return by, true, fmt.Errorf("find the user of %s: %w", ev.Actor, err)
	}
	if len(users) == 1 {
		return domain.UserActor(p.AccountID, users[0], domain.Credential{}), true, nil
	}
	bindings, err := e.Store.RoleBindings().List(ctx, p.AccountID, store.RoleBindingFilter{})
	if err != nil {
		return by, true, fmt.Errorf("list role bindings: %w", err)
	}
	return by, len(bindings) > 0, nil
}

// permitted runs check for a linked user, whose command needs p, and replies
// when it is denied. Empty p needs nothing, and github:<login> comes from the
// allowlist. When ok is false the command stops with err.
func (e *engine) permitted(ctx context.Context, ev github.Event, by domain.Actor, p domain.Permission, check func() error) (ok bool, err error) {
	if by.Kind != domain.ActorUser || p == "" {
		return true, nil
	}
	err = check()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, authz.ErrDenied):
		return false, e.reply(ctx, ev, fmt.Sprintf("You need the permission `%s` on this project to do that.", p))
	}
	return false, err
}

// auditIgnored records a command from somebody who is not a linked member or
// on the allowlist.
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
	e.Log.Warn("command from an unknown GitHub user", "account_id", p.AccountID, "project_id", p.ID, "login", ev.Actor, "command", cmd)
	return nil
}

// decidable returns the gate a command without an assumption is about: the
// oldest pending gate about the issue or PR that is not an assumption review.
// A review of an assumption sits on the issue of its goal or task, where the
// spec or the merge gate also is, and "@forecrew approve" meant for those
// must not accept a high-impact assumption: that takes its name,
// "@forecrew approve A-3".
func decidable(pending []domain.Gate, assumption string) (domain.Gate, bool) {
	i := slices.IndexFunc(pending, func(g domain.Gate) bool {
		n, ok := registers.ReviewedRef(g)
		if assumption != "" {
			return ok && domain.Ref(domain.RefAssumption, n) == assumption
		}
		return g.Kind != domain.GateAssumptionReview
	})
	if i < 0 {
		return domain.Gate{}, false
	}
	return pending[i], true
}

// runCommand applies the command of by to the gate it is about (see
// decidable) and replies with the outcome. A linked user needs the permission
// of CommandAccess; by is github:<login> only on the allowlist's say.
func (e *engine) runCommand(ctx context.Context, p domain.Project, ev github.Event, by domain.Actor, cmd, assumption, feedback string) error {
	access := CommandAccess()[cmd]
	if ok, err := e.permitted(ctx, ev, by, access.Permission, func() error {
		return e.Authz.Authorize(ctx, by, access.Permission, authz.Project(p.AccountID, p.ID))
	}); !ok {
		return err
	}
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
	g, ok := decidable(pending, assumption)
	if !ok {
		if assumption != "" {
			return e.reply(ctx, ev, fmt.Sprintf("No review of %s is waiting here.", assumption))
		}
		return e.reply(ctx, ev, "Only assumption reviews are waiting here: name the assumption to decide one, for example `@forecrew approve A-3`.\n\n"+statusReply(pending))
	}
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
	if access.Gate {
		if ok, err := e.permitted(ctx, ev, by, e.Authz.GatePermission(ctx, g), func() error { return e.Authz.AuthorizeGate(ctx, by, g) }); !ok {
			return err
		}
	}
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

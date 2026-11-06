package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
)

// maxBugBody bounds the issue text taken as a bug report: it comes from
// GitHub, written by anyone who can open an issue.
const maxBugBody = 16 << 10

// reportBug runs "@forecrew bug": the issue of the comment becomes the goal
// issue of a bug, which starts the bug-fix playbook. The caller has checked
// that the author may.
func (e *engine) reportBug(ctx context.Context, p domain.Project, ev github.Event, by domain.Actor) error {
	switch {
	case ev.IsPR:
		return e.reply(ctx, ev, "Report a bug on an issue, not on a pull request.")
	case e.Bugs == nil:
		return e.reply(ctx, ev, "Bug reports are not available here.")
	}
	issue, err := e.Tracker.GetIssue(ctx, ev.Repo, ev.Number)
	if err != nil {
		return fmt.Errorf("get issue %s#%d: %w", ev.Repo, ev.Number, err)
	}
	if issue.State == "closed" {
		return e.reply(ctx, ev, "This issue is closed.")
	}
	if len(issue.Body) > maxBugBody {
		issue.Body = string([]rune(issue.Body[:maxBugBody])) + "\n\n(cut: the issue is longer)"
	}
	g, err := e.Bugs.ReportIssueAsBug(ctx, by, p, issue)
	if errors.Is(err, mcp.ErrInvalidInput) {
		return e.reply(ctx, ev, strings.TrimSuffix(err.Error(), ": "+mcp.ErrInvalidInput.Error())+".")
	}
	if err != nil {
		return fmt.Errorf("report %s#%d as a bug: %w", ev.Repo, ev.Number, err)
	}
	return e.reply(ctx, ev, fmt.Sprintf("%s: a test that reproduces the bug comes first, then the fix. I will ask you to approve the merge.", g.Ref()))
}

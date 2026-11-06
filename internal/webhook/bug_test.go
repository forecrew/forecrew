package webhook

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
)

// fakeBugs records the issues reported as bugs.
type fakeBugs struct {
	issues []github.Issue
	by     []domain.Actor
	err    error
}

func (f *fakeBugs) ReportIssueAsBug(_ context.Context, by domain.Actor, _ domain.Project, issue github.Issue) (domain.Goal, error) {
	f.issues, f.by = append(f.issues, issue), append(f.by, by)
	return domain.Goal{Number: 4}, f.err
}

func (e *env) bugs() *fakeBugs {
	f := &fakeBugs{}
	e.eng.Bugs = f
	return f
}

func (e *env) bugIssue(body string) {
	e.t.Helper()
	_, err := e.tracker.CreateIssue(e.t.Context(), repo, github.IssueInput{Title: "Crash on save", Body: body})
	must(e.t, err)
}

// An allowlisted user's "@forecrew bug" makes the issue a bug goal.
func TestCommandBug(t *testing.T) {
	e := newEnv(t)
	e.bugIssue("Steps to reproduce")
	f := e.bugs()
	e.send("d1", "issue_comment", fixture(t, "issue_comment_bug.json"))
	if len(f.issues) != 1 || f.issues[0].Number != 1 || f.issues[0].Title != "Crash on save" || f.issues[0].Body != "Steps to reproduce" || f.by[0].String() != "github:octocat" {
		t.Fatalf("reported = %+v by %v", f.issues, f.by)
	}
	if r := e.replies(); len(r) != 1 || !strings.HasPrefix(r[0], "GOAL-4:") {
		t.Fatalf("replies = %q", r)
	}
}

// A user who is not on the allowlist is ignored and audited.
func TestCommandBugFromStrangerIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.bugIssue("x")
	f := e.bugs()
	e.send("d1", "issue_comment", fixture(t, "issue_comment_bug_stranger.json"))
	if len(f.issues) != 0 || len(e.replies()) != 0 {
		t.Fatalf("a stranger reported a bug: %+v, replies %q", f.issues, e.replies())
	}
	if got := e.auditActions(); len(got) != 1 || got[0] != "command.ignored" {
		t.Errorf("audit = %v", got)
	}
}

func TestCommandBugNeedsGoalCreate(t *testing.T) {
	e := newEnv(t)
	e.bugIssue("x")
	f := e.bugs()
	e.linked(domain.RoleViewer)
	e.comment("@forecrew bug")
	if r := e.replies(); len(f.issues) != 0 || len(r) != 1 || !strings.Contains(r[0], "goal.create") {
		t.Fatalf("reported %+v, replies = %q", f.issues, r)
	}
	e2 := newEnv(t)
	e2.bugIssue("x")
	f2 := e2.bugs()
	id := e2.linked(domain.RoleContributor)
	e2.comment("@forecrew bug")
	if len(f2.issues) != 1 || f2.by[0].String() != "user:"+id.String() {
		t.Fatalf("reported %+v by %v", f2.issues, f2.by)
	}
}

func TestCommandBugReplies(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(e *env)
		event github.Event
		want  string
	}{
		{"pull request", func(e *env) { e.bugs() }, github.Event{IsPR: true}, "not on a pull request"},
		{"no reporter", func(*env) {}, github.Event{}, "not available"},
		{"closed", func(e *env) {
			e.bugs()
			closed := "closed"
			_, err := e.tracker.UpdateIssue(e.t.Context(), repo, 1, github.IssueUpdate{State: &closed})
			must(e.t, err)
		}, github.Event{}, "closed"},
		{"refused", func(e *env) { e.bugs().err = fmt.Errorf("issue #1 is GOAL-2 already: %w", mcp.ErrInvalidInput) }, github.Event{}, "issue #1 is GOAL-2 already."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.bugIssue("x")
			tt.setup(e)
			ev := tt.event
			ev.Kind, ev.Action, ev.Repo, ev.Number, ev.Actor, ev.Body = "issue_comment", "created", repo, 1, "octocat", "@forecrew bug"
			must(t, e.eng.handle(t.Context(), ev))
			if r := e.replies(); len(r) != 1 || !strings.Contains(r[0], tt.want) || strings.Contains(r[0], "invalid input") {
				t.Fatalf("replies = %q, want %q", r, tt.want)
			}
		})
	}
}

func TestCommandBugErrorsAndLimits(t *testing.T) {
	e := newEnv(t)
	e.bugIssue(strings.Repeat("é", maxBugBody))
	f := e.bugs()
	e.send("d1", "issue_comment", fixture(t, "issue_comment_bug.json"))
	if len(f.issues) != 1 || len(f.issues[0].Body) > maxBugBody+100 || !strings.HasSuffix(f.issues[0].Body, "(cut: the issue is longer)") {
		t.Errorf("body of %d bytes", len(f.issues[0].Body))
	}
	f.err = fmt.Errorf("engine down")
	ev := github.Event{Kind: "issue_comment", Action: "created", Repo: repo, Number: 1, Actor: "octocat", Body: "@forecrew bug"}
	if err := e.eng.handle(t.Context(), ev); err == nil {
		t.Error("a failure of the reporter was swallowed")
	}
	ev.Number = 9
	if err := e.eng.handle(t.Context(), ev); err == nil {
		t.Error("a missing issue was swallowed")
	}
}

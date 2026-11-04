package testkit

import (
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

// events collects what the tracker emits, parsed as forecrewd parses it.
func events(t *testing.T, tr *Tracker) *[]github.Event {
	t.Helper()
	var got []github.Event
	tr.OnEvent(func(e Event) {
		ev, err := github.ParseEvent(e.Kind, e.Payload)
		must(t, err)
		got = append(got, ev)
	})
	return &got
}

func TestTrackerEvents(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	got := events(t, tr)
	must(t, tr.AddBranch("acme/app", "agent/a"))
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "A", Body: "body", Head: "agent/a", Base: "main"})
	must(t, err)

	must(t, tr.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksPending, 1))
	must(t, tr.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksFailure, 1))
	must(t, tr.SetCheckStatus("acme/app", pr.HeadSHA, github.ChecksSuccess, 1))
	must(t, tr.RequestChecks("acme/app", pr.HeadSHA))
	_, err = tr.CreateComment(ctx, "acme/app", pr.Number, "from the bot")
	must(t, err)
	_, err = tr.AddComment("acme/app", pr.Number, "octocat", "@forecrew approve")
	must(t, err)
	issue, _ := tr.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "I"})
	_, err = tr.AddComment("acme/app", issue.Number, "octocat", "a thought")
	must(t, err)
	_, err = tr.MergePR(ctx, "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	must(t, tr.AddBranch("acme/app", "agent/b"))
	closed, _ := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "B", Head: "agent/b", Base: "main"})
	must(t, tr.ClosePR("acme/app", closed.Number))

	want := []github.Event{
		{Kind: "check_suite", Action: "completed", Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Conclusion: "failure", Actor: "github-actions[bot]"},
		{Kind: "check_suite", Action: "completed", Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Conclusion: "success", Actor: "github-actions[bot]"},
		{Kind: "check_suite", Action: "requested", Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Actor: "github-actions[bot]"},
		{Kind: "issue_comment", Action: "created", Repo: "acme/app", Number: pr.Number, Actor: "octocat", Body: "@forecrew approve", IsPR: true},
		{Kind: "issue_comment", Action: "created", Repo: "acme/app", Number: issue.Number, Actor: "octocat", Body: "a thought"},
		{Kind: "pull_request", Action: "closed", Repo: "acme/app", Number: pr.Number, SHA: pr.HeadSHA, Actor: "octocat", Body: "body", Merged: true},
		{Kind: "pull_request", Action: "closed", Repo: "acme/app", Number: closed.Number, SHA: closed.HeadSHA, Actor: "octocat"},
	}
	if len(*got) != len(want) {
		t.Fatalf("events = %+v", *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, (*got)[i], want[i])
		}
	}
}

func TestTrackerEventsOnErrors(t *testing.T) {
	tr, _ := newTracker(t)
	got := events(t, tr)
	_, err := tr.MergePR(t.Context(), "acme/app", 9, github.MergeInput{})
	wantErr(t, err, github.ErrNotFound)
	wantErr(t, tr.ClosePR("acme/app", 9), github.ErrNotFound)
	_, err = tr.AddComment("acme/app", 9, "octocat", "x")
	wantErr(t, err, github.ErrNotFound)
	wantErr(t, tr.SetCheckStatus("acme/other", "sha", github.ChecksSuccess, 1), github.ErrNotInstalled)
	wantErr(t, tr.RequestChecks("acme/other", "sha"), github.ErrNotInstalled)
	if len(*got) != 0 {
		t.Errorf("events = %+v", *got)
	}
}

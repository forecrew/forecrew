package testkit

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

func newTracker(t *testing.T) (*Tracker, *Clock) {
	t.Helper()
	clock := NewClock(t0)
	tr := NewTracker(clock.Now)
	tr.Install("acme/app")
	return tr, clock
}

func TestTrackerInstall(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	if ok, _ := tr.IsInstalled(ctx, "ACME/app"); !ok {
		t.Fatal("not installed")
	}
	if ok, _ := tr.IsInstalled(ctx, "acme/other"); ok {
		t.Fatal("other installed")
	}
	_, err := tr.CreateIssue(ctx, "acme/other", github.IssueInput{})
	wantErr(t, err, github.ErrNotInstalled)
	tok, err := tr.InstallationToken(ctx, "acme/app", github.AccessWrite)
	if err != nil || tok.Value == "" || !tok.ExpiresAt.Equal(t0.Add(time.Hour)) || tr.Tokens != 1 {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	_, err = tr.InstallationToken(ctx, "acme/other", github.AccessRead)
	wantErr(t, err, github.ErrNotInstalled)
}

func TestTrackerIssues(t *testing.T) {
	tr, clock := newTracker(t)
	ctx := t.Context()
	parent, err := tr.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "Goal", Labels: []string{"forecrew"}})
	must(t, err)
	child, _ := tr.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "Task"})
	if parent.Number != 1 || child.Number != 2 || parent.State != "open" {
		t.Fatalf("issues = %+v %+v", parent, child)
	}
	must(t, tr.AddSubIssue(ctx, "acme/app", 1, 2))
	must(t, tr.AddSubIssue(ctx, "acme/app", 1, 2))
	wantErr(t, tr.AddSubIssue(ctx, "acme/app", 1, 99), github.ErrNotFound)
	wantErr(t, tr.AddSubIssue(ctx, "acme/app", 99, 1), github.ErrNotFound)
	subs, err := tr.ListSubIssues(ctx, "acme/app", 1)
	if err != nil || len(subs) != 1 || subs[0].Title != "Task" {
		t.Fatalf("subs = %+v, %v", subs, err)
	}
	_, err = tr.ListSubIssues(ctx, "acme/app", 99)
	wantErr(t, err, github.ErrNotFound)

	closed, title := "closed", "Goal v2"
	upd, err := tr.UpdateIssue(ctx, "acme/app", 1, github.IssueUpdate{Title: &title, State: &closed})
	if err != nil || upd.Title != title || upd.State != "closed" {
		t.Fatalf("update = %+v, %v", upd, err)
	}
	_, err = tr.UpdateIssue(ctx, "acme/app", 99, github.IssueUpdate{})
	wantErr(t, err, github.ErrNotFound)
	got, err := tr.GetIssue(ctx, "acme/app", 1)
	if err != nil || got.Title != title {
		t.Fatalf("get = %+v, %v", got, err)
	}
	_, err = tr.GetIssue(ctx, "acme/app", 99)
	wantErr(t, err, github.ErrNotFound)

	must(t, tr.EnsureLabel(ctx, "acme/app", github.Label{Name: "Needs-You", Color: "ff0000"}))
	if l, err := tr.Label("acme/app", "needs-you"); err != nil || l.Color != "ff0000" {
		t.Fatalf("label = %+v, %v", l, err)
	}
	_, err = tr.Label("acme/app", "missing")
	wantErr(t, err, github.ErrNotFound)
	must(t, tr.AddLabels(ctx, "acme/app", 2, "Needs-You", "Needs-You"))
	wantErr(t, tr.AddLabels(ctx, "acme/app", 2, "missing"), github.ErrNotFound)
	wantErr(t, tr.AddLabels(ctx, "acme/app", 99, "Needs-You"), github.ErrNotFound)
	if got, _ := tr.GetIssue(ctx, "acme/app", 2); len(got.Labels) != 1 {
		t.Fatalf("labels = %v", got.Labels)
	}
	must(t, tr.RemoveLabel(ctx, "acme/app", 2, "needs-you"))
	must(t, tr.RemoveLabel(ctx, "acme/app", 2, "needs-you"))
	if got, _ := tr.GetIssue(ctx, "acme/app", 2); len(got.Labels) != 0 {
		t.Fatalf("labels = %v", got.Labels)
	}

	_, err = tr.CreateComment(ctx, "acme/app", 1, "spec ready")
	must(t, err)
	clock.Advance(time.Minute)
	c, err := tr.AddComment("acme/app", 1, "octocat", "@forecrew approve")
	must(t, err)
	comments, err := tr.ListComments(ctx, "acme/app", 1, t0)
	if err != nil || len(comments) != 1 || comments[0].ID != c.ID || comments[0].Author != "octocat" {
		t.Fatalf("comments = %+v, %v", comments, err)
	}
	_, err = tr.ListComments(ctx, "acme/app", 99, t0)
	wantErr(t, err, github.ErrNotFound)
	_, err = tr.AddComment("acme/app", 99, "a", "b")
	wantErr(t, err, github.ErrNotFound)
}

func TestTrackerPullRequests(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "Add login", Head: "agent/task-1", Base: "main"})
	if err != nil || pr.Number != 1 || pr.HeadSHA != "agent/task-1-1" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	found, err := tr.FindPR(ctx, "acme/app", "agent/task-1")
	if err != nil || found.Number != 1 {
		t.Fatalf("FindPR = %+v, %v", found, err)
	}
	_, err = tr.FindPR(ctx, "acme/app", "agent/other")
	wantErr(t, err, github.ErrNotFound)
	body := "new body"
	upd, err := tr.UpdatePR(ctx, "acme/app", 1, github.PRUpdate{Body: &body})
	if err != nil || upd.Body != body {
		t.Fatalf("UpdatePR = %+v, %v", upd, err)
	}
	_, err = tr.UpdatePR(ctx, "acme/app", 9, github.PRUpdate{})
	wantErr(t, err, github.ErrNotFound)

	must(t, tr.SetHeadSHA("acme/app", 1, "sha2"))
	wantErr(t, tr.SetHeadSHA("acme/app", 9, "x"), github.ErrNotFound)
	rv, err := tr.CreateReview(ctx, "acme/app", 1, github.ReviewInput{Event: github.ReviewRequestChanges, Body: "tests"})
	if err != nil || rv.SHA != "sha2" {
		t.Fatalf("review = %+v, %v", rv, err)
	}
	_, err = tr.CreateReview(ctx, "acme/app", 9, github.ReviewInput{})
	wantErr(t, err, github.ErrNotFound)
	reviews, err := tr.ListReviews(ctx, "acme/app", 1)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
	_, err = tr.ListReviews(ctx, "acme/app", 9)
	wantErr(t, err, github.ErrNotFound)

	cs, err := tr.CheckStatus(ctx, "acme/app", "sha2")
	if err != nil || cs.State != github.ChecksNone {
		t.Fatalf("checks = %+v, %v", cs, err)
	}
	must(t, tr.SetCheckStatus("acme/app", "sha2", github.ChecksSuccess, 3))
	if cs, _ := tr.CheckStatus(ctx, "acme/app", "sha2"); cs.State != github.ChecksSuccess || cs.Total != 3 {
		t.Fatalf("checks = %+v", cs)
	}

	_, err = tr.MergePR(ctx, "acme/app", 1, github.MergeInput{Method: github.MergeSquash, SHA: "stale"})
	wantErr(t, err, github.ErrNotMergeable)
	sha, err := tr.MergePR(ctx, "acme/app", 1, github.MergeInput{Method: github.MergeSquash, SHA: "sha2"})
	if err != nil || sha != "merge-1" {
		t.Fatalf("merge = %s, %v", sha, err)
	}
	got, err := tr.GetPR(ctx, "acme/app", 1)
	if err != nil || !got.Merged || got.State != "closed" {
		t.Fatalf("merged pr = %+v, %v", got, err)
	}
	_, err = tr.MergePR(ctx, "acme/app", 1, github.MergeInput{})
	wantErr(t, err, github.ErrNotMergeable)
	wantErr(t, tr.ClosePR("acme/app", 9), github.ErrNotFound)
	_, err = tr.MergePR(ctx, "acme/app", 9, github.MergeInput{})
	wantErr(t, err, github.ErrNotFound)
	_, err = tr.GetPR(ctx, "acme/app", 9)
	wantErr(t, err, github.ErrNotFound)
}

func TestTrackerClosePR(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "x", Head: "agent/task-1", Base: "main"})
	must(t, err)
	must(t, tr.ClosePR("acme/app", pr.Number))
	got, err := tr.GetPR(ctx, "acme/app", pr.Number)
	if err != nil || got.State != "closed" || got.Merged {
		t.Fatalf("closed pr = %+v, %v", got, err)
	}
	if i, err := tr.GetIssue(ctx, "acme/app", pr.Number); err != nil || i.State != "closed" {
		t.Fatalf("issue = %+v, %v", i, err)
	}
}

func TestTrackerBranchExists(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	if ok, err := tr.BranchExists(ctx, "acme/app", "agent/task-1"); err != nil || ok {
		t.Fatalf("before = %v, %v", ok, err)
	}
	if err := tr.AddBranch("acme/app", "agent/task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Head: "agent/task-2", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"agent/task-1", "agent/task-2"} {
		if ok, err := tr.BranchExists(ctx, "acme/app", b); err != nil || !ok {
			t.Fatalf("%s = %v, %v", b, ok, err)
		}
	}
	_, err := tr.BranchExists(ctx, "acme/other", "x")
	wantErr(t, err, github.ErrNotInstalled)
	wantErr(t, tr.AddBranch("acme/other", "x"), github.ErrNotInstalled)
}

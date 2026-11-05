package testkit

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"slices"
	"strings"
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
	must(t, tr.AddBranch("acme/app", "agent/task-1"))
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
	if err != nil || !got.Merged || got.State != "closed" || got.MergeSHA != "merge-1" {
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

func TestTrackerFiles(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	must(t, tr.AddBranch("acme/app", "agent/task-1"))
	pr, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "x", Head: "agent/task-1", Base: "main"})
	must(t, err)
	must(t, tr.SetFile("acme/app", "agent/task-1", "a/b.txt", []byte("one")))
	wantErr(t, tr.SetFile("acme/other", "main", "x", nil), github.ErrNotInstalled)
	got, err := tr.GetFile(ctx, "acme/app", "agent/task-1", "a/b.txt")
	if err != nil || string(got) != "one" {
		t.Fatalf("file = %q, %v", got, err)
	}
	_, err = tr.GetFile(ctx, "acme/app", "main", "a/b.txt")
	wantErr(t, err, github.ErrNotFound)
	_, err = tr.GetFile(ctx, "acme/other", "main", "a/b.txt")
	wantErr(t, err, github.ErrNotInstalled)

	// Merging brings the head branch's files to the base.
	_, err = tr.MergePR(ctx, "acme/app", pr.Number, github.MergeInput{})
	must(t, err)
	for _, ref := range []string{"main", "merge-1"} {
		if got, err = tr.GetFile(ctx, "acme/app", ref, "a/b.txt"); err != nil || string(got) != "one" {
			t.Fatalf("file at %s = %q, %v", ref, got, err)
		}
	}
}

func TestTrackerClosePR(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	must(t, tr.AddBranch("acme/app", "agent/task-1"))
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
	must(t, tr.AddBranch("acme/app", "agent/task-2"))
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

func TestTrackerDiscoveryReads(t *testing.T) {
	tr, _ := newTracker(t)
	ctx := t.Context()
	must(t, tr.AddAutolink("acme/app", github.Autolink{KeyPrefix: "ENG-", URLTemplate: "https://acme.atlassian.net/browse/ENG-<num>"}))
	for range 3 {
		_, err := tr.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "i"})
		must(t, err)
	}
	must(t, tr.AddBranch("acme/app", "feature"))
	_, err := tr.CreatePR(ctx, "acme/app", github.PRInput{Title: "p", Head: "feature", Base: "main"})
	must(t, err)

	links, err := tr.ListAutolinks(ctx, "acme/app")
	must(t, err)
	if len(links) != 1 || links[0].KeyPrefix != "ENG-" {
		t.Errorf("autolinks = %+v", links)
	}
	issues, err := tr.ListRecentIssues(ctx, "acme/app", 2)
	must(t, err)
	if len(issues) != 2 || issues[0].Number != 3 || issues[1].Number != 2 {
		t.Errorf("recent issues = %+v, want numbers 3 and 2", issues)
	}
	pulls, err := tr.ListRecentPulls(ctx, "acme/app", 100)
	must(t, err)
	if len(pulls) != 1 || pulls[0].Number != 4 {
		t.Errorf("recent pulls = %+v, want PR 4", pulls)
	}

	must(t, tr.Forbid("acme/app"))
	_, err = tr.ListAutolinks(ctx, "acme/app")
	wantErr(t, err, github.ErrForbidden)
	_, err = tr.ListRecentIssues(ctx, "acme/app", 1)
	wantErr(t, err, github.ErrForbidden)
	_, err = tr.ListRecentPulls(ctx, "acme/app", 1)
	wantErr(t, err, github.ErrForbidden)
	_, err = tr.ListRecentPulls(ctx, "acme/other", 1)
	wantErr(t, err, github.ErrNotInstalled)
	wantErr(t, tr.AddAutolink("acme/other", github.Autolink{}), github.ErrNotInstalled)
	wantErr(t, tr.Forbid("acme/other"), github.ErrNotInstalled)
}

func repoWithTwoCommits(t *testing.T) *Tracker {
	t.Helper()
	tr := NewTracker(nil)
	tr.Install("acme/app")
	for ref, files := range map[string]map[string]string{
		"before": {"README.md": "# App\n", "internal/old.go": "package old\nfunc Old() {}\n", "keep.go": "package keep\nfunc Keep() {}\n"},
		"after":  {"README.md": "# App v2\n", "internal/new.go": "package old\nfunc Old() {}\n", "keep.go": "package keep\nfunc Keep() {}\n", "added.go": "package a\nfunc Added() {}\n"},
	} {
		for p, c := range files {
			must(t, tr.SetFile("acme/app", ref, p, []byte(c)))
		}
	}
	return tr
}

func TestTrackerTree(t *testing.T) {
	tr := repoWithTwoCommits(t)
	tree, err := tr.Tree(t.Context(), "acme/app", "before")
	if err != nil || len(tree) != 3 || tree[0].Path != "README.md" || len(tree[0].SHA) != 40 {
		t.Fatalf("tree = %+v, %v", tree, err)
	}
	// An empty file has git's well-known blob ID.
	if got := blobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("blob sha of an empty file = %s", got)
	}
	if _, err := tr.Tree(t.Context(), "acme/app", "gone"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("tree of an unknown ref: %v", err)
	}
}

func TestTrackerCompare(t *testing.T) {
	tr := repoWithTwoCommits(t)
	ctx := t.Context()
	cmp, err := tr.Compare(ctx, "acme/app", "before", "after")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]github.FileChange{}
	for _, f := range cmp.Files {
		got[f.Path] = f
	}
	if len(got) != 3 || got["README.md"].Status != github.FileModified || !strings.Contains(got["README.md"].Patch, "+# App v2") ||
		got["internal/new.go"].Status != github.FileRenamed || got["internal/new.go"].PreviousPath != "internal/old.go" ||
		got["added.go"].Status != github.FileAdded {
		t.Errorf("comparison = %+v", cmp)
	}
	back, _ := tr.Compare(ctx, "acme/app", "after", "before")
	var removed []string
	for _, f := range back.Files {
		if f.Status == github.FileRemoved {
			removed = append(removed, f.Path)
		}
	}
	if !slices.Equal(removed, []string{"added.go"}) {
		t.Errorf("removed = %v", removed)
	}
	if _, err := tr.Compare(ctx, "acme/app", "before", "gone"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("compare with an unknown ref: %v", err)
	}
}

func TestTrackerCommitsAndHeads(t *testing.T) {
	tr := repoWithTwoCommits(t)
	ctx := t.Context()
	c, err := tr.LastCommit(ctx, "acme/app", "after", "keep.go")
	if err != nil || c.Author != "octocat" {
		t.Errorf("default commit = %+v, %v", c, err)
	}
	must(t, tr.SetLastCommit("acme/app", "keep.go", github.Commit{SHA: "x", Author: "bot", Bot: true}))
	if c, _ = tr.LastCommit(ctx, "acme/app", "after", "keep.go"); !c.Bot {
		t.Errorf("set commit = %+v", c)
	}
	if _, err := tr.LastCommit(ctx, "acme/app", "after", "nope"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("commit of a missing file: %v", err)
	}
	if _, err := tr.HeadSHA(ctx, "acme/app", "main"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("head of an unknown branch: %v", err)
	}
	must(t, tr.SetHeadOfBranch("acme/app", "main", "after"))
	if sha, err := tr.HeadSHA(ctx, "acme/app", "main"); err != nil || sha != "after" {
		t.Errorf("head = %q, %v", sha, err)
	}
}

func TestTrackerArchive(t *testing.T) {
	tr := repoWithTwoCommits(t)
	rc, err := tr.Archive(t.Context(), "acme/app", "after")
	must(t, err)
	gz, err := gzip.NewReader(rc)
	must(t, err)
	tr2 := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr2.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if len(names) != 4 || names[0] != "acme-app-after/README.md" {
		t.Errorf("archive = %v", names)
	}
	if _, err := tr.Archive(t.Context(), "acme/app", "gone"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("archive of an unknown ref: %v", err)
	}
}

// Like GitHub, the fake refuses a pull request from a branch that is not there.
func TestTrackerCreatePRNeedsTheBranch(t *testing.T) {
	tr := NewTracker(nil)
	tr.Install("acme/app")
	if _, err := tr.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "x", Head: "agent/none", Base: "main"}); !errors.Is(err, github.ErrInvalid) {
		t.Fatalf("CreatePR without a branch = %v", err)
	}
	must(t, tr.AddBranch("acme/app", "agent/none"))
	must(t, tr.AddBranch("acme/app", "agent/none"))
	if _, err := tr.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "x", Head: "agent/none", Base: "main"}); err != nil {
		t.Errorf("CreatePR with a branch: %v", err)
	}
}

func TestTrackerUpdateCommentAndCheckRun(t *testing.T) {
	tr := NewTracker(nil)
	tr.Install("acme/app")
	ctx := t.Context()
	issue, err := tr.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "t"})
	must(t, err)
	mine, err := tr.CreateComment(ctx, "acme/app", issue.Number, "v1")
	must(t, err)
	theirs, err := tr.AddComment("acme/app", issue.Number, "ana", "hi")
	must(t, err)
	got, err := tr.UpdateComment(ctx, "acme/app", mine.ID, "v2")
	if err != nil || got.Body != "v2" || got.ID != mine.ID {
		t.Fatalf("UpdateComment = %+v, %v", got, err)
	}
	if cs, _ := tr.ListComments(ctx, "acme/app", issue.Number, time.Time{}); len(cs) != 2 || cs[0].Body != "v2" {
		t.Fatalf("comments = %+v", cs)
	}
	if bot, err := tr.Bot(ctx); err != nil || bot != TrackerBot || mine.Author != bot {
		t.Fatalf("Bot = %q, %v", bot, err)
	}
	// Like a write token, the fake edits anybody's comment.
	if c, err := tr.UpdateComment(ctx, "acme/app", theirs.ID, "x"); err != nil || c.Author != "ana" || c.Body != "x" {
		t.Fatalf("UpdateComment = %+v, %v", c, err)
	}
	_, err = tr.UpdateComment(ctx, "acme/app", 999, "x")
	wantErr(t, err, github.ErrNotFound)
	_, err = tr.UpdateComment(ctx, "acme/none", 1, "x")
	wantErr(t, err, github.ErrNotInstalled)

	in := github.CheckRunInput{Name: "forecrew/verification", SHA: "s1", Conclusion: github.ConclusionNeutral, Title: "t", Summary: "a"}
	must(t, tr.SetCheckRun(ctx, "acme/app", in))
	in.Conclusion = github.ConclusionSuccess
	must(t, tr.SetCheckRun(ctx, "acme/app", in))
	if run, ok := tr.CheckRun("acme/app", "s1", in.Name); !ok || run.Conclusion != github.ConclusionSuccess {
		t.Fatalf("CheckRun = %+v, %v", run, ok)
	}
	if _, ok := tr.CheckRun("acme/app", "s2", in.Name); ok {
		t.Fatal("check run on another sha")
	}
	must(t, tr.ForbidChecks("acme/app"))
	wantErr(t, tr.SetCheckRun(ctx, "acme/app", in), github.ErrForbidden)
}

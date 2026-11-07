package testkit

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

var _ github.Tracker = (*Tracker)(nil)

// Tracker is an in-memory github.Tracker. Repos must be installed with
// Install; calls on other repos fail with github.ErrNotInstalled. Issues and
// PRs share one number sequence per repo, as on GitHub.
type Tracker struct {
	mu    sync.Mutex
	now   func() time.Time
	repos map[string]*fakeRepo
	// Tokens counts the installation tokens issued.
	Tokens int
	sinks  []func(Event)
}

type fakeRepo struct {
	now       func() time.Time
	next      int
	issues    map[int]*fakeIssue
	prs       map[int]*github.PullRequest
	labels    map[string]github.Label
	checks    map[string]github.CheckStatus
	checkRuns map[string]github.CheckRunInput // by sha and name, see SetCheckRun
	noChecks  bool                            // SetCheckRun fails with github.ErrForbidden
	reviews   map[int][]github.Review
	commentID int64
	branches  map[string]bool
	files     map[string][]byte        // by ref and path, see fileKey
	deleted   map[string][]string      // by branch, see DeleteFile
	heads     map[string]string        // branch to commit, see SetHeadOfBranch
	commits   map[string]github.Commit // by path, see SetLastCommit
	remote    string                   // bare git repository, see UseRemote
	autolinks []github.Autolink
	forbidden bool // the discovery reads fail with github.ErrForbidden
}

type fakeIssue struct {
	issue    github.Issue
	comments []github.Comment
	children []int
	events   []github.IssueEvent
}

// NewTracker returns a tracker without repos. now defaults to the real UTC time.
func NewTracker(now func() time.Time) *Tracker {
	return &Tracker{now: orNow(now), repos: map[string]*fakeRepo{}}
}

// Install installs the GitHub App on repo.
func (t *Tracker) Install(repo string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repos[strings.ToLower(repo)] = &fakeRepo{
		now:    t.now,
		issues: map[int]*fakeIssue{}, prs: map[int]*github.PullRequest{}, labels: map[string]github.Label{},
		checks: map[string]github.CheckStatus{}, checkRuns: map[string]github.CheckRunInput{}, reviews: map[int][]github.Review{},
		branches: map[string]bool{}, files: map[string][]byte{}, heads: map[string]string{}, commits: map[string]github.Commit{}, deleted: map[string][]string{},
	}
}

// SetCheckStatus sets the aggregate check status of a commit. A success or
// a failure emits a check_suite completed event.
func (t *Tracker) SetCheckStatus(repo, sha string, state github.CheckState, total int) error {
	var ev Event
	err := t.with(repo, func(r *fakeRepo) error {
		r.checks[sha] = github.CheckStatus{SHA: sha, State: state, Total: total}
		if state == github.ChecksSuccess || state == github.ChecksFailure {
			ev = checkSuiteEvent(repo, "completed", sha, string(state), r.openPRsAt(sha))
		}
		return nil
	})
	if err == nil {
		t.fire(ev)
	}
	return err
}

// RequestChecks emits the check_suite requested event GitHub sends for a
// push, whether the repo has checks or not.
func (t *Tracker) RequestChecks(repo, sha string) error {
	var ev Event
	err := t.with(repo, func(r *fakeRepo) error {
		ev = checkSuiteEvent(repo, "requested", sha, "", r.openPRsAt(sha))
		return nil
	})
	if err == nil {
		t.fire(ev)
	}
	return err
}

// SetHeadSHA simulates a push to an open PR's branch.
func (t *Tracker) SetHeadSHA(repo string, number int, sha string) error {
	return t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err == nil {
			pr.HeadSHA = sha
		}
		return err
	})
}

// ClosePR simulates closing a PR without merging it. It emits a
// pull_request closed event.
func (t *Tracker) ClosePR(repo string, number int) error {
	var ev Event
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err == nil {
			pr.State = "closed"
			r.issues[number].issue.State = "closed"
			ev = prClosedEvent(repo, *pr)
		}
		return err
	})
	if err == nil {
		t.fire(ev)
	}
	return err
}

// AddComment simulates a comment by a GitHub user. It emits an
// issue_comment created event.
func (t *Tracker) AddComment(repo string, number int, author, body string) (github.Comment, error) {
	var ev Event
	c, err := t.addComment(repo, number, author, body, &ev)
	if err == nil {
		t.fire(ev)
	}
	return c, err
}

func (t *Tracker) addComment(repo string, number int, author, body string, ev *Event) (github.Comment, error) {
	var c github.Comment
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		r.commentID++
		c = github.Comment{ID: r.commentID, Author: author, Bot: author == TrackerBot, Body: body, CreatedAt: t.now()}
		i.comments = append(i.comments, c)
		i.issue.UpdatedAt = t.now()
		_, isPR := r.prs[number]
		*ev = commentEvent(repo, number, isPR, c)
		return nil
	})
	return c, err
}

func (t *Tracker) with(repo string, fn func(r *fakeRepo) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.repos[strings.ToLower(repo)]
	if !ok {
		return fmt.Errorf("%s: %w", repo, github.ErrNotInstalled)
	}
	return fn(r)
}

func (r *fakeRepo) issue(number int) (*fakeIssue, error) {
	i, ok := r.issues[number]
	if !ok {
		return nil, fmt.Errorf("issue %d: %w", number, github.ErrNotFound)
	}
	return i, nil
}

func (r *fakeRepo) pr(number int) (*github.PullRequest, error) {
	pr, ok := r.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d: %w", number, github.ErrNotFound)
	}
	r.syncHead(pr)
	return pr, nil
}

// syncHead moves the head of an open PR to its branch in the remote.
func (r *fakeRepo) syncHead(pr *github.PullRequest) {
	if sha, ok := r.branchSHA(pr.Head); ok && pr.State == "open" {
		pr.HeadSHA = sha
	}
}

func (r *fakeRepo) newIssue(repo string, title, body string, labels []string) *fakeIssue {
	r.next++
	i := &fakeIssue{issue: github.Issue{
		Number: r.next, Title: title, Body: body, State: "open", Labels: slices.Clone(labels),
		URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, r.next), UpdatedAt: r.now(),
	}}
	r.issues[r.next] = i
	return i
}

// IsInstalled implements github.Tracker.
func (t *Tracker) IsInstalled(_ context.Context, repo string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.repos[strings.ToLower(repo)]
	return ok, nil
}

// CreateIssue implements github.Tracker.
func (t *Tracker) CreateIssue(_ context.Context, repo string, in github.IssueInput) (github.Issue, error) {
	var out github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		out = r.newIssue(repo, in.Title, in.Body, in.Labels).issue
		return nil
	})
	return out, err
}

// GetIssue implements github.Tracker.
func (t *Tracker) GetIssue(_ context.Context, repo string, number int) (github.Issue, error) {
	var out github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err == nil {
			out = i.issue
		}
		return err
	})
	return out, err
}

// UpdateIssue implements github.Tracker.
func (t *Tracker) UpdateIssue(_ context.Context, repo string, number int, in github.IssueUpdate) (github.Issue, error) {
	var out github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		setIf(&i.issue.Title, in.Title)
		setIf(&i.issue.Body, in.Body)
		setIf(&i.issue.State, in.State)
		i.issue.UpdatedAt = t.now()
		out = i.issue
		return nil
	})
	return out, err
}

func setIf(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// AddSubIssue implements github.Tracker.
func (t *Tracker) AddSubIssue(_ context.Context, repo string, parent, child int) error {
	return t.with(repo, func(r *fakeRepo) error {
		p, err := r.issue(parent)
		if err != nil {
			return err
		}
		if _, err := r.issue(child); err != nil {
			return err
		}
		if !slices.Contains(p.children, child) {
			p.children = append(p.children, child)
		}
		return nil
	})
}

// ListSubIssues implements github.Tracker.
func (t *Tracker) ListSubIssues(_ context.Context, repo string, parent int) ([]github.Issue, error) {
	var out []github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		p, err := r.issue(parent)
		if err != nil {
			return err
		}
		for _, c := range p.children {
			out = append(out, r.issues[c].issue)
		}
		return nil
	})
	return out, err
}

// ListOpenIssues implements github.Tracker. The fake has no pull request
// among its issues.
func (t *Tracker) ListOpenIssues(_ context.Context, repo, label string) ([]github.Issue, error) {
	var out []github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		for _, n := range slices.Sorted(maps.Keys(r.issues)) {
			if i := r.issues[n].issue; i.State == "open" && slices.Contains(i.Labels, label) {
				out = append(out, i)
			}
		}
		return nil
	})
	return out, err
}

// ListIssuesSince implements github.Tracker.
func (t *Tracker) ListIssuesSince(_ context.Context, repo string, since time.Time) ([]github.Issue, error) {
	var out []github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		for _, n := range slices.Sorted(maps.Keys(r.issues)) {
			if i := r.issues[n].issue; !i.UpdatedAt.Before(since) {
				out = append(out, i)
			}
		}
		return nil
	})
	slices.SortStableFunc(out, func(a, b github.Issue) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return out, err
}

// ListIssueEvents implements github.Tracker.
func (t *Tracker) ListIssueEvents(_ context.Context, repo string, number int) ([]github.IssueEvent, error) {
	var out []github.IssueEvent
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err == nil {
			out = slices.Clone(i.events)
		}
		return err
	})
	return out, err
}

// LabelIssue adds a label to an issue as a person does on GitHub.
func (t *Tracker) LabelIssue(repo string, number int, by github.User, label string) error {
	return t.editIssue(repo, number, func(i *fakeIssue) {
		i.issue.Labels = append(i.issue.Labels, label)
		i.events = append(i.events, github.IssueEvent{Event: github.IssueLabeled, Actor: by, Value: label, At: t.now()})
	})
}

// AssignIssue assigns an issue to a user as a person does on GitHub.
func (t *Tracker) AssignIssue(repo string, number int, by, to github.User) error {
	return t.editIssue(repo, number, func(i *fakeIssue) {
		i.issue.Assignees = append(i.issue.Assignees, to)
		i.events = append(i.events, github.IssueEvent{Event: github.IssueAssigned, Actor: by, Value: to.Login, At: t.now()})
	})
}

// EditIssue changes an issue directly, for what only GitHub can set: its
// type, author or pull request mark.
func (t *Tracker) EditIssue(repo string, number int, fn func(*github.Issue)) error {
	return t.editIssue(repo, number, func(i *fakeIssue) { fn(&i.issue) })
}

// CommentAs adds a person's comment to an issue.
func (t *Tracker) CommentAs(repo string, number int, by github.User, body string) (github.Comment, error) {
	var c github.Comment
	err := t.with(repo, func(r *fakeRepo) error {
		r.commentID++
		c = github.Comment{ID: r.commentID, Author: by.Login, AuthorID: by.ID, Bot: by.Bot, Body: body, CreatedAt: t.now()}
		return nil
	})
	if err != nil {
		return c, err
	}
	return c, t.editIssue(repo, number, func(i *fakeIssue) { i.comments = append(i.comments, c) })
}

func (t *Tracker) editIssue(repo string, number int, fn func(*fakeIssue)) error {
	return t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		fn(i)
		i.issue.UpdatedAt = t.now()
		return nil
	})
}

// CreateComment implements github.Tracker. The author is "forecrew[bot]";
// the bot's own comments emit no event.
func (t *Tracker) CreateComment(_ context.Context, repo string, number int, body string) (github.Comment, error) {
	return t.addComment(repo, number, TrackerBot, body, new(Event))
}

// TrackerBot is the login of the bot that makes the comments of the fake
// tracker.
const TrackerBot = "forecrew[bot]"

// Bot implements github.Tracker.
func (t *Tracker) Bot(context.Context) (string, error) { return TrackerBot, nil }

// UpdateComment implements github.Tracker. Like a token with write access,
// it edits any comment.
func (t *Tracker) UpdateComment(_ context.Context, repo string, id int64, body string) (github.Comment, error) {
	var out github.Comment
	err := t.with(repo, func(r *fakeRepo) error {
		for _, i := range r.issues {
			for k := range i.comments {
				if c := &i.comments[k]; c.ID == id {
					c.Body = body
					out = *c
					return nil
				}
			}
		}
		return fmt.Errorf("comment %d: %w", id, github.ErrNotFound)
	})
	return out, err
}

// SetCheckRun implements github.Tracker: one run per sha and name, replaced
// by the next call.
func (t *Tracker) SetCheckRun(_ context.Context, repo string, in github.CheckRunInput) error {
	return t.with(repo, func(r *fakeRepo) error {
		if r.noChecks {
			return fmt.Errorf("check run %s: %w", in.Name, github.ErrForbidden)
		}
		r.checkRuns[in.SHA+"\x00"+in.Name] = in
		return nil
	})
}

// CheckRun returns the check run name published on sha by SetCheckRun.
func (t *Tracker) CheckRun(repo, sha, name string) (github.CheckRunInput, bool) {
	var out github.CheckRunInput
	var ok bool
	_ = t.with(repo, func(r *fakeRepo) error {
		out, ok = r.checkRuns[sha+"\x00"+name]
		return nil
	})
	return out, ok
}

// ForbidChecks makes SetCheckRun fail with github.ErrForbidden, as when the
// App lacks the checks permission.
func (t *Tracker) ForbidChecks(repo string) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.noChecks = true
		return nil
	})
}

// ListComments implements github.Tracker.
func (t *Tracker) ListComments(_ context.Context, repo string, number int, since time.Time) ([]github.Comment, error) {
	var out []github.Comment
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		for _, c := range i.comments {
			if c.CreatedAt.After(since) {
				out = append(out, c)
			}
		}
		return nil
	})
	return out, err
}

// EnsureLabel implements github.Tracker.
func (t *Tracker) EnsureLabel(_ context.Context, repo string, l github.Label) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.labels[strings.ToLower(l.Name)] = l
		return nil
	})
}

// Label returns a label of the repo.
func (t *Tracker) Label(repo, name string) (github.Label, error) {
	var out github.Label
	err := t.with(repo, func(r *fakeRepo) error {
		l, ok := r.labels[strings.ToLower(name)]
		if !ok {
			return fmt.Errorf("label %s: %w", name, github.ErrNotFound)
		}
		out = l
		return nil
	})
	return out, err
}

// AddLabels implements github.Tracker. Labels must exist (EnsureLabel).
func (t *Tracker) AddLabels(_ context.Context, repo string, number int, labels ...string) error {
	return t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		for _, l := range labels {
			if _, ok := r.labels[strings.ToLower(l)]; !ok {
				return fmt.Errorf("label %s: %w", l, github.ErrNotFound)
			}
			if !slices.Contains(i.issue.Labels, l) {
				i.issue.Labels = append(i.issue.Labels, l)
			}
		}
		return nil
	})
}

// RemoveLabel implements github.Tracker.
func (t *Tracker) RemoveLabel(_ context.Context, repo string, number int, label string) error {
	return t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err == nil {
			i.issue.Labels = slices.DeleteFunc(i.issue.Labels, func(l string) bool { return strings.EqualFold(l, label) })
		}
		return err
	})
}

// CreatePR implements github.Tracker. The head SHA is "<head>-1" until
// SetHeadSHA changes it.
func (t *Tracker) CreatePR(_ context.Context, repo string, in github.PRInput) (github.PullRequest, error) {
	var out github.PullRequest
	err := t.with(repo, func(r *fakeRepo) error {
		// GitHub refuses a pull request from a branch that does not exist.
		if _, inRemote := r.branchSHA(in.Head); !r.branches[in.Head] && !inRemote {
			return fmt.Errorf("create pull request from %s: the head branch does not exist: %w", in.Head, github.ErrInvalid)
		}
		i := r.newIssue(repo, in.Title, in.Body, nil)
		n := i.issue.Number
		r.prs[n] = &github.PullRequest{
			Number: n, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, HeadSHA: in.Head + "-1",
			State: "open", Draft: in.Draft, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n),
		}
		r.syncHead(r.prs[n])
		out = *r.prs[n]
		return nil
	})
	return out, err
}

// GetPR implements github.Tracker.
func (t *Tracker) GetPR(_ context.Context, repo string, number int) (github.PullRequest, error) {
	var out github.PullRequest
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err == nil {
			out = *pr
		}
		return err
	})
	return out, err
}

// FindPR implements github.Tracker.
func (t *Tracker) FindPR(_ context.Context, repo, branch string) (github.PullRequest, error) {
	var out github.PullRequest
	err := t.with(repo, func(r *fakeRepo) error {
		for _, pr := range r.prs {
			if pr.Head == branch && pr.State == "open" {
				r.syncHead(pr)
				out = *pr
				return nil
			}
		}
		return fmt.Errorf("pull request for %s: %w", branch, github.ErrNotFound)
	})
	return out, err
}

// UpdatePR implements github.Tracker.
func (t *Tracker) UpdatePR(_ context.Context, repo string, number int, in github.PRUpdate) (github.PullRequest, error) {
	var out github.PullRequest
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err != nil {
			return err
		}
		setIf(&pr.Title, in.Title)
		setIf(&pr.Body, in.Body)
		setIf(&pr.State, in.State)
		if in.State != nil {
			r.issues[number].issue.State = *in.State
		}
		out = *pr
		return nil
	})
	return out, err
}

// CreateReview implements github.Tracker.
func (t *Tracker) CreateReview(_ context.Context, repo string, number int, in github.ReviewInput) (github.Review, error) {
	var out github.Review
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err != nil {
			return err
		}
		out = github.Review{ID: int64(len(r.reviews[number]) + 1), Author: "forecrew[bot]", Event: in.Event, Body: in.Body, SHA: pr.HeadSHA}
		r.reviews[number] = append(r.reviews[number], out)
		return nil
	})
	return out, err
}

// ListReviews implements github.Tracker.
func (t *Tracker) ListReviews(_ context.Context, repo string, number int) ([]github.Review, error) {
	var out []github.Review
	err := t.with(repo, func(r *fakeRepo) error {
		if _, err := r.pr(number); err != nil {
			return err
		}
		out = slices.Clone(r.reviews[number])
		return nil
	})
	return out, err
}

// BranchExists implements github.Tracker. A branch exists when it is the head
// of a PR created through the fake or was added with AddBranch.
func (t *Tracker) BranchExists(_ context.Context, repo, branch string) (bool, error) {
	var ok bool
	err := t.with(repo, func(r *fakeRepo) error {
		_, inRemote := r.branchSHA(branch)
		ok = r.branches[branch] || inRemote
		for _, pr := range r.prs {
			ok = ok || pr.Head == branch
		}
		return nil
	})
	return ok, err
}

func fileKey(ref, path string) string { return ref + "\x00" + path }

// SetFile simulates a commit of the file at path to the branch ref. Merging
// a PR copies the files of its head branch to its base and to the merge
// commit.
func (t *Tracker) SetFile(repo, ref, path string, content []byte) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.files[fileKey(ref, path)] = slices.Clone(content)
		return nil
	})
}

// GetFile implements github.Tracker.
func (t *Tracker) GetFile(_ context.Context, repo, ref, path string) ([]byte, error) {
	var out []byte
	err := t.with(repo, func(r *fakeRepo) error {
		b, ok := r.files[fileKey(ref, path)]
		if r.remote != "" {
			if content, err := gitRaw(r.remote, "cat-file", "blob", ref+":"+path); err == nil {
				b, ok = content, true
			}
		}
		if !ok {
			return fmt.Errorf("%s at %s: %w", path, ref, github.ErrNotFound)
		}
		out = slices.Clone(b)
		return nil
	})
	return out, err
}

// CreateBranch implements github.Tracker: the branch starts with the files
// set at sha.
func (t *Tracker) CreateBranch(_ context.Context, repo, branch, sha string) error {
	return t.with(repo, func(r *fakeRepo) error {
		if r.branches[branch] {
			return nil
		}
		r.branches[branch] = true
		r.heads[branch] = sha
		for p, b := range r.refFiles(sha) {
			r.files[fileKey(branch, p)] = b
		}
		return nil
	})
}

// WriteFile implements github.Tracker.
func (t *Tracker) WriteFile(_ context.Context, repo, branch, path string, content []byte, _ string) (bool, error) {
	changed := false
	err := t.with(repo, func(r *fakeRepo) error {
		if _, inRemote := r.branchSHA(branch); !r.branches[branch] && !inRemote && r.heads[branch] == "" {
			return fmt.Errorf("write %s on %s: %w", path, branch, github.ErrInvalid)
		}
		key := fileKey(branch, path)
		old, ok := r.files[key]
		if changed = !ok || !bytes.Equal(old, content); changed {
			r.files[key] = slices.Clone(content)
		}
		return nil
	})
	return changed, err
}

// DeleteFile implements github.Tracker. Merging the branch deletes the file
// from the base.
func (t *Tracker) DeleteFile(_ context.Context, repo, branch, path, _ string) (bool, error) {
	found := false
	err := t.with(repo, func(r *fakeRepo) error {
		key := fileKey(branch, path)
		if _, found = r.files[key]; found {
			delete(r.files, key)
			r.deleted[branch] = append(r.deleted[branch], path)
		}
		return nil
	})
	return found, err
}

// AddBranch simulates a pushed branch.
func (t *Tracker) AddBranch(repo, branch string) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.branches[branch] = true
		return nil
	})
}

// CheckStatus implements github.Tracker. Commits without SetCheckStatus have
// no checks (state none).
func (t *Tracker) CheckStatus(_ context.Context, repo, sha string) (github.CheckStatus, error) {
	var out github.CheckStatus
	err := t.with(repo, func(r *fakeRepo) error {
		cs, ok := r.checks[sha]
		if !ok {
			cs = github.CheckStatus{SHA: sha, State: github.ChecksNone}
		}
		out = cs
		return nil
	})
	return out, err
}

// MergePR implements github.Tracker. The merge commit is "merge-<number>",
// or, for a repo with a remote (UseRemote), the squash commit made there. It
// emits a pull_request closed event.
func (t *Tracker) MergePR(_ context.Context, repo string, number int, in github.MergeInput) (string, error) {
	var sha string
	var ev Event
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err != nil {
			return err
		}
		if pr.State != "open" || (in.SHA != "" && in.SHA != pr.HeadSHA) {
			return fmt.Errorf("pull request %d: %w", number, github.ErrNotMergeable)
		}
		sha = fmt.Sprintf("merge-%d", number)
		if r.remote != "" {
			if sha, err = r.squashMerge(pr.Base, pr.HeadSHA, fmt.Sprintf("%s (#%d)", pr.Title, number)); err != nil {
				return fmt.Errorf("pull request %d: %w: %w", number, github.ErrNotMergeable, err)
			}
		}
		pr.State, pr.Merged, pr.MergeSHA = "closed", true, sha
		r.issues[number].issue.State = "closed"
		for k, b := range maps.Clone(r.files) {
			if ref, path, _ := strings.Cut(k, "\x00"); ref == pr.Head {
				r.files[fileKey(pr.Base, path)] = b
				r.files[fileKey(sha, path)] = b
			}
		}
		for _, path := range r.deleted[pr.Head] {
			delete(r.files, fileKey(pr.Base, path))
		}
		ev = prClosedEvent(repo, *pr)
		return nil
	})
	if err == nil {
		t.fire(ev)
	}
	return sha, err
}

// InstallationToken implements github.Tracker. Tokens last an hour.
func (t *Tracker) InstallationToken(_ context.Context, repo string, access github.TokenAccess) (github.Token, error) {
	var out github.Token
	err := t.with(repo, func(*fakeRepo) error {
		t.Tokens++
		out = github.Token{Value: fmt.Sprintf("ghs_fake_%s_%d", access, t.Tokens), ExpiresAt: t.now().Add(time.Hour)}
		return nil
	})
	return out, err
}

// AddAutolink adds an autolink to repo.
func (t *Tracker) AddAutolink(repo string, a github.Autolink) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.autolinks = append(r.autolinks, a)
		return nil
	})
}

// Forbid makes the discovery reads of repo (autolinks, recent issues and
// pull requests) fail with github.ErrForbidden, as when the App lacks a
// permission.
func (t *Tracker) Forbid(repo string) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.forbidden = true
		return nil
	})
}

// denied returns the error of a discovery read of repo, or nil.
func (r *fakeRepo) denied(repo string) error {
	if r.forbidden {
		return fmt.Errorf("discover %s: %w", repo, github.ErrForbidden)
	}
	return nil
}

// ListAutolinks implements github.Tracker.
func (t *Tracker) ListAutolinks(_ context.Context, repo string) ([]github.Autolink, error) {
	var out []github.Autolink
	err := t.with(repo, func(r *fakeRepo) error {
		if err := r.denied(repo); err != nil {
			return err
		}
		out = slices.Clone(r.autolinks)
		return nil
	})
	return out, err
}

// ListRecentIssues implements github.Tracker. Pull requests are not issues.
func (t *Tracker) ListRecentIssues(_ context.Context, repo string, limit int) ([]github.Issue, error) {
	var out []github.Issue
	err := t.with(repo, func(r *fakeRepo) error {
		if err := r.denied(repo); err != nil {
			return err
		}
		var numbers []int
		for n := range r.issues {
			if r.prs[n] == nil { // a PR shares its number with an issue, as on GitHub
				numbers = append(numbers, n)
			}
		}
		for _, n := range recentFirst(numbers, limit) {
			out = append(out, r.issues[n].issue)
		}
		return nil
	})
	return out, err
}

// ListRecentPulls implements github.Tracker.
func (t *Tracker) ListRecentPulls(_ context.Context, repo string, limit int) ([]github.PullRequest, error) {
	var out []github.PullRequest
	err := t.with(repo, func(r *fakeRepo) error {
		if err := r.denied(repo); err != nil {
			return err
		}
		for _, n := range recentFirst(slices.Collect(maps.Keys(r.prs)), limit) {
			out = append(out, *r.prs[n])
		}
		return nil
	})
	return out, err
}

// recentFirst returns the largest numbers, newest first, at most limit.
func recentFirst(numbers []int, limit int) []int {
	ns := slices.Clone(numbers)
	slices.Sort(ns)
	slices.Reverse(ns)
	if limit > 0 && len(ns) > limit {
		ns = ns[:limit]
	}
	return ns
}

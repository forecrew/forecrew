package testkit

import (
	"context"
	"fmt"
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
}

type fakeRepo struct {
	next      int
	issues    map[int]*fakeIssue
	prs       map[int]*github.PullRequest
	labels    map[string]github.Label
	checks    map[string]github.CheckStatus
	reviews   map[int][]github.Review
	commentID int64
}

type fakeIssue struct {
	issue    github.Issue
	comments []github.Comment
	children []int
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
		issues: map[int]*fakeIssue{}, prs: map[int]*github.PullRequest{}, labels: map[string]github.Label{},
		checks: map[string]github.CheckStatus{}, reviews: map[int][]github.Review{},
	}
}

// SetCheckStatus sets the aggregate check status of a commit.
func (t *Tracker) SetCheckStatus(repo, sha string, state github.CheckState, total int) error {
	return t.with(repo, func(r *fakeRepo) error {
		r.checks[sha] = github.CheckStatus{SHA: sha, State: state, Total: total}
		return nil
	})
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

// AddComment simulates a comment by a GitHub user.
func (t *Tracker) AddComment(repo string, number int, author, body string) (github.Comment, error) {
	var c github.Comment
	err := t.with(repo, func(r *fakeRepo) error {
		i, err := r.issue(number)
		if err != nil {
			return err
		}
		r.commentID++
		c = github.Comment{ID: r.commentID, Author: author, Body: body, CreatedAt: t.now()}
		i.comments = append(i.comments, c)
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
	return pr, nil
}

func (r *fakeRepo) newIssue(repo string, title, body string, labels []string) *fakeIssue {
	r.next++
	i := &fakeIssue{issue: github.Issue{
		Number: r.next, Title: title, Body: body, State: "open", Labels: slices.Clone(labels),
		URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, r.next),
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

// CreateComment implements github.Tracker. The author is "forecrew[bot]".
func (t *Tracker) CreateComment(_ context.Context, repo string, number int, body string) (github.Comment, error) {
	return t.AddComment(repo, number, "forecrew[bot]", body)
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
		i := r.newIssue(repo, in.Title, in.Body, nil)
		n := i.issue.Number
		r.prs[n] = &github.PullRequest{
			Number: n, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, HeadSHA: in.Head + "-1",
			State: "open", Draft: in.Draft, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n),
		}
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

// MergePR implements github.Tracker. The merge commit is "merge-<number>".
func (t *Tracker) MergePR(_ context.Context, repo string, number int, in github.MergeInput) (string, error) {
	var sha string
	err := t.with(repo, func(r *fakeRepo) error {
		pr, err := r.pr(number)
		if err != nil {
			return err
		}
		if pr.State != "open" || (in.SHA != "" && in.SHA != pr.HeadSHA) {
			return fmt.Errorf("pull request %d: %w", number, github.ErrNotMergeable)
		}
		pr.State, pr.Merged = "closed", true
		r.issues[number].issue.State = "closed"
		sha = fmt.Sprintf("merge-%d", number)
		return nil
	})
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

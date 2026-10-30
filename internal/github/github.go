// Package github defines how forecrewd talks to GitHub through the Forecrew
// GitHub App. Repos are "owner/name". internal/testkit has an in-memory fake.
package github

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	// ErrNotFound means the issue, PR, label or repo does not exist.
	ErrNotFound = errors.New("github: not found")
	// ErrNotInstalled means the GitHub App is not installed on the repo.
	ErrNotInstalled = errors.New("github: app not installed")
	// ErrNotMergeable means GitHub refused the merge (conflicts, failing
	// required checks, head moved past MergeInput.SHA).
	ErrNotMergeable = errors.New("github: not mergeable")
	// ErrInvalid means GitHub rejected the request as invalid (HTTP 422) or
	// the repo name is not "owner/name".
	ErrInvalid = errors.New("github: invalid request")
	// ErrRateLimited means GitHub rate-limited the request and the wait was
	// too long, or retries ran out.
	ErrRateLimited = errors.New("github: rate limited")
)

// Issue is a GitHub issue.
type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"` // open or closed
	Labels []string `json:"labels"`
	URL    string   `json:"url"`
}

// IssueInput creates an issue.
type IssueInput struct {
	Title  string
	Body   string
	Labels []string
}

// IssueUpdate changes an issue; nil fields are left alone.
type IssueUpdate struct {
	Title *string
	Body  *string
	State *string // open or closed
}

// Comment is an issue or PR conversation comment.
type Comment struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"` // GitHub login
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Label is a repository label.
type Label struct {
	Name        string
	Color       string // six hex digits, no '#'
	Description string
}

// PullRequest is a GitHub pull request.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Head    string `json:"head"` // branch
	Base    string `json:"base"` // branch
	HeadSHA string `json:"head_sha"`
	State   string `json:"state"` // open or closed
	Merged  bool   `json:"merged"`
	// MergeSHA is the merge commit of a merged PR.
	MergeSHA string `json:"merge_sha,omitempty"`
	Draft    bool   `json:"draft"`
	URL      string `json:"url"`
	// Mergeable is nil while GitHub has not computed it yet.
	Mergeable *bool `json:"mergeable"`
}

// PRInput opens a pull request.
type PRInput struct {
	Title string
	Body  string
	Head  string
	Base  string
	Draft bool
}

// PRUpdate changes a pull request; nil fields are left alone.
type PRUpdate struct {
	Title *string
	Body  *string
}

// ReviewEvent is the verdict of a review.
type ReviewEvent string

// Review events.
const (
	ReviewComment        ReviewEvent = "COMMENT"
	ReviewApprove        ReviewEvent = "APPROVE"
	ReviewRequestChanges ReviewEvent = "REQUEST_CHANGES"
)

// Review is a pull request review.
type Review struct {
	ID     int64       `json:"id"`
	Author string      `json:"author"`
	Event  ReviewEvent `json:"event"` // GitHub review state mapped to the event names (APPROVED -> APPROVE, ...)
	Body   string      `json:"body"`
	SHA    string      `json:"sha"` // commit reviewed
}

// ReviewInput submits a review.
type ReviewInput struct {
	Event ReviewEvent
	Body  string
	// Comments are inline comments on the new side of the diff. Comments on
	// lines outside the diff are appended to Body instead.
	Comments []InlineComment
}

// InlineComment is an inline review comment.
type InlineComment struct {
	Path string
	Line int
	Body string
}

// CheckState is the aggregate state of the check runs and commit statuses
// of a commit.
type CheckState string

// Check states. Failure means at least one check run or status finished
// unsuccessfully (failure, cancelled, timed_out, action_required, error),
// even while others are still running. Success means every check finished
// successfully (success, neutral, skipped). Pending means no check failed and
// at least one has not finished. None means the commit has no checks.
const (
	ChecksPending CheckState = "pending"
	ChecksSuccess CheckState = "success"
	ChecksFailure CheckState = "failure"
	ChecksNone    CheckState = "none"
)

// CheckStatus is the aggregate check status of a commit.
type CheckStatus struct {
	SHA   string     `json:"sha"`
	State CheckState `json:"state"`
	Total int        `json:"total"` // check runs plus commit statuses
}

// MergeMethod is how a PR is merged.
type MergeMethod string

// Merge methods.
const (
	MergeMerge  MergeMethod = "merge"
	MergeSquash MergeMethod = "squash"
	MergeRebase MergeMethod = "rebase"
)

// MergeInput merges a PR. SHA, when set, must be the PR's head.
type MergeInput struct {
	Method MergeMethod
	SHA    string
}

// TokenAccess is the contents permission of an installation token.
type TokenAccess string

// Token access levels: write for writing roles, read for read-only roles.
const (
	AccessRead  TokenAccess = "read"
	AccessWrite TokenAccess = "write"
)

// Token is a short-lived installation token scoped to one repo.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Tracker is the GitHub API surface forecrewd uses.
type Tracker interface {
	// IsInstalled reports whether the GitHub App is installed on repo.
	IsInstalled(ctx context.Context, repo string) (bool, error)

	CreateIssue(ctx context.Context, repo string, in IssueInput) (Issue, error)
	GetIssue(ctx context.Context, repo string, number int) (Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int, in IssueUpdate) (Issue, error)
	// AddSubIssue makes child a sub-issue of parent.
	AddSubIssue(ctx context.Context, repo string, parent, child int) error
	ListSubIssues(ctx context.Context, repo string, parent int) ([]Issue, error)

	// CreateComment comments on an issue or PR conversation.
	CreateComment(ctx context.Context, repo string, number int, body string) (Comment, error)
	// ListComments returns the comments created after since, oldest first.
	ListComments(ctx context.Context, repo string, number int, since time.Time) ([]Comment, error)

	// EnsureLabel creates the label or updates its color and description.
	EnsureLabel(ctx context.Context, repo string, l Label) error
	AddLabels(ctx context.Context, repo string, number int, labels ...string) error
	// RemoveLabel succeeds when the label is not on the issue.
	RemoveLabel(ctx context.Context, repo string, number int, label string) error

	CreatePR(ctx context.Context, repo string, in PRInput) (PullRequest, error)
	GetPR(ctx context.Context, repo string, number int) (PullRequest, error)
	// FindPR returns the open PR whose head is branch, or ErrNotFound.
	FindPR(ctx context.Context, repo, branch string) (PullRequest, error)
	UpdatePR(ctx context.Context, repo string, number int, in PRUpdate) (PullRequest, error)
	CreateReview(ctx context.Context, repo string, number int, in ReviewInput) (Review, error)
	ListReviews(ctx context.Context, repo string, number int) ([]Review, error)
	// BranchExists reports whether branch exists in repo.
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	// GetFile returns the content of the file at path in repo at ref (a
	// branch, tag or commit SHA). ErrNotFound when the file or ref does not
	// exist or path is a directory.
	GetFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	// CheckStatus aggregates the check runs and commit statuses of sha.
	CheckStatus(ctx context.Context, repo, sha string) (CheckStatus, error)
	// MergePR merges and returns the merge commit SHA; ErrNotMergeable when
	// GitHub refuses.
	MergePR(ctx context.Context, repo string, number int, in MergeInput) (string, error)

	// InstallationToken returns a token scoped to repo with contents access.
	InstallationToken(ctx context.Context, repo string, access TokenAccess) (Token, error)
}

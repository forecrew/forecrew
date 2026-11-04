package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v93/github"
)

// Webhook errors.
var (
	// ErrBadSignature means X-Hub-Signature-256 is missing, malformed, uses
	// another algorithm or does not match the body.
	ErrBadSignature = errors.New("github: bad webhook signature")
	// ErrUnsupportedEvent means the event type is not one Forecrew uses (for
	// example ping); callers acknowledge and ignore it.
	ErrUnsupportedEvent = errors.New("github: unsupported webhook event")
	// ErrBadPayload means the payload is not valid JSON for its event type.
	ErrBadPayload = errors.New("github: bad webhook payload")
)

const signaturePrefix = "sha256="

// VerifySignature checks the X-Hub-Signature-256 header of a delivery against
// the webhook secret, comparing in constant time. An empty secret never
// verifies: anybody could compute that signature.
func VerifySignature(secret, body []byte, header string) error {
	if len(secret) == 0 {
		return fmt.Errorf("%w: empty secret", ErrBadSignature)
	}
	hexSum, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok {
		if header == "" {
			return fmt.Errorf("%w: header missing", ErrBadSignature)
		}
		return fmt.Errorf("%w: want %s", ErrBadSignature, signaturePrefix)
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return fmt.Errorf("%w: not hex", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("%w: mismatch", ErrBadSignature)
	}
	return nil
}

// Event is the part of a webhook delivery Forecrew looks at. It carries facts
// only; T11 turns them into signals. Fields that do not apply to an event
// kind are zero.
type Event struct {
	Kind   string // X-GitHub-Event: issues, issue_comment, pull_request, ...
	Action string // opened, closed, created, submitted, completed, ...
	Repo   string // owner/name
	Number int    // issue or PR number; for check events the first associated PR, or 0
	Actor  string // login that caused the event
	SHA    string // head SHA for PR, review, check and status events
	// Conclusion is the check run or suite conclusion, or the commit status
	// state (success, failure, error, pending).
	Conclusion  string
	Body        string // comment, review or issue/PR body text of the triggering object
	Merged      bool   // pull_request closed by a merge
	IsPR        bool   // issue_comment on a pull request
	ReviewState string // pull_request_review: approved, changes_requested, commented, ...
	RunID       int64  // workflow_run: the run; SHA is its head and Conclusion its conclusion
	// Push events: the ref pushed (refs/heads/main), the commit it pointed to
	// before (SHA is the one after), and the default branch of the repo.
	Ref           string
	Before        string
	DefaultBranch string
}

// ParseEvent parses a webhook payload. kind is the X-GitHub-Event header.
func ParseEvent(kind string, payload []byte) (Event, error) {
	switch kind {
	case "issues", "issue_comment", "pull_request", "pull_request_review", "check_suite", "check_run", "status", "workflow_run", "push":
	default:
		return Event{}, fmt.Errorf("%w: %q", ErrUnsupportedEvent, kind)
	}
	raw, err := gh.ParseWebHook(kind, payload)
	if err != nil {
		return Event{}, fmt.Errorf("parse %s event: %w: %w", kind, ErrBadPayload, err)
	}
	e := Event{Kind: kind}
	switch p := raw.(type) {
	case *gh.IssuesEvent:
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.Number, e.Body = p.GetIssue().GetNumber(), p.GetIssue().GetBody()
	case *gh.IssueCommentEvent:
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.Number, e.Body = p.GetIssue().GetNumber(), p.GetComment().GetBody()
		e.IsPR = p.GetIssue().IsPullRequest()
	case *gh.PullRequestEvent:
		pr := p.GetPullRequest()
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.Number, e.SHA, e.Body, e.Merged = pr.GetNumber(), pr.GetHead().GetSHA(), pr.GetBody(), pr.GetMerged()
	case *gh.PullRequestReviewEvent:
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.Number, e.SHA = p.GetPullRequest().GetNumber(), p.GetReview().GetCommitID()
		e.Body, e.ReviewState = p.GetReview().GetBody(), strings.ToLower(p.GetReview().GetState())
	case *gh.CheckSuiteEvent:
		cs := p.GetCheckSuite()
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.SHA, e.Conclusion = cs.GetHeadSHA(), cs.GetConclusion()
		if prs := cs.PullRequests; len(prs) > 0 {
			e.Number = prs[0].GetNumber()
		}
	case *gh.CheckRunEvent:
		cr := p.GetCheckRun()
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.SHA, e.Conclusion = cr.GetHeadSHA(), cr.GetConclusion()
		if prs := cr.PullRequests; len(prs) > 0 {
			e.Number = prs[0].GetNumber()
		}
	case *gh.WorkflowRunEvent:
		run := p.GetWorkflowRun()
		e.Action, e.Repo, e.Actor = p.GetAction(), p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.RunID, e.SHA, e.Conclusion = run.GetID(), run.GetHeadSHA(), run.GetConclusion()
	case *gh.PushEvent:
		e.Action, e.Repo, e.Actor = "pushed", p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		if p.GetDeleted() {
			e.Action = "deleted"
		}
		e.Ref, e.Before, e.SHA, e.DefaultBranch = p.GetRef(), p.GetBefore(), p.GetAfter(), p.GetRepo().GetDefaultBranch()
	case *gh.StatusEvent:
		e.Repo, e.Actor = p.GetRepo().GetFullName(), p.GetSender().GetLogin()
		e.SHA, e.Conclusion = p.GetSHA(), p.GetState()
	}
	return e, nil
}

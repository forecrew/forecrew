package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"
)

func toPR(p *gh.PullRequest) PullRequest {
	return PullRequest{
		Number: p.GetNumber(), Title: p.GetTitle(), Body: p.GetBody(),
		Head: p.GetHead().GetRef(), Base: p.GetBase().GetRef(), HeadSHA: p.GetHead().GetSHA(),
		State: p.GetState(), Merged: p.GetMerged(), MergeSHA: p.GetMergeCommitSHA(), Draft: p.GetDraft(), URL: p.GetHTMLURL(),
		Mergeable: p.Mergeable,
	}
}

// CreatePR implements Tracker.
func (c *Client) CreatePR(ctx context.Context, repo string, in PRInput) (PullRequest, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	p, _, err := g.PullRequests.Create(ctx, owner, name, gh.CreatePullRequest{
		Title: &in.Title, Body: &in.Body, Head: in.Head, Base: in.Base, Draft: &in.Draft,
	})
	if err != nil {
		return PullRequest{}, wrap("open pull request in "+repo, err)
	}
	return toPR(p), nil
}

// GetPR implements Tracker.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (PullRequest, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	p, _, err := g.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return PullRequest{}, wrap(fmt.Sprintf("get pull request %s#%d", repo, number), err)
	}
	return toPR(p), nil
}

// FindPR implements Tracker.
func (c *Client) FindPR(ctx context.Context, repo, branch string) (PullRequest, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	prs, _, err := g.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + branch, PerPage: 1,
	})
	if err != nil {
		return PullRequest{}, wrap("find pull request for "+branch, err)
	}
	if len(prs) == 0 {
		return PullRequest{}, fmt.Errorf("pull request for %s in %s: %w", branch, repo, ErrNotFound)
	}
	return toPR(prs[0]), nil
}

// UpdatePR implements Tracker.
func (c *Client) UpdatePR(ctx context.Context, repo string, number int, in PRUpdate) (PullRequest, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	p, _, err := g.PullRequests.Edit(ctx, owner, name, number, &gh.PullRequest{Title: in.Title, Body: in.Body})
	if err != nil {
		return PullRequest{}, wrap(fmt.Sprintf("update pull request %s#%d", repo, number), err)
	}
	return toPR(p), nil
}

func toReview(r *gh.PullRequestReview) Review {
	ev := ReviewEvent(r.GetState())
	switch r.GetState() {
	case "APPROVED":
		ev = ReviewApprove
	case "CHANGES_REQUESTED":
		ev = ReviewRequestChanges
	case "COMMENTED":
		ev = ReviewComment
	}
	return Review{ID: r.GetID(), Author: r.GetUser().GetLogin(), Event: ev, Body: r.GetBody(), SHA: r.GetCommitID()}
}

// ListReviews implements Tracker.
func (c *Client) ListReviews(ctx context.Context, repo string, number int) ([]Review, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	rs, err := all(func(o gh.ListOptions) ([]*gh.PullRequestReview, *gh.Response, error) {
		return g.PullRequests.ListReviews(ctx, owner, name, number, &o)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list reviews of %s#%d", repo, number), err)
	}
	out := make([]Review, 0, len(rs))
	for _, r := range rs {
		out = append(out, toReview(r))
	}
	return out, nil
}

// CreateReview implements Tracker. Inline comments on lines outside the diff
// would make GitHub reject the whole review, so they move into the body.
func (c *Client) CreateReview(ctx context.Context, repo string, number int, in ReviewInput) (Review, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Review{}, err
	}
	op := fmt.Sprintf("review %s#%d", repo, number)
	var inline []*gh.DraftReviewComment
	var kept, moved []InlineComment
	if len(in.Comments) > 0 {
		files, err := all(func(o gh.ListOptions) ([]*gh.CommitFile, *gh.Response, error) {
			return g.PullRequests.ListFiles(ctx, owner, name, number, &o)
		})
		if err != nil {
			return Review{}, wrap("list files of "+op, err)
		}
		patches := make(map[string]map[int]bool, len(files))
		for _, f := range files {
			patches[f.GetFilename()] = diffLines(f.GetPatch())
		}
		for _, cm := range in.Comments {
			if patches[cm.Path][cm.Line] {
				inline = append(inline, &gh.DraftReviewComment{Path: &cm.Path, Line: &cm.Line, Side: new("RIGHT"), Body: &cm.Body})
				kept = append(kept, cm)
				continue
			}
			moved = append(moved, cm)
		}
	}
	post := func(body string, inline []*gh.DraftReviewComment) (*gh.PullRequestReview, error) {
		r, _, err := g.PullRequests.CreateReview(ctx, owner, name, number, &gh.PullRequestReviewRequest{
			Body: &body, Event: new(string(in.Event)), Comments: inline,
		})
		return r, err
	}
	r, err := post(reviewBody(in.Body, moved), inline)
	if err != nil && len(inline) > 0 && statusOf(err) == http.StatusUnprocessableEntity {
		// GitHub still rejected an inline comment (for example a stale diff):
		// post the whole review as one body instead of losing it.
		r, err = post(reviewBody(in.Body, append(moved, kept...)), nil)
	}
	if err != nil {
		return Review{}, wrap("post "+op, err)
	}
	return toReview(r), nil
}

// reviewBody appends the comments that cannot be inline to the review body.
func reviewBody(body string, moved []InlineComment) string {
	if len(moved) == 0 {
		return body
	}
	if body != "" {
		body += "\n\n"
	}
	body += "Comments outside the diff:\n\n"
	lines := make([]string, len(moved))
	for i, cm := range moved {
		lines[i] = fmt.Sprintf("- `%s:%d`: %s", cm.Path, cm.Line, cm.Body)
	}
	return body + strings.Join(lines, "\n")
}

// diffLines returns the new-side line numbers a unified-diff patch shows
// (added and context lines): the only lines GitHub accepts comments on.
func diffLines(patch string) map[int]bool {
	lines := map[int]bool{}
	next := 0
	for l := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			// @@ -a,b +c,d @@
			_, rest, _ := strings.Cut(l, "+")
			start, _, _ := strings.Cut(rest, " ")
			start, _, _ = strings.Cut(start, ",")
			next, _ = strconv.Atoi(start)
		case next == 0, strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
		default:
			lines[next] = true
			next++
		}
	}
	return lines
}

// MergePR implements Tracker.
func (c *Client) MergePR(ctx context.Context, repo string, number int, in MergeInput) (string, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return "", err
	}
	res, _, err := g.PullRequests.Merge(ctx, owner, name, number, "", &gh.PullRequestOptions{
		MergeMethod: string(in.Method), SHA: in.SHA,
	})
	if err != nil {
		op := fmt.Sprintf("merge %s#%d", repo, number)
		switch statusOf(err) {
		case http.StatusMethodNotAllowed, http.StatusConflict:
			return "", fmt.Errorf("%s: %w: %w", op, ErrNotMergeable, err)
		}
		return "", wrap(op, err)
	}
	return res.GetSHA(), nil
}

// BranchExists implements Tracker. It reads the exact ref, so a renamed
// branch's old name does not exist (the branches API would follow the rename).
func (c *Client) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return false, err
	}
	_, _, err = g.Git.GetRef(ctx, owner, name, "heads/"+branch)
	switch {
	case err == nil:
		return true, nil
	case statusOf(err) == http.StatusNotFound:
		return false, nil
	}
	return false, wrap(fmt.Sprintf("check branch %q of %s", branch, repo), err)
}

// GetFile implements Tracker.
func (c *Client) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	op := fmt.Sprintf("get %s at %s of %s", path, ref, repo)
	file, _, _, err := g.Repositories.GetContents(ctx, owner, name, path, &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, wrap(op, err)
	}
	if file == nil {
		return nil, fmt.Errorf("%s: %w: it is a directory", op, ErrNotFound)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return []byte(content), nil
}

package github

import (
	"context"

	gh "github.com/google/go-github/v93/github"
)

// ListAutolinks implements Tracker.
func (c *Client) ListAutolinks(ctx context.Context, repo string) ([]Autolink, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	links, _, err := g.Repositories.ListAutolinks(ctx, owner, name)
	if err != nil {
		return nil, wrap("list autolinks of "+repo, err)
	}
	out := make([]Autolink, 0, len(links))
	for _, l := range links {
		out = append(out, Autolink{KeyPrefix: l.GetKeyPrefix(), URLTemplate: l.GetURLTemplate()})
	}
	return out, nil
}

// ListRecentIssues implements Tracker. The issues endpoint also returns pull
// requests, which are left out here.
func (c *Client) ListRecentIssues(ctx context.Context, repo string, limit int) ([]Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	list, _, err := g.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{
		State: "all", Sort: "created", Direction: "desc", ListOptions: gh.ListOptions{PerPage: limit},
	})
	if err != nil {
		return nil, wrap("list issues of "+repo, err)
	}
	out := []Issue{}
	for _, i := range list {
		if !i.IsPullRequest() {
			out = append(out, toIssue(i))
		}
	}
	return out, nil
}

// ListRecentPulls implements Tracker.
func (c *Client) ListRecentPulls(ctx context.Context, repo string, limit int) ([]PullRequest, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	list, _, err := g.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "all", Sort: "created", Direction: "desc", PerPage: limit,
	})
	if err != nil {
		return nil, wrap("list pull requests of "+repo, err)
	}
	out := []PullRequest{}
	for _, p := range list {
		out = append(out, toPR(p))
	}
	return out, nil
}

package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v93/github"
)

// CheckStatus implements Tracker: it combines the latest check runs and the
// latest commit statuses of sha.
func (c *Client) CheckStatus(ctx context.Context, repo, sha string) (CheckStatus, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return CheckStatus{}, err
	}
	op := fmt.Sprintf("check status of %s@%s", repo, sha)
	runs, err := all(func(o gh.ListOptions) ([]*gh.CheckRun, *gh.Response, error) {
		res, resp, err := g.Checks.ListCheckRunsForRef(ctx, owner, name, sha, &gh.ListCheckRunsOptions{ListOptions: o})
		return res.GetCheckRuns(), resp, err
	})
	if err != nil {
		return CheckStatus{}, wrap("list check runs of "+op, err)
	}
	statuses, err := all(func(o gh.ListOptions) ([]*gh.RepoStatus, *gh.Response, error) {
		res, resp, err := g.Repositories.GetCombinedStatus(ctx, owner, name, sha, &o)
		return res.GetStatuses(), resp, err
	})
	if err != nil {
		return CheckStatus{}, wrap("list statuses of "+op, err)
	}

	var pending, failed bool
	for _, r := range runs {
		switch {
		case r.GetStatus() != "completed":
			pending = true
		case !passed(r.GetConclusion()):
			failed = true
		}
	}
	for _, s := range statuses {
		switch s.GetState() {
		case "success":
		case "failure", "error":
			failed = true
		default:
			pending = true
		}
	}
	out := CheckStatus{SHA: sha, Total: len(runs) + len(statuses)}
	switch {
	case out.Total == 0:
		out.State = ChecksNone
	case failed:
		out.State = ChecksFailure
	case pending:
		out.State = ChecksPending
	default:
		out.State = ChecksSuccess
	}
	return out, nil
}

// passed reports whether a completed check run's conclusion counts as success.
func passed(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

var _ Tracker = (*Client)(nil)

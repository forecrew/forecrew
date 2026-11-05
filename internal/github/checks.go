package github

import (
	"context"
	"fmt"
	"strings"

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

// SetCheckRun implements Tracker.
func (c *Client) SetCheckRun(ctx context.Context, repo string, in CheckRunInput) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	op := fmt.Sprintf("check run %s on %s@%s", in.Name, repo, in.SHA)
	existing, err := all(func(o gh.ListOptions) ([]*gh.CheckRun, *gh.Response, error) {
		res, resp, err := g.Checks.ListCheckRunsForRef(ctx, owner, name, in.SHA, &gh.ListCheckRunsOptions{CheckName: &in.Name, AppID: &c.appID, ListOptions: o})
		return res.GetCheckRuns(), resp, err
	})
	if err != nil {
		return wrap("list "+op, err)
	}
	summary := in.Summary
	if len(summary) > MaxCheckSummary {
		summary = strings.ToValidUTF8(summary[:MaxCheckSummary], "")
	}
	output := &gh.CheckRunOutput{Title: &in.Title, Summary: &summary}
	completed, conclusion := "completed", string(in.Conclusion)
	done := gh.Timestamp{Time: c.now()}
	if len(existing) > 0 {
		_, _, err = g.Checks.UpdateCheckRun(ctx, owner, name, existing[0].GetID(), gh.UpdateCheckRunOptions{
			Name: in.Name, Status: &completed, Conclusion: &conclusion, CompletedAt: &done, Output: output,
		})
		if err != nil {
			return wrap("update "+op, err)
		}
		return nil
	}
	_, _, err = g.Checks.CreateCheckRun(ctx, owner, name, gh.CreateCheckRunOptions{
		Name: in.Name, HeadSHA: in.SHA, Status: &completed, Conclusion: &conclusion, CompletedAt: &done, Output: output,
	})
	if err != nil {
		return wrap("create "+op, err)
	}
	return nil
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

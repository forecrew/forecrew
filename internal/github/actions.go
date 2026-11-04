package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	gh "github.com/google/go-github/v93/github"
)

// ErrTooLarge means a download is larger than the caller allows.
var ErrTooLarge = errors.New("github: download too large")

// ErrGone means an artifact expired or was deleted (HTTP 410).
var ErrGone = errors.New("github: gone")

// WorkflowRun is a GitHub Actions workflow run.
type WorkflowRun struct {
	ID int64 `json:"id"`
	// Title is the run's display title, which the workflow's run-name sets.
	Title string `json:"title"`
	// Status is queued, in_progress, completed, waiting, requested or pending.
	Status string `json:"status"`
	// Conclusion is set once Status is completed: success, failure,
	// cancelled, timed_out, skipped, neutral, action_required or stale.
	Conclusion string    `json:"conclusion"`
	HeadSHA    string    `json:"head_sha"`
	Attempt    int       `json:"attempt"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
}

// Completed reports whether the run has finished, however it ended.
func (r WorkflowRun) Completed() bool { return r.Status == "completed" }

// RunArtifact is an artifact a workflow run uploaded.
type RunArtifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Expired bool   `json:"expired"`
}

// Actions is the GitHub Actions surface of the provider that runs command
// jobs on GitHub-hosted runners. Client implements it; the App needs actions
// read and write, and contents and workflows write for WriteFile.
type Actions interface {
	// DispatchWorkflow starts the workflow file (e.g. forecrew-matrix.yml)
	// on ref, a branch or tag, with inputs, and returns the run's ID, or 0
	// when GitHub did not say. ErrNotFound: the workflow or ref does not
	// exist; ErrInvalid: it does not accept workflow_dispatch or the inputs.
	DispatchWorkflow(ctx context.Context, repo, file, ref string, inputs map[string]string) (int64, error)
	// ListWorkflowRuns returns the newest runs of the workflow file started
	// by workflow_dispatch, newest first (one page, up to 100).
	ListWorkflowRuns(ctx context.Context, repo, file string) ([]WorkflowRun, error)
	GetWorkflowRun(ctx context.Context, repo string, id int64) (WorkflowRun, error)
	// CancelWorkflowRun cancels a run; one that already finished is not an
	// error.
	CancelWorkflowRun(ctx context.Context, repo string, id int64) error
	// ListRunArtifacts returns the artifacts of a run.
	ListRunArtifacts(ctx context.Context, repo string, runID int64) ([]RunArtifact, error)
	// DownloadArtifact returns the zip of an artifact. ErrTooLarge when it
	// is over max bytes, ErrGone when it expired.
	DownloadArtifact(ctx context.Context, repo string, id, max int64) ([]byte, error)
	// WriteFile creates or updates path on branch with content and reports
	// whether it changed anything; a file that already holds content is left
	// alone.
	WriteFile(ctx context.Context, repo, branch, path string, content []byte, message string) (bool, error)
}

var _ Actions = (*Client)(nil)

// DispatchWorkflow implements Actions.
func (c *Client) DispatchWorkflow(ctx context.Context, repo, file, ref string, inputs map[string]string) (int64, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return 0, err
	}
	req := gh.CreateWorkflowDispatchEventRequest{Ref: ref, Inputs: map[string]any{}, ReturnRunDetails: new(true)}
	for k, v := range inputs {
		req.Inputs[k] = v
	}
	details, _, err := g.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, name, file, req)
	if err != nil {
		return 0, wrap(fmt.Sprintf("dispatch %s on %s of %s", file, ref, repo), err)
	}
	return details.GetWorkflowRunID(), nil
}

func newRun(r *gh.WorkflowRun) WorkflowRun {
	return WorkflowRun{
		ID: r.GetID(), Title: r.GetDisplayTitle(), Status: r.GetStatus(), Conclusion: r.GetConclusion(),
		HeadSHA: r.GetHeadSHA(), Attempt: r.GetRunAttempt(), URL: r.GetHTMLURL(), CreatedAt: r.GetCreatedAt().UTC(),
	}
}

// ListWorkflowRuns implements Actions.
func (c *Client) ListWorkflowRuns(ctx context.Context, repo, file string) ([]WorkflowRun, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	opts := &gh.ListWorkflowRunsOptions{Event: "workflow_dispatch", PerPage: 100}
	runs, _, err := g.Actions.ListWorkflowRunsByFileName(ctx, owner, name, file, opts)
	if err != nil {
		return nil, wrap(fmt.Sprintf("list runs of %s in %s", file, repo), err)
	}
	out := make([]WorkflowRun, 0, len(runs.WorkflowRuns))
	for _, r := range runs.WorkflowRuns {
		out = append(out, newRun(r))
	}
	return out, nil
}

// GetWorkflowRun implements Actions.
func (c *Client) GetWorkflowRun(ctx context.Context, repo string, id int64) (WorkflowRun, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return WorkflowRun{}, err
	}
	r, _, err := g.Actions.GetWorkflowRunByID(ctx, owner, name, id)
	if err != nil {
		return WorkflowRun{}, wrap(fmt.Sprintf("get run %d of %s", id, repo), err)
	}
	return newRun(r), nil
}

// CancelWorkflowRun implements Actions. GitHub answers 409 for a run that
// cannot be cancelled any more, which is what the caller wants.
func (c *Client) CancelWorkflowRun(ctx context.Context, repo string, id int64) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	_, err = g.Actions.CancelWorkflowRunByID(ctx, owner, name, id)
	if _, accepted := errors.AsType[*gh.AcceptedError](err); accepted || statusOf(err) == http.StatusConflict {
		return nil // go-github reports the 202 of a queued cancellation as an error
	}
	if err != nil {
		return wrap(fmt.Sprintf("cancel run %d of %s", id, repo), err)
	}
	return nil
}

// ListRunArtifacts implements Actions.
func (c *Client) ListRunArtifacts(ctx context.Context, repo string, runID int64) ([]RunArtifact, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	list, _, err := g.Actions.ListWorkflowRunArtifacts(ctx, owner, name, runID, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list artifacts of run %d of %s", runID, repo), err)
	}
	out := make([]RunArtifact, 0, len(list.Artifacts))
	for _, a := range list.Artifacts {
		out = append(out, RunArtifact{ID: a.GetID(), Name: a.GetName(), Size: a.GetSizeInBytes(), Expired: a.GetExpired()})
	}
	return out, nil
}

// maxArtifactRedirects is how many redirects GitHub's download endpoint may
// follow before the URL of the file.
const maxArtifactRedirects = 3

// DownloadArtifact implements Actions.
func (c *Client) DownloadArtifact(ctx context.Context, repo string, id, max int64) ([]byte, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	op := fmt.Sprintf("download artifact %d of %s", id, repo)
	u, apiResp, err := g.Actions.DownloadArtifact(ctx, owner, name, id, maxArtifactRedirects)
	if statusOf(err) == http.StatusGone || (err != nil && apiResp != nil && apiResp.StatusCode == http.StatusGone) {
		return nil, fmt.Errorf("%s: %w", op, ErrGone)
	}
	if err != nil {
		return nil, wrap(op, err)
	}
	// The URL is a short-lived one on GitHub's storage: no credentials.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	resp, err := c.http.Do(req) // #nosec G107 G704 -- the URL is the one GitHub's API redirected to
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: storage answered %s", op, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: %w: over %d bytes", op, ErrTooLarge, max)
	}
	return data, nil
}

// WriteFile implements Actions.
func (c *Client) WriteFile(ctx context.Context, repo, branch, path string, content []byte, message string) (bool, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return false, err
	}
	op := fmt.Sprintf("write %s on %s of %s", path, branch, repo)
	opts := &gh.RepositoryContentFileOptions{Message: &message, Content: content, Branch: &branch}
	cur, _, _, err := g.Repositories.GetContents(ctx, owner, name, path, &gh.RepositoryContentGetOptions{Ref: branch})
	switch {
	case statusOf(err) == http.StatusNotFound:
		_, _, err = g.Repositories.CreateFile(ctx, owner, name, path, opts)
	case err != nil:
		return false, wrap(op, err)
	default:
		old, cerr := cur.GetContent()
		if cerr != nil {
			return false, fmt.Errorf("%s: %w", op, cerr)
		}
		if bytes.Equal([]byte(old), content) {
			return false, nil
		}
		opts.SHA = cur.SHA
		_, _, err = g.Repositories.UpdateFile(ctx, owner, name, path, opts)
	}
	if err != nil {
		return false, wrap(op, err)
	}
	return true, nil
}

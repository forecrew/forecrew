// Package smoke drives a real deployed forecrewd through an onboarding and a
// small goal on a sandbox repo, approving every gate as the human, and
// reports what happened. It is run by `make smoke-live`, never in CI: it
// spends real model money.
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// DefaultGoal is the goal of the smoke test: small, one file, no code.
const DefaultGoal = "Add a CONTRIBUTING.md section on running tests"

// Config describes the instance and the sandbox repo.
type Config struct {
	// ServerURL and Token are those of forecrewd and an API token.
	ServerURL string
	Token     string
	// Repo is the sandbox repo (owner/name) and Project the name of its
	// project (default: the repo name).
	Repo    string
	Project string
	// Goal and GoalBody default to DefaultGoal and a short explanation.
	Goal     string
	GoalBody string
	// GitHubToken (required) reads the sandbox repo's pull requests, to
	// check on GitHub that the PRs are merged and the task PR has a review.
	GitHubToken string
	GitHubAPI   string // default https://api.github.com
	// Poll is how often the instance is looked at (default 5 s) and Timeout
	// bounds the whole run (default 45 minutes).
	Poll    time.Duration
	Timeout time.Duration
	HTTP    *http.Client
	Now     func() time.Time
}

func (c *Config) defaults() {
	if c.Project == "" {
		_, c.Project, _ = strings.Cut(c.Repo, "/")
	}
	if c.Goal == "" {
		c.Goal = DefaultGoal
	}
	if c.GoalBody == "" {
		c.GoalBody = "Contributors do not know how to run the tests. Add a short section to CONTRIBUTING.md that says how."
	}
	if c.GitHubAPI == "" {
		c.GitHubAPI = "https://api.github.com"
	}
	if c.Poll <= 0 {
		c.Poll = 5 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 45 * time.Minute
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: time.Minute}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	c.ServerURL = strings.TrimSuffix(c.ServerURL, "/")
}

// Phase is a timed part of the run.
type Phase struct {
	Name     string
	Duration time.Duration
}

// Check is one assertion of the smoke test.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// JobLine is a job of the run.
type JobLine struct {
	Ref      string
	Role     string
	Status   domain.JobStatus
	Duration time.Duration
	CostUSD  float64
}

// Report is what a run found. It is complete even when the run failed
// halfway: Err says why it stopped.
type Report struct {
	Repo     string
	Goal     string
	Started  time.Time
	Phases   []Phase
	Checks   []Check
	Jobs     []JobLine
	CostUSD  float64
	Duration time.Duration
	Err      error
}

// Passed reports whether the run finished and every check holds.
func (r *Report) Passed() bool {
	return r.Err == nil && len(r.Checks) > 0 && !slices.ContainsFunc(r.Checks, func(c Check) bool { return !c.OK })
}

func (r *Report) check(name string, ok bool, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

// Markdown renders the report.
func (r *Report) Markdown() string {
	var b strings.Builder
	verdict := "PASSED"
	if !r.Passed() {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "# Live smoke test: %s\n\n", verdict)
	fmt.Fprintf(&b, "- Repo: `%s`\n- Goal: %s\n- Started: %s\n- Total time: %s\n- Total cost: $%.4f\n\n",
		r.Repo, r.Goal, r.Started.UTC().Format(time.RFC3339), r.Duration.Round(time.Second), r.CostUSD)
	if r.Err != nil {
		fmt.Fprintf(&b, "Stopped early: %v\n\n", r.Err)
	}
	b.WriteString("## Checks\n\n")
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAILED"
		}
		fmt.Fprintf(&b, "- %s: %s (%s)\n", c.Name, mark, c.Detail)
	}
	b.WriteString("\n## Durations\n\n| Phase | Time |\n|---|---|\n")
	for _, p := range r.Phases {
		fmt.Fprintf(&b, "| %s | %s |\n", p.Name, p.Duration.Round(time.Second))
	}
	b.WriteString("\n## Jobs\n\n| Job | Role | Status | Time | Cost |\n|---|---|---|---|---|\n")
	for _, j := range r.Jobs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | $%.4f |\n", j.Ref, j.Role, j.Status, j.Duration.Round(time.Second), j.CostUSD)
	}
	return b.String()
}

// runner is a run in progress.
type runner struct {
	cfg Config
	rep *Report
}

// Run onboards the sandbox repo if needed, then delivers the goal, approving
// every gate of the project's goal. It returns the report; the error is
// non-nil only when the run could not start (bad configuration).
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.GitHubToken == "" || !strings.Contains(cfg.Repo, "/") {
		return nil, errors.New("smoke: server URL, token, GitHub token and repo (owner/name) are required")
	}
	cfg.defaults()
	r := &runner{cfg: cfg, rep: &Report{Repo: cfg.Repo, Goal: cfg.Goal, Started: cfg.Now()}}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	r.rep.Err = r.steps(ctx)
	r.rep.Duration = cfg.Now().Sub(r.rep.Started)
	r.collectJobs(context.WithoutCancel(ctx))
	return r.rep, nil
}

func (r *runner) steps(ctx context.Context) error {
	if err := r.timed("worker online", func() error { return r.workerOnline(ctx) }); err != nil {
		return err
	}
	if err := r.timed("onboarding", func() error { return r.onboard(ctx) }); err != nil {
		return err
	}
	return r.timed("goal", func() error { return r.deliver(ctx) })
}

func (r *runner) timed(name string, fn func() error) error {
	start := r.cfg.Now()
	err := fn()
	r.rep.Phases = append(r.rep.Phases, Phase{Name: name, Duration: r.cfg.Now().Sub(start)})
	return err
}

func (r *runner) workerOnline(ctx context.Context) error {
	var ws []mcp.WorkerInfo
	if err := r.call(ctx, http.MethodGet, "/workers", nil, &ws); err != nil {
		return err
	}
	online := 0
	for _, w := range ws {
		if w.Online {
			online++
		}
	}
	r.rep.check("a worker is online", online > 0, "%d online of %d", online, len(ws))
	if online == 0 {
		return errors.New("no worker is online")
	}
	return nil
}

func (r *runner) onboard(ctx context.Context) error {
	var p domain.Project
	err := r.call(ctx, http.MethodGet, "/projects/"+url.PathEscape(r.cfg.Project), nil, &p)
	var apiErr *apiError
	if err != nil && (!errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound) {
		return err
	}
	if err == nil && p.Status == domain.ProjectOnboarded {
		return r.findProfilePR(ctx)
	}
	if err := r.call(ctx, http.MethodPost, "/projects", mcp.OnboardProjectInput{Repo: r.cfg.Repo, Name: r.cfg.Project}, nil); err != nil {
		return err
	}
	g, err := r.approve(ctx, domain.GateProfileApproval)
	if err != nil {
		return err
	}
	r.rep.check("profile PR", g.SubjectNumber > 0, "%s#%d", r.cfg.Repo, g.SubjectNumber)
	r.githubMerged(ctx, "profile PR merged", g.SubjectNumber)
	return r.until(ctx, "project onboarded", func() (bool, error) {
		var p domain.Project
		err := r.call(ctx, http.MethodGet, "/projects/"+url.PathEscape(r.cfg.Project), nil, &p)
		return p.Status == domain.ProjectOnboarded, err
	})
}

func (r *runner) deliver(ctx context.Context) error {
	var goal domain.Goal
	in := mcp.CreateGoalInput{Project: r.cfg.Project, Title: r.cfg.Goal, Body: r.cfg.GoalBody}
	if err := r.call(ctx, http.MethodPost, "/goals", in, &goal); err != nil {
		return err
	}
	for {
		var d mcp.GoalDetail
		if err := r.call(ctx, http.MethodGet, "/goals/"+goal.Ref(), nil, &d); err != nil {
			return err
		}
		switch d.Goal.Status {
		case domain.GoalDone:
			r.rep.check("goal done", true, "%s", goal.Ref())
			r.verify(ctx, d)
			return nil
		case domain.GoalCanceled:
			return fmt.Errorf("%s was canceled", goal.Ref())
		}
		for _, g := range d.Gates {
			if g.Status != domain.GatePending {
				continue
			}
			if g.Kind == domain.GateNeedsYou {
				return fmt.Errorf("%s needs a human: %s", g.Ref, g.Title)
			}
			if err := r.call(ctx, http.MethodPost, "/gates/"+g.Ref+"/approve", nil, nil); err != nil {
				return err
			}
		}
		if err := sleep(ctx, r.cfg.Poll); err != nil {
			return err
		}
	}
}

// verify checks the task PRs and their reviews.
func (r *runner) verify(ctx context.Context, d mcp.GoalDetail) {
	prs := 0
	for _, t := range d.Tasks {
		for _, c := range t.Changes {
			prs++
			r.rep.check("task PR of "+t.Ref, c.State == domain.ChangeMerged, "%s#%d is %s", c.Repo, c.Number, c.State)
			r.githubMerged(ctx, "task PR "+t.Ref+" merged", c.Number)
			r.githubReview(ctx, t.Ref, c.Number)
		}
	}
	r.rep.check("task PRs exist", prs > 0, "%d", prs)
}

// approve waits for a pending gate of kind and approves it.
func (r *runner) approve(ctx context.Context, kind domain.GateKind) (mcp.GateInfo, error) {
	var found mcp.GateInfo
	err := r.until(ctx, string(kind)+" gate", func() (bool, error) {
		var gates []mcp.GateInfo
		if err := r.call(ctx, http.MethodGet, "/gates?project="+url.QueryEscape(r.cfg.Project), nil, &gates); err != nil {
			return false, err
		}
		for _, g := range gates {
			if g.Kind == kind {
				found = g
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return found, err
	}
	return found, r.call(ctx, http.MethodPost, "/gates/"+found.Ref+"/approve", nil, nil)
}

// collectJobs lists the jobs of the project for the report and adds the
// cost of the onboarding to the goal's.
func (r *runner) collectJobs(ctx context.Context) {
	var jobs []domain.Job
	if err := r.call(ctx, http.MethodGet, "/jobs?limit=500&project="+url.QueryEscape(r.cfg.Project), nil, &jobs); err != nil {
		return
	}
	total := 0.0
	for _, j := range slices.Backward(jobs) {
		line := JobLine{Ref: j.Ref(), Role: j.Role, Status: j.Status, CostUSD: j.CostUSD}
		if !j.FinishedAt.IsZero() && !j.StartedAt.IsZero() {
			line.Duration = j.FinishedAt.Sub(j.StartedAt)
		}
		total += j.CostUSD
		r.rep.Jobs = append(r.rep.Jobs, line)
	}
	r.rep.CostUSD = total
	reviews := 0
	for _, j := range jobs {
		if j.Role == "reviewer" && j.Status == domain.JobSucceeded {
			reviews++
		}
	}
	r.rep.check("a review job succeeded", reviews > 0, "%d", reviews)
}

// until polls cond every Poll until it holds.
func (r *runner) until(ctx context.Context, what string, cond func() (bool, error)) error {
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if err := sleep(ctx, r.cfg.Poll); err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// apiError is an error answer of forecrewd.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("forecrewd answered %d: %s", e.Status, e.Body) }

// call calls /api/v1 and decodes the answer into out.
func (r *runner) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	data, err := r.do(ctx, method, r.cfg.ServerURL+"/api/v1"+path, r.cfg.Token, body)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (r *runner) do(ctx context.Context, method, target, token string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return data, nil
}

// findProfilePR handles a project onboarded before the run: the merged PR of
// the onboarding branch must still be on GitHub, or the run fails.
func (r *runner) findProfilePR(ctx context.Context) error {
	owner, _, _ := strings.Cut(r.cfg.Repo, "/")
	var raw []struct {
		Number   int     `json:"number"`
		MergedAt *string `json:"merged_at"`
	}
	path := fmt.Sprintf("/repos/%s/pulls?state=closed&head=%s", r.cfg.Repo, url.QueryEscape(owner+":agent/onboarding"))
	err := r.github(ctx, path, &raw)
	for _, p := range raw {
		if p.MergedAt != nil {
			r.rep.check("profile PR", true, "%s#%d, found on GitHub (project was onboarded before this run)", r.cfg.Repo, p.Number)
			return nil
		}
	}
	r.rep.check("profile PR", false, "no merged PR from agent/onboarding found (err=%v)", err)
	return errors.New("the profile PR of this project is not on GitHub: onboard a fresh sandbox repo")
}

// githubMerged checks on GitHub that PR number is merged.
func (r *runner) githubMerged(ctx context.Context, name string, number int) {
	var pr struct {
		Merged bool `json:"merged"`
	}
	err := r.github(ctx, fmt.Sprintf("/repos/%s/pulls/%d", r.cfg.Repo, number), &pr)
	r.rep.check(name, err == nil && pr.Merged, "%s#%d merged=%v err=%v", r.cfg.Repo, number, pr.Merged, err)
}

// githubReview checks that the reviewer left a review on the PR.
func (r *runner) githubReview(ctx context.Context, task string, number int) {
	var reviews []struct {
		State string `json:"state"`
	}
	err := r.github(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews", r.cfg.Repo, number), &reviews)
	r.rep.check("review on the PR of "+task, err == nil && len(reviews) > 0, "%s#%d has %d reviews, err=%v", r.cfg.Repo, number, len(reviews), err)
}

func (r *runner) github(ctx context.Context, path string, out any) error {
	data, err := r.do(ctx, http.MethodGet, r.cfg.GitHubAPI+path, r.cfg.GitHubToken, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

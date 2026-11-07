package github

import (
	"context"
	"fmt"
	"net/http"
	"time"

	gh "github.com/google/go-github/v93/github"
)

const perPage = 100

// all collects every page of a list call. fetch gets the page options and
// returns the items and the response.
func all[T any](fetch func(opts gh.ListOptions) ([]T, *gh.Response, error)) ([]T, error) {
	var out []T
	opts := gh.ListOptions{PerPage: perPage}
	for {
		items, resp, err := fetch(opts)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// optional returns nil for an empty string, so the field is left out of the request.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toUser(u *gh.User) User {
	return User{ID: u.GetID(), Login: u.GetLogin(), Bot: u.GetType() == "Bot"}
}

func toIssue(i *gh.Issue) Issue {
	out := Issue{
		Number: i.GetNumber(), Title: i.GetTitle(), Body: i.GetBody(), State: i.GetState(), URL: i.GetHTMLURL(),
		Type: i.GetType().GetName(), Author: toUser(i.GetUser()), UpdatedAt: i.GetUpdatedAt().UTC(), PullRequest: i.IsPullRequest(),
	}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	for _, a := range i.Assignees {
		out.Assignees = append(out.Assignees, toUser(a))
	}
	return out
}

func toComment(c *gh.IssueComment) Comment {
	u := toUser(c.GetUser())
	return Comment{ID: c.GetID(), Author: u.Login, AuthorID: u.ID, Bot: u.Bot, Body: c.GetBody(), CreatedAt: c.GetCreatedAt().UTC()}
}

// CreateIssue implements Tracker.
func (c *Client) CreateIssue(ctx context.Context, repo string, in IssueInput) (Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := g.Issues.Create(ctx, owner, name, gh.CreateIssueRequest{Title: in.Title, Body: &in.Body, Labels: in.Labels})
	if err != nil {
		return Issue{}, wrap("create issue in "+repo, err)
	}
	return toIssue(i), nil
}

// GetIssue implements Tracker.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := g.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return Issue{}, wrap(fmt.Sprintf("get issue %s#%d", repo, number), err)
	}
	return toIssue(i), nil
}

// UpdateIssue implements Tracker.
func (c *Client) UpdateIssue(ctx context.Context, repo string, number int, in IssueUpdate) (Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := g.Issues.Update(ctx, owner, name, number, gh.UpdateIssueRequest{Title: in.Title, Body: in.Body, State: in.State})
	if err != nil {
		return Issue{}, wrap(fmt.Sprintf("update issue %s#%d", repo, number), err)
	}
	return toIssue(i), nil
}

// AddSubIssue implements Tracker. The REST API takes the child's database ID,
// not its number.
func (c *Client) AddSubIssue(ctx context.Context, repo string, parent, child int) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	ci, _, err := g.Issues.Get(ctx, owner, name, child)
	if err != nil {
		return wrap(fmt.Sprintf("get issue %s#%d", repo, child), err)
	}
	if _, _, err := g.SubIssue.Add(ctx, owner, name, int64(parent), gh.SubIssueRequest{SubIssueID: ci.GetID()}); err != nil {
		return wrap(fmt.Sprintf("add sub-issue %s#%d to #%d", repo, child, parent), err)
	}
	return nil
}

// ListSubIssues implements Tracker.
func (c *Client) ListSubIssues(ctx context.Context, repo string, parent int) ([]Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	subs, err := all(func(o gh.ListOptions) ([]*gh.SubIssue, *gh.Response, error) {
		return g.SubIssue.ListByIssue(ctx, owner, name, int64(parent), &o)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list sub-issues of %s#%d", repo, parent), err)
	}
	out := make([]Issue, 0, len(subs))
	for _, s := range subs {
		i := gh.Issue(*s)
		out = append(out, toIssue(&i))
	}
	return out, nil
}

// ListOpenIssues implements Tracker. GitHub lists pull requests as issues too;
// they are left out.
func (c *Client) ListOpenIssues(ctx context.Context, repo, label string) ([]Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListByRepoOptions{State: "open", Labels: []string{label}, Sort: "created", Direction: "asc"}
	items, err := all(func(o gh.ListOptions) ([]*gh.Issue, *gh.Response, error) {
		opts.ListOptions = o
		return g.Issues.ListByRepo(ctx, owner, name, opts)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list issues of %s labeled %s", repo, label), err)
	}
	var out []Issue
	for _, i := range items {
		if !i.IsPullRequest() {
			out = append(out, toIssue(i))
		}
	}
	return out, nil
}

// ListIssuesSince implements Tracker. GitHub lists pull requests as issues
// too; they are left out.
func (c *Client) ListIssuesSince(ctx context.Context, repo string, since time.Time) ([]Issue, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListByRepoOptions{State: "all", Since: since, Sort: "updated", Direction: "asc"}
	items, err := all(func(o gh.ListOptions) ([]*gh.Issue, *gh.Response, error) {
		opts.ListOptions = o
		return g.Issues.ListByRepo(ctx, owner, name, opts)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list issues of %s updated since %s", repo, since), err)
	}
	var out []Issue
	for _, i := range items {
		if !i.IsPullRequest() {
			out = append(out, toIssue(i))
		}
	}
	return out, nil
}

// ListIssueEvents implements Tracker. Who did an assignment is its assigner,
// when GitHub names one.
func (c *Client) ListIssueEvents(ctx context.Context, repo string, number int) ([]IssueEvent, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	events, err := all(func(o gh.ListOptions) ([]*gh.IssueEvent, *gh.Response, error) {
		return g.Issues.ListIssueEvents(ctx, owner, name, number, &o)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list events of %s#%d", repo, number), err)
	}
	var out []IssueEvent
	for _, e := range events {
		item := IssueEvent{Event: e.GetEvent(), Actor: toUser(e.GetActor()), At: e.GetCreatedAt().UTC()}
		switch item.Event {
		case IssueAssigned:
			item.Value = e.GetAssignee().GetLogin()
			if e.Assigner != nil {
				item.Actor = toUser(e.Assigner)
			}
		case IssueLabeled:
			item.Value = e.GetLabel().GetName()
		default:
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

// CreateComment implements Tracker.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (Comment, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Comment{}, err
	}
	cm, _, err := g.Issues.CreateComment(ctx, owner, name, number, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return Comment{}, wrap(fmt.Sprintf("comment on %s#%d", repo, number), err)
	}
	return toComment(cm), nil
}

// UpdateComment implements Tracker.
func (c *Client) UpdateComment(ctx context.Context, repo string, id int64, body string) (Comment, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return Comment{}, err
	}
	cm, _, err := g.Issues.UpdateComment(ctx, owner, name, id, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return Comment{}, wrap(fmt.Sprintf("update comment %d of %s", id, repo), err)
	}
	return toComment(cm), nil
}

// ListComments implements Tracker. GitHub filters by update time, so comments
// are filtered again by creation time.
func (c *Client) ListComments(ctx context.Context, repo string, number int, since time.Time) ([]Comment, error) {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListCommentsOptions{Sort: new("created"), Direction: new("asc")}
	if !since.IsZero() {
		opts.Since = &since
	}
	cms, err := all(func(o gh.ListOptions) ([]*gh.IssueComment, *gh.Response, error) {
		opts.ListOptions = o
		return g.Issues.ListComments(ctx, owner, name, number, opts)
	})
	if err != nil {
		return nil, wrap(fmt.Sprintf("list comments of %s#%d", repo, number), err)
	}
	var out []Comment
	for _, cm := range cms {
		if cm.GetCreatedAt().After(since) {
			out = append(out, toComment(cm))
		}
	}
	return out, nil
}

// EnsureLabel implements Tracker.
func (c *Client) EnsureLabel(ctx context.Context, repo string, l Label) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	_, _, err = g.Issues.CreateLabel(ctx, owner, name, gh.CreateIssueLabelRequest{Name: l.Name, Color: optional(l.Color), Description: optional(l.Description)})
	if err == nil {
		return nil
	}
	if statusOf(err) != http.StatusUnprocessableEntity { // 422: already exists
		return wrap("create label "+l.Name, err)
	}
	if _, _, err := g.Issues.UpdateLabel(ctx, owner, name, l.Name, gh.UpdateIssueLabelRequest{Color: optional(l.Color), Description: optional(l.Description)}); err != nil {
		return wrap("update label "+l.Name, err)
	}
	return nil
}

// AddLabels implements Tracker.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels ...string) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	if _, _, err := g.Issues.AddLabelsToIssue(ctx, owner, name, number, labels); err != nil {
		return wrap(fmt.Sprintf("add labels to %s#%d", repo, number), err)
	}
	return nil
}

// RemoveLabel implements Tracker.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	g, owner, name, err := c.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := g.Issues.RemoveLabelForIssue(ctx, owner, name, number, label); err != nil && statusOf(err) != http.StatusNotFound {
		return wrap(fmt.Sprintf("remove label %s from %s#%d", label, repo, number), err)
	}
	return nil
}

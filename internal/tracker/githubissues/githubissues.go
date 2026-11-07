// Package githubissues is the tracker adapter of GitHub Issues, over the
// existing GitHub App (docs/contracts/tracker.md).
package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Kind is the kind of the connections of this adapter.
const Kind = "github"

// States of an issue.
const (
	stateOpen   = "open"
	stateClosed = "closed"
)

// Labels on the issues Forecrew opens, which are never tickets.
var ownLabels = []string{"forecrew:goal", "forecrew:task", "forecrew:bug"}

// Client is the part of GitHub the adapter uses; github.Tracker implements it.
type Client interface {
	IsInstalled(ctx context.Context, repo string) (bool, error)
	GetIssue(ctx context.Context, repo string, number int) (github.Issue, error)
	UpdateIssue(ctx context.Context, repo string, number int, in github.IssueUpdate) (github.Issue, error)
	ListIssuesSince(ctx context.Context, repo string, since time.Time) ([]github.Issue, error)
	ListIssueEvents(ctx context.Context, repo string, number int) ([]github.IssueEvent, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (github.Comment, error)
	ListComments(ctx context.Context, repo string, number int, since time.Time) ([]github.Comment, error)
}

// Deps are what the adapters of a forecrewd share.
type Deps struct {
	GitHub Client
	Store  store.Store
	// Secret is the webhook secret of the App.
	Secret []byte
	Now    func() time.Time // defaults to the real UTC time
}

// Config is tracker_connections.config of this kind.
type Config struct {
	// Repos are the repos the connection reads: repos of projects of its
	// organization.
	Repos []string `json:"repos"`
	// BotLogin is the App's user; its events are Forecrew's own.
	BotLogin string `json:"bot_login,omitempty"`
}

// Factory returns the factory to register for Kind.
func Factory(d Deps) tracker.Factory {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return func(c domain.TrackerConnection) (tracker.Adapter, error) {
		raw, err := json.Marshal(c.Config)
		if err != nil {
			return nil, fmt.Errorf("encode config: %w", err)
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("config of %s: %v: %w", c.Name, err, tracker.ErrInvalid)
		}
		if len(cfg.Repos) == 0 {
			return nil, fmt.Errorf("config of %s needs repos: %w", c.Name, tracker.ErrInvalid)
		}
		return &Adapter{d: d, conn: c, cfg: cfg}, nil
	}
}

// Adapter is one connection.
type Adapter struct {
	d    Deps
	conn domain.TrackerConnection
	cfg  Config
}

var (
	_ tracker.Adapter = (*Adapter)(nil)
	_ tracker.Linker  = (*Adapter)(nil)
)

// Kind implements tracker.Adapter.
func (a *Adapter) Kind() string { return Kind }

// Capabilities implements tracker.Adapter. GitHub cannot assign an issue to
// an App, so the assignee trigger needs a machine user.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Webhooks: true, Assignment: true, Labels: true, Transitions: true, Comments: true}
}

// Verify implements tracker.Adapter. The App serves every organization of
// this forecrewd, so a connection may only name repos of its own projects:
// otherwise it could read and write the issues of another organization.
func (a *Adapter) Verify(ctx context.Context) error {
	for _, repo := range a.cfg.Repos {
		projects, err := a.d.Store.Projects().ListByRepo(ctx, repo)
		if err != nil {
			return fmt.Errorf("find the project of %s: %w", repo, err)
		}
		if !slices.ContainsFunc(projects, func(p domain.Project) bool { return p.AccountID == a.conn.AccountID }) {
			return fmt.Errorf("%s is not a repo of a project of this organization: %w", repo, tracker.ErrInvalid)
		}
		installed, err := a.d.GitHub.IsInstalled(ctx, repo)
		if err != nil {
			return fmt.Errorf("check the GitHub App on %s: %w", repo, err)
		}
		if !installed {
			return fmt.Errorf("the GitHub App is not installed on %s: %w", repo, tracker.ErrInvalid)
		}
	}
	return nil
}

func (a *Adapter) ours(repo string) bool {
	return slices.ContainsFunc(a.cfg.Repos, func(r string) bool { return strings.EqualFold(r, repo) })
}

func key(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

// parseKey splits a key of a repo of this connection.
func (a *Adapter) parseKey(k string) (string, int, error) {
	repo, num, ok := strings.Cut(k, "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n < 1 || !a.ours(repo) {
		return "", 0, fmt.Errorf("%s: %w", k, tracker.ErrNotFound)
	}
	return repo, n, nil
}

// ParseEvent implements tracker.Adapter. Of the payload it keeps which issue
// it is about, and who sent it to drop the App's own writes.
func (a *Adapter) ParseEvent(r tracker.Request) ([]tracker.Event, error) {
	if err := github.VerifySignature(a.d.Secret, r.Body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		return nil, fmt.Errorf("%w: %w", tracker.ErrBadSignature, err)
	}
	kind := r.Header.Get("X-GitHub-Event")
	if kind != "issues" && kind != "issue_comment" {
		return nil, nil
	}
	ev, err := github.ParseEvent(kind, r.Body)
	if err != nil {
		return nil, err
	}
	if ev.IsPR || !a.ours(ev.Repo) {
		return nil, nil
	}
	self := a.cfg.BotLogin != "" && strings.EqualFold(ev.Actor, a.cfg.BotLogin)
	return []tracker.Event{{ID: r.Header.Get("X-GitHub-Delivery"), Key: key(ev.Repo, ev.Number), Self: self}}, nil
}

// Changes implements tracker.Adapter. The cursor is the last update seen;
// GitHub's since includes it, so the newest issue comes again, which does no
// harm: an event is only a hint.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	if cursor == "" {
		return nil, a.d.Now().Format(time.RFC3339Nano), nil
	}
	since, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", cursor, err)
	}
	var events []tracker.Event
	newest := since
	for _, repo := range a.cfg.Repos {
		issues, err := a.d.GitHub.ListIssuesSince(ctx, repo, since)
		if err != nil {
			return nil, "", err
		}
		for _, i := range issues {
			events = append(events, tracker.Event{Key: key(repo, i.Number)})
			if i.UpdatedAt.After(newest) {
				newest = i.UpdatedAt
			}
		}
	}
	return events, newest.Format(time.RFC3339Nano), nil
}

func user(u github.User) tracker.User {
	if u.ID == 0 {
		return tracker.User{Name: u.Login, Bot: u.Bot}
	}
	return tracker.User{ID: strconv.FormatInt(u.ID, 10), Name: u.Login, Bot: u.Bot}
}

// assignee is the assignee a ticket shows: the trigger's if the issue has it.
func (a *Adapter) assignee(i github.Issue) tracker.User {
	if len(i.Assignees) == 0 {
		return tracker.User{}
	}
	if n := slices.IndexFunc(i.Assignees, func(u github.User) bool { return strings.EqualFold(u.Login, a.conn.Triggers.Assignee) }); n >= 0 {
		return user(i.Assignees[n])
	}
	return user(i.Assignees[0])
}

func hasLabel(i github.Issue, want string) bool {
	return slices.ContainsFunc(i.Labels, func(l string) bool { return strings.EqualFold(l, want) })
}

// ticketType is the issue's type, or bug for the label of that name on a repo
// without issue types.
func ticketType(i github.Issue) string {
	if i.Type == "" && hasLabel(i, "bug") {
		return "bug"
	}
	return i.Type
}

func handovers(events []github.IssueEvent) []tracker.Handover {
	out := make([]tracker.Handover, 0, len(events))
	for _, e := range events {
		trigger := tracker.TriggerLabel
		if e.Event == github.IssueAssigned {
			trigger = tracker.TriggerAssignee
		}
		out = append(out, tracker.Handover{Trigger: trigger, Value: e.Value, By: user(e.Actor), At: e.At})
	}
	return out
}

// Get implements tracker.Adapter. Who handed the issue over comes from its
// timeline, so polling knows what a webhook would.
func (a *Adapter) Get(ctx context.Context, k string) (tracker.Ticket, error) {
	repo, n, err := a.parseKey(k)
	if err != nil {
		return tracker.Ticket{}, err
	}
	i, err := a.d.GitHub.GetIssue(ctx, repo, n)
	if errors.Is(err, github.ErrNotFound) || (err == nil && i.PullRequest) {
		return tracker.Ticket{}, fmt.Errorf("%s: %w", k, tracker.ErrNotFound)
	}
	if err != nil {
		return tracker.Ticket{}, err
	}
	t := tracker.Ticket{
		Ref: tracker.Ref{Connection: a.conn.ID, Key: k}, Project: repo, Title: i.Title, Body: i.Body, Type: ticketType(i), Labels: i.Labels,
		Assignee: a.assignee(i), Reporter: user(i.Author), State: i.State, Closed: i.State == stateClosed, URL: i.URL, Updated: i.UpdatedAt,
		Issue: &tracker.RepoIssue{Repo: repo, Number: n},
	}
	comments, err := a.d.GitHub.ListComments(ctx, repo, n, time.Time{})
	if err != nil {
		return tracker.Ticket{}, err
	}
	for _, c := range comments {
		t.Comments = append(t.Comments, tracker.Comment{
			ID: strconv.FormatInt(c.ID, 10), Author: user(github.User{ID: c.AuthorID, Login: c.Author, Bot: c.Bot}), Body: c.Body, At: c.CreatedAt,
		})
	}
	if slices.ContainsFunc(ownLabels, func(l string) bool { return hasLabel(i, l) }) {
		return t, nil // nobody can hand over an issue Forecrew opened
	}
	events, err := a.d.GitHub.ListIssueEvents(ctx, repo, n)
	if err != nil {
		return tracker.Ticket{}, err
	}
	t.Handovers = handovers(events)
	return t, nil
}

// Comment implements tracker.Adapter.
func (a *Adapter) Comment(ctx context.Context, k, body string) (string, error) {
	repo, n, err := a.parseKey(k)
	if err != nil {
		return "", err
	}
	c, err := a.d.GitHub.CreateComment(ctx, repo, n, body)
	if errors.Is(err, github.ErrNotFound) {
		return "", fmt.Errorf("%s: %w", k, tracker.ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(c.ID, 10), nil
}

// Transition implements tracker.Adapter.
func (a *Adapter) Transition(ctx context.Context, k, state string) error {
	repo, n, err := a.parseKey(k)
	if err != nil {
		return err
	}
	if state != stateOpen && state != stateClosed {
		return fmt.Errorf("an issue is %s or %s, not %q", stateOpen, stateClosed, state)
	}
	_, err = a.d.GitHub.UpdateIssue(ctx, repo, n, github.IssueUpdate{State: &state})
	if errors.Is(err, github.ErrNotFound) {
		return fmt.Errorf("%s: %w", k, tracker.ErrNotFound)
	}
	return err
}

// States implements tracker.Adapter.
func (a *Adapter) States(context.Context) ([]tracker.State, error) {
	return []tracker.State{{Name: stateOpen}, {Name: stateClosed, Closed: true}}, nil
}

// LinkedUser implements tracker.Linker: the member linked to the GitHub
// account by its stable ID, when there is exactly one.
func (a *Adapter) LinkedUser(ctx context.Context, accountID uuid.UUID, u tracker.User) (uuid.UUID, bool, error) {
	id, err := strconv.ParseInt(u.ID, 10, 64)
	if err != nil || id <= 0 {
		return uuid.Nil(), false, nil
	}
	users, err := a.d.Store.Directory().GitHubMembers(ctx, accountID, id, "")
	if err != nil {
		return uuid.Nil(), false, fmt.Errorf("find the user of %s: %w", u.Name, err)
	}
	if len(users) != 1 {
		return uuid.Nil(), false, nil
	}
	return users[0], true, nil
}

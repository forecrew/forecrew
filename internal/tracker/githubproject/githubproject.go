// Package githubproject is the tracker adapter of a GitHub Projects (v2)
// board: a card moved to a column hands its ticket to Forecrew, and Forecrew
// moves the card as the goal advances (docs/contracts/tracker.md).
package githubproject

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
	"github.com/forecrew/forecrew/internal/tracker/ghidentity"
)

// Kind is the kind of the connections of this adapter.
const Kind = "github_project"

// moveSkew is how far a timeline event may be from the time GitHub gives the
// card's column and still be the move that put the card there.
const moveSkew = time.Minute

const (
	defaultField  = "Status"
	defaultClosed = "Done"
)

// Issues is the part of GitHub Issues the adapter uses, for the comments of
// a card that is an issue; github.Tracker implements it.
type Issues interface {
	CreateComment(ctx context.Context, repo string, number int, body string) (github.Comment, error)
	ListComments(ctx context.Context, repo string, number int, since time.Time) ([]github.Comment, error)
}

// Deps are what the adapters of a forecrewd share.
type Deps struct {
	Projects github.Projects
	Issues   Issues
	Store    store.Store
	// Secret is the webhook secret of the App.
	Secret []byte
	Now    func() time.Time // defaults to the real UTC time
}

// Config is tracker_connections.config of this kind.
type Config struct {
	// Owner is the organization that owns the board; Project its number.
	Owner   string `json:"owner"`
	Project int    `json:"project"`
	// Field is the single-select field whose options are the columns.
	Field string `json:"field,omitempty"`
	// Closed are the columns that count as finished. Default: Done.
	Closed []string `json:"closed,omitempty"`
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
		if cfg.Owner == "" || cfg.Project < 1 {
			return nil, fmt.Errorf("config of %s needs owner (an organization) and project (the board's number): %w", c.Name, tracker.ErrInvalid)
		}
		if cfg.Field == "" {
			cfg.Field = defaultField
		}
		if len(cfg.Closed) == 0 {
			cfg.Closed = []string{defaultClosed}
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

// Capabilities implements tracker.Adapter. A card is handed over by its
// column; assignees and labels belong to the issue, and the issue's tracker
// is the GitHub one.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Webhooks: true, Columns: true, Transitions: true, Comments: true}
}

func (a *Adapter) board(ctx context.Context) (github.ProjectBoard, error) {
	b, err := a.d.Projects.ProjectBoard(ctx, a.cfg.Owner, a.cfg.Project, a.cfg.Field)
	if errors.Is(err, github.ErrNotFound) {
		return b, fmt.Errorf("%w: %w", err, tracker.ErrInvalid)
	}
	return b, err
}

// column finds a column of the board by name, ignoring case.
func column(b github.ProjectBoard, name string) (github.ProjectOption, bool) {
	i := slices.IndexFunc(b.Options, func(o github.ProjectOption) bool { return strings.EqualFold(o.Name, name) })
	if i < 0 {
		return github.ProjectOption{}, false
	}
	return b.Options[i], true
}

// Verify implements tracker.Adapter. The App serves every organization of
// this forecrewd, so a connection may only name a board of an organization
// that owns a repo of one of its own projects: otherwise it could read and
// move the cards of another organization.
func (a *Adapter) Verify(ctx context.Context) error {
	if t := a.conn.Triggers; t.Assignee != "" || t.Label != "" {
		return fmt.Errorf("a board hands a ticket over by its column: only the state trigger works: %w", tracker.ErrInvalid)
	}
	projects, err := a.d.Store.Projects().List(ctx, a.conn.AccountID)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if !slices.ContainsFunc(projects, func(p domain.Project) bool {
		owner, _, _ := strings.Cut(p.Repo, "/")
		return strings.EqualFold(owner, a.cfg.Owner)
	}) {
		return fmt.Errorf("%s owns no repo of a project of this organization: %w", a.cfg.Owner, tracker.ErrInvalid)
	}
	b, err := a.board(ctx)
	if errors.Is(err, github.ErrForbidden) || errors.Is(err, github.ErrNotInstalled) {
		return fmt.Errorf("the GitHub App cannot read project %d of %s: install it on the organization and give it the Projects permission: %w", a.cfg.Project, a.cfg.Owner, tracker.ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("read project %d of %s: %w", a.cfg.Project, a.cfg.Owner, err)
	}
	if t := a.conn.Triggers.State; t != "" {
		if _, ok := column(b, t); !ok {
			return fmt.Errorf("%q is not a column of project %d (it has %s): %w", t, a.cfg.Project, names(b), tracker.ErrInvalid)
		}
	}
	return nil
}

func names(b github.ProjectBoard) string {
	out := make([]string, len(b.Options))
	for i, o := range b.Options {
		out[i] = o.Name
	}
	return strings.Join(out, ", ")
}

// ParseEvent implements tracker.Adapter. Of a projects_v2_item delivery it
// keeps which card it is about, and who sent it to drop the App's own moves.
// GitHub sends the event for organization-level boards only, and only to an
// App with the organization permission Projects.
func (a *Adapter) ParseEvent(r tracker.Request) ([]tracker.Event, error) {
	if err := github.VerifySignature(a.d.Secret, r.Body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		return nil, fmt.Errorf("%w: %w", tracker.ErrBadSignature, err)
	}
	if r.Header.Get("X-GitHub-Event") != "projects_v2_item" {
		return nil, nil
	}
	var p struct {
		Action string `json:"action"`
		Item   struct {
			NodeID      string `json:"node_id"`
			ContentType string `json:"content_type"`
		} `json:"projects_v2_item"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, fmt.Errorf("parse projects_v2_item event: %w", err)
	}
	if p.Item.NodeID == "" || p.Item.ContentType == "PullRequest" || p.Action == "deleted" || !strings.EqualFold(p.Organization.Login, a.cfg.Owner) {
		return nil, nil
	}
	self := a.cfg.BotLogin != "" && strings.EqualFold(p.Sender.Login, a.cfg.BotLogin)
	return []tracker.Event{{ID: r.Header.Get("X-GitHub-Delivery"), Key: p.Item.NodeID, Self: self}}, nil
}

// Changes implements tracker.Adapter. GitHub cannot list a board's cards by
// time, so each poll reads the board and keeps the cards changed since the
// cursor, the last change seen; the newest card comes again, which does no
// harm: an event is only a hint. Polling also sees what no event reaches: a
// board whose events do not arrive, an edit of an issue's text, a comment.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	if cursor == "" {
		return nil, a.d.Now().Format(time.RFC3339Nano), nil
	}
	since, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", cursor, err)
	}
	items, err := a.d.Projects.ProjectItems(ctx, a.cfg.Owner, a.cfg.Project, a.cfg.Field, since)
	if err != nil {
		return nil, "", err
	}
	var events []tracker.Event
	newest := since
	for _, it := range items {
		if it.Kind == github.ItemOther {
			continue
		}
		events = append(events, tracker.Event{Key: it.ID})
		if it.UpdatedAt.After(newest) {
			newest = it.UpdatedAt
		}
	}
	return events, newest.Format(time.RFC3339Nano), nil
}

// item reads a card. A card is a ticket only if it is a draft or an issue of a
// repo of one of the organization's projects: a board holds issues of any
// repo, and nothing outside the projects is read, so one foreign card cannot
// fail the connection either.
func (a *Adapter) item(ctx context.Context, key string) (github.ProjectItem, error) {
	it, err := a.d.Projects.ProjectItem(ctx, a.cfg.Owner, a.cfg.Project, a.cfg.Field, key)
	if errors.Is(err, github.ErrNotFound) || (err == nil && it.Kind == github.ItemOther) {
		return github.ProjectItem{}, fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	if err != nil {
		return github.ProjectItem{}, err
	}
	if it.Kind == github.ItemIssue {
		projects, err := a.d.Store.Projects().ListByRepo(ctx, it.Repo)
		if err != nil {
			return github.ProjectItem{}, fmt.Errorf("find the project of %s: %w", it.Repo, err)
		}
		if !slices.ContainsFunc(projects, func(p domain.Project) bool { return p.AccountID == a.conn.AccountID }) {
			return github.ProjectItem{}, fmt.Errorf("%s: %s is not a repo of a project of this organization: %w", key, it.Repo, tracker.ErrNotFound)
		}
	}
	return it, nil
}

func (a *Adapter) finished(it github.ProjectItem) bool {
	return it.Closed || it.Archived || slices.ContainsFunc(a.cfg.Closed, func(c string) bool { return strings.EqualFold(c, it.Status) })
}

// Get implements tracker.Adapter. Who moved the card comes from the timeline
// of its issue, so polling knows what a webhook would; a draft has no
// timeline, and a move by a board workflow has no person.
func (a *Adapter) Get(ctx context.Context, key string) (tracker.Ticket, error) {
	it, err := a.item(ctx, key)
	if err != nil {
		return tracker.Ticket{}, err
	}
	t := tracker.Ticket{
		Ref: tracker.Ref{Connection: a.conn.ID, Key: key}, Project: it.Repo, Title: it.Title, Body: it.Body, Type: ghidentity.TicketType(it.Type, it.Labels),
		Labels: it.Labels, Reporter: ghidentity.User(it.Author), State: it.Status, Closed: a.finished(it), URL: it.URL, Updated: it.UpdatedAt,
	}
	if len(it.Assignees) > 0 {
		t.Assignee = ghidentity.User(it.Assignees[0])
	}
	if t.URL == "" {
		t.URL = fmt.Sprintf("https://github.com/orgs/%s/projects/%d", a.cfg.Owner, a.cfg.Project)
	}
	if it.Kind == github.ItemIssue {
		t.Issue = &tracker.RepoIssue{Repo: it.Repo, Number: it.Number}
		if t.Comments, err = a.comments(ctx, it); err != nil {
			return tracker.Ticket{}, err
		}
	}
	if it.Status == "" || ghidentity.IsOwn(it.Labels) { // nobody can hand over an issue Forecrew opened
		return t, nil
	}
	h, err := a.handover(ctx, it)
	if err != nil {
		return tracker.Ticket{}, err
	}
	t.Handovers = []tracker.Handover{h}
	return t, nil
}

// handover is the card's move into its column. Only a move into the column
// of the trigger matters, and only a person's: otherwise By stays empty and
// the core sends the ticket to the Inbox.
func (a *Adapter) handover(ctx context.Context, it github.ProjectItem) (tracker.Handover, error) {
	h := tracker.Handover{Trigger: tracker.TriggerState, Value: it.Status, At: it.StatusAt}
	if it.Kind != github.ItemIssue || !strings.EqualFold(it.Status, a.conn.Triggers.State) {
		return h, nil
	}
	b, err := a.board(ctx)
	if err != nil {
		return h, err
	}
	changes, err := a.d.Projects.ProjectStatusChanges(ctx, a.cfg.Owner, it.IssueNodeID)
	if err != nil {
		return h, err
	}
	for _, c := range slices.Backward(changes) {
		if c.Project != b.ID || !strings.EqualFold(c.To, it.Status) {
			continue
		}
		// The newest move into the column is the card's own only if it is the
		// one that put it there: a card that was removed and added again, or
		// moved with no event, would otherwise be credited to an earlier
		// person's move.
		if c.At.Before(it.StatusAt.Add(-moveSkew)) {
			break
		}
		h.At = c.At
		if !c.Automated {
			h.By = ghidentity.User(c.Actor)
		}
		break
	}
	return h, nil
}

func (a *Adapter) comments(ctx context.Context, it github.ProjectItem) ([]tracker.Comment, error) {
	cs, err := a.d.Issues.ListComments(ctx, it.Repo, it.Number, time.Time{})
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Comment, 0, len(cs))
	for _, c := range cs {
		out = append(out, tracker.Comment{
			ID: strconv.FormatInt(c.ID, 10), Author: ghidentity.User(github.User{ID: c.AuthorID, Login: c.Author, Bot: c.Bot}), Body: c.Body, At: c.CreatedAt,
		})
	}
	return out, nil
}

// Comment implements tracker.Adapter. A draft takes no comments: it counts as
// gone, and the write is dropped.
func (a *Adapter) Comment(ctx context.Context, key, body string) (string, error) {
	it, err := a.item(ctx, key)
	if err != nil {
		return "", err
	}
	if it.Kind != github.ItemIssue {
		return "", fmt.Errorf("%s is a draft card, which has no comments: %w", key, tracker.ErrNotFound)
	}
	c, err := a.d.Issues.CreateComment(ctx, it.Repo, it.Number, body)
	if errors.Is(err, github.ErrNotFound) {
		return "", fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(c.ID, 10), nil
}

// Transition implements tracker.Adapter. A column that was renamed or
// deleted is reported, and nothing moves.
func (a *Adapter) Transition(ctx context.Context, key, state string) error {
	b, err := a.board(ctx)
	if err != nil {
		return err
	}
	opt, ok := column(b, state)
	if !ok {
		return fmt.Errorf("%q is not a column of project %d any more (it has %s): fix the route's states: %w", state, a.cfg.Project, names(b), tracker.ErrInvalid)
	}
	err = a.d.Projects.SetProjectStatus(ctx, a.cfg.Owner, b, key, opt.ID)
	if errors.Is(err, github.ErrNotFound) {
		return fmt.Errorf("%s: %w", key, tracker.ErrNotFound)
	}
	return err
}

// States implements tracker.Adapter: the columns.
func (a *Adapter) States(ctx context.Context) ([]tracker.State, error) {
	b, err := a.board(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.State, 0, len(b.Options))
	for _, o := range b.Options {
		closed := slices.ContainsFunc(a.cfg.Closed, func(c string) bool { return strings.EqualFold(c, o.Name) })
		out = append(out, tracker.State{Name: o.Name, Closed: closed})
	}
	return out, nil
}

// LinkedUser implements tracker.Linker.
func (a *Adapter) LinkedUser(ctx context.Context, accountID uuid.UUID, u tracker.User) (uuid.UUID, bool, error) {
	return ghidentity.LinkedUser(ctx, a.d.Store, accountID, u)
}

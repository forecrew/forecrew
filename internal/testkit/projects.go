package testkit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

var _ github.Projects = (*Projects)(nil)

// Projects is an in-memory github.Projects: one board of an organization,
// whose cards are issues of a Tracker, or drafts. Like GitHub, it records who
// moved a card, which Move does as a person and MoveByWorkflow as automation.
type Projects struct {
	mu     sync.Mutex
	tr     *Tracker
	owner  string
	number int
	board  github.ProjectBoard
	cards  []*fakeCard
	seq    int
	moves  map[string][]github.ProjectStatusChange // by issue node ID
	// Writes counts the status changes the App made, and Reads the queries.
	Writes, Reads int
	// Err, when set, fails every call: GitHub is down.
	Err error
}

type fakeCard struct {
	id        string
	repo      string // empty for a draft
	number    int
	title     string // of a draft
	body      string
	status    string
	statusAt  time.Time
	archived  bool
	updatedAt time.Time
	pr        bool
}

// NewProjects returns the board number of owner, with these columns in its
// single-select field. Issues of tr that are cards are added with AddIssue.
func NewProjects(tr *Tracker, owner string, number int, columns ...string) *Projects {
	p := &Projects{tr: tr, owner: owner, number: number, moves: map[string][]github.ProjectStatusChange{}}
	p.board = github.ProjectBoard{ID: fmt.Sprintf("PVT_%d", number), FieldID: "PVTSSF_status"}
	for _, c := range columns {
		p.board.Options = append(p.board.Options, github.ProjectOption{ID: "opt-" + strings.ToLower(c), Name: c})
	}
	return p
}

func (p *Projects) now() time.Time { return p.tr.now() }

// IssueNodeID is the node ID the fake gives an issue.
func IssueNodeID(repo string, number int) string { return fmt.Sprintf("I_%s#%d", repo, number) }

func (p *Projects) add(c *fakeCard) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	c.id = fmt.Sprintf("PVTI_%d", p.seq)
	c.updatedAt = p.now()
	p.cards = append(p.cards, c)
	return c.id
}

// AddIssue puts an issue on the board, in no column, and returns the card's ID.
func (p *Projects) AddIssue(repo string, number int) string {
	return p.add(&fakeCard{repo: repo, number: number})
}

// AddPullRequest puts a pull request on the board.
func (p *Projects) AddPullRequest(repo string, number int) string {
	return p.add(&fakeCard{repo: repo, number: number, pr: true})
}

// AddDraft puts a draft card on the board.
func (p *Projects) AddDraft(title, body string) string {
	return p.add(&fakeCard{title: title, body: body})
}

func (p *Projects) card(id string) (*fakeCard, bool) {
	for _, c := range p.cards {
		if c.id == id {
			return c, true
		}
	}
	return nil, false
}

// Move moves a card to a column as a person does.
func (p *Projects) Move(id string, by github.User, column string) { p.move(id, &by, column, false) }

// WorkflowOwner is who GitHub names as the actor of a board workflow: the
// person who set it up, whose hand did not move the card.
var WorkflowOwner = github.User{ID: 13, Login: "workflow-owner"}

// MoveByWorkflow moves a card to a column as a board workflow does. GitHub
// flags the event as automated, and names an actor all the same.
func (p *Projects) MoveByWorkflow(id, column string) { p.move(id, &WorkflowOwner, column, true) }

// MoveUnrecorded moves a card to a column with no event on the timeline of its
// issue, as when GitHub does not say who did it.
func (p *Projects) MoveUnrecorded(id, column string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.card(id)
	if !ok {
		panic("no card " + id)
	}
	c.status, c.statusAt, c.updatedAt = column, p.now(), p.now()
}

// MoveOnAnotherBoard records that a person moved the same issue's card on
// another board to a column, which the timeline of the issue shows next to
// this board's moves.
func (p *Projects) MoveOnAnotherBoard(repo string, number int, by github.User, column string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := IssueNodeID(repo, number)
	p.moves[n] = append(p.moves[n], github.ProjectStatusChange{Actor: by, To: column, At: p.now(), Project: "PVT_other"})
}

func (p *Projects) move(id string, by *github.User, column string, automated bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.card(id)
	if !ok {
		panic("no card " + id)
	}
	ch := github.ProjectStatusChange{From: c.status, To: column, At: p.now(), Automated: automated, Project: p.board.ID}
	if by != nil {
		ch.Actor = *by
	}
	c.status, c.statusAt, c.updatedAt = column, ch.At, ch.At
	if c.repo != "" {
		n := IssueNodeID(c.repo, c.number)
		p.moves[n] = append(p.moves[n], ch)
	}
}

// Touch marks a card as changed, as an edit does.
func (p *Projects) Touch(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.card(id); ok {
		c.updatedAt = p.now()
	}
}

// Archive archives a card; Remove deletes it.
func (p *Projects) Archive(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.card(id); ok {
		c.archived, c.updatedAt = true, p.now()
	}
}

// Remove deletes a card.
func (p *Projects) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cards = slices.DeleteFunc(p.cards, func(c *fakeCard) bool { return c.id == id })
}

// RenameColumn renames a column; its cards go with it.
func (p *Projects) RenameColumn(from, to string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, o := range p.board.Options {
		if o.Name == from {
			p.board.Options[i].Name = to
		}
	}
	for _, c := range p.cards {
		if c.status == from {
			c.status = to
		}
	}
}

// RemoveColumn deletes a column; its cards are left in none.
func (p *Projects) RemoveColumn(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.board.Options = slices.DeleteFunc(p.board.Options, func(o github.ProjectOption) bool { return o.Name == name })
	for _, c := range p.cards {
		if c.status == name {
			c.status = ""
		}
	}
}

// Column returns the column of a card.
func (p *Projects) Column(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.card(id); ok {
		return c.status
	}
	return ""
}

func (p *Projects) ours(owner string, number int) bool {
	return strings.EqualFold(owner, p.owner) && number == p.number
}

// item builds what GitHub says of a card.
func (p *Projects) item(ctx context.Context, c *fakeCard) github.ProjectItem {
	it := github.ProjectItem{ID: c.id, Kind: github.ItemDraft, Archived: c.archived, Status: c.status, StatusAt: c.statusAt, UpdatedAt: c.updatedAt, Title: c.title, Body: c.body}
	switch {
	case c.pr:
		return github.ProjectItem{ID: c.id, Kind: github.ItemOther, Archived: c.archived, Status: c.status, StatusAt: c.statusAt, UpdatedAt: c.updatedAt}
	case c.repo == "":
		return it
	}
	i, err := p.tr.GetIssue(ctx, c.repo, c.number)
	if err != nil {
		return github.ProjectItem{ID: c.id, Kind: github.ItemOther, Archived: c.archived, Status: c.status, StatusAt: c.statusAt, UpdatedAt: c.updatedAt} // GitHub redacts what the App cannot read
	}
	it.Kind, it.Title, it.Body, it.URL, it.Repo, it.Number, it.IssueNodeID = github.ItemIssue, i.Title, i.Body, i.URL, c.repo, c.number, IssueNodeID(c.repo, c.number)
	it.Closed, it.Type, it.Labels, it.Assignees, it.Author = i.State == "closed", i.Type, i.Labels, i.Assignees, i.Author
	if i.UpdatedAt.After(it.UpdatedAt) {
		it.UpdatedAt = i.UpdatedAt
	}
	return it
}

// ProjectBoard implements github.Projects.
func (p *Projects) ProjectBoard(_ context.Context, owner string, number int, _ string) (github.ProjectBoard, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Reads++
	if p.Err != nil {
		return github.ProjectBoard{}, p.Err
	}
	if !p.ours(owner, number) {
		return github.ProjectBoard{}, fmt.Errorf("project %d of %s: %w", number, owner, github.ErrNotFound)
	}
	b := p.board
	b.Options = slices.Clone(b.Options)
	return b, nil
}

// ProjectItems implements github.Projects.
func (p *Projects) ProjectItems(ctx context.Context, owner string, number int, _ string, since time.Time) ([]github.ProjectItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Reads++
	if p.Err != nil {
		return nil, p.Err
	}
	if !p.ours(owner, number) {
		return nil, fmt.Errorf("project %d of %s: %w", number, owner, github.ErrNotFound)
	}
	var out []github.ProjectItem
	for _, c := range p.cards {
		if it := p.item(ctx, c); !it.UpdatedAt.Before(since) {
			out = append(out, it)
		}
	}
	return out, nil
}

// ProjectItem implements github.Projects.
func (p *Projects) ProjectItem(ctx context.Context, owner string, number int, _, id string) (github.ProjectItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Reads++
	if p.Err != nil {
		return github.ProjectItem{}, p.Err
	}
	c, ok := p.card(id)
	if !ok || !p.ours(owner, number) {
		return github.ProjectItem{}, fmt.Errorf("card %s: %w", id, github.ErrNotFound)
	}
	return p.item(ctx, c), nil
}

// ProjectStatusChanges implements github.Projects.
func (p *Projects) ProjectStatusChanges(_ context.Context, _, issueNodeID string) ([]github.ProjectStatusChange, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Reads++
	if p.Err != nil {
		return nil, p.Err
	}
	return slices.Clone(p.moves[issueNodeID]), nil
}

// SetProjectStatus implements github.Projects. The App is the actor.
func (p *Projects) SetProjectStatus(_ context.Context, owner string, board github.ProjectBoard, itemID, optionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	c, ok := p.card(itemID)
	i := slices.IndexFunc(p.board.Options, func(o github.ProjectOption) bool { return o.ID == optionID })
	if !ok || !p.ours(owner, p.number) || board.ID != p.board.ID || i < 0 {
		return fmt.Errorf("move card %s: %w", itemID, github.ErrNotFound)
	}
	p.Writes++
	ch := github.ProjectStatusChange{Actor: github.User{ID: 12, Login: TrackerBot, Bot: true}, From: c.status, To: p.board.Options[i].Name, At: p.now(), Project: p.board.ID}
	c.status, c.statusAt, c.updatedAt = ch.To, ch.At, ch.At
	if c.repo != "" {
		n := IssueNodeID(c.repo, c.number)
		p.moves[n] = append(p.moves[n], ch)
	}
	return nil
}

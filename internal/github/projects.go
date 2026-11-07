package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"
)

// ProjectItemKind is what a card of a project board is.
type ProjectItemKind string

// Kinds of cards.
const (
	ItemIssue ProjectItemKind = "issue"
	ItemDraft ProjectItemKind = "draft" // a card that is not an issue and has no repo
	ItemOther ProjectItemKind = "other" // a pull request, or a card the App cannot read
)

// ProjectOption is a column of a board: an option of its single-select field.
type ProjectOption struct {
	ID   string
	Name string
}

// ProjectBoard is a board and the single-select field that holds its columns.
type ProjectBoard struct {
	ID      string // the project's node ID
	FieldID string
	Options []ProjectOption
}

// ProjectItem is a card.
type ProjectItem struct {
	ID       string // the item's node ID: stable for the life of the card
	Kind     ProjectItemKind
	Archived bool
	// Status is the column's name, and StatusAt when the card got there;
	// empty for a card with no column.
	Status   string
	StatusAt time.Time
	Title    string
	Body     string
	URL      string // the issue's
	// The issue the card is; zero for a draft.
	Repo        string
	Number      int
	IssueNodeID string
	Closed      bool // the issue is closed
	Type        string
	Labels      []string
	Assignees   []User
	Author      User
	UpdatedAt   time.Time // the newest change of the card or its issue
}

// ProjectStatusChange is a card moving between columns, as the timeline of
// its issue tells.
type ProjectStatusChange struct {
	Actor     User // empty when the event has none
	From, To  string
	At        time.Time
	Automated bool   // a board workflow moved it, not a person
	Project   string // the project's node ID
}

// Projects is the GitHub Projects (v2) surface of the project board tracker.
// Client implements it. Boards are organization-level: an App cannot reach a
// board a user owns. The App needs the organization permission Projects, read
// and write, and issues read for the timeline.
type Projects interface {
	// ProjectBoard reads a board and its single-select field. ErrNotFound: the
	// organization has no such board, or the field is not a single select.
	ProjectBoard(ctx context.Context, owner string, number int, field string) (ProjectBoard, error)
	// ProjectItems returns the cards changed at or after since, in any state.
	ProjectItems(ctx context.Context, owner string, number int, field string, since time.Time) ([]ProjectItem, error)
	// ProjectItem reads one card by node ID. ErrNotFound when it is gone or
	// belongs to another board.
	ProjectItem(ctx context.Context, owner string, number int, field, id string) (ProjectItem, error)
	// ProjectStatusChanges returns the moves of an issue's cards between
	// columns, oldest first.
	ProjectStatusChanges(ctx context.Context, owner, issueNodeID string) ([]ProjectStatusChange, error)
	// SetProjectStatus moves a card to a column of board.
	SetProjectStatus(ctx context.Context, owner string, board ProjectBoard, itemID, optionID string) error
}

var _ Projects = (*Client)(nil)

// graphqlURL is the GraphQL endpoint next to the REST API root.
func (c *Client) graphqlURL() string {
	base := strings.TrimSuffix(c.base.String(), "/")
	if root, ok := strings.CutSuffix(base, "/api/v3"); ok {
		return root + "/api/graphql"
	}
	return base + "/graphql"
}

// orgToken returns the Client's own installation token for an organization,
// cached like the repo tokens.
func (c *Client) orgToken(ctx context.Context, org string) (Token, error) {
	key := "org:" + strings.ToLower(org)
	c.mu.Lock()
	tok, ok := c.own[key]
	id, known := c.installations[key]
	c.mu.Unlock()
	if ok && c.now().Before(tok.ExpiresAt.Add(-refreshMargin)) {
		return tok, nil
	}
	app, err := c.appAPI()
	if err != nil {
		return Token{}, err
	}
	if !known {
		inst, _, err := app.Apps.GetOrganizationInstallation(ctx, org)
		if err != nil {
			if statusOf(err) == http.StatusNotFound {
				return Token{}, fmt.Errorf("installation of %s: %w", org, ErrNotInstalled)
			}
			return Token{}, wrap("find installation of "+org, err)
		}
		id = inst.GetID()
	}
	// Only what the board needs: moving cards, and reading the issues behind them.
	perms := &gh.InstallationPermissions{OrganizationProjects: new("write"), Issues: new("read")}
	it, _, err := app.Apps.CreateInstallationToken(ctx, id, &gh.InstallationTokenOptions{Permissions: perms})
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			c.mu.Lock()
			delete(c.installations, key)
			c.mu.Unlock()
			return Token{}, fmt.Errorf("token for %s: %w", org, ErrNotInstalled)
		}
		return Token{}, wrap("create installation token for "+org, err)
	}
	tok = Token{Value: it.GetToken(), ExpiresAt: it.GetExpiresAt().UTC()}
	c.mu.Lock()
	c.installations[key], c.own[key] = id, tok
	c.mu.Unlock()
	return tok, nil
}

type graphqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// contentError reports whether an error is about a card's content only: the
// issue behind a card that the App may not read. The card is then one it
// cannot use, and the rest of the answer is good.
func (e graphqlError) contentError() bool {
	n := len(e.Path)
	if n == 0 || e.Path[n-1] != "content" {
		return false
	}
	return e.Path[0] == "node" || slices.Contains(e.Path, any("nodes"))
}

// graphql runs a query as the App installed on org and decodes its data.
func (c *Client) graphql(ctx context.Context, org, query string, vars map[string]any, out any) error {
	tok, err := c.orgToken(ctx, org)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("encode graphql request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read graphql answer: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("graphql: status %d: %w", resp.StatusCode, ErrForbidden)
	case http.StatusTooManyRequests:
		return fmt.Errorf("graphql: %w", ErrRateLimited)
	default:
		return fmt.Errorf("graphql: status %d", resp.StatusCode)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphqlError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode graphql answer: %w", err)
	}
	hasData := len(env.Data) > 0 && string(env.Data) != "null"
	if i := slices.IndexFunc(env.Errors, func(e graphqlError) bool { return !hasData || !e.contentError() }); i >= 0 {
		return graphqlFailure(env.Errors[i:])
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode graphql data: %w", err)
	}
	return nil
}

// graphqlFailure maps the errors of an answer to the package's sentinels.
func graphqlFailure(errs []graphqlError) error {
	e := errs[0]
	msg := fmt.Sprintf("graphql: %s: %s", e.Type, e.Message)
	switch e.Type {
	case "NOT_FOUND":
		return fmt.Errorf("%s: %w", msg, ErrNotFound)
	case "FORBIDDEN", "INSUFFICIENT_SCOPES":
		return fmt.Errorf("%s: %w", msg, ErrForbidden)
	case "RATE_LIMITED":
		return fmt.Errorf("%s: %w", msg, ErrRateLimited)
	}
	return errors.New(msg)
}

type gqlActor struct {
	Typename   string `json:"__typename"`
	Login      string `json:"login"`
	DatabaseID int64  `json:"databaseId"`
}

func (a *gqlActor) user() User {
	if a == nil {
		return User{}
	}
	return User{ID: a.DatabaseID, Login: a.Login, Bot: a.Typename == "Bot"}
}

const actorFields = `__typename login ... on User { databaseId } ... on Bot { databaseId }`

const itemFields = `
id type isArchived updatedAt
status: fieldValueByName(name: $field) { ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt } }
content {
  __typename
  ... on Issue {
    id number title body state url updatedAt repository { nameWithOwner }
    author { ` + actorFields + ` }
    labels(first: 50) { nodes { name } }
    assignees(first: 10) { nodes { login databaseId } }
    issueType { name }
  }
  ... on DraftIssue { title body updatedAt }
}`

type gqlItem struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	IsArchived bool      `json:"isArchived"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Status     *struct {
		Name      string    `json:"name"`
		UpdatedAt time.Time `json:"updatedAt"`
	} `json:"status"`
	Content *struct {
		Typename   string    `json:"__typename"`
		ID         string    `json:"id"`
		Number     int       `json:"number"`
		Title      string    `json:"title"`
		Body       string    `json:"body"`
		State      string    `json:"state"`
		URL        string    `json:"url"`
		UpdatedAt  time.Time `json:"updatedAt"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
		Author *gqlActor `json:"author"`
		Labels struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		Assignees struct {
			Nodes []gqlActor `json:"nodes"`
		} `json:"assignees"`
		IssueType *struct {
			Name string `json:"name"`
		} `json:"issueType"`
	} `json:"content"`
}

func (g gqlItem) item() ProjectItem {
	it := ProjectItem{ID: g.ID, Kind: ItemOther, Archived: g.IsArchived, UpdatedAt: g.UpdatedAt.UTC()}
	if g.Status != nil {
		it.Status, it.StatusAt = g.Status.Name, g.Status.UpdatedAt.UTC()
		it.UpdatedAt = newest(it.UpdatedAt, it.StatusAt)
	}
	c := g.Content
	if c == nil {
		return it
	}
	it.UpdatedAt = newest(it.UpdatedAt, c.UpdatedAt.UTC())
	switch c.Typename {
	case "Issue":
		it.Kind = ItemIssue
		it.Title, it.Body, it.URL, it.Repo, it.Number, it.IssueNodeID = c.Title, c.Body, c.URL, c.Repository.NameWithOwner, c.Number, c.ID
		it.Closed = c.State == "CLOSED"
		it.Author = c.Author.user()
		if c.IssueType != nil {
			it.Type = c.IssueType.Name
		}
		for _, l := range c.Labels.Nodes {
			it.Labels = append(it.Labels, l.Name)
		}
		for i := range c.Assignees.Nodes {
			it.Assignees = append(it.Assignees, c.Assignees.Nodes[i].user())
		}
	case "DraftIssue":
		it.Kind = ItemDraft
		it.Title, it.Body = c.Title, c.Body
	}
	return it
}

func newest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

const boardQuery = `query($owner: String!, $number: Int!, $field: String!) {
  organization(login: $owner) { projectV2(number: $number) { id field(name: $field) { ... on ProjectV2SingleSelectField { id options { id name } } } } }
}`

// ProjectBoard implements Projects.
func (c *Client) ProjectBoard(ctx context.Context, owner string, number int, field string) (ProjectBoard, error) {
	var out struct {
		Organization *struct {
			ProjectV2 *struct {
				ID    string `json:"id"`
				Field *struct {
					ID      string `json:"id"`
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"field"`
			} `json:"projectV2"`
		} `json:"organization"`
	}
	if err := c.graphql(ctx, owner, boardQuery, map[string]any{"owner": owner, "number": number, "field": field}, &out); err != nil {
		return ProjectBoard{}, err
	}
	if out.Organization == nil || out.Organization.ProjectV2 == nil {
		return ProjectBoard{}, fmt.Errorf("project %d of %s: %w", number, owner, ErrNotFound)
	}
	p := out.Organization.ProjectV2
	if p.Field == nil || p.Field.ID == "" {
		return ProjectBoard{}, fmt.Errorf("project %d of %s has no single-select field %q: %w", number, owner, field, ErrNotFound)
	}
	b := ProjectBoard{ID: p.ID, FieldID: p.Field.ID}
	for _, o := range p.Field.Options {
		b.Options = append(b.Options, ProjectOption{ID: o.ID, Name: o.Name})
	}
	return b, nil
}

// scanFields is all a poll needs of a card: which it is and when it changed.
// Get reads the rest.
const scanFields = `
id isArchived updatedAt
status: fieldValueByName(name: $field) { ... on ProjectV2ItemFieldSingleSelectValue { name updatedAt } }
content { __typename ... on Issue { updatedAt } ... on DraftIssue { updatedAt } }`

const itemsQuery = `query($owner: String!, $number: Int!, $field: String!, $after: String) {
  organization(login: $owner) { projectV2(number: $number) {
    items(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes {` + scanFields + `} }
  } }
}`

// MaxScanPages bounds the pages of 100 cards a poll reads: a board of more
// cards than that is left to webhooks.
const MaxScanPages = 50

// ProjectItems implements Projects. GraphQL cannot filter a board's items by
// time, so this reads them all, up to MaxScanPages pages, and keeps the recent
// ones. The cards it returns have no more than their kind, column and time.
func (c *Client) ProjectItems(ctx context.Context, owner string, number int, field string, since time.Time) ([]ProjectItem, error) {
	var items []ProjectItem
	var after *string
	for pages := 1; ; pages++ {
		var out struct {
			Organization *struct {
				ProjectV2 *struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []gqlItem `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"organization"`
		}
		vars := map[string]any{"owner": owner, "number": number, "field": field, "after": after}
		if err := c.graphql(ctx, owner, itemsQuery, vars, &out); err != nil {
			return nil, err
		}
		if out.Organization == nil || out.Organization.ProjectV2 == nil {
			return nil, fmt.Errorf("project %d of %s: %w", number, owner, ErrNotFound)
		}
		page := out.Organization.ProjectV2.Items
		for _, n := range page.Nodes {
			if it := n.item(); !it.UpdatedAt.Before(since) {
				items = append(items, it)
			}
		}
		if !page.PageInfo.HasNextPage {
			return items, nil
		}
		if pages >= MaxScanPages {
			return nil, fmt.Errorf("project %d of %s has more than %d cards: too large to poll, rely on webhooks: %w", number, owner, MaxScanPages*100, ErrTooLarge)
		}
		after = &page.PageInfo.EndCursor
	}
}

const itemQuery = `query($id: ID!, $field: String!) {
  node(id: $id) { ... on ProjectV2Item { project { number owner { ... on Organization { login } } }` + itemFields + ` } }
}`

// ProjectItem implements Projects.
func (c *Client) ProjectItem(ctx context.Context, owner string, number int, field, id string) (ProjectItem, error) {
	var out struct {
		Node *struct {
			gqlItem
			Project *struct {
				Number int `json:"number"`
				Owner  struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"project"`
		} `json:"node"`
	}
	err := c.graphql(ctx, owner, itemQuery, map[string]any{"id": id, "field": field}, &out)
	if err != nil {
		return ProjectItem{}, err
	}
	n := out.Node
	if n == nil || n.ID == "" || n.Project == nil || n.Project.Number != number || !strings.EqualFold(n.Project.Owner.Login, owner) {
		return ProjectItem{}, fmt.Errorf("card %s of project %d of %s: %w", id, number, owner, ErrNotFound)
	}
	return n.item(), nil
}

const changesQuery = `query($id: ID!) {
  node(id: $id) { ... on Issue { timelineItems(last: 50, itemTypes: [PROJECT_V2_ITEM_STATUS_CHANGED_EVENT]) { nodes {
    ... on ProjectV2ItemStatusChangedEvent { createdAt status previousStatus wasAutomated project { id } actor { ` + actorFields + ` } }
  } } } }
}`

// ProjectStatusChanges implements Projects.
func (c *Client) ProjectStatusChanges(ctx context.Context, owner, issueNodeID string) ([]ProjectStatusChange, error) {
	var out struct {
		Node *struct {
			TimelineItems struct {
				Nodes []struct {
					CreatedAt      time.Time `json:"createdAt"`
					Status         string    `json:"status"`
					PreviousStatus string    `json:"previousStatus"`
					WasAutomated   bool      `json:"wasAutomated"`
					Project        *struct {
						ID string `json:"id"`
					} `json:"project"`
					Actor *gqlActor `json:"actor"`
				} `json:"nodes"`
			} `json:"timelineItems"`
		} `json:"node"`
	}
	if err := c.graphql(ctx, owner, changesQuery, map[string]any{"id": issueNodeID}, &out); err != nil {
		return nil, err
	}
	if out.Node == nil {
		return nil, fmt.Errorf("issue %s: %w", issueNodeID, ErrNotFound)
	}
	var changes []ProjectStatusChange
	for _, n := range out.Node.TimelineItems.Nodes {
		ch := ProjectStatusChange{Actor: n.Actor.user(), From: n.PreviousStatus, To: n.Status, At: n.CreatedAt.UTC(), Automated: n.WasAutomated}
		if n.Project != nil {
			ch.Project = n.Project.ID
		}
		changes = append(changes, ch)
	}
	return changes, nil
}

const setStatusMutation = `mutation($project: ID!, $item: ID!, $field: ID!, $option: String!) {
  updateProjectV2ItemFieldValue(input: {projectId: $project, itemId: $item, fieldId: $field, value: {singleSelectOptionId: $option}}) { projectV2Item { id } }
}`

// SetProjectStatus implements Projects.
func (c *Client) SetProjectStatus(ctx context.Context, owner string, board ProjectBoard, itemID, optionID string) error {
	var out json.RawMessage
	vars := map[string]any{"project": board.ID, "item": itemID, "field": board.FieldID, "option": optionID}
	return c.graphql(ctx, owner, setStatusMutation, vars, &out)
}

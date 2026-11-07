// Package linear is the tracker adapter of Linear (docs/trackers/linear.md),
// over its GraphQL API and signed webhooks. A ticket is a Linear issue; its
// key is the identifier (ENG-7) and its project is the team's key.
package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Kind is the kind of the connections of this adapter.
const Kind = "linear"

// DefaultEndpoint is Linear's GraphQL API.
const DefaultEndpoint = "https://api.linear.app/graphql"

// Polling and webhooks.
const (
	// overlap is how far back a poll looks before its cursor, for an issue
	// saved while the last poll ran.
	overlap  = time.Minute
	pageSize = 100
	maxPages = 10
	// webhookWindow is how old a delivery may be: Linear recommends a minute,
	// against replays.
	webhookWindow = time.Minute
)

// Deps are what the Linear adapters of a forecrewd share.
type Deps struct {
	Secrets secrets.Store
	// Endpoint is the GraphQL URL; tests point it at a fake.
	Endpoint string
	// PrivateAddresses lets the endpoint resolve to a private address
	// (trackers.private_addresses); off, it must be public.
	PrivateAddresses bool
	Now              func() time.Time
	Proxy            func(*http.Request) (*url.URL, error)
	Lookup           func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial and RootCAs let tests reach a fake over TLS.
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	RootCAs []*x509.Certificate
	Sleep   func(ctx context.Context, d time.Duration) error
	Timeout time.Duration
}

func (d Deps) withDefaults() Deps {
	if d.Endpoint == "" {
		d.Endpoint = DefaultEndpoint
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Proxy == nil {
		d.Proxy = http.ProxyFromEnvironment
	}
	if d.Lookup == nil {
		d.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, wait time.Duration) error {
			t := time.NewTimer(wait)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if d.Timeout == 0 {
		d.Timeout = defaultTimeout
	}
	return d
}

// viewer is the user of a connection's token, kept between adapters of the
// connection: webhooks use its ID to drop Forecrew's own writes.
type viewer struct{ ID, Name string }

// Factory returns the factory to register for Kind.
func Factory(d Deps) tracker.Factory {
	d = d.withDefaults()
	cache := &sync.Map{} // connection ID and token ref -> viewer
	return func(c domain.TrackerConnection) (tracker.Adapter, error) {
		cfg, err := parseConfig(c.Config)
		if err != nil {
			return nil, fmt.Errorf("connection %s: %w", c.Name, err)
		}
		a := &Adapter{d: d, conn: c, cfg: cfg, cache: cache}
		a.c = &client{http: d.httpClient(), d: d, endpoint: d.Endpoint, auth: a.authorization}
		return a, nil
	}
}

// Adapter is one connection.
type Adapter struct {
	d     Deps
	conn  domain.TrackerConnection
	cfg   Config
	c     *client
	cache *sync.Map

	mu    sync.Mutex
	token string
}

var (
	_ tracker.Adapter = (*Adapter)(nil)
	_ tracker.Checker = (*Adapter)(nil)
)

// Kind implements tracker.Adapter.
func (a *Adapter) Kind() string { return Kind }

// Capabilities implements tracker.Adapter. Webhooks are on when the
// connection has a signing secret.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Webhooks: a.cfg.WebhookSecretRef != "", Assignment: true, Labels: true, Columns: true, Transitions: true, Comments: true}
}

func (a *Adapter) secret(ctx context.Context, ref string) (string, error) {
	if a.d.Secrets == nil {
		return "", errors.New("the secret store is not configured: set FORECREW_SECRET_KEY")
	}
	b, err := secrets.Resolve(ctx, a.d.Secrets, a.conn.AccountID, ref)
	if err != nil {
		return "", fmt.Errorf("read %s of %s: %w", ref, a.conn.Name, err)
	}
	return string(b), nil
}

// authorization is the Authorization header: a personal API key goes as it
// is, an OAuth token as a Bearer.
func (a *Adapter) authorization(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == "" {
		t, err := a.secret(ctx, a.cfg.TokenRef)
		if err != nil {
			return "", err
		}
		a.token = strings.TrimSpace(t)
	}
	if a.cfg.Auth == AuthOAuthToken {
		return "Bearer " + a.token, nil
	}
	return a.token, nil
}

func (a *Adapter) viewerKey() string { return a.conn.ID.String() + "\x00" + a.cfg.TokenRef }

// me reads the token's user, once per connection and token.
func (a *Adapter) me(ctx context.Context) (viewer, error) {
	if v, ok := a.cache.Load(a.viewerKey()); ok {
		return v.(viewer), nil
	}
	var out struct {
		Viewer struct{ ID, Name string } `json:"viewer"`
	}
	if err := a.c.gql(ctx, `query { viewer { id name } }`, nil, &out, true); err != nil {
		return viewer{}, fmt.Errorf("read the token's user: %w", err)
	}
	v := viewer{ID: out.Viewer.ID, Name: out.Viewer.Name}
	a.cache.Store(a.viewerKey(), v)
	return v, nil
}

func (a *Adapter) notFound(key string) error { return fmt.Errorf("%s: %w", key, tracker.ErrNotFound) }

// teamList is the teams of the connection as a GraphQL list literal; the keys
// were checked against teamKeyRE, so they hold nothing to escape.
func (a *Adapter) teamList() string {
	q := make([]string, len(a.cfg.Teams))
	for i, t := range a.cfg.Teams {
		q[i] = strconv.Quote(t)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// ---- tests

// Verify implements tracker.Adapter.
func (a *Adapter) Verify(ctx context.Context) error { return tracker.Failed(a.Check(ctx)) }

// Check implements tracker.Checker: what can fail, each on its own.
func (a *Adapter) Check(ctx context.Context) []tracker.Check {
	out := []tracker.Check{}
	add := func(name string, err error, ok string) bool {
		c := tracker.Check{Name: name, OK: err == nil, Detail: ok}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
		return err == nil
	}
	skip := func(names ...string) []tracker.Check {
		for _, n := range names {
			out = append(out, tracker.Check{Name: n, Detail: "not checked: an earlier check failed"})
		}
		return out
	}
	a.cache.Delete(a.viewerKey()) // a test asks again
	v, err := a.me(ctx)
	var certErr *tls.CertificateVerificationError
	var api *apiError
	switch {
	case errors.As(err, &certErr):
		add("address", nil, "reached "+a.d.Endpoint)
		add("certificate", fmt.Errorf("not trusted: %v", certErr.Err), "")
		return skip("login", "projects", "transitions")
	case err != nil && !errors.As(err, &api):
		add("address", err, "")
		return skip("certificate", "login", "projects", "transitions")
	}
	add("address", nil, "reached "+a.d.Endpoint)
	add("certificate", nil, "trusted")
	if !add("login", err, "signed in as "+v.Name+" ("+v.ID+")") {
		return skip("projects", "transitions")
	}
	if !add("projects", a.checkTeams(ctx), strings.Join(a.cfg.Teams, ", ")+": found") {
		return skip("transitions")
	}
	states, err := a.States(ctx)
	if err == nil && !slices.ContainsFunc(states, func(s tracker.State) bool { return s.Closed }) {
		err = errors.New("no workflow state of the teams is completed or canceled")
	}
	add("transitions", err, fmt.Sprintf("%d workflow states", len(states)))
	return out
}

// checkTeams checks that the token's user sees each team.
func (a *Adapter) checkTeams(ctx context.Context) error {
	var out struct {
		Teams struct {
			Nodes []struct{ Key string } `json:"nodes"`
		} `json:"teams"`
	}
	if err := a.c.gql(ctx, `query { teams(first: 250) { nodes { key } } }`, nil, &out, true); err != nil {
		return fmt.Errorf("list the teams: %w", err)
	}
	var missing []string
	for _, t := range a.cfg.Teams {
		if !slices.ContainsFunc(out.Teams.Nodes, func(n struct{ Key string }) bool { return n.Key == t }) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the token's user cannot see the team %s", strings.Join(missing, ", "))
	}
	return nil
}

// ---- webhooks

// ParseEvent implements tracker.Adapter. A delivery is signed with the
// webhook's secret (Linear-Signature, HMAC-SHA256 of the body in hex) and
// dated; one older than a minute is a replay. Of the payload it keeps the
// issue it is about, and who did it, to drop the connection's own writes.
func (a *Adapter) ParseEvent(r tracker.Request) ([]tracker.Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := a.verified(ctx, r)
	if err != nil {
		return nil, err
	}
	if p.Action == "remove" {
		return nil, nil
	}
	key, id := p.issue()
	if key == "" && id != "" { // the payload names the issue by its ID only
		if key, err = a.identifier(ctx, id); err != nil {
			return nil, err
		}
	}
	if !a.cfg.ours(key) {
		return nil, nil
	}
	delivery := r.Header.Get("Linear-Delivery")
	if delivery == "" {
		sum := sha256.Sum256(r.Body)
		delivery = hex.EncodeToString(sum[:16])
	}
	self := false
	if v, ok := a.cache.Load(a.viewerKey()); ok && p.Actor != nil {
		self = p.Actor.ID != "" && p.Actor.ID == v.(viewer).ID
	}
	return []tracker.Event{{ID: delivery, Key: key, Self: self}}, nil
}

// payload is what of a webhook delivery the adapter reads.
type payload struct {
	Action           string `json:"action"`
	Type             string `json:"type"`
	WebhookTimestamp int64  `json:"webhookTimestamp"`
	Actor            *struct {
		ID string `json:"id"`
	} `json:"actor"`
	Data struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Number     int    `json:"number"`
		IssueID    string `json:"issueId"`
		Team       *struct {
			Key string `json:"key"`
		} `json:"team"`
		Issue *struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
		} `json:"issue"`
	} `json:"data"`
}

// issue is the key and the ID of the issue a delivery is about, whichever it
// gives; neither for a type of event the adapter does not use.
func (p payload) issue() (key, id string) {
	d := p.Data
	switch p.Type {
	case "Issue":
		if key = d.Identifier; key == "" && d.Team != nil && d.Number > 0 {
			key = d.Team.Key + "-" + strconv.Itoa(d.Number)
		}
		return key, d.ID
	case "Comment":
		if d.Issue != nil {
			return d.Issue.Identifier, d.Issue.ID
		}
		return "", d.IssueID
	}
	return "", ""
}

// verified checks the signature and the date of a delivery, then reads it.
func (a *Adapter) verified(ctx context.Context, r tracker.Request) (payload, error) {
	if a.cfg.WebhookSecretRef == "" {
		return payload{}, fmt.Errorf("%w: the connection takes no webhooks", tracker.ErrBadSignature)
	}
	secret, err := a.secret(ctx, a.cfg.WebhookSecretRef)
	if err != nil {
		return payload{}, err
	}
	got, err := hex.DecodeString(r.Header.Get("Linear-Signature"))
	if err != nil || len(got) == 0 {
		return payload{}, fmt.Errorf("%w: no Linear-Signature", tracker.ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(secret)))
	mac.Write(r.Body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return payload{}, fmt.Errorf("%w: the signature does not match", tracker.ErrBadSignature)
	}
	var p payload
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return payload{}, fmt.Errorf("read the delivery: %w", err)
	}
	age := a.d.Now().Sub(time.UnixMilli(p.WebhookTimestamp))
	if p.WebhookTimestamp == 0 || age > webhookWindow || age < -webhookWindow {
		return payload{}, fmt.Errorf("%w: the delivery is not from the last minute", tracker.ErrBadSignature)
	}
	return p, nil
}

// identifier turns an issue's ID into its key.
func (a *Adapter) identifier(ctx context.Context, id string) (string, error) {
	var out struct {
		Issue *struct{ Identifier string } `json:"issue"`
	}
	if err := a.c.gql(ctx, `query($id: String!) { issue(id: $id) { identifier } }`, map[string]any{"id": id}, &out, true); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.notFound() {
			return "", nil
		}
		return "", fmt.Errorf("read the issue of a delivery: %w", err)
	}
	if out.Issue == nil {
		return "", nil
	}
	return out.Issue.Identifier, nil
}

// ---- polling

// Changes implements tracker.Adapter: the issues of the teams updated since
// the cursor, and those with a comment edited since. The cursor is the
// newest update seen.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	if cursor == "" {
		return nil, a.d.Now().Format(time.RFC3339Nano), nil
	}
	since, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", cursor, err)
	}
	from := since.Add(-overlap).UTC().Format(time.RFC3339Nano)
	newest := since
	seen := map[string]bool{}
	var events []tracker.Event
	take := func(key, updated string) error {
		t, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return fmt.Errorf("time %q: %w", updated, err)
		}
		if t.After(newest) {
			newest = t
		}
		if a.cfg.ours(key) && !seen[key] {
			seen[key] = true
			events = append(events, tracker.Event{Key: key})
		}
		return nil
	}
	issues := fmt.Sprintf(`query($after: String) { issues(first: %d, after: $after, orderBy: updatedAt, filter: {team: {key: {in: %s}}, updatedAt: {gte: %q}}) {
		nodes { identifier updatedAt } pageInfo { hasNextPage endCursor } } }`, pageSize, a.teamList(), from)
	err = page(ctx, a.c, issues, "issues", func(n struct{ Identifier, UpdatedAt string }) error { return take(n.Identifier, n.UpdatedAt) })
	if err != nil {
		return nil, "", fmt.Errorf("poll the issues: %w", err)
	}
	comments := fmt.Sprintf(`query($after: String) { comments(first: %d, after: $after, filter: {updatedAt: {gte: %q}}) {
		nodes { updatedAt issue { identifier } } pageInfo { hasNextPage endCursor } } }`, pageSize, from)
	err = page(ctx, a.c, comments, "comments", func(n struct {
		UpdatedAt string
		Issue     *struct{ Identifier string }
	}) error {
		if n.Issue == nil {
			return nil
		}
		return take(n.Issue.Identifier, n.UpdatedAt)
	})
	if err != nil {
		return nil, "", fmt.Errorf("poll the comments: %w", err)
	}
	return events, newest.UTC().Format(time.RFC3339Nano), nil
}

// page reads a connection field page after page, up to maxPages.
func page[N any](ctx context.Context, c *client, query, field string, each func(N) error) error {
	after := ""
	for range maxPages {
		vars := map[string]any{"after": nil}
		if after != "" {
			vars["after"] = after
		}
		var raw map[string]struct {
			Nodes    []N `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		if err := c.gql(ctx, query, vars, &raw, true); err != nil {
			return err
		}
		conn := raw[field]
		for _, n := range conn.Nodes {
			if err := each(n); err != nil {
				return err
			}
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			return nil
		}
		after = conn.PageInfo.EndCursor
	}
	return nil
}

// ---- tickets

type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (u *user) neutral() tracker.User {
	if u == nil {
		return tracker.User{}
	}
	return tracker.User{ID: u.ID, Name: u.Name}
}

const issueQuery = `query($id: String!) { issue(id: $id) {
	id identifier title description url updatedAt
	team { key }
	state { name type }
	assignee { id name }
	creator { id name }
	parent { identifier }
	labels(first: 100) { nodes { name } }
	comments(last: 100) { nodes { id body createdAt user { id name } } }
	history(last: 100) { nodes { createdAt actor { id name } toAssigneeId addedLabels { name } toState { name } } }
} }`

type issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	UpdatedAt   string `json:"updatedAt"`
	Team        struct{ Key string }
	State       struct{ Name, Type string }
	Assignee    *user
	Creator     *user
	Parent      *struct{ Identifier string }
	Labels      struct{ Nodes []struct{ Name string } }
	Comments    struct {
		Nodes []struct {
			ID        string
			Body      string
			CreatedAt string `json:"createdAt"`
			User      *user
		}
	}
	History struct {
		Nodes []struct {
			CreatedAt    string `json:"createdAt"`
			Actor        *user
			ToAssigneeID string `json:"toAssigneeId"`
			AddedLabels  []struct{ Name string }
			ToState      *struct{ Name string }
		}
	}
}

func closedType(t string) bool { return t == "completed" || t == "canceled" }

func when(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// Get implements tracker.Adapter. Who handed the issue over comes from its
// history, so polling knows what a webhook would. An issue of another team is
// not found: the connection reads only its own.
func (a *Adapter) Get(ctx context.Context, key string) (tracker.Ticket, error) {
	if !a.cfg.ours(key) {
		return tracker.Ticket{}, a.notFound(key)
	}
	var out struct {
		Issue *issue `json:"issue"`
	}
	if err := a.c.gql(ctx, issueQuery, map[string]any{"id": key}, &out, true); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.notFound() {
			return tracker.Ticket{}, a.notFound(key)
		}
		return tracker.Ticket{}, fmt.Errorf("read %s: %w", key, err)
	}
	i := out.Issue
	if i == nil || i.Identifier != key || !slices.Contains(a.cfg.Teams, i.Team.Key) { // moved to another team
		return tracker.Ticket{}, a.notFound(key)
	}
	return a.ticket(i)
}

// ticket turns an issue into the neutral ticket.
func (a *Adapter) ticket(i *issue) (tracker.Ticket, error) {
	updated, err := when(i.UpdatedAt)
	if err != nil {
		return tracker.Ticket{}, err
	}
	t := tracker.Ticket{
		Ref: tracker.Ref{Connection: a.conn.ID, Key: i.Identifier}, Project: i.Team.Key, Title: i.Title, Body: i.Description,
		State: i.State.Name, Closed: closedType(i.State.Type), URL: i.URL, Updated: updated,
		Assignee: i.Assignee.neutral(), Reporter: i.Creator.neutral(),
	}
	for _, l := range i.Labels.Nodes {
		t.Labels = append(t.Labels, l.Name)
		if strings.EqualFold(l.Name, "bug") {
			t.Type = "Bug" // Linear has no issue types: its Bug label stands for one
		}
	}
	if i.Parent != nil {
		t.Parent = i.Parent.Identifier
	}
	if t.Comments, err = i.comments(); err != nil {
		return tracker.Ticket{}, err
	}
	if t.Handovers, err = i.handovers(); err != nil {
		return tracker.Ticket{}, err
	}
	return t, nil
}

// comments are the issue's comments, oldest first: Linear sends the newest first.
func (i *issue) comments() ([]tracker.Comment, error) {
	var out []tracker.Comment
	for _, c := range i.Comments.Nodes {
		at, err := when(c.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, tracker.Comment{ID: c.ID, Author: c.User.neutral(), Body: c.Body, At: at})
	}
	sort.SliceStable(out, func(x, y int) bool { return out[x].At.Before(out[y].At) })
	return out, nil
}

// handovers read who assigned, labeled or moved the issue from its history,
// oldest first.
func (i *issue) handovers() ([]tracker.Handover, error) {
	var out []tracker.Handover
	for _, h := range i.History.Nodes {
		at, err := when(h.CreatedAt)
		if err != nil {
			return nil, err
		}
		by := h.Actor.neutral()
		if h.ToAssigneeID != "" {
			out = append(out, tracker.Handover{Trigger: tracker.TriggerAssignee, Value: h.ToAssigneeID, By: by, At: at})
		}
		for _, l := range h.AddedLabels {
			out = append(out, tracker.Handover{Trigger: tracker.TriggerLabel, Value: l.Name, By: by, At: at})
		}
		if h.ToState != nil {
			out = append(out, tracker.Handover{Trigger: tracker.TriggerState, Value: h.ToState.Name, By: by, At: at})
		}
	}
	sort.SliceStable(out, func(x, y int) bool { return out[x].At.Before(out[y].At) })
	return out, nil
}

// issueID reads the ID of an issue, which mutations take.
func (a *Adapter) issueID(ctx context.Context, key string) (string, error) {
	if !a.cfg.ours(key) {
		return "", a.notFound(key)
	}
	var out struct {
		Issue *struct {
			ID   string
			Team struct{ Key string }
		} `json:"issue"`
	}
	err := a.c.gql(ctx, `query($id: String!) { issue(id: $id) { id team { key } } }`, map[string]any{"id": key}, &out, true)
	var api *apiError
	switch {
	case errors.As(err, &api) && api.notFound():
		return "", a.notFound(key)
	case err != nil:
		return "", fmt.Errorf("read %s: %w", key, err)
	case out.Issue == nil || !slices.Contains(a.cfg.Teams, out.Issue.Team.Key):
		return "", a.notFound(key)
	}
	return out.Issue.ID, nil
}

// Comment implements tracker.Adapter. Linear takes Markdown. The mutation is
// never tried again within the call.
func (a *Adapter) Comment(ctx context.Context, key, body string) (string, error) {
	id, err := a.issueID(ctx, key)
	if err != nil {
		return "", err
	}
	var out struct {
		CommentCreate struct {
			Success bool
			Comment *struct{ ID string }
		} `json:"commentCreate"`
	}
	q := `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id } } }`
	if err := a.c.gql(ctx, q, map[string]any{"input": map[string]any{"issueId": id, "body": body}}, &out, false); err != nil {
		return "", fmt.Errorf("comment on %s: %w", key, err)
	}
	if !out.CommentCreate.Success || out.CommentCreate.Comment == nil {
		return "", fmt.Errorf("comment on %s: linear did not create it", key)
	}
	return out.CommentCreate.Comment.ID, nil
}

// Transition implements tracker.Adapter. A Linear workflow lets an issue go to
// any state of its team, so only a state the team has not is blocked. An issue
// already there is left alone.
func (a *Adapter) Transition(ctx context.Context, key, state string) error {
	if !a.cfg.ours(key) {
		return a.notFound(key)
	}
	var out struct {
		Issue *struct {
			ID    string
			State struct{ Name string }
			Team  struct {
				Key    string
				States struct {
					Nodes []struct{ ID, Name string }
				}
			}
		} `json:"issue"`
	}
	err := a.c.gql(ctx, `query($id: String!) { issue(id: $id) { id state { name } team { key states(first: 250) { nodes { id name } } } } }`, map[string]any{"id": key}, &out, true)
	var api *apiError
	switch {
	case errors.As(err, &api) && api.notFound():
		return a.notFound(key)
	case err != nil:
		return fmt.Errorf("read the states of %s: %w", key, err)
	case out.Issue == nil || !slices.Contains(a.cfg.Teams, out.Issue.Team.Key):
		return a.notFound(key)
	}
	i := out.Issue
	if strings.EqualFold(i.State.Name, state) {
		return nil
	}
	var names []string
	for _, s := range i.Team.States.Nodes {
		names = append(names, s.Name)
		if !strings.EqualFold(s.Name, state) {
			continue
		}
		q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
		var res struct {
			IssueUpdate struct{ Success bool } `json:"issueUpdate"`
		}
		if err := a.c.gql(ctx, q, map[string]any{"id": i.ID, "input": map[string]any{"stateId": s.ID}}, &res, false); err != nil {
			return fmt.Errorf("move %s to %s: %w", key, state, err)
		}
		if !res.IssueUpdate.Success {
			return fmt.Errorf("move %s to %s: linear did not do it", key, state)
		}
		return nil
	}
	return fmt.Errorf("the team %s has no state %q; it has %s: %w", i.Team.Key, state, strings.Join(names, ", "), tracker.ErrBlocked)
}

// States implements tracker.Adapter: the workflow states of the connection's
// teams, closed when completed or canceled.
func (a *Adapter) States(ctx context.Context) ([]tracker.State, error) {
	q := fmt.Sprintf(`query($after: String) { workflowStates(first: 250, after: $after, filter: {team: {key: {in: %s}}}) {
		nodes { name type } pageInfo { hasNextPage endCursor } } }`, a.teamList())
	var out []tracker.State
	err := page(ctx, a.c, q, "workflowStates", func(n struct{ Name, Type string }) error {
		i := slices.IndexFunc(out, func(s tracker.State) bool { return strings.EqualFold(s.Name, n.Name) })
		switch {
		case i < 0:
			out = append(out, tracker.State{Name: n.Name, Closed: closedType(n.Type)})
		case !closedType(n.Type):
			out[i].Closed = false // open in one team: not a state to close with
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the workflow states: %w", err)
	}
	return out, nil
}

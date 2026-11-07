// Package jira is the tracker adapter of Jira, Cloud and Data Center alike
// (docs/trackers/jira.md). The two share most of the REST API; what differs
// (the API version, the text format, how users are named and how a search
// pages) is in small functions that look at the deployment, read from
// serverInfo when a connection is set up.
package jira

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/tracker"
)

// Kind is the kind of the connections of this adapter.
const Kind = "jira"

// MinDataCenter is the oldest Jira Data Center the adapter supports, the
// oldest Long Term Support release Atlassian still supported when it was
// written.
var MinDataCenter = [2]int{10, 3}

// Polling.
const (
	// overlap is how far back a poll looks before its cursor: JQL compares
	// minutes, and an issue saved while the last poll ran must come again.
	overlap   = 5 * time.Minute
	pageSize  = 100
	maxPages  = 10
	jiraTime  = "2006-01-02T15:04:05.000-0700"
	jqlLayout = "2006/01/02 15:04"
)

// Deps are what the Jira adapters of a forecrewd share.
type Deps struct {
	Secrets secrets.Store
	// PrivateAddresses lets connections reach private and loopback
	// addresses: a self-hosted forecrewd in the same network as its Jira
	// (trackers.private_addresses). Off, a base URL or a redirect to such an
	// address is refused, so a connection cannot reach Forecrew's own
	// network.
	PrivateAddresses bool
	Now              func() time.Time
	// Proxy picks the proxy of a request; it defaults to the server's
	// HTTPS_PROXY and NO_PROXY.
	Proxy func(*http.Request) (*url.URL, error)
	// Lookup resolves a host; it defaults to the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial replaces the dialer, in tests.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Sleep waits out a rate limit.
	Sleep func(ctx context.Context, d time.Duration) error
	// Timeout bounds one request.
	Timeout time.Duration
	// Gateway is where scoped Cloud tokens go: https://api.atlassian.com.
	Gateway string
}

func (d Deps) withDefaults() Deps {
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
	if d.Gateway == "" {
		d.Gateway = "https://api.atlassian.com"
	}
	return d
}

// me is the user of a connection's token, kept between adapters of the
// connection: polls need its time zone, webhooks its ID.
type me struct {
	ID       string
	Name     string
	TimeZone string
}

// Factory returns the factory to register for Kind.
func Factory(d Deps) tracker.Factory {
	d = d.withDefaults()
	cache := &sync.Map{} // connection ID and token ref -> me
	return func(c domain.TrackerConnection) (tracker.Adapter, error) {
		cfg, err := parseConfig(c.Config)
		if err != nil {
			return nil, fmt.Errorf("connection %s: %w", c.Name, err)
		}
		hc, err := d.httpClient(cfg.CABundle)
		if err != nil {
			return nil, err
		}
		a := &Adapter{d: d, conn: c, cfg: cfg, cache: cache}
		a.c = &client{http: hc, d: d, auth: a.authorization}
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
	_ tracker.Adapter  = (*Adapter)(nil)
	_ tracker.Preparer = (*Adapter)(nil)
	_ tracker.Checker  = (*Adapter)(nil)
)

// Kind implements tracker.Adapter.
func (a *Adapter) Kind() string { return Kind }

// Capabilities implements tracker.Adapter. Webhooks are on when the
// connection has a webhook secret.
func (a *Adapter) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{
		Webhooks: a.cfg.WebhookSecretRef != "", Assignment: true, Labels: true, Columns: true, Transitions: true, Comments: true,
	}
}

// ---- addresses and credentials

func (a *Adapter) site() *url.URL {
	u, _ := url.Parse(a.cfg.BaseURL) // validated
	return u
}

// api is the root of the REST API: the site, or Atlassian's gateway for a
// scoped token.
func (a *Adapter) api() *url.URL {
	if a.cfg.Auth == AuthScopedToken && a.cfg.CloudID != "" {
		u, _ := url.Parse(a.d.Gateway) // a constant or a test's
		return u.JoinPath("ex", "jira", a.cfg.CloudID)
	}
	return a.site()
}

func (a *Adapter) secret(ctx context.Context, ref string) (string, error) {
	if a.d.Secrets == nil {
		return "", fmt.Errorf("the secret store is not configured: set FORECREW_SECRET_KEY")
	}
	b, err := secrets.Resolve(ctx, a.d.Secrets, a.conn.AccountID, ref)
	if err != nil {
		return "", fmt.Errorf("read %s of %s: %w", ref, a.conn.Name, err)
	}
	return string(b), nil
}

// authorization is the Authorization header of the connection's token.
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
	if a.cfg.Auth == AuthPAT {
		return "Bearer " + a.token, nil
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.cfg.Email+":"+a.token)), nil
}

// ---- what differs between Cloud and Data Center

func (a *Adapter) cloud() bool { return a.cfg.Deployment == Cloud }

// rest is the path of a resource of the REST API: version 3 on Cloud, whose
// text is ADF, and 2 on Data Center, whose text is wiki markup.
func (a *Adapter) rest(parts ...string) string {
	v := "2"
	if a.cloud() {
		v = "3"
	}
	return "rest/api/" + v + "/" + strings.Join(parts, "/")
}

// text converts a body of the deployment's format to Markdown.
func (a *Adapter) text(raw json.RawMessage) (string, error) {
	if a.cloud() {
		return adfToMarkdown(raw)
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("read wiki markup: %w", err)
	}
	if s == nil {
		return "", nil
	}
	return wikiToMarkdown(*s), nil
}

// body converts Markdown to the deployment's format.
func (a *Adapter) body(md string) any {
	if a.cloud() {
		return markdownToADF(md)
	}
	return markdownToWiki(md)
}

// jiraUser is a user as either deployment sends it.
type jiraUser struct {
	AccountID   string `json:"accountId"`   // Cloud
	AccountType string `json:"accountType"` // Cloud: atlassian, app or customer
	Key         string `json:"key"`         // Data Center: stable, unlike the name
	Name        string `json:"name"`        // Data Center: the username, which can change
	DisplayName string `json:"displayName"`
	TimeZone    string `json:"timeZone"`
}

// user is the stable identity of a user: the account ID on Cloud, the user
// key on Data Center, never the username or email, which can change.
func (a *Adapter) user(u *jiraUser) tracker.User {
	if u == nil {
		return tracker.User{}
	}
	if a.cloud() {
		return tracker.User{ID: u.AccountID, Name: u.DisplayName, Bot: u.AccountType == "app"}
	}
	return tracker.User{ID: u.Key, Name: u.DisplayName}
}

// search runs a JQL query, page after page, up to maxPages. Cloud pages with
// a token, Data Center with an offset.
func (a *Adapter) search(ctx context.Context, jql string, each func(issue) error) error {
	token, start := "", 0
	for range maxPages {
		q := url.Values{"jql": {jql}, "fields": {"updated"}, "maxResults": {strconv.Itoa(pageSize)}}
		var page struct {
			Issues        []issue `json:"issues"`
			NextPageToken string  `json:"nextPageToken"`
			Total         int     `json:"total"`
		}
		path := a.rest("search")
		if a.cloud() {
			path = a.rest("search", "jql")
			if token != "" {
				q.Set("nextPageToken", token)
			}
		} else {
			q.Set("startAt", strconv.Itoa(start))
		}
		if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: path, query: q}, &page); err != nil {
			return fmt.Errorf("search: %w", err)
		}
		for _, i := range page.Issues {
			if err := each(i); err != nil {
				return err
			}
		}
		start += len(page.Issues)
		token = page.NextPageToken
		if len(page.Issues) == 0 || (a.cloud() && token == "") || (!a.cloud() && start >= page.Total) {
			return nil
		}
	}
	return nil
}

// ---- setup and tests

type serverInfo struct {
	DeploymentType string `json:"deploymentType"`
	Version        string `json:"version"`
	VersionNumbers []int  `json:"versionNumbers"`
	BaseURL        string `json:"baseUrl"`
}

func (s serverInfo) deployment() string {
	if strings.EqualFold(s.DeploymentType, "Cloud") {
		return Cloud
	}
	return DataCenter
}

// serverInfo reads what Jira says of itself, anonymously first: it is
// public on both deployments unless the site turned that off, and while a
// plan applies the token is not readable yet.
func (a *Adapter) serverInfo(ctx context.Context) (serverInfo, error) {
	var info serverInfo
	r := call{method: http.MethodGet, base: a.site(), path: "rest/api/2/serverInfo", anon: true}
	err := a.c.do(ctx, r, &info)
	if errors.Is(err, errLogin) || errors.Is(err, errDenied) {
		r.anon = false
		err = a.c.do(ctx, r, &info)
	}
	return info, err
}

func supported(v []int) bool {
	for i, least := range MinDataCenter {
		if i >= len(v) || v[i] != least {
			return i < len(v) && v[i] > least
		}
	}
	return true
}

// Prepare implements tracker.Preparer: the deployment from serverInfo, and
// the cloud ID a scoped token needs.
func (a *Adapter) Prepare(ctx context.Context) (map[string]any, error) {
	info, err := a.serverInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the server info of %s: %w", a.cfg.BaseURL, err)
	}
	cfg := a.cfg
	cfg.Deployment, cfg.CloudID = info.deployment(), ""
	if cfg.Deployment == DataCenter && !supported(info.VersionNumbers) {
		return nil, invalid("Jira %s is older than %d.%d, the oldest Data Center version supported", info.Version, MinDataCenter[0], MinDataCenter[1])
	}
	if cfg.Auth == AuthScopedToken && cfg.Deployment == Cloud {
		var tenant struct {
			CloudID string `json:"cloudId"`
		}
		if err := a.c.do(ctx, call{method: http.MethodGet, base: a.site(), path: "_edge/tenant_info", anon: true}, &tenant); err != nil {
			return nil, fmt.Errorf("read the cloud ID of %s: %w", a.cfg.BaseURL, err)
		}
		cfg.CloudID = tenant.CloudID
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	a.cfg = cfg
	return cfg.toMap(), nil
}

// Derived implements tracker.Preparer.
func (a *Adapter) Derived() []string { return slices.Clone(derived) }

// ready prepares a connection that was stored before it was set up.
func (a *Adapter) ready(ctx context.Context) error {
	if a.cfg.Deployment != "" {
		return nil
	}
	_, err := a.Prepare(ctx)
	return err
}

// myself reads the token's user, once per connection and token.
func (a *Adapter) myself(ctx context.Context) (me, error) {
	key := a.conn.ID.String() + "\x00" + a.cfg.TokenRef
	if m, ok := a.cache.Load(key); ok {
		return m.(me), nil
	}
	var u jiraUser
	if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("myself")}, &u); err != nil {
		return me{}, fmt.Errorf("read the token's user: %w", err)
	}
	m := me{ID: a.user(&u).ID, Name: u.DisplayName, TimeZone: u.TimeZone}
	a.cache.Store(key, m)
	return m, nil
}

// Verify implements tracker.Adapter.
func (a *Adapter) Verify(ctx context.Context) error {
	return tracker.Failed(a.Check(ctx))
}

// Check implements tracker.Checker: each of what fails on-premise reported
// on its own, and what depends on a failed check skipped.
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
	info, err := a.serverInfo(ctx)
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &certErr):
		add("address", nil, "reached "+a.cfg.BaseURL)
		add("certificate", fmt.Errorf("not trusted: %v; add the CA of the server to ca_bundle", certErr.Err), "")
		return skip("login", "projects", "transitions")
	case err != nil:
		add("address", err, "")
		return skip("certificate", "login", "projects", "transitions")
	}
	if a.cfg.Deployment == "" {
		_, err = a.Prepare(ctx)
	} else if info.deployment() != a.cfg.Deployment {
		err = invalid("the server is %s now, not %s: set the connection up again", info.deployment(), a.cfg.Deployment)
	} else if info.deployment() == DataCenter && !supported(info.VersionNumbers) {
		err = invalid("Jira %s is older than %d.%d", info.Version, MinDataCenter[0], MinDataCenter[1])
	}
	if !add("address", err, fmt.Sprintf("Jira %s %s at %s", info.deployment(), info.Version, a.cfg.BaseURL)) {
		return skip("certificate", "login", "projects", "transitions")
	}
	add("certificate", nil, "trusted")
	m, err := a.myself(ctx)
	if !add("login", err, "signed in as "+m.Name+" ("+m.ID+")") {
		return skip("projects", "transitions")
	}
	if !add("projects", a.checkProjects(ctx), strings.Join(a.cfg.Projects, ", ")+": browse, comment and transition") {
		return skip("transitions")
	}
	states, err := a.States(ctx)
	if err == nil && !slices.ContainsFunc(states, func(s tracker.State) bool { return s.Closed }) {
		err = errors.New("no status of the projects is in the done category")
	}
	add("transitions", err, fmt.Sprintf("%d statuses", len(states)))
	return out
}

// checkProjects checks that the token's user sees each project and may
// comment and move its issues.
func (a *Adapter) checkProjects(ctx context.Context) error {
	need := []string{"BROWSE_PROJECTS", "ADD_COMMENTS", "TRANSITION_ISSUES"}
	var errs []error
	for _, p := range a.cfg.Projects {
		var perms struct {
			Permissions map[string]struct {
				Have bool `json:"havePermission"`
			} `json:"permissions"`
		}
		q := url.Values{"projectKey": {p}, "permissions": {strings.Join(need, ",")}}
		if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("mypermissions"), query: q}, &perms); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		var missing []string
		for _, n := range need {
			if !perms.Permissions[n].Have {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("%s: the token's user lacks %s", p, strings.Join(missing, ", ")))
		}
	}
	return errors.Join(errs...)
}

// ---- webhooks

// ParseEvent implements tracker.Adapter. Of the payload it keeps the issue's
// key, and who sent it to drop the connection's own writes.
func (a *Adapter) ParseEvent(r tracker.Request) ([]tracker.Event, error) {
	if a.cfg.WebhookSecretRef == "" {
		return nil, fmt.Errorf("%w: the connection takes no webhooks", tracker.ErrBadSignature)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	secret, err := a.secret(ctx, a.cfg.WebhookSecretRef)
	if err != nil {
		return nil, err
	}
	if err := a.checkSignature(r, []byte(secret)); err != nil {
		return nil, err
	}
	var p struct {
		Issue struct {
			Key string `json:"key"`
		} `json:"issue"`
		User *jiraUser `json:"user"`
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		return nil, fmt.Errorf("read the delivery: %w", err)
	}
	if !a.cfg.ours(p.Issue.Key) {
		return nil, nil
	}
	id := r.Header.Get("X-Atlassian-Webhook-Identifier")
	if id == "" {
		sum := sha256.Sum256(r.Body)
		id = hex.EncodeToString(sum[:16])
	}
	self := false
	if m, ok := a.cache.Load(a.conn.ID.String() + "\x00" + a.cfg.TokenRef); ok && p.User != nil {
		u := a.user(p.User)
		self = u.ID != "" && u.ID == m.(me).ID
	}
	return []tracker.Event{{ID: id, Key: p.Issue.Key, Self: self}}, nil
}

// checkSignature checks the HMAC of a webhook with a secret, or the secret
// in the URL of a sender that cannot sign.
func (a *Adapter) checkSignature(r tracker.Request, secret []byte) error {
	if a.cfg.WebhookSigning == SigningURL {
		if hmac.Equal([]byte(r.Query.Get("secret")), secret) {
			return nil
		}
		return fmt.Errorf("%w: wrong or missing secret in the URL", tracker.ErrBadSignature)
	}
	sig, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature"), "sha256=")
	got, err := hex.DecodeString(sig)
	if !ok || err != nil {
		return fmt.Errorf("%w: no sha256 X-Hub-Signature", tracker.ErrBadSignature)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(r.Body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("%w: the signature does not match", tracker.ErrBadSignature)
	}
	return nil
}

// ---- polling

// Changes implements tracker.Adapter. The cursor is the newest update seen.
// JQL takes minutes in the time zone of the token's user, so the query goes
// back by overlap from the cursor, in that zone.
func (a *Adapter) Changes(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	if cursor == "" {
		return nil, a.d.Now().Format(time.RFC3339Nano), nil
	}
	since, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("cursor %q: %w", cursor, err)
	}
	if err := a.ready(ctx); err != nil {
		return nil, "", err
	}
	m, err := a.myself(ctx)
	if err != nil {
		return nil, "", err
	}
	jql := fmt.Sprintf(`project in (%s) AND updated >= "%s" ORDER BY updated ASC`, quoted(a.cfg.Projects), jqlTime(since.Add(-overlap), m.TimeZone))
	var events []tracker.Event
	newest := since
	err = a.search(ctx, jql, func(i issue) error {
		if a.cfg.ours(i.Key) {
			events = append(events, tracker.Event{Key: i.Key})
		}
		if t := i.Fields.Updated.Time; t.After(newest) {
			newest = t
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return events, newest.UTC().Format(time.RFC3339Nano), nil
}

func quoted(keys []string) string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = `"` + k + `"`
	}
	return strings.Join(out, ", ")
}

// jqlTime formats t as JQL reads it: minutes, in the user's time zone. An
// unknown zone is UTC, and the overlap covers the rest.
func jqlTime(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	return t.In(loc).Format(jqlLayout)
}

// ---- tickets

// timestamp is a time as Jira writes it.
type timestamp struct{ time.Time }

func (t *timestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return err
	}
	v, err := time.Parse(jiraTime, s)
	if err != nil {
		if v, err = time.Parse(time.RFC3339Nano, s); err != nil {
			return fmt.Errorf("time %q: %w", s, err)
		}
	}
	t.Time = v.UTC()
	return nil
}

type status struct {
	Name     string `json:"name"`
	Category struct {
		Key string `json:"key"` // new, indeterminate or done
	} `json:"statusCategory"`
}

type comment struct {
	ID      string          `json:"id"`
	Author  *jiraUser       `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created timestamp       `json:"created"`
}

type history struct {
	Author  *jiraUser `json:"author"`
	Created timestamp `json:"created"`
	Items   []struct {
		Field      string `json:"field"`
		From       string `json:"from"`
		FromString string `json:"fromString"`
		To         string `json:"to"`
		ToString   string `json:"toString"`
	} `json:"items"`
}

type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		IssueType   struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Labels     []string `json:"labels"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		Assignee *jiraUser `json:"assignee"`
		Reporter *jiraUser `json:"reporter"`
		Status   status    `json:"status"`
		Parent   *struct {
			Key string `json:"key"`
		} `json:"parent"`
		Updated timestamp `json:"updated"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Comment struct {
			Comments []comment `json:"comments"`
			Total    int       `json:"total"`
		} `json:"comment"`
	} `json:"fields"`
	Changelog struct {
		Histories  []history `json:"histories"`
		Total      int       `json:"total"`
		MaxResults int       `json:"maxResults"`
	} `json:"changelog"`
}

const issueFields = "summary,description,issuetype,labels,components,assignee,reporter,status,parent,updated,project,comment"

func (a *Adapter) notFound(key string) error { return fmt.Errorf("%s: %w", key, tracker.ErrNotFound) }

// Get implements tracker.Adapter. Who handed the issue over comes from its
// changelog, so polling knows what a webhook would. An issue of another
// project is not found: the connection reads only its own.
func (a *Adapter) Get(ctx context.Context, key string) (tracker.Ticket, error) {
	if !a.cfg.ours(key) {
		return tracker.Ticket{}, a.notFound(key)
	}
	if err := a.ready(ctx); err != nil {
		return tracker.Ticket{}, err
	}
	var i issue
	q := url.Values{"fields": {issueFields}, "expand": {"changelog"}}
	if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("issue", key), query: q}, &i); err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			return tracker.Ticket{}, a.notFound(key)
		}
		return tracker.Ticket{}, fmt.Errorf("read %s: %w", key, err)
	}
	if i.Key != key || !a.cfg.ours(i.Key) { // moved to another project
		return tracker.Ticket{}, a.notFound(key)
	}
	f := i.Fields
	body, err := a.text(f.Description)
	if err != nil {
		return tracker.Ticket{}, fmt.Errorf("%s: %w", key, err)
	}
	t := tracker.Ticket{
		Ref: tracker.Ref{Connection: a.conn.ID, Key: key}, Project: f.Project.Key, Title: f.Summary, Body: body, Type: f.IssueType.Name,
		Labels: f.Labels, Assignee: a.user(f.Assignee), Reporter: a.user(f.Reporter), State: f.Status.Name, Closed: f.Status.Category.Key == "done",
		URL: a.site().JoinPath("browse", key).String(), Updated: f.Updated.Time,
	}
	for _, c := range f.Components {
		t.Components = append(t.Components, c.Name)
	}
	if f.Parent != nil {
		t.Parent = f.Parent.Key
	}
	comments, err := a.comments(ctx, key, f.Comment.Comments, f.Comment.Total)
	if err != nil {
		return tracker.Ticket{}, err
	}
	for _, c := range comments {
		text, err := a.text(c.Body)
		if err != nil {
			return tracker.Ticket{}, fmt.Errorf("comment %s of %s: %w", c.ID, key, err)
		}
		t.Comments = append(t.Comments, tracker.Comment{ID: c.ID, Author: a.user(c.Author), Body: text, At: c.Created.Time})
	}
	histories, err := a.histories(ctx, key, i)
	if err != nil {
		return tracker.Ticket{}, err
	}
	t.Handovers = a.handovers(histories)
	return t, nil
}

// comments returns the latest comments: the issue holds the first page, and
// a long thread needs its last.
func (a *Adapter) comments(ctx context.Context, key string, first []comment, total int) ([]comment, error) {
	if total <= len(first) {
		return first, nil
	}
	var page struct {
		Comments []comment `json:"comments"`
	}
	q := url.Values{"startAt": {strconv.Itoa(max(0, total-pageSize))}, "maxResults": {strconv.Itoa(pageSize)}}
	if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("issue", key, "comment"), query: q}, &page); err != nil {
		return nil, fmt.Errorf("read the comments of %s: %w", key, err)
	}
	return page.Comments, nil
}

// histories returns the changelog, oldest first. Data Center gives all of
// it with the issue; Cloud gives a page, and the newest are on the last.
func (a *Adapter) histories(ctx context.Context, key string, i issue) ([]history, error) {
	h := i.Changelog.Histories
	if !a.cloud() || i.Changelog.Total <= len(h) {
		slices.SortStableFunc(h, func(x, y history) int { return x.Created.Compare(y.Created.Time) })
		return h, nil
	}
	var page struct {
		Values []history `json:"values"`
	}
	q := url.Values{"startAt": {strconv.Itoa(max(0, i.Changelog.Total-pageSize))}, "maxResults": {strconv.Itoa(pageSize)}}
	if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("issue", key, "changelog"), query: q}, &page); err != nil {
		return nil, fmt.Errorf("read the changelog of %s: %w", key, err)
	}
	slices.SortStableFunc(page.Values, func(x, y history) int { return x.Created.Compare(y.Created.Time) })
	return page.Values, nil
}

// handovers reads who assigned, labeled or moved the issue. The assignee is
// the stable ID of the deployment, which the changelog carries in to.
func (a *Adapter) handovers(histories []history) []tracker.Handover {
	var out []tracker.Handover
	for _, h := range histories {
		by := a.user(h.Author)
		for _, it := range h.Items {
			switch strings.ToLower(it.Field) {
			case "assignee":
				if it.To != "" {
					out = append(out, tracker.Handover{Trigger: tracker.TriggerAssignee, Value: it.To, By: by, At: h.Created.Time})
				}
			case "labels":
				before := strings.Fields(it.FromString)
				for l := range strings.FieldsSeq(it.ToString) {
					if !slices.Contains(before, l) {
						out = append(out, tracker.Handover{Trigger: tracker.TriggerLabel, Value: l, By: by, At: h.Created.Time})
					}
				}
			case "status":
				out = append(out, tracker.Handover{Trigger: tracker.TriggerState, Value: it.ToString, By: by, At: h.Created.Time})
			}
		}
	}
	return out
}

// Comment implements tracker.Adapter, in the format the deployment takes.
func (a *Adapter) Comment(ctx context.Context, key, body string) (string, error) {
	if !a.cfg.ours(key) {
		return "", a.notFound(key)
	}
	if err := a.ready(ctx); err != nil {
		return "", err
	}
	var c struct {
		ID string `json:"id"`
	}
	in := map[string]any{"body": a.body(body)}
	if err := a.c.do(ctx, call{method: http.MethodPost, base: a.api(), path: a.rest("issue", key, "comment"), in: in}, &c); err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			return "", a.notFound(key)
		}
		return "", fmt.Errorf("comment on %s: %w", key, err)
	}
	return c.ID, nil
}

type transition struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	To     status `json:"to"`
	Fields map[string]struct {
		Required   bool   `json:"required"`
		HasDefault bool   `json:"hasDefaultValue"`
		Name       string `json:"name"`
	} `json:"fields"`
}

// Transition implements tracker.Adapter. Workflows are the company's own,
// so it takes a transition the issue offers that reaches the status, and
// forces nothing: none, or one that needs a field Forecrew does not have, is
// tracker.ErrBlocked.
func (a *Adapter) Transition(ctx context.Context, key, state string) error {
	if !a.cfg.ours(key) {
		return a.notFound(key)
	}
	if err := a.ready(ctx); err != nil {
		return err
	}
	path := a.rest("issue", key, "transitions")
	var offered struct {
		Transitions []transition `json:"transitions"`
	}
	q := url.Values{"expand": {"transitions.fields"}}
	if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: path, query: q}, &offered); err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			return a.notFound(key)
		}
		return fmt.Errorf("read the transitions of %s: %w", key, err)
	}
	i := slices.IndexFunc(offered.Transitions, func(t transition) bool { return strings.EqualFold(t.To.Name, state) })
	if i < 0 {
		names := make([]string, 0, len(offered.Transitions))
		for _, t := range offered.Transitions {
			names = append(names, t.To.Name)
		}
		return fmt.Errorf("no transition of %s reaches %s; it offers %s: %w", key, state, orNone(names), tracker.ErrBlocked)
	}
	t := offered.Transitions[i]
	var needs []string
	for id, f := range t.Fields {
		if f.Required && !f.HasDefault {
			needs = append(needs, cmp(f.Name, id))
		}
	}
	if len(needs) > 0 {
		slices.Sort(needs)
		return fmt.Errorf("the transition %q of %s needs %s, which Forecrew does not fill: %w", t.Name, key, strings.Join(needs, ", "), tracker.ErrBlocked)
	}
	in := map[string]any{"transition": map[string]string{"id": t.ID}}
	err := a.c.do(ctx, call{method: http.MethodPost, base: a.api(), path: path, in: in}, nil)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusBadRequest { // a validator or a screen said no
		return fmt.Errorf("the transition %q of %s was refused: %s: %w", t.Name, key, api.Message, tracker.ErrBlocked)
	}
	if err != nil {
		return fmt.Errorf("move %s to %s: %w", key, state, err)
	}
	return nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// States implements tracker.Adapter: the statuses of the connection's
// projects, closed when in the done category.
func (a *Adapter) States(ctx context.Context) ([]tracker.State, error) {
	if err := a.ready(ctx); err != nil {
		return nil, err
	}
	var out []tracker.State
	for _, p := range a.cfg.Projects {
		var types []struct {
			Statuses []status `json:"statuses"`
		}
		if err := a.c.do(ctx, call{method: http.MethodGet, base: a.api(), path: a.rest("project", p, "statuses")}, &types); err != nil {
			return nil, fmt.Errorf("read the statuses of %s: %w", p, err)
		}
		for _, t := range types {
			for _, s := range t.Statuses {
				if !slices.ContainsFunc(out, func(x tracker.State) bool { return strings.EqualFold(x.Name, s.Name) }) {
					out = append(out, tracker.State{Name: s.Name, Closed: s.Category.Key == "done"})
				}
			}
		}
	}
	return out, nil
}

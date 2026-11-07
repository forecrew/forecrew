package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
)

// fakeLinear answers the GraphQL calls the adapter makes, in the shapes
// Linear documents (connections of nodes with pageInfo, errors with
// extensions.code, 429 with Retry-After). It is no recording of the real
// service: docs/trackers/linear.md lists what is unverified.
type fakeLinear struct {
	t   *testing.T
	srv *httptest.Server
	key string // the Authorization header it takes

	mu       sync.Mutex
	now      time.Time
	teams    map[string][]fakeState // team key -> states
	issues   map[string]*fakeIssue  // identifier
	next     map[string]int
	limited  []string // Retry-After values of the next answers
	mutated  int      // mutations that reached it
	calls    int
	pageSize int // forces paging when > 0
	redirect string
}

type fakeState struct{ ID, Name, Type string }

type fakeIssue struct {
	ID, Identifier, Team, Title, Description, State string
	Labels                                          []string
	Assignee                                        string
	Updated                                         time.Time
	Comments                                        []fakeComment
	History                                         []fakeHistory
}

type fakeComment struct {
	ID, Body, User string
	At             time.Time
}

type fakeHistory struct {
	At       time.Time
	Actor    string
	Assignee string
	Label    string
	State    string
}

func states(prefix string) []fakeState {
	return []fakeState{
		{prefix + "-backlog", "Backlog", "backlog"}, {prefix + "-todo", "Todo", "unstarted"}, {prefix + "-doing", "In Progress", "started"},
		{prefix + "-review", "In Review", "started"}, {prefix + "-done", "Done", "completed"}, {prefix + "-canceled", "Canceled", "canceled"},
	}
}

func newFakeLinear(t *testing.T) *fakeLinear {
	f := &fakeLinear{
		t: t, key: "lin_api_secret", now: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
		teams: map[string][]fakeState{"ENG": states("eng"), "OPS": states("ops")}, issues: map[string]*fakeIssue{}, next: map[string]int{},
	}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinear) answer(w http.ResponseWriter, status int, data any, errs ...map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out := map[string]any{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	_ = json.NewEncoder(w).Encode(out)
}

func fail(message, code string) map[string]any {
	return map[string]any{"message": message, "extensions": map[string]any{"code": code}}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

var (
	gteRE   = regexp.MustCompile(`gte: "([^"]+)"`)
	teamsRE = regexp.MustCompile(`key: \{in: \[([^\]]*)\]\}`)
)

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.redirect != "" {
		http.Redirect(w, r, f.redirect, http.StatusFound)
		return
	}
	if len(f.limited) > 0 {
		w.Header().Set("Retry-After", f.limited[0])
		f.limited = f.limited[1:]
		f.answer(w, http.StatusTooManyRequests, nil, fail("Rate limit exceeded", "RATELIMITED"))
		return
	}
	if r.Header.Get("Authorization") != f.key {
		f.answer(w, http.StatusUnauthorized, nil, fail("Authentication required", "AUTHENTICATION_ERROR"))
		return
	}
	var in struct {
		Query     string
		Variables map[string]any
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.t.Errorf("bad request: %v", err)
	}
	q, v := in.Query, in.Variables
	str := func(k string) string { s, _ := v[k].(string); return s }
	switch {
	case strings.Contains(q, "commentCreate"):
		f.comment(w, v["input"].(map[string]any))
	case strings.Contains(q, "issueUpdate"):
		f.update(w, str("id"), v["input"].(map[string]any))
	case strings.Contains(q, "viewer"):
		f.answer(w, 200, map[string]any{"viewer": map[string]any{"id": "u-forecrew", "name": "Forecrew"}})
	case strings.Contains(q, "teams(first"):
		var nodes []map[string]any
		for _, k := range slices.Sorted(mapKeys(f.teams)) {
			nodes = append(nodes, map[string]any{"key": k})
		}
		f.answer(w, 200, map[string]any{"teams": map[string]any{"nodes": nodes}})
	case strings.Contains(q, "workflowStates"):
		f.workflowStates(w, q)
	case strings.Contains(q, "issues(first"):
		f.changed(w, q, str("after"), false)
	case strings.Contains(q, "comments(first"):
		f.changed(w, q, str("after"), true)
	case strings.Contains(q, "issue(id"):
		f.issue(w, q, str("id"))
	default:
		f.t.Errorf("unexpected query %s", q)
	}
}

func mapKeys[M ~map[K]V, K comparable, V any](m M) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func (f *fakeLinear) find(id string) *fakeIssue {
	if i, ok := f.issues[id]; ok {
		return i
	}
	for _, i := range f.issues {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func (f *fakeLinear) notFound(w http.ResponseWriter) {
	f.answer(w, 200, map[string]any{"issue": nil}, fail("Entity not found: Issue - Could not find referenced Issue.", "INVALID_INPUT"))
}

func (f *fakeLinear) issue(w http.ResponseWriter, q, id string) {
	i := f.find(id)
	if i == nil {
		f.notFound(w)
		return
	}
	switch {
	case strings.Contains(q, "history(last"):
		f.answer(w, 200, map[string]any{"issue": f.full(i)})
	case strings.Contains(q, "states(first"):
		var nodes []map[string]string
		for _, s := range f.teams[i.Team] {
			nodes = append(nodes, map[string]string{"id": s.ID, "name": s.Name})
		}
		f.answer(w, 200, map[string]any{"issue": map[string]any{
			"id": i.ID, "state": map[string]string{"name": i.State},
			"team": map[string]any{"key": i.Team, "states": map[string]any{"nodes": nodes}},
		}})
	case strings.Contains(q, "team { key }"):
		f.answer(w, 200, map[string]any{"issue": map[string]any{"id": i.ID, "team": map[string]string{"key": i.Team}}})
	default:
		f.answer(w, 200, map[string]any{"issue": map[string]any{"identifier": i.Identifier}})
	}
}

func (f *fakeLinear) stateType(team, name string) string {
	for _, s := range f.teams[team] {
		if s.Name == name {
			return s.Type
		}
	}
	return ""
}

func userOf(id string) any {
	if id == "" {
		return nil
	}
	return map[string]string{"id": id, "name": "Name of " + id}
}

func (f *fakeLinear) full(i *fakeIssue) map[string]any {
	var labels, comments, history []map[string]any
	for _, l := range i.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	// Linear's connections come newest first: the adapter sorts.
	for _, c := range slices.Backward(i.Comments) {

		comments = append(comments, map[string]any{"id": c.ID, "body": c.Body, "createdAt": ts(c.At), "user": userOf(c.User)})
	}
	for _, h := range slices.Backward(i.History) {

		node := map[string]any{"createdAt": ts(h.At), "actor": userOf(h.Actor), "toAssigneeId": nilIfEmpty(h.Assignee), "addedLabels": []map[string]any{}, "toState": nil}
		if h.Label != "" {
			node["addedLabels"] = []map[string]any{{"name": h.Label}}
		}
		if h.State != "" {
			node["toState"] = map[string]any{"name": h.State}
		}
		history = append(history, node)
	}
	return map[string]any{
		"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description,
		"url": "https://linear.app/acme/issue/" + i.Identifier, "updatedAt": ts(i.Updated),
		"team": map[string]string{"key": i.Team}, "state": map[string]string{"name": i.State, "type": f.stateType(i.Team, i.State)},
		"assignee": userOf(i.Assignee), "creator": userOf("u-marta"), "parent": nil,
		"labels": map[string]any{"nodes": labels}, "comments": map[string]any{"nodes": comments}, "history": map[string]any{"nodes": history},
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *fakeLinear) wanted(q string) []string {
	m := teamsRE.FindStringSubmatch(q)
	if m == nil {
		return nil
	}
	var out []string
	for p := range strings.SplitSeq(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(p), `"`))
	}
	return out
}

func (f *fakeLinear) workflowStates(w http.ResponseWriter, q string) {
	var nodes []map[string]string
	for _, team := range f.wanted(q) {
		for _, s := range f.teams[team] {
			nodes = append(nodes, map[string]string{"name": s.Name, "type": s.Type})
		}
	}
	f.answer(w, 200, map[string]any{"workflowStates": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}})
}

// changed serves the issues, or the comments, updated since the filter's
// time, newest first, a page at a time.
func (f *fakeLinear) changed(w http.ResponseWriter, q, after string, comments bool) {
	since, err := time.Parse(time.RFC3339Nano, gteRE.FindStringSubmatch(q)[1])
	if err != nil {
		f.t.Errorf("filter time: %v", err)
	}
	teams := f.wanted(q)
	type row struct {
		node map[string]any
		at   time.Time
	}
	var rows []row
	for _, key := range slices.Sorted(mapKeys(f.issues)) {
		i := f.issues[key]
		if !comments && slices.Contains(teams, i.Team) && !i.Updated.Before(since) {
			rows = append(rows, row{map[string]any{"identifier": i.Identifier, "updatedAt": ts(i.Updated)}, i.Updated})
		}
		if comments {
			for _, c := range i.Comments {
				if !c.At.Before(since) {
					rows = append(rows, row{map[string]any{"updatedAt": ts(c.At), "issue": map[string]any{"identifier": i.Identifier}}, c.At})
				}
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.at.Compare(a.at) })
	start := 0
	if after != "" {
		start, _ = strconv.Atoi(after)
	}
	size := cmpOr(f.pageSize, pageSize)
	end := min(len(rows), start+size)
	var nodes []map[string]any
	for _, r := range rows[start:end] {
		nodes = append(nodes, r.node)
	}
	field := "issues"
	if comments {
		field = "comments"
	}
	f.answer(w, 200, map[string]any{field: map[string]any{
		"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < len(rows), "endCursor": strconv.Itoa(end)},
	}})
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func (f *fakeLinear) comment(w http.ResponseWriter, in map[string]any) {
	i := f.find(in["issueId"].(string))
	if i == nil {
		f.answer(w, 200, nil, fail("Entity not found: Issue", "INVALID_INPUT"))
		return
	}
	f.mutated++
	f.now = f.now.Add(time.Second)
	c := fakeComment{ID: fmt.Sprintf("c-%d", f.mutated), Body: in["body"].(string), User: "u-forecrew", At: f.now}
	i.Comments = append(i.Comments, c)
	f.answer(w, 200, map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": c.ID}}})
}

func (f *fakeLinear) update(w http.ResponseWriter, id string, in map[string]any) {
	i := f.find(id)
	if i == nil {
		f.answer(w, 200, nil, fail("Entity not found: Issue", "INVALID_INPUT"))
		return
	}
	f.mutated++
	for _, s := range f.teams[i.Team] {
		if s.ID == in["stateId"] {
			f.now = f.now.Add(time.Second)
			i.State, i.Updated = s.Name, f.now
			i.History = append(i.History, fakeHistory{At: f.now, Actor: "u-forecrew", State: s.Name})
		}
	}
	f.answer(w, 200, map[string]any{"issueUpdate": map[string]any{"success": true}})
}

// create makes an open issue.
func (f *fakeLinear) create(team, title, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next[team]++
	key := fmt.Sprintf("%s-%d", team, f.next[team])
	f.now = f.now.Add(time.Second)
	f.issues[key] = &fakeIssue{ID: "uuid-" + key, Identifier: key, Team: team, Title: title, Description: body, State: "Todo", Updated: f.now}
	return key
}

func (f *fakeLinear) touch(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Second)
	f.issues[key].Updated = f.now
}

func (f *fakeLinear) edit(key string, fn func(*fakeIssue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Second)
	i := f.issues[key]
	fn(i)
	i.Updated = f.now
}

// signed is a delivery as Linear sends it, dated by the fake's clock.
func (f *fakeLinear) signed(secret string, payload map[string]any, delivery string) tracker.Request {
	f.mu.Lock()
	payload["webhookTimestamp"] = f.now.UnixMilli()
	f.mu.Unlock()
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	h := http.Header{}
	h.Set("Linear-Signature", hex.EncodeToString(mac.Sum(nil)))
	h.Set("Linear-Delivery", delivery)
	return tracker.Request{Header: h, Body: body}
}

func (f *fakeLinear) issueEvent(secret, key string) tracker.Request {
	f.mu.Lock()
	i := f.issues[key]
	f.mu.Unlock()
	return f.signed(secret, map[string]any{"action": "update", "type": "Issue", "data": map[string]any{"id": i.ID, "identifier": key, "team": map[string]string{"key": i.Team}}}, "d-"+key)
}

type world struct {
	f     *fakeLinear
	vault *secrets.Encrypted
	conn  domain.TrackerConnection
	deps  Deps
	slept []time.Duration
}

func newWorld(t *testing.T, edit ...func(cfg map[string]any)) *world {
	t.Helper()
	f := newFakeLinear(t)
	st := testkit.NewStore(nil)
	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	account := uuid.NewV7()
	by := domain.SystemActor(account, domain.ForecrewdName)
	for name, value := range map[string]string{"linear-key": f.key + "\n", "linear-oauth": "oauth-token", "linear-hook": "hook-secret"} {
		if err := vault.Put(t.Context(), by, account, name, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{"teams": []string{"ENG"}, "token_ref": "store:linear-key", "webhook_secret_ref": "store:linear-hook"}
	for _, e := range edit {
		e(cfg)
	}
	w := &world{f: f, vault: vault}
	w.conn = domain.TrackerConnection{ID: uuid.NewV7(), AccountID: account, Name: "linear", Kind: Kind, Config: cfg, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	w.deps = Deps{
		Secrets: vault, Endpoint: f.srv.URL, PrivateAddresses: true, RootCAs: []*x509.Certificate{f.srv.Certificate()}, Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		Sleep: func(_ context.Context, d time.Duration) error { w.slept = append(w.slept, d); return nil },
	}
	return w
}

func (w *world) adapter(t *testing.T) *Adapter {
	t.Helper()
	ad, err := Factory(w.deps)(w.conn)
	if err != nil {
		t.Fatal(err)
	}
	return ad.(*Adapter)
}

func macOf(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

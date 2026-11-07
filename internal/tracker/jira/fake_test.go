package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// fakeJira is a Jira of one deployment over httptest. Its fixed answers
// (serverInfo, myself, statuses and one issue) are recorded answers of each
// deployment in testdata; the issues tests create are built like them.
type fakeJira struct {
	t      *testing.T
	srv    *httptest.Server
	cloud  bool
	prefix string // the context path
	token  string // the Authorization header it takes

	mu       sync.Mutex
	now      time.Time
	tz       string
	issues   map[string]*fakeIssue
	next     int
	unreach  map[string]bool   // statuses no transition reaches
	needs    map[string]string // status -> a required field of its transition
	refuse   bool              // a POST of a transition answers 400
	limited  []string          // Retry-After values of the next answers, one each
	seen     []string          // method and path of each request
	jql      []string
	posted   []any // bodies of comments
	redirect string
}

type fakeIssue struct {
	Key, Summary, Status string
	Description          any
	Labels               []string
	Updated              time.Time
	Comments             []map[string]any
	Raw                  json.RawMessage // a recorded issue, served as it is
}

func fixture(t *testing.T, dir, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fakeJira) dir() string {
	if f.cloud {
		return "cloud"
	}
	return "dc"
}

func (f *fakeJira) version() string {
	if f.cloud {
		return "3"
	}
	return "2"
}

func newFakeJira(t *testing.T, cloud bool, prefix string) *fakeJira {
	f := &fakeJira{
		t: t, cloud: cloud, prefix: prefix, now: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC), tz: "Europe/Rome",
		issues: map[string]*fakeIssue{}, unreach: map[string]bool{}, needs: map[string]string{},
	}
	f.token = "Bearer dc-token"
	if cloud {
		f.token = "Basic " + basic("forecrew@acme.example", "cloud-token")
	}
	var rec fakeIssue
	rec.Raw = fixture(t, f.dir(), "issue.json")
	var head struct{ Key string }
	must(t, json.Unmarshal(rec.Raw, &head))
	rec.Key = head.Key
	f.issues[rec.Key] = &rec
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted CA's handshakes fail on purpose
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// caBundle is the PEM of the server's certificate.
func (f *fakeJira) caBundle() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

func (f *fakeJira) baseURL() string { return f.srv.URL + f.prefix }

func (f *fakeJira) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if raw, ok := v.(json.RawMessage); ok {
		_, _ = w.Write(raw)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

var (
	issuePath = regexp.MustCompile(`^issue/([A-Z0-9_]+-\d+)(/comment|/transitions|/changelog)?$`)
	jqlSince  = regexp.MustCompile(`updated >= "([^"]+)"`)
)

func (f *fakeJira) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	if len(f.limited) > 0 {
		w.Header().Set("Retry-After", f.limited[0])
		f.limited = f.limited[1:]
		f.write(w, http.StatusTooManyRequests, map[string]any{"errorMessages": []string{"Rate limit exceeded."}})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, f.prefix+"/")
	if !ok {
		f.write(w, http.StatusNotFound, nil)
		return
	}
	switch path {
	case "rest/api/2/serverInfo":
		if f.redirect != "" {
			http.Redirect(w, r, f.redirect, http.StatusFound)
			return
		}
		f.write(w, http.StatusOK, fixture(f.t, f.dir(), "serverInfo.json"))
		return
	case "_edge/tenant_info":
		f.write(w, http.StatusOK, map[string]string{"cloudId": "a436116f-02ce-4520-8fbb-7301462a1674"})
		return
	}
	path = strings.TrimPrefix(path, "ex/jira/a436116f-02ce-4520-8fbb-7301462a1674/")
	if r.Header.Get("Authorization") != f.token {
		f.write(w, http.StatusUnauthorized, map[string]any{"errorMessages": []string{"You are not authenticated."}})
		return
	}
	rest, ok := strings.CutPrefix(path, "rest/api/"+f.version()+"/")
	if !ok {
		f.write(w, http.StatusNotFound, map[string]any{"errorMessages": []string{"wrong API version"}})
		return
	}
	switch {
	case rest == "myself":
		var m map[string]any
		must(f.t, json.Unmarshal(fixture(f.t, f.dir(), "myself.json"), &m))
		m["timeZone"] = f.tz
		f.write(w, http.StatusOK, m)
	case rest == "mypermissions":
		perms := map[string]any{}
		for p := range strings.SplitSeq(r.URL.Query().Get("permissions"), ",") {
			perms[p] = map[string]any{"havePermission": true}
		}
		f.write(w, http.StatusOK, map[string]any{"permissions": perms})
	case strings.HasPrefix(rest, "project/") && strings.HasSuffix(rest, "/statuses"):
		f.write(w, http.StatusOK, fixture(f.t, f.dir(), "statuses.json"))
	case rest == "search/jql" && f.cloud, rest == "search" && !f.cloud:
		f.search(w, r)
	case issuePath.MatchString(rest):
		m := issuePath.FindStringSubmatch(rest)
		f.issue(w, r, m[1], m[2])
	default:
		f.write(w, http.StatusNotFound, map[string]any{"errorMessages": []string{"no such resource"}})
	}
}

func (f *fakeJira) search(w http.ResponseWriter, r *http.Request) {
	jql := r.URL.Query().Get("jql")
	f.jql = append(f.jql, jql)
	m := jqlSince.FindStringSubmatch(jql)
	loc, err := time.LoadLocation(f.tz)
	must(f.t, err)
	since, err := time.ParseInLocation(jqlLayout, m[1], loc)
	must(f.t, err)
	var keys []string
	for k, i := range f.issues {
		if i.Raw == nil && !i.Updated.Before(since) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return f.issues[a].Updated.Compare(f.issues[b].Updated) })
	issues := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		issues = append(issues, map[string]any{"key": k, "fields": map[string]any{"updated": f.issues[k].Updated.Format(jiraTime)}})
	}
	if f.cloud {
		f.write(w, http.StatusOK, map[string]any{"issues": issues, "isLast": true})
		return
	}
	f.write(w, http.StatusOK, map[string]any{"issues": issues, "startAt": 0, "maxResults": 100, "total": len(issues)})
}

func (f *fakeJira) category(status string) string {
	switch status {
	case "Done", "Closed":
		return "done"
	case "To Do", "Open":
		return "new"
	}
	return "indeterminate"
}

func (f *fakeJira) statuses() []string {
	if f.cloud {
		return []string{"To Do", "In Progress", "In Review", "Done"}
	}
	return []string{"Open", "In Progress", "Code Review", "Closed"}
}

func (f *fakeJira) issue(w http.ResponseWriter, r *http.Request, key, sub string) {
	i, ok := f.issues[key]
	switch {
	case !ok:
		f.write(w, http.StatusNotFound, map[string]any{"errorMessages": []string{"Issue Does Not Exist"}})
	case sub == "" && i.Raw != nil:
		f.write(w, http.StatusOK, i.Raw)
	case sub == "":
		f.write(w, http.StatusOK, map[string]any{"key": key, "fields": map[string]any{
			"summary": i.Summary, "description": i.Description, "issuetype": map[string]any{"name": "Task"}, "labels": i.Labels,
			"project": map[string]any{"key": strings.Split(key, "-")[0]}, "updated": i.Updated.Format(jiraTime),
			"status":  map[string]any{"name": i.Status, "statusCategory": map[string]any{"key": f.category(i.Status)}},
			"comment": map[string]any{"comments": i.Comments, "total": len(i.Comments)},
		}, "changelog": map[string]any{"histories": []any{}, "total": 0}})
	case sub == "/comment" && r.Method == http.MethodPost:
		f.comment(w, r, i)
	case sub == "/transitions" && r.Method == http.MethodGet:
		f.transitions(w)
	case sub == "/transitions" && r.Method == http.MethodPost:
		f.move(w, r, i)
	default:
		f.write(w, http.StatusMethodNotAllowed, nil)
	}
}

func (f *fakeJira) comment(w http.ResponseWriter, r *http.Request, i *fakeIssue) {
	var in struct{ Body any }
	must(f.t, json.NewDecoder(r.Body).Decode(&in))
	f.posted = append(f.posted, in.Body)
	f.next++
	id := fmt.Sprint(50000 + f.next)
	author := map[string]any{"key": "JIRAUSER10900", "name": "svc-forecrew", "displayName": "Forecrew"}
	if f.cloud {
		author = map[string]any{"accountId": "712020:9a1e3c55-4a1b-4f1e-9d5a-1c2b3d4e5f60", "displayName": "Forecrew"}
	}
	i.Comments = append(i.Comments, map[string]any{"id": id, "author": author, "body": in.Body, "created": f.now.Format(jiraTime)})
	f.write(w, http.StatusCreated, map[string]any{"id": id})
}

func (f *fakeJira) transitions(w http.ResponseWriter) {
	var ts []map[string]any
	for n, s := range f.statuses() {
		if f.unreach[s] {
			continue
		}
		fields := map[string]any{}
		if field, ok := f.needs[s]; ok {
			fields["resolution"] = map[string]any{"required": true, "hasDefaultValue": false, "name": field}
			fields["comment"] = map[string]any{"required": false, "name": "Comment"}
		}
		ts = append(ts, map[string]any{"id": fmt.Sprint(11 + n*10), "name": "Move to " + s, "to": map[string]any{"name": s}, "fields": fields})
	}
	f.write(w, http.StatusOK, map[string]any{"transitions": ts})
}

func (f *fakeJira) move(w http.ResponseWriter, r *http.Request, i *fakeIssue) {
	if f.refuse {
		f.write(w, http.StatusBadRequest, map[string]any{"errorMessages": []string{}, "errors": map[string]string{"customfield_10200": "Story points is required."}})
		return
	}
	var in struct {
		Transition struct{ ID string }
	}
	must(f.t, json.NewDecoder(r.Body).Decode(&in))
	for n, s := range f.statuses() {
		if fmt.Sprint(11+n*10) == in.Transition.ID {
			i.Status, i.Updated = s, f.now
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// create adds an issue as a person would, with Markdown made into the
// deployment's format the way Jira's editor would store it.
func (f *fakeJira) create(project, title, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	key := fmt.Sprintf("%s-%d", project, f.next)
	var desc any = markdownToWiki(body)
	if f.cloud {
		desc = json.RawMessage(markdownToADF(body))
	}
	if body == "" {
		desc = nil
	}
	f.issues[key] = &fakeIssue{Key: key, Summary: title, Description: desc, Status: f.statuses()[0], Updated: f.now}
	return key
}

func (f *fakeJira) touch(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Minute)
	f.issues[key].Summary += "!"
	f.issues[key].Updated = f.now
}

// world is a connection to a fake Jira.
type world struct {
	f       *fakeJira
	vault   secrets.Store
	account uuid.UUID
	conn    domain.TrackerConnection
	deps    Deps
	slept   []time.Duration
}

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func newWorld(t *testing.T, cloud bool, edit ...func(cfg map[string]any)) *world {
	t.Helper()
	prefix := ""
	if !cloud {
		prefix = "/jira"
	}
	f := newFakeJira(t, cloud, prefix)
	st := testkit.NewStore(nil)
	vault, err := secrets.New(st.Secrets(), make([]byte, secrets.KeySize))
	must(t, err)
	w := &world{f: f, vault: vault, account: uuid.NewV7()}
	by := domain.SystemActor(w.account, domain.ForecrewdName)
	must(t, vault.Put(t.Context(), by, w.account, "jira-token", []byte("dc-token")))
	must(t, vault.Put(t.Context(), by, w.account, "cloud-token", []byte("cloud-token\n")))
	must(t, vault.Put(t.Context(), by, w.account, "jira-hook", []byte("hook-secret")))
	cfg := map[string]any{"base_url": f.baseURL(), "projects": []string{"OPS"}, "auth": AuthPAT, "token_ref": "store:jira-token", "ca_bundle": f.caBundle()}
	if cloud {
		cfg = map[string]any{"base_url": f.baseURL(), "projects": []string{"ENG"}, "auth": AuthAPIToken, "email": "forecrew@acme.example", "token_ref": "store:cloud-token", "ca_bundle": f.caBundle()}
	}
	for _, e := range edit {
		e(cfg)
	}
	w.conn = domain.TrackerConnection{ID: uuid.NewV7(), AccountID: w.account, Name: "jira", Kind: Kind, Config: cfg, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	w.deps = Deps{
		Secrets: vault, PrivateAddresses: true, Now: func() time.Time { return f.now }, Gateway: f.srv.URL,
		Sleep: func(_ context.Context, d time.Duration) error { w.slept = append(w.slept, d); return nil },
	}
	return w
}

func (w *world) adapter(t *testing.T) *Adapter {
	t.Helper()
	ad, err := Factory(w.deps)(w.conn)
	must(t, err)
	return ad.(*Adapter)
}

// prepared returns an adapter of a connection set up as the tools set it up.
func (w *world) prepared(t *testing.T) *Adapter {
	t.Helper()
	cfg, err := w.adapter(t).Prepare(t.Context())
	must(t, err)
	w.conn.Config = cfg
	return w.adapter(t)
}

var _ tracker.Adapter = (*Adapter)(nil)

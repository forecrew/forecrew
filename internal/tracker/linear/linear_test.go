package linear

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

func TestConformance(t *testing.T) {
	w := newWorld(t)
	trackertest.Run(t, trackertest.Harness{
		Adapter:   w.adapter(t),
		NewTicket: func(_ *testing.T, title, body string) string { return w.f.create("ENG", title, body) },
		Touch:     func(_ *testing.T, key string) { w.f.touch(key) },
		Signed:    func(_ *testing.T, key string) tracker.Request { return w.f.issueEvent("hook-secret", key) },
		Forged:    func(_ *testing.T, key string) tracker.Request { return w.f.issueEvent("wrong", key) },
	})
}

func TestConformanceWithoutWebhooks(t *testing.T) {
	w := newWorld(t, func(cfg map[string]any) { delete(cfg, "webhook_secret_ref") })
	ad := w.adapter(t)
	if ad.Capabilities().Webhooks {
		t.Fatal("webhooks are on without a secret")
	}
	trackertest.Run(t, trackertest.Harness{
		Adapter:   ad,
		NewTicket: func(_ *testing.T, title, body string) string { return w.f.create("ENG", title, body) },
		Touch:     func(_ *testing.T, key string) { w.f.touch(key) },
	})
	if _, err := ad.ParseEvent(w.f.issueEvent("hook-secret", w.f.create("ENG", "x", ""))); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a delivery to a connection without a secret: %v", err)
	}
}

func TestGetReadsTheTicketAndWhoHandedItOver(t *testing.T) {
	w := newWorld(t)
	key := w.f.create("ENG", "Fix login", "Steps\n\n- one")
	t0 := w.f.now
	w.f.edit(key, func(i *fakeIssue) {
		i.Labels = []string{"Bug", "backend"}
		i.Assignee = "u-forecrew"
		i.History = []fakeHistory{
			{At: t0.Add(2 * time.Minute), Actor: "u-marta", Label: "forecrew"},
			{At: t0.Add(time.Minute), Actor: "u-marta", Assignee: "u-forecrew"},
			{At: t0.Add(3 * time.Minute), Actor: "", State: "Ready"},
		}
		i.Comments = []fakeComment{{ID: "c2", Body: "second", User: "u-marta", At: t0.Add(2 * time.Second)}, {ID: "c1", Body: "first", User: "", At: t0.Add(time.Second)}}
	})
	got, err := w.adapter(t).Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "ENG" || got.Type != "Bug" || got.Body != "Steps\n\n- one" || got.State != "Todo" || got.Closed || got.Assignee.ID != "u-forecrew" || got.Reporter.ID != "u-marta" {
		t.Errorf("ticket = %+v", got)
	}
	if !slices.Equal(got.Labels, []string{"Bug", "backend"}) || got.URL != "https://linear.app/acme/issue/ENG-1" {
		t.Errorf("labels %v, url %s", got.Labels, got.URL)
	}
	if len(got.Comments) != 2 || got.Comments[0].ID != "c1" || got.Comments[0].Author.ID != "" || got.Comments[1].Author.ID != "u-marta" {
		t.Errorf("comments = %+v", got.Comments)
	}
	var triggers []string
	for _, h := range got.Handovers {
		triggers = append(triggers, string(h.Trigger)+"="+h.Value+" by "+h.By.ID)
	}
	want := []string{"assignee=u-forecrew by u-marta", "label=forecrew by u-marta", "state=Ready by "}
	if !slices.Equal(triggers, want) {
		t.Errorf("handovers = %v, want %v", triggers, want)
	}
}

func TestGetRefusesWhatIsNotOurs(t *testing.T) {
	w := newWorld(t)
	other := w.f.create("OPS", "elsewhere", "")
	ad := w.adapter(t)
	for _, key := range []string{other, "ENG-999", "nonsense", "eng-1"} {
		if _, err := ad.Get(t.Context(), key); !errors.Is(err, tracker.ErrNotFound) {
			t.Errorf("Get(%s) = %v", key, err)
		}
	}
	if _, err := ad.Comment(t.Context(), other, "hi"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Comment on another team's issue: %v", err)
	}
	if err := ad.Transition(t.Context(), other, "Done"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Transition of another team's issue: %v", err)
	}
	if w.f.mutated != 0 {
		t.Errorf("%d writes reached Linear", w.f.mutated)
	}
	// An issue that moved to another team since its key was known.
	moved := w.f.create("ENG", "moved", "")
	w.f.edit(moved, func(i *fakeIssue) { i.Team = "OPS" })
	if _, err := ad.Get(t.Context(), moved); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get of a moved issue: %v", err)
	}
}

func TestTransition(t *testing.T) {
	w := newWorld(t)
	key := w.f.create("ENG", "t", "")
	ad := w.adapter(t)
	if err := ad.Transition(t.Context(), key, "in progress"); err != nil {
		t.Fatal(err)
	}
	if w.f.issues[key].State != "In Progress" || w.f.mutated != 1 {
		t.Errorf("state %q after %d writes", w.f.issues[key].State, w.f.mutated)
	}
	if err := ad.Transition(t.Context(), key, "In Progress"); err != nil || w.f.mutated != 1 {
		t.Errorf("moving to where it is: %v, %d writes", err, w.f.mutated)
	}
	err := ad.Transition(t.Context(), key, "Shipped")
	if !errors.Is(err, tracker.ErrBlocked) || !strings.Contains(err.Error(), "Backlog") {
		t.Errorf("a state the team lacks: %v", err)
	}
}

func TestStates(t *testing.T) {
	w := newWorld(t, func(cfg map[string]any) { cfg["teams"] = []string{"ENG", "OPS"} })
	w.f.teams["OPS"] = append(w.f.teams["OPS"], fakeState{"ops-x", "Shipped", "completed"})
	w.f.teams["ENG"] = append(w.f.teams["ENG"], fakeState{"eng-x", "Shipped", "started"})
	got, err := w.adapter(t).States(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closed := map[string]bool{}
	for _, s := range got {
		closed[s.Name] = s.Closed
	}
	if len(got) != 7 || !closed["Done"] || !closed["Canceled"] || closed["Todo"] || closed["Shipped"] {
		t.Errorf("states = %+v", got)
	}
}

func TestChangesFindsCommentsAndPages(t *testing.T) {
	w := newWorld(t)
	w.f.pageSize = 2
	ad := w.adapter(t)
	_, cursor, err := ad.Changes(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for range 5 {
		keys = append(keys, w.f.create("ENG", "n", ""))
	}
	other := w.f.create("OPS", "other team", "")
	w.f.edit(keys[0], func(i *fakeIssue) {
		i.Comments = append(i.Comments, fakeComment{ID: "c", Body: "reply", User: "u-marta", At: w.f.now.Add(time.Hour)})
	})
	events, next, err := ad.Changes(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Key)
	}
	slices.Sort(got)
	if !slices.Equal(got, keys) || slices.Contains(got, other) {
		t.Errorf("events = %v, want %v", got, keys)
	}
	// The cursor is the newest update seen, the comment's included.
	if at, _ := time.Parse(time.RFC3339Nano, next); !at.Equal(w.f.now.Add(time.Hour)) {
		t.Errorf("next cursor %s", next)
	}
	if _, _, err := ad.Changes(t.Context(), "yesterday"); err == nil {
		t.Error("a cursor that is no time was taken")
	}
}

func TestWebhooks(t *testing.T) {
	w := newWorld(t)
	ad := w.adapter(t)
	key := w.f.create("ENG", "hooked", "")
	other := w.f.create("OPS", "other", "")
	issue := w.f.issues[key]

	one := func(r tracker.Request) tracker.Event {
		t.Helper()
		evs, err := ad.ParseEvent(r)
		if err != nil || len(evs) != 1 {
			t.Fatalf("events %+v, err %v", evs, err)
		}
		return evs[0]
	}
	none := func(r tracker.Request) {
		t.Helper()
		if evs, err := ad.ParseEvent(r); err != nil || len(evs) != 0 {
			t.Fatalf("events %+v, err %v", evs, err)
		}
	}
	// A payload that gives the number and team, a comment that gives the issue's ID only.
	e := one(w.f.signed("hook-secret", map[string]any{"action": "create", "type": "Issue", "data": map[string]any{"id": issue.ID, "number": 1, "team": map[string]string{"key": "ENG"}}}, "d1"))
	if e.Key != key || e.ID != "d1" || e.Self {
		t.Errorf("event = %+v", e)
	}
	if e := one(w.f.signed("hook-secret", map[string]any{"action": "create", "type": "Comment", "data": map[string]any{"issueId": issue.ID}}, "d2")); e.Key != key {
		t.Errorf("comment event = %+v", e)
	}
	if e := one(w.f.signed("hook-secret", map[string]any{"action": "create", "type": "Comment", "data": map[string]any{"issue": map[string]string{"id": issue.ID, "identifier": key}}}, "")); e.Key != key || len(e.ID) != 32 {
		t.Errorf("comment event without a delivery header = %+v", e)
	}
	// Only the issue's ID, no team in the payload: the adapter asks Linear.
	if e := one(w.f.signed("hook-secret", map[string]any{"action": "update", "type": "Issue", "data": map[string]any{"id": issue.ID}}, "d3")); e.Key != key {
		t.Errorf("issue event by ID = %+v", e)
	}
	// Forecrew's own write, once the adapter knows its user.
	if _, err := ad.Get(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.me(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e := one(w.f.signed("hook-secret", map[string]any{"action": "update", "type": "Issue", "actor": map[string]string{"id": "u-forecrew"}, "data": map[string]any{"identifier": key}}, "d4")); !e.Self {
		t.Errorf("own write not marked: %+v", e)
	}
	// Not ours: another team, another type, a removal, an unknown issue.
	none(w.f.issueEvent("hook-secret", other))
	none(w.f.signed("hook-secret", map[string]any{"action": "update", "type": "Project", "data": map[string]any{"id": "p"}}, "d5"))
	none(w.f.signed("hook-secret", map[string]any{"action": "remove", "type": "Issue", "data": map[string]any{"identifier": key}}, "d6"))
	none(w.f.signed("hook-secret", map[string]any{"action": "update", "type": "Issue", "data": map[string]any{"id": "uuid-missing"}}, "d7"))
}

func TestWebhookReplayAndSignature(t *testing.T) {
	w := newWorld(t)
	ad := w.adapter(t)
	key := w.f.create("ENG", "x", "")
	r := w.f.issueEvent("hook-secret", key)
	w.f.mu.Lock()
	w.f.now = w.f.now.Add(2 * time.Minute) // the delivery is old now
	w.f.mu.Unlock()
	if _, err := ad.ParseEvent(r); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a replay: %v", err)
	}
	// A body changed after it was signed.
	fresh := w.f.issueEvent("hook-secret", key)
	fresh.Body = []byte(strings.Replace(string(fresh.Body), key, "ENG-2", 1))
	if _, err := ad.ParseEvent(fresh); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a changed body kept its signature: %v", err)
	}
	fresh = w.f.issueEvent("hook-secret", key)
	fresh.Header.Set("Linear-Signature", "zz")
	if _, err := ad.ParseEvent(fresh); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a signature that is no hex: %v", err)
	}
	// Signed, but dated never.
	body := []byte(`{"action":"update","type":"Issue","data":{"identifier":"` + key + `"}}`)
	h := http.Header{}
	h.Set("Linear-Signature", macOf("hook-secret", body))
	if _, err := ad.ParseEvent(tracker.Request{Header: h, Body: body}); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("a delivery without a date: %v", err)
	}
}

func checkNames(cs []tracker.Check) (out []string) {
	for _, c := range cs {
		out = append(out, c.Name+map[bool]string{true: ":ok", false: ":fail"}[c.OK])
	}
	return out
}

func TestCheck(t *testing.T) {
	for name, tt := range map[string]struct {
		edit func(*world)
		want []string
		// detail is where in the checks a text must be.
		at   int
		text string
	}{
		"all pass": {want: []string{"address:ok", "certificate:ok", "login:ok", "projects:ok", "transitions:ok"}, at: 2, text: "Forecrew (u-forecrew)"},
		"a refused token": {
			edit: func(w *world) { w.conn.Config["auth"] = AuthOAuthToken },
			want: []string{"address:ok", "certificate:ok", "login:fail", "projects:fail", "transitions:fail"}, at: 3, text: "not checked",
		},
		"a team the user cannot see": {
			edit: func(w *world) { w.conn.Config["teams"] = []string{"ENG", "HR"} },
			want: []string{"address:ok", "certificate:ok", "login:ok", "projects:fail", "transitions:fail"}, at: 3, text: "HR",
		},
		"no closed state": {
			edit: func(w *world) { w.f.teams["ENG"] = states("eng")[:4] },
			want: []string{"address:ok", "certificate:ok", "login:ok", "projects:ok", "transitions:fail"}, at: 4, text: "completed or canceled",
		},
		"an untrusted certificate": {
			edit: func(w *world) { w.deps.RootCAs = nil },
			want: []string{"address:ok", "certificate:fail", "login:fail", "projects:fail", "transitions:fail"}, at: 1, text: "not trusted",
		},
		"an address nothing answers on": {
			edit: func(w *world) { w.deps.Endpoint = "https://127.0.0.1:1/graphql" },
			want: []string{"address:fail", "certificate:fail", "login:fail", "projects:fail", "transitions:fail"}, at: 0, text: "connect",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if tt.edit != nil {
				tt.edit(w)
			}
			got := w.adapter(t).Check(t.Context())
			if !slices.Equal(checkNames(got), tt.want) || !strings.Contains(got[tt.at].Detail, tt.text) {
				t.Errorf("checks = %+v", got)
			}
			if failed := slices.ContainsFunc(tt.want, func(s string) bool { return strings.HasSuffix(s, ":fail") }); failed != (w.adapter(t).Verify(t.Context()) != nil) {
				t.Errorf("Verify disagrees with the checks")
			}
		})
	}
}

func TestRateLimitsAreWaitedOutForReadsOnly(t *testing.T) {
	w := newWorld(t)
	ad := w.adapter(t)
	key := w.f.create("ENG", "t", "")
	w.f.limited = []string{"2"}
	if _, err := ad.Get(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.slept, []time.Duration{2 * time.Second}) {
		t.Errorf("slept %v", w.slept)
	}
	// A long wait fails the call and says so.
	w.f.limited = []string{"3600"}
	if _, err := ad.Get(t.Context(), key); err == nil || !strings.Contains(err.Error(), "asks to wait") {
		t.Errorf("a long rate limit: %v", err)
	}
	// A mutation is never repeated within a call: it could happen twice.
	w.f.mu.Lock()
	w.f.limited = []string{"1"}
	calls := w.f.calls
	w.f.mu.Unlock()
	w.slept = nil
	id := w.f.issues[key].ID
	err := ad.c.gql(t.Context(), `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success } }`, map[string]any{"input": map[string]any{"issueId": id, "body": "x"}}, nil, false)
	if err == nil || len(w.slept) != 0 || w.f.calls != calls+1 || w.f.mutated != 0 {
		t.Errorf("err %v, slept %v, calls %d, mutations %d", err, w.slept, w.f.calls-calls, w.f.mutated)
	}
}

func TestErrorsNeverHoldTheToken(t *testing.T) {
	w := newWorld(t)
	w.f.key = "something else"
	_, err := w.adapter(t).Get(t.Context(), "ENG-1")
	if err == nil || !errors.Is(err, errLogin) {
		t.Fatalf("a refused token: %v", err)
	}
	if strings.Contains(err.Error(), "lin_api_secret") {
		t.Errorf("the token is in %q", err)
	}
}

func TestRedirectsStayOnTheServer(t *testing.T) {
	w := newWorld(t)
	other := newFakeLinear(t)
	w.f.redirect = other.srv.URL
	_, err := w.adapter(t).Get(t.Context(), "ENG-1")
	if !errors.Is(err, ErrRefused) {
		t.Errorf("a redirect to another host: %v", err)
	}
	if other.calls != 0 {
		t.Error("the other server was called")
	}
	w.f.redirect = w.f.srv.URL
	if _, err := w.adapter(t).Get(t.Context(), "ENG-1"); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("a redirect loop: %v", err)
	}
}

func TestPrivateAddressesAreRefusedByDefault(t *testing.T) {
	w := newWorld(t)
	w.deps.PrivateAddresses = false
	_, err := w.adapter(t).Get(t.Context(), "ENG-1")
	if !errors.Is(err, councils.ErrRefused) {
		t.Errorf("a loopback endpoint: %v", err)
	}
}

func TestOAuthTokenGoesAsBearer(t *testing.T) {
	w := newWorld(t, func(cfg map[string]any) { cfg["auth"], cfg["token_ref"] = AuthOAuthToken, "store:linear-oauth" })
	w.f.key = "Bearer oauth-token"
	if err := w.adapter(t).Verify(t.Context()); err != nil {
		t.Error(err)
	}
}

func TestConfig(t *testing.T) {
	good := func() map[string]any { return map[string]any{"teams": []any{"ENG"}, "token_ref": "store:k"} }
	cases := map[string]func(map[string]any){
		"unknown key":      func(c map[string]any) { c["base_url"] = "https://x" },
		"no teams":         func(c map[string]any) { c["teams"] = []any{} },
		"lowercase team":   func(c map[string]any) { c["teams"] = []any{"eng"} },
		"auth":             func(c map[string]any) { c["auth"] = "basic" },
		"inline token":     func(c map[string]any) { c["token_ref"] = "lin_api_abc" },
		"env token":        func(c map[string]any) { c["token_ref"] = "env:LINEAR" },
		"inline hook":      func(c map[string]any) { c["webhook_secret_ref"] = "whsec" },
		"teams not a list": func(c map[string]any) { c["teams"] = "ENG" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			c := good()
			edit(c)
			if _, err := parseConfig(c); !errors.Is(err, tracker.ErrInvalid) {
				t.Errorf("err = %v", err)
			}
		})
	}
	c, err := parseConfig(good())
	if err != nil || c.Auth != AuthAPIKey || !c.ours("ENG-12") || c.ours("OPS-1") || c.ours("ENG-0") || c.ours("ENG-1a") {
		t.Errorf("config %+v, %v", c, err)
	}
	w := newWorld(t)
	w.conn.Config = map[string]any{"teams": []any{}}
	if _, err := Factory(w.deps)(w.conn); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("Factory: %v", err)
	}
}

func TestSecretStoreMissing(t *testing.T) {
	w := newWorld(t)
	w.deps.Secrets = nil
	_, err := w.adapter(t).Get(t.Context(), "ENG-1")
	if err == nil || !strings.Contains(err.Error(), "FORECREW_SECRET_KEY") {
		t.Errorf("err = %v", err)
	}
	w = newWorld(t, func(cfg map[string]any) { cfg["token_ref"] = "store:missing" })
	if _, err := w.adapter(t).Get(t.Context(), "ENG-1"); err == nil || !strings.Contains(err.Error(), "store:missing") {
		t.Errorf("a token that is not in the store: %v", err)
	}
}

func TestCommentRoundTrip(t *testing.T) {
	w := newWorld(t)
	key := w.f.create("ENG", "c", "")
	ad := w.adapter(t)
	id, err := ad.Comment(t.Context(), key, "**done** in [PR](https://x)")
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	if body := w.f.issues[key].Comments[0].Body; body != "**done** in [PR](https://x)" {
		t.Errorf("Linear got %q", body)
	}
}

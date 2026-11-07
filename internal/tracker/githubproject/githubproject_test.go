package githubproject_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/githubissues"
	"github.com/forecrew/forecrew/internal/tracker/githubproject"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
	"github.com/forecrew/forecrew/internal/workflow"
)

const (
	repo   = "acme/app"
	lib    = "acme/lib"
	secret = "hook-secret"
)

var (
	ann = github.User{ID: 11, Login: "ann"}
	bob = github.User{ID: 12, Login: "bob"}
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type env struct {
	clock   *testkit.Clock
	gh      *testkit.Tracker
	board   *testkit.Projects
	st      *testkit.Store
	account domain.Account
	ad      tracker.Adapter
	apps    map[string]domain.Project // by repo
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := testkit.NewStore(nil)
	clock := testkit.NewClock(time.Now())
	e := &env{clock: clock, gh: testkit.NewTracker(clock.Now), st: st, account: domain.Account{Name: "default"}, apps: map[string]domain.Project{}}
	e.board = testkit.NewProjects(e.gh, "acme", 7, "Todo", "Forecrew", "In review", "Done")
	must(t, st.Accounts().Create(t.Context(), &e.account))
	for _, r := range []string{repo, lib} {
		p := domain.Project{AccountID: e.account.ID, Name: r[len("acme/"):], Repo: r, DefaultBranch: "main", Status: domain.ProjectOnboarded}
		must(t, st.Projects().Create(t.Context(), &p))
		e.apps[r] = p
		e.gh.Install(r)
	}
	e.ad = e.adapter(t, map[string]any{"owner": "acme", "project": 7, "bot_login": "forecrew[bot]"})
	return e
}

func (e *env) adapter(t *testing.T, config map[string]any) tracker.Adapter {
	t.Helper()
	ad, err := e.factory()(domain.TrackerConnection{AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: config, Triggers: domain.TrackerTriggers{State: "Forecrew"}})
	must(t, err)
	return ad
}

func (e *env) factory() tracker.Factory {
	return githubproject.Factory(githubproject.Deps{Projects: e.board, Issues: e.gh, Store: e.st, Secret: []byte(secret), Now: e.clock.Now})
}

// card is an issue of repo on the board, in no column.
func (e *env) card(t *testing.T, repo, title, body string) (key string, number int) {
	t.Helper()
	i, err := e.gh.CreateIssue(t.Context(), repo, github.IssueInput{Title: title, Body: body})
	must(t, err)
	return e.board.AddIssue(repo, i.Number), i.Number
}

func sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func delivery(t *testing.T, key, kind string, payload map[string]any) tracker.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	must(t, err)
	h := http.Header{}
	h.Set("X-GitHub-Event", kind)
	h.Set("X-GitHub-Delivery", "d-1")
	h.Set("X-Hub-Signature-256", sign(key, body))
	return tracker.Request{Header: h, Body: body}
}

func itemPayload(card, contentType, org, sender, action string) map[string]any {
	return map[string]any{
		"action": action, "projects_v2_item": map[string]any{"node_id": card, "content_type": contentType, "project_node_id": "PVT_7"},
		"organization": map[string]any{"login": org}, "sender": map[string]any{"login": sender},
	}
}

func TestConformance(t *testing.T) {
	e := newEnv(t)
	trackertest.Run(t, trackertest.Harness{
		Adapter: e.ad,
		NewTicket: func(t *testing.T, title, body string) string {
			key, _ := e.card(t, repo, title, body)
			e.board.Move(key, ann, "Todo")
			return key
		},
		Touch: func(_ *testing.T, key string) { e.board.Touch(key) },
		Signed: func(t *testing.T, key string) tracker.Request {
			return delivery(t, secret, "projects_v2_item", itemPayload(key, "Issue", "acme", "ann", "edited"))
		},
		Forged: func(t *testing.T, key string) tracker.Request {
			return delivery(t, "wrong", "projects_v2_item", itemPayload(key, "Issue", "acme", "ann", "edited"))
		},
	})
}

func TestFactory(t *testing.T) {
	e := newEnv(t)
	for name, config := range map[string]map[string]any{
		"no owner": {"project": 7}, "no project": {"owner": "acme"}, "bad type": {"owner": "acme", "project": "seven"},
	} {
		if _, err := e.factory()(domain.TrackerConnection{Name: "board", Kind: githubproject.Kind, Config: config}); !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	caps := e.ad.Capabilities()
	if !caps.Columns || !caps.Transitions || !caps.Webhooks || caps.Assignment || caps.Labels {
		t.Errorf("capabilities = %+v", caps)
	}
	if err := tracker.CheckTriggers(domain.TrackerConnection{Kind: githubproject.Kind, Triggers: domain.TrackerTriggers{Assignee: "x"}}, caps); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("an assignee trigger on a board: %v", err)
	}
}

// A connection cannot reach into the board of another organization, though
// the App is installed there.
func TestVerify(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	must(t, e.ad.Verify(ctx))
	other := domain.Account{Name: "other"}
	must(t, e.st.Accounts().Create(ctx, &other))
	must(t, e.st.Projects().Create(ctx, &domain.Project{AccountID: other.ID, Name: "secret", Repo: "rival/secret", DefaultBranch: "main", Status: domain.ProjectOnboarded}))
	rival := testkit.NewProjects(e.gh, "rival", 1, "Forecrew")
	ad, err := githubproject.Factory(githubproject.Deps{Projects: rival, Issues: e.gh, Store: e.st})(domain.TrackerConnection{
		AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: map[string]any{"owner": "rival", "project": 1}, Triggers: domain.TrackerTriggers{State: "Forecrew"},
	})
	must(t, err)
	if err := ad.Verify(ctx); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("another organization's board: %v", err)
	}

	for name, tc := range map[string]struct {
		config  map[string]any
		trigger string
		err     error
		failing error
	}{
		"no such board":      {map[string]any{"owner": "acme", "project": 99}, "Forecrew", tracker.ErrInvalid, nil},
		"no such column":     {map[string]any{"owner": "acme", "project": 7}, "Doing", tracker.ErrInvalid, nil},
		"column by any case": {map[string]any{"owner": "ACME", "project": 7}, "forecrew", nil, nil},
		"no permission":      {map[string]any{"owner": "acme", "project": 7}, "Forecrew", tracker.ErrInvalid, github.ErrForbidden},
		"github is down":     {map[string]any{"owner": "acme", "project": 7}, "Forecrew", github.ErrRateLimited, github.ErrRateLimited},
	} {
		e.board.Err = tc.failing
		ad, err := e.factory()(domain.TrackerConnection{AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: tc.config, Triggers: domain.TrackerTriggers{State: tc.trigger}})
		must(t, err)
		got := ad.Verify(ctx)
		if tc.err == nil && got != nil || tc.err != nil && !errors.Is(got, tc.err) {
			t.Errorf("%s: %v", name, got)
		}
	}
	e.board.Err = nil
	// No repo of the organization's own at all.
	empty := domain.Account{Name: "empty"}
	must(t, e.st.Accounts().Create(ctx, &empty))
	ad, err = e.factory()(domain.TrackerConnection{AccountID: empty.ID, Name: "board", Kind: githubproject.Kind, Config: map[string]any{"owner": "acme", "project": 7}, Triggers: domain.TrackerTriggers{State: "Forecrew"}})
	must(t, err)
	if err := ad.Verify(ctx); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("no projects: %v", err)
	}
}

func TestParseEvent(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct {
		name    string
		kind    string
		payload map[string]any
		want    []tracker.Event
	}{
		{"a move", "projects_v2_item", itemPayload("PVTI_1", "Issue", "acme", "ann", "edited"), []tracker.Event{{ID: "d-1", Key: "PVTI_1"}}},
		{"a draft", "projects_v2_item", itemPayload("PVTI_2", "DraftIssue", "ACME", "ann", "created"), []tracker.Event{{ID: "d-1", Key: "PVTI_2"}}},
		{"own move", "projects_v2_item", itemPayload("PVTI_1", "Issue", "acme", "forecrew[bot]", "edited"), []tracker.Event{{ID: "d-1", Key: "PVTI_1", Self: true}}},
		{"a pull request", "projects_v2_item", itemPayload("PVTI_3", "PullRequest", "acme", "ann", "edited"), nil},
		{"a deleted card", "projects_v2_item", itemPayload("PVTI_1", "Issue", "acme", "ann", "deleted"), nil},
		{"another organization", "projects_v2_item", itemPayload("PVTI_1", "Issue", "rival", "ann", "edited"), nil},
		{"no card", "projects_v2_item", itemPayload("", "Issue", "acme", "ann", "edited"), nil},
		{"an issue event", "issues", map[string]any{}, nil},
	} {
		got, err := e.ad.ParseEvent(delivery(t, secret, tt.kind, tt.payload))
		must(t, err)
		if len(got) != len(tt.want) || (len(got) == 1 && got[0] != tt.want[0]) {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
	// The App's own login is not known: nothing is Forecrew's.
	noBot := e.adapter(t, map[string]any{"owner": "acme", "project": 7})
	got, err := noBot.ParseEvent(delivery(t, secret, "projects_v2_item", itemPayload("PVTI_1", "Issue", "acme", "forecrew[bot]", "edited")))
	if err != nil || len(got) != 1 || got[0].Self {
		t.Errorf("without bot_login: %+v, %v", got, err)
	}
	bad := delivery(t, secret, "projects_v2_item", nil)
	bad.Body = []byte("nope")
	bad.Header.Set("X-Hub-Signature-256", sign(secret, bad.Body))
	if _, err := e.ad.ParseEvent(bad); err == nil {
		t.Error("an unreadable payload passed")
	}
}

func TestGetReadsWhoMovedTheCard(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	key, n := e.card(t, repo, "Crash on start", "It crashes.")
	must(t, e.gh.EditIssue(repo, n, func(i *github.Issue) { i.Type, i.Author, i.Labels = "Bug", ann, []string{"p1"} }))
	must(t, e.gh.AssignIssue(repo, n, ann, bob))
	_, err := e.gh.CommentAs(repo, n, ann, "more detail")
	must(t, err)
	_, err = e.gh.CreateComment(ctx, repo, n, "from the App")
	must(t, err)

	got, err := e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 0 || got.State != "" {
		t.Errorf("a card in no column: %+v", got.Handovers)
	}

	e.board.Move(key, ann, "Todo")
	e.board.Move(key, bob, "Forecrew")
	e.board.Move(key, ann, "Forecrew") // moved again, by someone else
	got, err = e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 1 {
		t.Fatalf("handovers = %+v", got.Handovers)
	}
	h := got.Handovers[0]
	if h.Trigger != tracker.TriggerState || h.Value != "Forecrew" || h.By.ID != "11" || h.By.Name != "ann" || h.At.IsZero() {
		t.Errorf("handover = %+v", h)
	}
	if got.State != "Forecrew" || got.Project != repo || got.Type != "Bug" || got.Reporter.ID != "11" || got.Assignee.ID != "12" || got.Closed ||
		got.Issue == nil || got.Issue.Repo != repo || got.Issue.Number != n || got.Title != "Crash on start" || got.Body != "It crashes." || got.URL == "" {
		t.Errorf("ticket = %+v", got)
	}
	if len(got.Comments) != 2 || got.Comments[0].Author.ID != "11" || !got.Comments[1].Author.Bot {
		t.Errorf("comments = %+v", got.Comments)
	}

	// A move out of the trigger column hands nothing over, and a move by
	// a workflow has no person behind it.
	e.board.Move(key, ann, "In review")
	got, err = e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 1 || got.Handovers[0].Value != "In review" || got.Handovers[0].By != (tracker.User{}) {
		t.Errorf("handover outside the trigger = %+v", got.Handovers)
	}
	e.board.MoveByWorkflow(key, "Forecrew")
	got, err = e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 1 || got.Handovers[0].By != (tracker.User{}) || got.Handovers[0].Value != "Forecrew" {
		t.Errorf("a workflow's move = %+v", got.Handovers)
	}
}

func TestGetAnIssueOfAnotherRepo(t *testing.T) {
	e := newEnv(t)
	key, n := e.card(t, lib, "Library bug", "")
	got, err := e.ad.Get(t.Context(), key)
	must(t, err)
	if got.Project != lib || got.Issue == nil || got.Issue.Repo != lib || got.Issue.Number != n {
		t.Errorf("ticket = %+v", got)
	}
}

func TestGetADraft(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	key := e.board.AddDraft("Idea", "Think about it")
	e.board.Move(key, ann, "Forecrew")
	got, err := e.ad.Get(ctx, key)
	must(t, err)
	if got.Project != "" || got.Issue != nil || got.Title != "Idea" || got.URL != "https://github.com/orgs/acme/projects/7" {
		t.Errorf("draft = %+v", got)
	}
	// No timeline: nobody is known to have moved it.
	if len(got.Handovers) != 1 || got.Handovers[0].By != (tracker.User{}) {
		t.Errorf("handover = %+v", got.Handovers)
	}
	if _, err := e.ad.Comment(ctx, key, "hi"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment on a draft: %v", err)
	}
	must(t, e.ad.Transition(ctx, key, "Done"))
	if got, err := e.ad.Get(ctx, key); err != nil || !got.Closed {
		t.Errorf("a draft in Done = %+v, %v", got, err)
	}
}

func TestGetOfCardsThatAreNotTickets(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	key, n := e.card(t, repo, "GOAL-1: mirrored", "")
	must(t, e.gh.LabelIssue(repo, n, ann, "forecrew:goal"))
	e.board.Move(key, ann, "Forecrew")
	got, err := e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 0 {
		t.Errorf("Forecrew's own issue can be handed over: %+v", got.Handovers)
	}
	pr := e.board.AddPullRequest(repo, 99)
	for _, k := range []string{pr, "PVTI_404", ""} {
		if _, err := e.ad.Get(ctx, k); !errors.Is(err, tracker.ErrNotFound) {
			t.Errorf("Get(%q): %v", k, err)
		}
	}
	// The issue is gone from GitHub, or the App cannot read it: GitHub redacts the card.
	lost := e.board.AddIssue("acme/missing", 1)
	if _, err := e.ad.Get(ctx, lost); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("a redacted card: %v", err)
	}
}

func TestFinishedCards(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	closed := func(key string) bool {
		got, err := e.ad.Get(ctx, key)
		must(t, err)
		return got.Closed
	}
	key, n := e.card(t, repo, "x", "")
	if closed(key) {
		t.Error("a new card is finished")
	}
	e.board.Move(key, ann, "Done")
	if !closed(key) {
		t.Error("a card in Done is not finished")
	}
	e.board.Move(key, ann, "In review")
	must(t, e.gh.EditIssue(repo, n, func(i *github.Issue) { i.State = "closed" }))
	if !closed(key) {
		t.Error("a closed issue is not finished")
	}
	archived, _ := e.card(t, repo, "y", "")
	e.board.Archive(archived)
	if !closed(archived) {
		t.Error("an archived card is not finished")
	}
	// Another column may count as finished.
	custom := e.adapter(t, map[string]any{"owner": "acme", "project": 7, "closed": []any{"In review"}})
	if got, err := custom.Get(ctx, key); err != nil || !got.Closed {
		t.Errorf("closed: [In review] = %+v, %v", got, err)
	}
	states, err := custom.States(ctx)
	must(t, err)
	if len(states) != 4 || states[2].Name != "In review" || !states[2].Closed || states[3].Closed {
		t.Errorf("states = %+v", states)
	}
}

// A column that was renamed or deleted is reported, and nothing moves.
func TestTransitionToAMissingColumn(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	key, _ := e.card(t, repo, "x", "")
	e.board.Move(key, ann, "Todo")
	e.board.RenameColumn("In review", "Review")
	err := e.ad.Transition(ctx, key, "In review")
	if !errors.Is(err, tracker.ErrInvalid) || !strings.Contains(err.Error(), "Review") {
		t.Errorf("renamed column: %v", err)
	}
	e.board.RemoveColumn("Done")
	if err := e.ad.Transition(ctx, key, "Done"); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("deleted column: %v", err)
	}
	if e.board.Column(key) != "Todo" || e.board.Writes != 0 {
		t.Errorf("a card moved: %q after %d writes", e.board.Column(key), e.board.Writes)
	}
	must(t, e.ad.Transition(ctx, key, "review")) // by its new name, in any case
	if e.board.Column(key) != "Review" {
		t.Errorf("column = %q", e.board.Column(key))
	}
	if err := e.ad.Transition(ctx, "PVTI_404", "Todo"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("a card that is gone: %v", err)
	}
	e.board.Err = github.ErrRateLimited
	if err := e.ad.Transition(ctx, key, "Todo"); !errors.Is(err, github.ErrRateLimited) {
		t.Errorf("GitHub down: %v", err)
	}
	if _, err := e.ad.States(ctx); !errors.Is(err, github.ErrRateLimited) {
		t.Errorf("States while GitHub is down: %v", err)
	}
	e.board.Err = nil
	// The trigger's column is gone: a move into the column of that name is
	// all that is left, and nothing hands over.
	e.board.RemoveColumn("Forecrew")
	got, err := e.ad.Get(ctx, key)
	must(t, err)
	if len(got.Handovers) != 1 || got.Handovers[0].Value != "Review" {
		t.Errorf("handovers = %+v", got.Handovers)
	}
}

func TestWrites(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.ad.Comment(ctx, "PVTI_404", "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment on nothing: %v", err)
	}
	gone := e.board.AddIssue("acme/missing", 1)
	if _, err := e.ad.Comment(ctx, gone, "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment on a card GitHub redacts: %v", err)
	}
	key, n := e.card(t, repo, "x", "")
	id, err := e.ad.Comment(ctx, key, "hello")
	must(t, err)
	cs, err := e.gh.ListComments(ctx, repo, n, time.Time{})
	must(t, err)
	if len(cs) != 1 || cs[0].Body != "hello" || id == "" {
		t.Errorf("comments = %+v, %q", cs, id)
	}
}

func TestChanges(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if _, _, err := e.ad.Changes(ctx, "not a time"); err == nil {
		t.Error("a bad cursor passed")
	}
	_, cursor, err := e.ad.Changes(ctx, "")
	must(t, err)
	e.clock.Advance(time.Second)
	key, _ := e.card(t, repo, "x", "")
	draft := e.board.AddDraft("d", "")
	pr := e.board.AddPullRequest(repo, 50)
	e.board.Move(key, ann, "Forecrew")
	events, next, err := e.ad.Changes(ctx, cursor)
	must(t, err)
	keys := map[string]bool{}
	for _, ev := range events {
		keys[ev.Key] = true
	}
	if !keys[key] || !keys[draft] || keys[pr] || len(events) != 2 || next == cursor {
		t.Errorf("events = %+v, cursor %s -> %s", events, cursor, next)
	}
	// Later, nothing is new: the cards of the cursor's own moment may come
	// again, and no others.
	e.clock.Advance(time.Minute)
	events, _, err = e.ad.Changes(ctx, next)
	must(t, err)
	for _, ev := range events {
		if !keys[ev.Key] {
			t.Errorf("event = %+v", ev)
		}
	}
	e.board.Touch(key)
	events, _, err = e.ad.Changes(ctx, next)
	must(t, err)
	if !hasKey(events, key) {
		t.Errorf("a touched card is not reported: %+v", events)
	}
	e.board.Err = github.ErrForbidden
	if _, _, err := e.ad.Changes(ctx, next); !errors.Is(err, github.ErrForbidden) {
		t.Errorf("GitHub refuses: %v", err)
	}
}

func TestLinkedUser(t *testing.T) {
	e := newEnv(t)
	l := e.ad.(tracker.Linker)
	for _, u := range []tracker.User{{Name: "no id"}, {ID: "abc"}, {ID: "11"}} {
		if id, ok, err := l.LinkedUser(context.Background(), e.account.ID, u); err != nil || ok {
			t.Errorf("%+v = %s, %v, %v", u, id, ok, err)
		}
	}
}

type runs struct{ started, canceled int }

func (r *runs) Start(_ context.Context, _ string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	r.started++
	return domain.WorkflowRun{ID: req.ID}, nil
}

func (r *runs) CancelRun(context.Context, uuid.UUID, uuid.UUID, string) error {
	r.canceled++
	return nil
}

// world is the core over a board and over GitHub Issues, on the same repos.
type world struct {
	*env
	svc     *tracker.Service
	runs    *runs
	member  domain.User
	boardC  domain.TrackerConnection
	issuesC domain.TrackerConnection
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := newEnv(t)
	ctx := t.Context()
	w := &world{env: e, runs: &runs{}}
	w.member = e.st.AddMember(e.account.ID, domain.User{Name: "Ann"}, "")
	testkit.Bind(t, e.st, e.account.ID, domain.SubjectUser, w.member.ID, domain.RoleScopeOrganization, domain.RoleMember, uuid.Nil())
	w.boardC = domain.TrackerConnection{AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: map[string]any{"owner": "acme", "project": 7}, Triggers: domain.TrackerTriggers{State: "Forecrew"}}
	must(t, e.st.TrackerConnections().Create(ctx, &w.boardC))
	w.issuesC = domain.TrackerConnection{AccountID: e.account.ID, Name: "issues", Kind: githubissues.Kind, Config: map[string]any{"repos": []any{repo, lib}}, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	must(t, e.st.TrackerConnections().Create(ctx, &w.issuesC))
	for _, c := range []domain.TrackerConnection{w.boardC, w.issuesC} {
		for _, r := range []string{repo, lib} {
			must(t, e.st.TrackerRoutes().Create(ctx, &domain.TrackerRoute{
				AccountID: e.account.ID, ConnectionID: c.ID, ProjectID: e.apps[r].ID, Match: domain.TrackerMatch{Project: r},
				States: map[domain.TicketState]string{domain.TicketInProgress: "In review", domain.TicketDone: "Done"},
			}))
		}
		must(t, e.st.TrackerIdentities().Create(ctx, &domain.TrackerIdentity{AccountID: e.account.ID, ConnectionID: c.ID, ExternalID: "11", UserID: w.member.ID, CreatedBy: "user:x"}))
	}
	registry := tracker.NewRegistry()
	registry.Register(githubproject.Kind, e.factory())
	registry.Register(githubissues.Kind, githubissues.Factory(githubissues.Deps{GitHub: e.gh, Store: e.st, Secret: []byte(secret)}))
	w.svc = tracker.New(tracker.Config{
		Store: e.st, Registry: registry, Gates: testkit.NewGates(e.st, testkit.NewSignalBus(e.st), nil), Authz: authz.New(e.st, nil), Runs: w.runs,
		Playbook: func(domain.Project, domain.GoalKind) string { return "goal" },
	})
	return w
}

func (w *world) hint(t *testing.T, c domain.TrackerConnection, key string) {
	t.Helper()
	must(t, w.svc.Hint(t.Context(), c, tracker.Event{Key: key}))
}

func (w *world) goals(t *testing.T, repo string) []domain.Goal {
	t.Helper()
	g, err := w.st.Goals().List(t.Context(), w.account.ID, store.GoalFilter{ProjectID: w.apps[repo].ID})
	must(t, err)
	return g
}

func (w *world) links(t *testing.T) []domain.TicketLink {
	t.Helper()
	l, err := w.st.TicketLinks().List(t.Context(), w.account.ID, store.TicketLinkFilter{})
	must(t, err)
	return l
}

// A card moved to the column starts its goal, whatever its event says; the
// goal then moves the card.
func TestAMoveStartsAGoalAndTheGoalMovesTheCard(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	key, n := w.card(t, repo, "Add export", "Export it all.")

	// A signed event for a card that is not in the column starts nothing.
	must(t, w.svc.Deliver(ctx, githubproject.Kind, delivery(t, secret, "projects_v2_item", itemPayload(key, "Issue", "acme", "ann", "edited"))))
	w.svc.Drain(ctx)
	if w.runs.started != 0 {
		t.Fatal("an event started work")
	}
	if err := w.svc.Deliver(ctx, githubproject.Kind, delivery(t, "wrong", "projects_v2_item", itemPayload(key, "Issue", "acme", "ann", "edited"))); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("forged: %v", err)
	}

	w.board.Move(key, ann, "Forecrew")
	moved := delivery(t, secret, "projects_v2_item", itemPayload(key, "Issue", "acme", "ann", "edited"))
	moved.Header.Set("X-GitHub-Delivery", "d-2")
	must(t, w.svc.Deliver(ctx, githubproject.Kind, moved))
	must(t, w.svc.Deliver(ctx, githubproject.Kind, moved)) // redelivered
	w.svc.Drain(ctx)
	if w.runs.started != 1 {
		t.Fatalf("runs = %d", w.runs.started)
	}
	goals := w.goals(t, repo)
	if len(goals) != 1 || goals[0].IssueNumber != n || goals[0].Title != "Add export" || goals[0].CreatedBy != "user:"+w.member.ID.String() {
		t.Fatalf("goals = %+v", goals)
	}
	must(t, w.svc.Sync(ctx))
	if got := w.board.Column(key); got != "In review" {
		t.Errorf("the card is in %q", got)
	}
	comments, err := w.gh.ListComments(ctx, repo, n, time.Time{})
	must(t, err)
	if len(comments) != 1 || !strings.Contains(comments[0].Body, goals[0].Ref()) {
		t.Errorf("comments = %+v", comments)
	}
	// Forecrew's own move does not start anything or come back as a handover.
	w.hint(t, w.boardC, key)
	if len(w.goals(t, repo)) != 1 {
		t.Error("a second goal")
	}
}

// The same work is a card and an issue with the label: one goal, one ticket.
func TestACardAndItsIssueMakeOneGoal(t *testing.T) {
	for _, first := range []string{"card first", "issue first"} {
		t.Run(first, func(t *testing.T) {
			w := newWorld(t)
			key, n := w.card(t, repo, "Add export", "Export it all.")
			card := func() {
				w.board.Move(key, ann, "Forecrew")
				w.hint(t, w.boardC, key)
			}
			issue := func() {
				must(t, w.gh.LabelIssue(repo, n, ann, "forecrew"))
				w.hint(t, w.issuesC, repo+"#"+strconv.Itoa(n))
			}
			if first == "card first" {
				card()
				issue()
			} else {
				issue()
				card()
			}
			w.oneGoal(t, n)
		})
	}
}

// oneGoal checks that the issue has one goal, one live ticket and one ended,
// and that a comment says which goal has the issue.
func (w *world) oneGoal(t *testing.T, issue int) {
	t.Helper()
	if got := len(w.goals(t, repo)); got != 1 || w.runs.started != 1 {
		t.Fatalf("goals = %d, runs = %d", got, w.runs.started)
	}
	live := 0
	for _, l := range w.links(t) {
		if l.Status.Live() {
			live++
		}
	}
	if live != 1 || len(w.links(t)) != 2 {
		t.Errorf("links = %+v", w.links(t))
	}
	must(t, w.svc.Sync(t.Context()))
	comments, err := w.gh.ListComments(t.Context(), repo, issue, time.Time{})
	must(t, err)
	if !mentions(comments, "works on this issue already") {
		t.Errorf("no comment tells which goal has the issue: %+v", comments)
	}
}

func mentions(cs []github.Comment, text string) bool {
	for _, c := range cs {
		if strings.Contains(c.Body, text) {
			return true
		}
	}
	return false
}

// A move with no known person starts nothing: the ticket goes to the Inbox.
func TestAMoveWithNoKnownActorStartsNothing(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	waiting := func(key string, reason domain.TicketWaitReason) {
		t.Helper()
		for _, l := range w.links(t) {
			if l.Key == key {
				if l.Status != domain.LinkWaiting || l.Reason != reason {
					t.Errorf("link of %s = %s %s, want waiting %s", key, l.Status, l.Reason, reason)
				}
				return
			}
		}
		t.Errorf("no link of %s", key)
	}

	byWorkflow, _ := w.card(t, repo, "Moved by a workflow", "")
	w.board.MoveByWorkflow(byWorkflow, "Forecrew")
	w.hint(t, w.boardC, byWorkflow)
	waiting(byWorkflow, domain.WaitUnknownPerson)

	stranger, _ := w.card(t, repo, "Moved by a stranger", "")
	w.board.Move(stranger, github.User{ID: 999, Login: "mallory"}, "Forecrew")
	w.hint(t, w.boardC, stranger)
	waiting(stranger, domain.WaitUnknownPerson)

	byBot, _ := w.card(t, repo, "Moved by a bot", "")
	w.board.Move(byBot, github.User{ID: 77, Login: "dependabot[bot]", Bot: true}, "Forecrew")
	w.hint(t, w.boardC, byBot)
	waiting(byBot, domain.WaitUnknownPerson)

	// A card that has no event at all, which GitHub cannot say who moved.
	noEvent, n := w.card(t, repo, "No event", "")
	must(t, w.gh.EditIssue(repo, n, func(*github.Issue) {}))
	w.board.MoveByWorkflow(noEvent, "Forecrew")
	w.hint(t, w.boardC, noEvent)
	waiting(noEvent, domain.WaitUnknownPerson)

	if w.runs.started != 0 || len(w.goals(t, repo)) != 0 {
		t.Errorf("work started: %d runs", w.runs.started)
	}
	// Polling finds the same.
	_, cursor, err := w.ad.Changes(ctx, "")
	must(t, err)
	late, _ := w.card(t, repo, "Late", "")
	w.board.MoveByWorkflow(late, "Forecrew")
	events, _, err := w.ad.Changes(ctx, cursor)
	must(t, err)
	for _, ev := range events {
		must(t, w.svc.Hint(ctx, w.boardC, ev))
	}
	waiting(late, domain.WaitUnknownPerson)
	if w.runs.started != 0 {
		t.Error("polling started work")
	}
}

// A draft has no repo, so no route can give it a project: the Inbox. Cards of
// two repos go to the project of their own repo.
func TestDraftsAndCardsOfSeveralRepos(t *testing.T) {
	w := newWorld(t)
	draft := w.board.AddDraft("Idea", "Think")
	w.board.Move(draft, ann, "Forecrew")
	w.hint(t, w.boardC, draft)
	appCard, _ := w.card(t, repo, "In app", "")
	libCard, _ := w.card(t, lib, "In lib", "")
	for _, k := range []string{appCard, libCard} {
		w.board.Move(k, ann, "Forecrew")
		w.hint(t, w.boardC, k)
	}
	if got := w.goals(t, repo); len(got) != 1 || got[0].Title != "In app" {
		t.Errorf("app goals = %+v", got)
	}
	if got := w.goals(t, lib); len(got) != 1 || got[0].Title != "In lib" {
		t.Errorf("lib goals = %+v", got)
	}
	for _, l := range w.links(t) {
		if l.Key == draft && (l.Status != domain.LinkWaiting || l.Reason != domain.WaitNoRoute || l.ProjectID != uuid.Nil()) {
			t.Errorf("draft link = %+v", l)
		}
	}
}

// A renamed trigger column hands nothing over; a deleted one neither.
func TestRenamedTriggerColumnStartsNothing(t *testing.T) {
	w := newWorld(t)
	key, _ := w.card(t, repo, "Add export", "")
	w.board.RenameColumn("Forecrew", "Agents")
	w.board.Move(key, ann, "Agents")
	w.hint(t, w.boardC, key)
	if len(w.links(t)) != 0 || w.runs.started != 0 {
		t.Errorf("links = %+v", w.links(t))
	}
	if err := w.ad.Verify(t.Context()); err != nil {
		// The adapter of the test env names Forecrew too: a renamed column is found when it is verified.
		if !errors.Is(err, tracker.ErrInvalid) {
			t.Error(err)
		}
	} else {
		t.Error("Verify did not report the renamed column")
	}
}

// A person moving a card to a finished column cancels its goal; a card
// that is gone is no event.
func TestACardThatIsDoneEndsItsTicket(t *testing.T) {
	w := newWorld(t)
	key, _ := w.card(t, repo, "Add export", "")
	w.board.Move(key, ann, "Forecrew")
	w.hint(t, w.boardC, key)
	if len(w.goals(t, repo)) != 1 {
		t.Fatal("no goal")
	}
	w.board.Move(key, ann, "Done")
	w.hint(t, w.boardC, key)
	if w.runs.canceled != 1 {
		t.Errorf("canceled runs = %d", w.runs.canceled)
	}
	w.board.Remove(key)
	w.hint(t, w.boardC, key) // a card that is gone is no event
}

// The timeline names the person who made the card's own move, not one who made
// an earlier or another board's.
func TestOnlyTheMoveThatPutTheCardThereIsCredited(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	by := func(key string) tracker.User {
		t.Helper()
		got, err := e.ad.Get(ctx, key)
		must(t, err)
		if len(got.Handovers) != 1 {
			t.Fatalf("handovers = %+v", got.Handovers)
		}
		return got.Handovers[0].By
	}

	// A card moved by ann, removed, and added again: its column was set again
	// with no event, an hour after her move.
	first, n := e.card(t, repo, "Add export", "")
	e.board.Move(first, ann, "Forecrew")
	if by(first).ID != "11" {
		t.Fatal("ann's own move was not credited")
	}
	e.clock.Advance(time.Hour)
	e.board.Remove(first)
	again := e.board.AddIssue(repo, n)
	e.board.MoveUnrecorded(again, "Forecrew")
	if got := by(again); got != (tracker.User{}) {
		t.Errorf("a card put back with no event is credited to %+v", got)
	}
	// Within a moment of the move, the event is still its own.
	e.clock.Advance(30 * time.Second)
	close, m := e.card(t, repo, "Close", "")
	e.board.Move(close, ann, "Forecrew")
	e.clock.Advance(30 * time.Second)
	e.board.MoveUnrecorded(close, "Forecrew")
	if by(close).ID != "11" {
		t.Errorf("an event a few seconds before the card's time is not credited: %+v", by(close))
	}
	_ = m

	// Another board's card of the same issue, moved by a member into a column
	// of the same name, is not this card's move.
	other, k := e.card(t, repo, "On two boards", "")
	e.board.MoveUnrecorded(other, "Forecrew")
	e.board.MoveOnAnotherBoard(repo, k, ann, "Forecrew")
	if got := by(other); got != (tracker.User{}) {
		t.Errorf("a move on another board is credited to %+v", got)
	}
}

// A move by a board workflow is not a person's, though GitHub names an actor.
func TestAWorkflowsMoveIsNobodys(t *testing.T) {
	e := newEnv(t)
	key, _ := e.card(t, repo, "Add export", "")
	e.board.MoveByWorkflow(key, "Forecrew")
	got, err := e.ad.Get(t.Context(), key)
	must(t, err)
	if len(got.Handovers) != 1 || got.Handovers[0].By != (tracker.User{}) {
		t.Errorf("handovers = %+v", got.Handovers)
	}
}

// An issue of a repo that is not a project of the organization is no ticket,
// and nothing of it is read: one such card on a board does not stop the rest.
func TestACardOfAForeignRepoIsNotATicket(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.gh.Install("rival/secret")
	spy := &spyIssues{Issues: e.gh}
	ad, err := githubproject.Factory(githubproject.Deps{Projects: e.board, Issues: spy, Store: e.st, Now: e.clock.Now})(domain.TrackerConnection{
		AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: map[string]any{"owner": "acme", "project": 7}, Triggers: domain.TrackerTriggers{State: "Forecrew"},
	})
	must(t, err)
	key, _ := e.card(t, "rival/secret", "Not ours", "")
	e.board.Move(key, ann, "Forecrew")
	if _, err := ad.Get(ctx, key); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Get: %v", err)
	}
	if _, err := ad.Comment(ctx, key, "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("Comment: %v", err)
	}
	e.board.Err = nil
	if spy.reads != 0 || spy.writes != 0 {
		t.Errorf("a foreign repo was reached: %d reads, %d writes", spy.reads, spy.writes)
	}
	ours, _ := e.card(t, repo, "Ours", "")
	if _, err := ad.Get(ctx, ours); err != nil {
		t.Errorf("a card of a project: %v", err)
	}
	if spy.reads == 0 {
		t.Error("the spy sees nothing")
	}
}

type spyIssues struct {
	githubproject.Issues
	reads, writes int
}

func (s *spyIssues) ListComments(ctx context.Context, repo string, number int, since time.Time) ([]github.Comment, error) {
	s.reads++
	return s.Issues.ListComments(ctx, repo, number, since)
}

func (s *spyIssues) CreateComment(ctx context.Context, repo string, number int, body string) (github.Comment, error) {
	s.writes++
	return s.Issues.CreateComment(ctx, repo, number, body)
}

// The trigger of a board is a column.
func TestVerifyRefusesTheTriggersOfAnIssue(t *testing.T) {
	e := newEnv(t)
	for name, tr := range map[string]domain.TrackerTriggers{
		"assignee": {State: "Forecrew", Assignee: "bot"}, "label": {State: "Forecrew", Label: "forecrew"}, "label only": {Label: "forecrew"},
	} {
		ad, err := e.factory()(domain.TrackerConnection{AccountID: e.account.ID, Name: "board", Kind: githubproject.Kind, Config: map[string]any{"owner": "acme", "project": 7}, Triggers: tr})
		must(t, err)
		if err := ad.Verify(t.Context()); !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func hasKey(events []tracker.Event, key string) bool {
	for _, ev := range events {
		if ev.Key == key {
			return true
		}
	}
	return false
}

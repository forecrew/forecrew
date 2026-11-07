package githubissues_test

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
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
	"github.com/forecrew/forecrew/internal/workflow"
)

const (
	repo   = "acme/app"
	secret = "hook-secret"
)

var (
	ann = github.User{ID: 11, Login: "ann"}
	bot = github.User{ID: 12, Login: "forecrew-machine"}
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type env struct {
	gh      *testkit.Tracker
	st      *testkit.Store
	account domain.Account
	ad      tracker.Adapter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := testkit.NewStore(nil)
	e := &env{gh: testkit.NewTracker(nil), st: st, account: domain.Account{Name: "default"}}
	must(t, st.Accounts().Create(t.Context(), &e.account))
	p := domain.Project{AccountID: e.account.ID, Name: "app", Repo: repo, DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	e.gh.Install(repo)
	e.ad = e.adapter(t, map[string]any{"repos": []any{repo}, "bot_login": "forecrew[bot]"})
	return e
}

func (e *env) adapter(t *testing.T, config map[string]any) tracker.Adapter {
	t.Helper()
	ad, err := e.factory()(domain.TrackerConnection{AccountID: e.account.ID, Name: "gh", Kind: githubissues.Kind, Config: config, Triggers: domain.TrackerTriggers{Assignee: bot.Login, Label: "forecrew"}})
	must(t, err)
	return ad
}

func (e *env) factory() tracker.Factory {
	return githubissues.Factory(githubissues.Deps{GitHub: e.gh, Store: e.st, Secret: []byte(secret)})
}

func (e *env) issue(t *testing.T, title, body string) int {
	t.Helper()
	i, err := e.gh.CreateIssue(t.Context(), repo, github.IssueInput{Title: title, Body: body})
	must(t, err)
	return i.Number
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

func issuePayload(fullName string, number int, sender string) map[string]any {
	return map[string]any{
		"action": "labeled", "issue": map[string]any{"number": number, "title": "A title in the payload", "labels": []any{map[string]any{"name": "forecrew"}}},
		"repository": map[string]any{"full_name": fullName}, "sender": map[string]any{"login": sender, "id": 11},
	}
}

func TestConformance(t *testing.T) {
	e := newEnv(t)
	trackertest.Run(t, trackertest.Harness{
		Adapter: e.ad,
		NewTicket: func(t *testing.T, title, body string) string {
			return repo + "#" + itoa(e.issue(t, title, body))
		},
		Touch: func(t *testing.T, key string) {
			must(t, e.gh.EditIssue(repo, number(t, key), func(i *github.Issue) { i.UpdatedAt = time.Now().UTC() }))
		},
		Signed: func(t *testing.T, key string) tracker.Request {
			return delivery(t, secret, "issues", issuePayload(repo, number(t, key), "ann"))
		},
		Forged: func(t *testing.T, key string) tracker.Request {
			return delivery(t, "wrong", "issues", issuePayload(repo, number(t, key), "ann"))
		},
	})
}

func TestFactory(t *testing.T) {
	e := newEnv(t)
	for name, config := range map[string]map[string]any{"no repos": {}, "bad type": {"repos": "acme/app"}} {
		_, err := e.factory()(domain.TrackerConnection{Name: "gh", Kind: githubissues.Kind, Config: config})
		if !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A connection cannot reach into the repos of another organization, though
// the App is installed there.
func TestVerify(t *testing.T) {
	e := newEnv(t)
	must(t, e.ad.Verify(t.Context()))
	other := domain.Account{Name: "other"}
	must(t, e.st.Accounts().Create(t.Context(), &other))
	p := domain.Project{AccountID: other.ID, Name: "secret", Repo: "rival/secret", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, e.st.Projects().Create(t.Context(), &p))
	e.gh.Install("rival/secret")
	err := e.adapter(t, map[string]any{"repos": []any{"rival/secret"}}).Verify(t.Context())
	if !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("another organization's repo: %v", err)
	}
	q := domain.Project{AccountID: e.account.ID, Name: "lib", Repo: "acme/lib", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, e.st.Projects().Create(t.Context(), &q))
	err = e.adapter(t, map[string]any{"repos": []any{"acme/lib"}}).Verify(t.Context())
	if !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("no App on the repo: %v", err)
	}
}

func TestParseEvent(t *testing.T) {
	e := newEnv(t)
	comment := func(pr bool) map[string]any {
		p := issuePayload(repo, 4, "ann")
		p["action"] = "created"
		p["comment"] = map[string]any{"body": "hi"}
		if pr {
			p["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "https://x"}
		}
		return p
	}
	for _, tt := range []struct {
		name    string
		kind    string
		payload map[string]any
		want    []tracker.Event
	}{
		{"issue", "issues", issuePayload(repo, 4, "ann"), []tracker.Event{{ID: "d-1", Key: repo + "#4"}}},
		{"comment", "issue_comment", comment(false), []tracker.Event{{ID: "d-1", Key: repo + "#4"}}},
		{"comment on a PR", "issue_comment", comment(true), nil},
		{"another repo", "issues", issuePayload("acme/lib", 4, "ann"), nil},
		{"own write", "issues", issuePayload(repo, 4, "forecrew[bot]"), []tracker.Event{{ID: "d-1", Key: repo + "#4", Self: true}}},
		{"another event", "push", map[string]any{}, nil},
	} {
		got, err := e.ad.ParseEvent(delivery(t, secret, tt.kind, tt.payload))
		must(t, err)
		if len(got) != len(tt.want) || (len(got) == 1 && got[0] != tt.want[0]) {
			t.Errorf("%s: %+v, want %+v", tt.name, got, tt.want)
		}
	}
	bad := delivery(t, secret, "issues", map[string]any{})
	bad.Body = []byte("nope")
	bad.Header.Set("X-Hub-Signature-256", sign(secret, bad.Body))
	if _, err := e.ad.ParseEvent(bad); err == nil {
		t.Error("an unreadable payload passed")
	}
}

func TestGetReadsWhoHandedItOver(t *testing.T) {
	e := newEnv(t)
	n := e.issue(t, "Crash on start", "It crashes.")
	must(t, e.gh.LabelIssue(repo, n, ann, "forecrew"))
	must(t, e.gh.AssignIssue(repo, n, ann, github.User{ID: 5, Login: "someone"}))
	must(t, e.gh.AssignIssue(repo, n, ann, bot))
	must(t, e.gh.EditIssue(repo, n, func(i *github.Issue) { i.Type, i.Author = "Bug", ann }))
	_, err := e.gh.CommentAs(repo, n, ann, "more detail")
	must(t, err)
	_, err = e.gh.CreateComment(t.Context(), repo, n, "from the App")
	must(t, err)
	got, err := e.ad.Get(t.Context(), repo+"#"+itoa(n))
	must(t, err)
	if len(got.Handovers) != 3 || got.Handovers[0].Trigger != tracker.TriggerLabel || got.Handovers[0].By.ID != "11" || got.Handovers[2].Trigger != tracker.TriggerAssignee || got.Handovers[2].Value != bot.Login {
		t.Errorf("handovers = %+v", got.Handovers)
	}
	if got.Assignee.Name != bot.Login || got.Reporter.ID != "11" || got.Type != "Bug" || got.Project != repo || got.Issue == nil || got.Issue.Number != n {
		t.Errorf("ticket = %+v", got)
	}
	if len(got.Comments) != 2 || got.Comments[0].Author.ID != "11" || !got.Comments[1].Author.Bot {
		t.Errorf("comments = %+v", got.Comments)
	}
}

func TestGetOfIssuesThatAreNotTickets(t *testing.T) {
	e := newEnv(t)
	n := e.issue(t, "GOAL-1: mirrored", "")
	must(t, e.gh.LabelIssue(repo, n, ann, "forecrew"))
	must(t, e.gh.LabelIssue(repo, n, ann, "forecrew:goal"))
	bug := e.issue(t, "A person's issue, reported with @forecrew bug", "")
	must(t, e.gh.LabelIssue(repo, bug, ann, "forecrew"))
	must(t, e.gh.LabelIssue(repo, bug, ann, "forecrew:bug"))
	for _, i := range []int{n, bug} {
		got, err := e.ad.Get(t.Context(), repo+"#"+itoa(i))
		must(t, err)
		if len(got.Handovers) != 0 {
			t.Errorf("Forecrew's own issue can be handed over: %+v", got.Handovers)
		}
	}
	pr := e.issue(t, "a pull request", "")
	must(t, e.gh.EditIssue(repo, pr, func(i *github.Issue) { i.PullRequest = true }))
	for _, key := range []string{repo + "#" + itoa(pr), "acme/lib#1", repo, repo + "#x", repo + "#0"} {
		if _, err := e.ad.Get(t.Context(), key); !errors.Is(err, tracker.ErrNotFound) {
			t.Errorf("Get(%s): %v", key, err)
		}
	}
}

func TestBugLabelIsTheType(t *testing.T) {
	e := newEnv(t)
	n := e.issue(t, "Broken", "")
	must(t, e.gh.LabelIssue(repo, n, ann, "Bug"))
	got, err := e.ad.Get(t.Context(), repo+"#"+itoa(n))
	must(t, err)
	if got.Type != "bug" {
		t.Errorf("type = %q", got.Type)
	}
}

func TestWrites(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if err := e.ad.Transition(ctx, repo+"#404", "closed"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("transition of nothing: %v", err)
	}
	if _, err := e.ad.Comment(ctx, repo+"#404", "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment on nothing: %v", err)
	}
	if _, err := e.ad.Comment(ctx, "other/repo#1", "x"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("comment outside the connection: %v", err)
	}
	if err := e.ad.Transition(ctx, "other/repo#1", "closed"); !errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("transition outside the connection: %v", err)
	}
	n := e.issue(t, "x", "")
	if err := e.ad.Transition(ctx, repo+"#"+itoa(n), "doing"); err == nil {
		t.Error("a state GitHub does not have")
	}
}

func TestChangesCursor(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	if _, _, err := e.ad.Changes(ctx, "not a time"); err == nil {
		t.Error("a bad cursor passed")
	}
	if _, _, err := e.adapter(t, map[string]any{"repos": []any{"not/installed"}}).Changes(ctx, time.Now().Format(time.RFC3339Nano)); err == nil {
		t.Error("an unreadable repo passed")
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

func number(t *testing.T, key string) int {
	t.Helper()
	n, err := strconv.Atoi(key[len(repo)+1:])
	must(t, err)
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

type runs struct{ started int }

func (r *runs) Start(_ context.Context, _ string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	r.started++
	return domain.WorkflowRun{ID: req.ID}, nil
}

func (r *runs) CancelRun(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

// A real GitHub flow through the core: the payload is believed in nothing, and
// who labeled the issue decides.
func TestTheCoreOverGitHub(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	p, err := e.st.Projects().ListByRepo(ctx, repo)
	must(t, err)
	member := e.st.AddMember(e.account.ID, domain.User{Name: "Ann"}, "")
	testkit.Bind(t, e.st, e.account.ID, domain.SubjectUser, member.ID, domain.RoleScopeOrganization, domain.RoleMember, uuid.Nil())
	conn := domain.TrackerConnection{AccountID: e.account.ID, Name: "issues", Kind: githubissues.Kind, Config: map[string]any{"repos": []any{repo}}, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	must(t, e.st.TrackerConnections().Create(ctx, &conn))
	must(t, e.st.TrackerRoutes().Create(ctx, &domain.TrackerRoute{AccountID: e.account.ID, ConnectionID: conn.ID, ProjectID: p[0].ID, Match: domain.TrackerMatch{Project: repo}}))
	must(t, e.st.TrackerIdentities().Create(ctx, &domain.TrackerIdentity{AccountID: e.account.ID, ConnectionID: conn.ID, ExternalID: "11", UserID: member.ID, CreatedBy: "user:x"}))
	registry := tracker.NewRegistry()
	registry.Register(githubissues.Kind, e.factory())
	started := &runs{}
	svc := tracker.New(tracker.Config{
		Store: e.st, Registry: registry, Gates: testkit.NewGates(e.st, testkit.NewSignalBus(e.st), nil), Authz: authz.New(e.st, nil), Runs: started,
		Playbook: func(domain.Project, domain.GoalKind) string { return "goal" },
	})
	n := e.issue(t, "Add export", "Export it all.")

	// Signed, says the issue is labeled and by whom; on GitHub it is not.
	must(t, svc.Deliver(ctx, githubissues.Kind, delivery(t, secret, "issues", issuePayload(repo, n, "ann"))))
	svc.Drain(ctx)
	if started.started != 0 {
		t.Fatal("a payload started work")
	}
	if err := svc.Deliver(ctx, githubissues.Kind, delivery(t, "wrong", "issues", issuePayload(repo, n, "ann"))); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("forged: %v", err)
	}

	must(t, e.gh.LabelIssue(repo, n, ann, "forecrew"))
	labeled := delivery(t, secret, "issues", issuePayload(repo, n, "ann"))
	labeled.Header.Set("X-GitHub-Delivery", "d-2")
	must(t, svc.Deliver(ctx, githubissues.Kind, labeled))
	must(t, svc.Deliver(ctx, githubissues.Kind, labeled)) // redelivered
	svc.Drain(ctx)
	if started.started != 1 {
		t.Fatalf("runs = %d", started.started)
	}
	goals, err := e.st.Goals().List(ctx, e.account.ID, store.GoalFilter{ProjectID: p[0].ID})
	must(t, err)
	if len(goals) != 1 || goals[0].IssueNumber != n || goals[0].Title != "Add export" {
		t.Fatalf("goals = %+v", goals)
	}
	must(t, svc.Sync(ctx))
	comments, err := e.gh.ListComments(ctx, repo, n, time.Time{})
	must(t, err)
	if len(comments) != 1 || !strings.Contains(comments[0].Body, goals[0].Ref()) {
		t.Errorf("comments = %+v", comments)
	}
}

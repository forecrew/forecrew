//go:build integration

package postgres

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func TestMigrate(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	must(t, Migrate(ctx, pool)) // second run is a no-op
	var versions, accounts int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&versions))
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&accounts))
	if versions != 4 || accounts != 1 {
		t.Fatalf("versions = %d, accounts = %d", versions, accounts)
	}
}

func TestAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &a))
	got, err := f.st.Accounts().Get(ctx, a.ID)
	if err != nil || got.Name != "other" || got.CreatedAt.IsZero() {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := f.st.Accounts().List(ctx)
	if err != nil || len(list) != 2 || list[1].ID != a.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_, err = f.st.Accounts().Get(ctx, domain.NewID())
	wantErr(t, err, store.ErrNotFound)
}

func TestProjects(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Projects()

	p := f.project
	if p.Status != domain.ProjectRegistered || p.DefaultBranch != "main" || p.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", p)
	}
	dup := domain.Project{AccountID: f.account, Name: "other", Repo: "ACME/FORECREW"}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)
	dup = domain.Project{AccountID: f.account, Name: "forecrew", Repo: "acme/x"}
	wantErr(t, repo.Create(ctx, &dup), store.ErrConflict)
	orphan := domain.Project{AccountID: domain.NewID(), Name: "x", Repo: "a/b"}
	wantErr(t, repo.Create(ctx, &orphan), store.ErrNotFound)

	got, err := repo.Get(ctx, f.account, p.ID)
	if err != nil || got.Repo != "Acme/forecrew" || got.Profile != nil || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("get = %+v, %v", got, err)
	}
	for _, q := range []string{"forecrew", "FORECREW", "acme/forecrew"} {
		if got, err := repo.GetByName(ctx, f.account, q); err != nil || got.ID != p.ID {
			t.Fatalf("GetByName(%q) = %+v, %v", q, got, err)
		}
	}
	_, err = repo.GetByName(ctx, f.account, "nope")
	wantErr(t, err, store.ErrNotFound)

	// A second account may register the same repo; ListByRepo sees both.
	other := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &other))
	f.newProject(t, other.ID, "forecrew")
	byRepo, err := repo.ListByRepo(ctx, "acme/FORECREW")
	if err != nil || len(byRepo) != 2 {
		t.Fatalf("ListByRepo = %+v, %v", byRepo, err)
	}
	list, err := repo.List(ctx, f.account)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	must(t, repo.UpdateStatus(ctx, f.account, p.ID, domain.ProjectRegistered, domain.ProjectOnboarding))
	wantErr(t, repo.UpdateStatus(ctx, f.account, p.ID, domain.ProjectRegistered, domain.ProjectOnboarded), domain.ErrInvalidTransition)
	wantErr(t, repo.UpdateStatus(ctx, f.account, p.ID, domain.ProjectRegistered, domain.ProjectOnboarding), store.ErrConflict)
	wantErr(t, repo.UpdateStatus(ctx, f.account, domain.NewID(), domain.ProjectRegistered, domain.ProjectOnboarding), store.ErrNotFound)

	profile := json.RawMessage(`{"name": "forecrew"}`)
	must(t, repo.SetProfile(ctx, f.account, p.ID, profile))
	got, _ = repo.Get(ctx, f.account, p.ID)
	if got.Status != domain.ProjectOnboarding || string(got.Profile) != `{"name": "forecrew"}` || !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("after updates = %+v", got)
	}
	must(t, repo.SetProfile(ctx, f.account, p.ID, nil))
	wantErr(t, repo.SetProfile(ctx, f.account, domain.NewID(), profile), store.ErrNotFound)
}

func TestGoals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Goals()
	g := f.newGoal(t)
	g2 := domain.Goal{AccountID: f.account, ProjectID: f.project.ID, Title: "Two", IssueNumber: 7, CreatedBy: "user:me", Status: domain.GoalPlanned, Platforms: []string{"Web", "ios", "web"}}
	must(t, repo.Create(ctx, &g2))
	if g.Number != 1 || g2.Number != 2 || g.Status != domain.GoalDraft || g.Ref() != "GOAL-1" {
		t.Fatalf("goals = %+v %+v", g, g2)
	}
	got, err := repo.GetByNumber(ctx, f.account, 2)
	if err != nil || got.ID != g2.ID || got.IssueNumber != 7 || got.Status != domain.GoalPlanned || !slices.Equal(got.Platforms, []string{"ios", "web"}) {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, f.account, g.ID); err != nil {
		t.Fatal(err)
	}
	_, err = repo.GetByNumber(ctx, f.account, 9)
	wantErr(t, err, store.ErrNotFound)

	list, err := repo.List(ctx, f.account, store.GoalFilter{ProjectID: f.project.ID})
	if err != nil || len(list) != 2 || list[0].ID != g.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, store.GoalFilter{Status: domain.GoalPlanned})
	if len(list) != 1 || list[0].ID != g2.ID {
		t.Fatalf("by status = %+v", list)
	}
	list, _ = repo.List(ctx, f.account, store.GoalFilter{Limit: 1})
	if len(list) != 1 {
		t.Fatalf("limit = %+v", list)
	}
	list, _ = repo.List(ctx, f.account, store.GoalFilter{Status: domain.GoalDone})
	if list == nil || len(list) != 0 {
		t.Fatalf("empty list = %#v", list)
	}

	g.Title, g.IssueNumber = "Renamed", 12
	must(t, repo.Update(ctx, &g))
	got, _ = repo.Get(ctx, f.account, g.ID)
	if got.Title != "Renamed" || got.IssueNumber != 12 {
		t.Fatalf("updated = %+v", got)
	}
	g.IssueNumber = 0
	must(t, repo.Update(ctx, &g))
	got, _ = repo.Get(ctx, f.account, g.ID)
	if got.IssueNumber != 0 {
		t.Fatalf("issue = %d", got.IssueNumber)
	}
	missing := domain.Goal{AccountID: f.account, ID: domain.NewID()}
	wantErr(t, repo.Update(ctx, &missing), store.ErrNotFound)

	must(t, repo.UpdateStatus(ctx, f.account, g.ID, domain.GoalDraft, domain.GoalSpecReview))
	wantErr(t, repo.UpdateStatus(ctx, f.account, g.ID, domain.GoalSpecReview, domain.GoalDone), domain.ErrInvalidTransition)
	wantErr(t, repo.UpdateStatus(ctx, f.account, g.ID, domain.GoalDraft, domain.GoalSpecReview), store.ErrConflict)
}

func TestSpecsAndTasks(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t)
	s1 := f.newSpec(t, g, 1)
	s2 := f.newSpec(t, g, 2)
	if s1.Number != 1 || s2.Number != 2 || s2.Ref() != "SPEC-2" {
		t.Fatalf("specs = %+v %+v", s1, s2)
	}
	dup := domain.Spec{AccountID: f.account, ProjectID: f.project.ID, GoalID: g.ID, Revision: 2, Body: "x"}
	wantErr(t, f.st.Specs().Create(ctx, &dup), store.ErrConflict)

	got, err := f.st.Specs().Get(ctx, f.account, s1.ID)
	if err != nil || got.Body != "spec" || got.JobID != uuid.Nil() {
		t.Fatalf("get = %+v, %v", got, err)
	}
	all, err := f.st.Specs().ListByGoal(ctx, f.account, g.ID)
	if err != nil || len(all) != 2 || all[0].Revision != 1 {
		t.Fatalf("list = %+v, %v", all, err)
	}
	latest, err := f.st.Specs().Latest(ctx, f.account, g.ID)
	if err != nil || latest.ID != s2.ID {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	_, err = f.st.Specs().Latest(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)

	repo := f.st.Tasks()
	t2 := f.newTask(t, g, s2, 2)
	t1 := f.newTask(t, g, s2, 1)
	if t2.Number != 1 || t1.Number != 2 || t1.Status != domain.TaskOpen || t1.Ref() != "TASK-2" {
		t.Fatalf("tasks = %+v %+v", t1, t2)
	}
	list, err := repo.List(ctx, f.account, store.TaskFilter{GoalID: g.ID, SpecID: s2.ID, ProjectID: f.project.ID})
	if err != nil || len(list) != 2 || list[0].ID != t1.ID {
		t.Fatalf("list orders by seq: %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, store.TaskFilter{Status: domain.TaskClosed})
	if len(list) != 0 {
		t.Fatalf("by status = %+v", list)
	}
	if got, err := repo.GetByNumber(ctx, f.account, 2); err != nil || got.ID != t1.ID {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	_, err = repo.Get(ctx, f.account, domain.NewID())
	wantErr(t, err, store.ErrNotFound)

	t1.Title, t1.Body, t1.Seq, t1.IssueNumber, t1.Branch = "New", "body", 5, 33, "agent/task-2"
	must(t, repo.Update(ctx, &t1))
	got2, err := repo.Get(ctx, f.account, t1.ID)
	if err != nil || got2.Title != "New" || got2.Seq != 5 || got2.IssueNumber != 33 || got2.Branch != "agent/task-2" {
		t.Fatalf("updated = %+v, %v", got2, err)
	}
	byIssue, err := repo.GetByIssue(ctx, f.account, f.project.ID, 33)
	if err != nil || byIssue.ID != t1.ID {
		t.Fatalf("by issue = %+v, %v", byIssue, err)
	}
	_, err = repo.GetByIssue(ctx, f.account, f.project.ID, 34)
	wantErr(t, err, store.ErrNotFound)
	dupIssue := t2
	dupIssue.ID, dupIssue.IssueNumber = uuid.Nil(), 33
	wantErr(t, repo.Create(ctx, &dupIssue), store.ErrConflict)
	must(t, repo.Update(ctx, &t2))
	missing := domain.Task{AccountID: f.account, ID: domain.NewID()}
	wantErr(t, repo.Update(ctx, &missing), store.ErrNotFound)

	must(t, repo.UpdateStatus(ctx, f.account, t1.ID, domain.TaskOpen, domain.TaskInProgress))
	wantErr(t, repo.UpdateStatus(ctx, f.account, t1.ID, domain.TaskInProgress, domain.TaskMerged), domain.ErrInvalidTransition)
	wantErr(t, repo.UpdateStatus(ctx, f.account, t1.ID, domain.TaskOpen, domain.TaskInProgress), store.ErrConflict)
}

func TestChanges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := f.st.Changes()
	g := f.newGoal(t)
	task := f.newTask(t, g, f.newSpec(t, g, 1), 1)

	c := domain.Change{AccountID: f.account, ProjectID: f.project.ID, TaskID: task.ID, Repo: "Acme/Forecrew", Number: 4,
		Branch: "agent/task-1", BaseRef: "main", HeadSHA: "aaa", State: domain.ChangeOpen, URL: "https://x/4"}
	must(t, repo.Upsert(ctx, &c))
	again := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 4,
		Branch: "agent/task-1", BaseRef: "main", HeadSHA: "bbb", State: domain.ChangeMerged, URL: "https://x/4"}
	must(t, repo.Upsert(ctx, &again))
	if again.ID != c.ID || again.TaskID != task.ID || again.HeadSHA != "bbb" || again.State != domain.ChangeMerged || !again.UpdatedAt.After(c.CreatedAt) {
		t.Fatalf("upsert = %+v (first %+v)", again, c)
	}
	other := domain.Change{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/forecrew", Number: 5, Branch: "b", BaseRef: "main", HeadSHA: "c", State: domain.ChangeOpen, URL: "u"}
	must(t, repo.Upsert(ctx, &other))

	got, err := repo.Get(ctx, f.account, c.ID)
	if err != nil || got.HeadSHA != "bbb" || got.Repo != "Acme/Forecrew" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	byPR, err := repo.GetByPR(ctx, f.account, "ACME/forecrew", 5)
	if err != nil || byPR.ID != other.ID || byPR.TaskID != uuid.Nil() {
		t.Fatalf("by PR = %+v, %v", byPR, err)
	}
	_, err = repo.GetByPR(ctx, f.account, "acme/forecrew", 99)
	wantErr(t, err, store.ErrNotFound)
	list, err := repo.List(ctx, f.account, store.ChangeFilter{ProjectID: f.project.ID})
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	list, _ = repo.List(ctx, f.account, store.ChangeFilter{TaskID: task.ID, State: domain.ChangeMerged, Limit: 5})
	if len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("filtered = %+v", list)
	}
}

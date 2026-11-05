package tools_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

const verifiedSHA = "abc1234"

// newTaskWithReports stores a task with two pull requests, the second one
// newer, and a verification report for the first.
func (f *fixture) newTaskWithReports(t *testing.T) (domain.Task, domain.Change, domain.Change) {
	t.Helper()
	ctx := t.Context()
	g := f.newGoal(t, "Login")
	spec := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1, Body: "v1"}
	must(t, f.st.Specs().Create(ctx, &spec))
	task := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, SpecID: spec.ID, Seq: 1, Title: "task", Status: domain.TaskInReview}
	must(t, f.st.Tasks().Create(ctx, &task))
	first := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: task.ID, Repo: "acme/app", Number: 8, Branch: "a", HeadSHA: verifiedSHA, State: domain.ChangeClosed, URL: "https://github.com/acme/app/pull/8"}
	must(t, f.st.Changes().Upsert(ctx, &first))
	f.clock.Advance(time.Minute)
	second := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: task.ID, Repo: "acme/app", Number: 9, Branch: "b", HeadSHA: "def5678", State: domain.ChangeOpen, URL: "https://github.com/acme/app/pull/9"}
	must(t, f.st.Changes().Upsert(ctx, &second))
	return task, first, second
}

func (f *fixture) storeReport(t *testing.T, c domain.Change, sha string, status domain.VerificationStatus) domain.VerificationReport {
	t.Helper()
	raw := `{"version":1,"sha":"` + sha + `","status":"` + string(status) + `","results":[],"criteria":{"AC-1":{"status":"untested","tests":{}}},"new_tests":{},"flaky":[],"environments":{},` +
		`"blocking":` + map[bool]string{true: `["AC-1 has no test"]`, false: `[]`}[status != domain.VerificationPassed] + `}`
	f.clock.Advance(time.Minute)
	r := domain.VerificationReport{AccountID: f.account.ID, ProjectID: f.project.ID, ChangeID: c.ID, SHA: sha, Status: status, Report: []byte(raw)}
	must(t, f.st.VerificationReports().Upsert(t.Context(), &r))
	return r
}

func TestVerificationReport(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task, first, second := f.newTaskWithReports(t)
	stale := f.storeReport(t, first, verifiedSHA, domain.VerificationFailed)

	// By number and project, the report of the head commit.
	got, err := f.svc.VerificationReport(ctx, f.by, mcp.VerificationReportInput{Change: "8", Project: "app"})
	if err != nil || got.ID != stale.ID || got.Change != 8 || got.URL != first.URL || !got.Current || got.Status != domain.VerificationFailed {
		t.Fatalf("by number = %+v, %v", got, err)
	}
	if !strings.Contains(got.Untrusted, "<untrusted-data") || !strings.Contains(got.Untrusted, "AC-1 has no test") {
		t.Errorf("untrusted = %q", got.Untrusted)
	}

	// The newest pull request of the task has no report: not found.
	for name, in := range map[string]mcp.VerificationReportInput{
		"task ref":   {Change: task.Ref()},
		"task uuid":  {Change: task.ID.String(), Project: "app"},
		"number":     {Change: "9", Project: "app"},
		"URL":        {Change: second.URL},
		"URL http":   {Change: "http://github.com/acme/app/pull/9/"},
		"spaces":     {Change: " 9 ", Project: "app"},
		"task+proj.": {Change: task.Ref(), Project: "acme/app"},
	} {
		_, err := f.svc.VerificationReport(ctx, f.by, in)
		wantErr(t, err, mcp.ErrNotFound)
		if err != nil && !strings.Contains(err.Error(), "no verification report yet") {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// An earlier head's report is returned as not current.
	f.storeReport(t, second, "0000000", domain.VerificationIncomplete)
	got, err = f.svc.VerificationReport(ctx, f.by, mcp.VerificationReportInput{Change: task.Ref()})
	if err != nil || got.Change != 9 || got.Current || got.SHA != "0000000" || got.Status != domain.VerificationIncomplete {
		t.Fatalf("stale = %+v, %v", got, err)
	}
	cur := f.storeReport(t, second, "def5678", domain.VerificationPassed)
	got, err = f.svc.VerificationReport(ctx, f.by, mcp.VerificationReportInput{Change: second.URL})
	if err != nil || got.ID != cur.ID || !got.Current {
		t.Fatalf("current = %+v, %v", got, err)
	}
}

func TestVerificationReportErrors(t *testing.T) {
	f := newFixture(t)
	task, _, _ := f.newTaskWithReports(t)
	other := domain.Project{AccountID: f.account.ID, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(t.Context(), &other))
	empty := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: task.GoalID, SpecID: task.SpecID, Seq: 2, Title: "t", Status: domain.TaskOpen}
	must(t, f.st.Tasks().Create(t.Context(), &empty))
	tests := []struct {
		name string
		in   mcp.VerificationReportInput
		want error
	}{
		{"empty", mcp.VerificationReportInput{}, mcp.ErrInvalidInput},
		{"number without project", mcp.VerificationReportInput{Change: "8"}, mcp.ErrInvalidInput},
		{"unknown project", mcp.VerificationReportInput{Change: "8", Project: "nope"}, mcp.ErrNotFound},
		{"unknown number", mcp.VerificationReportInput{Change: "99", Project: "app"}, mcp.ErrNotFound},
		{"number of another project", mcp.VerificationReportInput{Change: "8", Project: "other"}, mcp.ErrNotFound},
		{"bad URL", mcp.VerificationReportInput{Change: "https://github.com/acme/app/issues/9"}, mcp.ErrInvalidInput},
		{"URL of another scheme", mcp.VerificationReportInput{Change: "ftp://github.com/acme/app/pull/9"}, mcp.ErrInvalidInput},
		{"unknown pull request URL", mcp.VerificationReportInput{Change: "https://github.com/acme/app/pull/99"}, mcp.ErrNotFound},
		{"bad ref", mcp.VerificationReportInput{Change: "later"}, mcp.ErrInvalidInput},
		{"unknown task", mcp.VerificationReportInput{Change: "TASK-99"}, mcp.ErrNotFound},
		{"task without a pull request", mcp.VerificationReportInput{Change: empty.Ref()}, mcp.ErrNotFound},
		{"task of another project", mcp.VerificationReportInput{Change: task.Ref(), Project: "other"}, mcp.ErrNotFound},
		{"task with an unknown project", mcp.VerificationReportInput{Change: task.Ref(), Project: "nope"}, mcp.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.VerificationReport(t.Context(), f.by, tt.in)
			wantErr(t, err, tt.want)
		})
	}
}

func TestVerificationReportOfAnotherAccount(t *testing.T) {
	f := newFixture(t)
	_, first, _ := f.newTaskWithReports(t)
	f.storeReport(t, first, verifiedSHA, domain.VerificationPassed)
	by := domain.Actor{AccountID: uuid.New(), Kind: domain.ActorMCP, Name: "x"}
	_, err := f.svc.VerificationReport(t.Context(), by, mcp.VerificationReportInput{Change: first.URL})
	wantErr(t, err, mcp.ErrNotFound)
}

func TestVerificationReportRejectsAStoredReportThatIsInvalid(t *testing.T) {
	f := newFixture(t)
	_, first, _ := f.newTaskWithReports(t)
	r := domain.VerificationReport{AccountID: f.account.ID, ProjectID: f.project.ID, ChangeID: first.ID, SHA: verifiedSHA, Status: domain.VerificationPassed, Report: []byte(`{"version":9}`)}
	must(t, f.st.VerificationReports().Upsert(t.Context(), &r))
	if _, err := f.svc.VerificationReport(t.Context(), f.by, mcp.VerificationReportInput{Change: first.URL}); err == nil || strings.Contains(err.Error(), store.ErrNotFound.Error()) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetGoalLinksTheVerification(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task, first, second := f.newTaskWithReports(t)
	goal, err := f.st.Goals().Get(ctx, f.account.ID, task.GoalID)
	must(t, err)
	d, err := f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: goal.Ref()})
	must(t, err)
	if d.Tasks[0].Verification != nil {
		t.Fatalf("a task without reports has %+v", d.Tasks[0].Verification)
	}
	f.storeReport(t, first, verifiedSHA, domain.VerificationFailed)
	f.storeReport(t, second, "0000000", domain.VerificationIncomplete)
	d, err = f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: goal.Ref()})
	must(t, err)
	v := d.Tasks[0].Verification
	if v == nil || v.Status != domain.VerificationIncomplete || v.SHA != "0000000" || v.Current || v.URL != "/api/v1/changes/"+task.Ref()+"/verification" {
		t.Fatalf("verification = %+v", v)
	}
	f.storeReport(t, second, "def5678", domain.VerificationPassed)
	d, err = f.svc.GetGoal(ctx, f.by, mcp.GoalRefInput{Goal: goal.Ref()})
	must(t, err)
	if v := d.Tasks[0].Verification; v == nil || v.Status != domain.VerificationPassed || !v.Current {
		t.Fatalf("verification = %+v", v)
	}
}

// TestVerificationReportChecksTheProject: every way to name a change resolves
// to its project, and the caller needs job.read there.
func TestVerificationReportChecksTheProject(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task, first, _ := f.newTaskWithReports(t)
	f.storeReport(t, first, verifiedSHA, domain.VerificationFailed)
	second := f.secondProject(t)
	elsewhere, _ := f.member(t, projectScope, domain.RoleViewer, second.ID)
	role := domain.Role{AccountID: f.account.ID, Name: "goals", Scope: projectScope, Permissions: []domain.Permission{domain.PermGoalRead}, Custom: true}
	must(t, f.st.Roles().Create(ctx, &role))
	noJobs, id := f.member(t, "", "", uuid.Nil())
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectUser, SubjectID: id, RoleID: role.ID, ProjectID: f.project.ID, CreatedBy: "user:op"}))
	viewer, _ := f.member(t, projectScope, domain.RoleViewer, f.project.ID)

	for name, in := range map[string]mcp.VerificationReportInput{
		"number": {Change: "8", Project: "app"},
		"url":    {Change: first.URL},
	} {
		for who, by := range map[string]domain.Actor{"another project": elsewhere, "no job.read": noJobs} {
			_, err := f.svc.VerificationReport(ctx, by, in)
			if !errors.Is(err, authz.ErrDenied) {
				t.Errorf("%s, %s: err = %v", name, who, err)
			}
		}
		if got, err := f.svc.VerificationReport(ctx, viewer, in); err != nil || got.Change != 8 {
			t.Errorf("%s: viewer = %+v, %v", name, got, err)
		}
	}
	// A task ref means its newest pull request, which has no report here: the
	// refusal comes first, so it does not tell.
	_, err := f.svc.VerificationReport(ctx, elsewhere, mcp.VerificationReportInput{Change: task.Ref()})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.VerificationReport(ctx, viewer, mcp.VerificationReportInput{Change: task.Ref()})
	wantErr(t, err, mcp.ErrNotFound)
}

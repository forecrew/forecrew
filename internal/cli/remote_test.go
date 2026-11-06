package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

const apiToken = "fcm_cli-test"

type remoteEnv struct {
	svc    *testkit.ToolService
	server *httptest.Server
	env    Env
}

func newRemoteEnv(t *testing.T) *remoteEnv {
	t.Helper()
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	if err := st.Accounts().Create(t.Context(), &acc); err != nil {
		t.Fatal(err)
	}
	if err := st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: acc.ID, Name: "cli", TokenHash: mcp.HashToken(apiToken)}); err != nil {
		t.Fatal(err)
	}
	e := &remoteEnv{svc: &testkit.ToolService{}}
	handler := api.New(api.Config{Tools: e.svc, Artifacts: e.svc, Tokens: st.MCPTokens(), Accounts: st.Accounts(), Authz: authz.New(st, nil), Directory: st.Directory(), Logger: slog.New(slog.DiscardHandler)}).Handler()
	e.server = httptest.NewServer(handler)
	t.Cleanup(e.server.Close)
	vars := map[string]string{EnvURL: e.server.URL, EnvToken: apiToken}
	e.env = Env{Getenv: func(k string) string { return vars[k] }, HTTP: e.server.Client()}
	return e
}

func (e *remoteEnv) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunWith(t.Context(), args, &out, &errOut, "test", e.env)
	return out.String(), errOut.String(), code
}

func TestWorkersCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListWorkersFunc = func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error) {
		return []mcp.WorkerInfo{
			{Name: "build-1", Pool: "build", Version: "v1", Labels: []string{"linux", "runtime:codex"}, Online: true, RunningJobs: []string{"JOB-3", "JOB-4"}},
			{Name: "mac", Pool: "control"},
			{Name: "old", RevokedAt: time.Now()},
		}, nil
	}
	out, errOut, code := e.run(t, "workers")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"NAME", "NAME     POOL     STATUS", "build-1  build    online   v1       linux,runtime:codex  JOB-3,JOB-4",
		"mac      control  offline  -", "old      build    revoked"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestOnboardCommand(t *testing.T) {
	e := newRemoteEnv(t)
	run := domain.NewID()
	e.svc.OnboardProjectFunc = func(_ context.Context, _ domain.Actor, in mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error) {
		return mcp.OnboardProjectOutput{Project: domain.Project{Repo: in.Repo}, RunID: run}, nil
	}
	out, errOut, code := e.run(t, "onboard", "acme/app", "--name", "App", "--default-branch", "trunk")
	if code != 0 || errOut != "" || out != "Onboarding of acme/app started (run "+run.String()+")\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if in := e.svc.Calls()[0].Input.(mcp.OnboardProjectInput); in != (mcp.OnboardProjectInput{Repo: "acme/app", Name: "App", DefaultBranch: "trunk"}) {
		t.Errorf("input = %+v", in)
	}
	out, _, code = e.run(t, "onboard", "acme/app", "--refresh")
	if code != 0 || !strings.HasPrefix(out, "Refresh of the tests of acme/app started") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	if in := e.svc.Calls()[1].Input.(mcp.OnboardProjectInput); in != (mcp.OnboardProjectInput{Repo: "acme/app", Refresh: true}) {
		t.Errorf("input = %+v", in)
	}
	for _, args := range [][]string{{"onboard"}, {"onboard", "a", "b"}} {
		if _, _, code := e.run(t, args...); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
	e.svc.OnboardProjectFunc = func(context.Context, domain.Actor, mcp.OnboardProjectInput) (mcp.OnboardProjectOutput, error) {
		return mcp.OnboardProjectOutput{}, fmt.Errorf("project app is registered: %w", mcp.ErrInvalidInput)
	}
	if _, errOut, code := e.run(t, "onboard", "acme/app", "--refresh"); code == 0 || !strings.Contains(errOut, "registered") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestProjectsCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListProjectsFunc = func(context.Context, domain.Actor, any) ([]domain.Project, error) {
		return []domain.Project{{Name: "app", Repo: "acme/app", Status: domain.ProjectOnboarded}}, nil
	}
	out, _, code := e.run(t, "projects")
	if code != 0 || !strings.Contains(out, "app   acme/app  onboarded") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
	e.svc.ListProjectsFunc = nil
	if out, _, _ := e.run(t, "projects"); strings.Count(out, "\n") != 1 {
		t.Errorf("empty list prints only the header:\n%s", out)
	}
}

func TestGatesCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListPendingGatesFunc = func(context.Context, domain.Actor, mcp.ProjectFilterInput) ([]mcp.GateInfo, error) {
		return []mcp.GateInfo{
			{Kind: domain.GateMergeApproval, Title: "Merge PR 9", Ref: "GATE-7", Project: "app", GoalRef: "GOAL-1", TaskRef: "TASK-2", URL: "https://github.com/acme/app/pull/9"},
			{Kind: domain.GateSpecApproval, Title: "Spec", Ref: "GATE-8", Project: "app", GoalRef: "GOAL-1"},
			{Kind: domain.GateNeedsYou, Title: "Stuck", Ref: "GATE-9", Project: "app"},
		}, nil
	}
	out, _, code := e.run(t, "gates")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"GATE-7  merge_approval  app      TASK-2  Merge PR 9  https://github.com/acme/app/pull/9", "GATE-8  spec_approval   app      GOAL-1  Spec        -", "GATE-9  needs_you       app      -       Stuck       -"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestApproveAndChanges(t *testing.T) {
	e := newRemoteEnv(t)
	decided := func(status domain.GateStatus) func(context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error) {
		return func(_ context.Context, _ domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
			return domain.Gate{Number: 7, Status: status, Feedback: in.Feedback}, nil
		}
	}
	e.svc.ApproveFunc = decided(domain.GateApproved)
	e.svc.RequestChangesFunc = decided(domain.GateChangesRequested)

	if out, _, code := e.run(t, "approve", "GATE-7"); code != 0 || out != "GATE-7 is approved\n" {
		t.Errorf("approve: code %d, %q", code, out)
	}
	if out, _, code := e.run(t, "approve", "GATE-7", "--feedback", "ship it"); code != 0 || out != "GATE-7 is approved\n" {
		t.Errorf("approve with feedback: code %d, %q", code, out)
	}
	if out, _, code := e.run(t, "changes", "GATE-7", "add", "more tests"); code != 0 || out != "GATE-7 is changes_requested\n" {
		t.Errorf("changes: code %d, %q", code, out)
	}
	calls := e.svc.Calls()
	want := []mcp.GateDecisionInput{{Gate: "GATE-7"}, {Gate: "GATE-7", Feedback: "ship it"}, {Gate: "GATE-7", Feedback: "add more tests"}}
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	for i, c := range calls {
		if c.Input != want[i] || c.Actor.Name != "cli" {
			t.Errorf("call %d = %+v, want input %+v by cli", i, c, want[i])
		}
	}
	e.svc.RejectFunc = decided(domain.GateRejected)
	if out, _, code := e.run(t, "reject", "GATE-7", "--feedback", "not now"); code != 0 || out != "GATE-7 is rejected\n" {
		t.Errorf("reject: code %d, %q", code, out)
	}
	if got := e.svc.Calls(); len(got) != 4 || got[3].Tool != mcp.ToolReject || got[3].Input != (mcp.GateDecisionInput{Gate: "GATE-7", Feedback: "not now"}) {
		t.Errorf("reject call = %+v", got)
	}
	if _, _, code := e.run(t, "changes", "GATE-7"); code != 1 {
		t.Error("changes needs feedback")
	}
}

func TestJobsShow(t *testing.T) {
	e := newRemoteEnv(t)
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	e.svc.JobStatusFunc = func(_ context.Context, _ domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{
			Role: "developer", Status: domain.JobFailed, Attempts: 3, MaxAttempts: 3, CostUSD: 1.5, CreatedAt: created, FinishedAt: created.Add(time.Hour),
			Spec: domain.JobSpec{Branch: "agent/task-1"},
			Ref:  in.Job, Reason: "attempts exhausted",
		}, nil
	}
	out, _, code := e.run(t, "jobs", "show", "JOB-41")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"Job:       JOB-41", "Status:    failed", "Reason:    attempts exhausted", "Worker:    -", "Attempts:  3 of 3", "Branch:    agent/task-1", "Cost:      $1.50", "Created:   2026-03-01 12:00:00 UTC", "Finished:  2026-03-01 13:00:00 UTC"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Ref: "JOB-2"}, nil
	}
	if out, _, _ := e.run(t, "jobs", "show", "JOB-2"); strings.Contains(out, "Finished") {
		t.Errorf("unfinished job shows a finish time:\n%s", out)
	}
}

func TestJobsCancel(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.CancelJobFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Ref: "JOB-41", Status: domain.JobCanceled}, nil
	}
	out, errOut, code := e.run(t, "jobs", "cancel", "JOB-41")
	if code != 0 || out != "JOB-41 is canceled\n" || errOut != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if calls := e.svc.Calls(); len(calls) != 1 || calls[0].Tool != mcp.ToolCancelJob || calls[0].Input != (mcp.JobRefInput{Job: "JOB-41"}) {
		t.Errorf("calls = %+v", calls)
	}
	e.svc.CancelJobFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{}, fmt.Errorf("job: %w", domain.ErrInvalidTransition)
	}
	if _, errOut, code := e.run(t, "jobs", "cancel", "JOB-41"); code != 1 || !strings.Contains(errOut, "invalid state transition") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestTokensCommand(t *testing.T) {
	e := newRemoteEnv(t)
	used := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	id := domain.NewID()
	e.svc.ListTokensFunc = func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error) {
		return []mcp.TokenInfo{
			{ID: id, Name: "laptop", Current: true, LastUsedAt: used},
			{Name: "ci"},
			{Name: "old", RevokedAt: used},
		}, nil
	}
	out, errOut, code := e.run(t, "tokens")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"NAME", "laptop  " + id.String() + "  active (this one)  2026-03-01T12:00:00Z", "ci      ", "active             -", "old     ", "revoked            -"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	e.svc.RevokeTokenFunc = func(_ context.Context, _ domain.Actor, in mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
		return mcp.NewRevokeTokenOutput(mcp.TokenInfo{Name: in.Token}, in.Token == "laptop"), nil
	}
	if out, _, code := e.run(t, "tokens", "revoke", "ci"); code != 0 || out != "Token ci revoked.\n" {
		t.Errorf("code %d, stdout %q", code, out)
	}
	if out, _, code := e.run(t, "tokens", "revoke", "laptop"); code != 0 || !strings.Contains(out, "token you are using") {
		t.Errorf("code %d, stdout %q", code, out)
	}
	e.svc.RevokeTokenFunc = func(context.Context, domain.Actor, mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
		return mcp.RevokeTokenOutput{}, fmt.Errorf("token ghost does not exist: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "tokens", "revoke", "ghost"); code != 1 || !strings.Contains(errOut, "does not exist") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestRemoteErrors(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{}, fmt.Errorf("job JOB-9: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "jobs", "show", "JOB-9"); code != 1 || errOut != "forecrew: job JOB-9: not found\n" {
		t.Errorf("not found: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "workers", "--token", "fcm_wrong"); code != 1 || !strings.Contains(errOut, "unauthorized: check the token") {
		t.Errorf("wrong token: code %d, %q", code, errOut)
	}
	if n := len(e.svc.Calls()); n != 1 {
		t.Errorf("service called %d times, want 1", n)
	}

	noToken := e.env
	noToken.Getenv = func(k string) string {
		if k == EnvToken {
			return ""
		}
		return e.env.Getenv(k)
	}
	var errOut bytes.Buffer
	if code := RunWith(t.Context(), []string{"workers"}, &bytes.Buffer{}, &errOut, "test", noToken); code != 1 || !strings.Contains(errOut.String(), "no token") {
		t.Errorf("no token: code %d, %q", code, errOut.String())
	}

	if _, errOut, code := e.run(t, "workers", "--url", "http://127.0.0.1:1"); code != 1 || !strings.Contains(errOut, "call forecrewd") {
		t.Errorf("unreachable: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "workers", "--url", "://bad"); code != 1 || !strings.Contains(errOut, "bad --url") {
		t.Errorf("bad url: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "workers", "--url", "forecrew.example.com"); code != 1 || !strings.Contains(errOut, "bad --url") {
		t.Errorf("url without scheme: code %d, %q", code, errOut)
	}
}

func TestRemoteUnexpectedAnswers(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"html error page", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
		}, "forecrewd answered 502 Bad Gateway"},
		{"not json", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }, "decode answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			env := Env{Getenv: func(k string) string { return map[string]string{EnvURL: srv.URL + "/", EnvToken: "x"}[k] }}
			var errOut bytes.Buffer
			if code := RunWith(t.Context(), []string{"projects"}, &bytes.Buffer{}, &errOut, "test", env); code != 1 || !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("code %d, %q; want %q", code, errOut.String(), tt.want)
			}
		})
	}
}

func TestDefaultURL(t *testing.T) {
	env := Env{Getenv: func(string) string { return "" }}
	if r := newRemote(env); r.url != DefaultURL {
		t.Errorf("url = %q", r.url)
	}
}

func TestPlainHTTPOnlyToLocalhost(t *testing.T) {
	e := newRemoteEnv(t)
	for _, u := range []string{"http://forecrew.example.com", "http://10.0.0.5:8080", "http://localhost.evil.com"} {
		if _, errOut, code := e.run(t, "workers", "--url", u); code != 1 || !strings.Contains(errOut, "refusing to send the token over plain http") {
			t.Errorf("%s: code %d, %q", u, code, errOut)
		}
	}
	for _, u := range []string{"http://localhost:1", "http://127.0.0.1:1", "http://[::1]:1"} {
		// Allowed: the call is attempted, and fails to connect.
		if _, errOut, code := e.run(t, "workers", "--url", u); code != 1 || !strings.Contains(errOut, "call forecrewd") {
			t.Errorf("%s: code %d, %q", u, code, errOut)
		}
	}
	if n := len(e.svc.Calls()); n != 0 {
		t.Errorf("service called %d times", n)
	}
}

func TestTokenFlagWarning(t *testing.T) {
	e := newRemoteEnv(t)
	if _, errOut, code := e.run(t, "workers", "--token", apiToken); code != 0 || !strings.Contains(errOut, "warning: --token") || strings.Contains(errOut, apiToken) {
		t.Errorf("flag: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "workers"); code != 0 || errOut != "" {
		t.Errorf("env: code %d, %q", code, errOut)
	}
}

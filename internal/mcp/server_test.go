package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

type env struct {
	svc    *testkit.ToolService
	server *httptest.Server
	store  *testkit.Store
	logs   *bytes.Buffer
	actor  domain.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := testkit.NewStore(nil)
	account := domain.Account{ID: domain.NewID(), Name: "acme"}
	if err := st.Accounts().Create(t.Context(), &account); err != nil {
		t.Fatal(err)
	}
	tok := domain.MCPToken{AccountID: account.ID, Name: "laptop", TokenHash: mcp.HashToken(testToken)}
	if err := st.MCPTokens().Create(t.Context(), &tok); err != nil {
		t.Fatal(err)
	}
	e := &env{svc: &testkit.ToolService{}, store: st, logs: &bytes.Buffer{},
		actor: domain.Actor{AccountID: account.ID, Kind: domain.ActorMCP, Name: "laptop"}}
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.server = httptest.NewServer(mcp.NewHandler(e.svc, st.MCPTokens(), "test", logger))
	t.Cleanup(e.server.Close)
	return e
}

type headerTransport struct{ header http.Header }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	maps.Copy(r.Header, h.header)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) connect(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	cs, err := e.tryConnect(t, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func (e *env) tryConnect(t *testing.T, token string) (*sdk.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: headerTransport{http.Header{"Authorization": {"Bearer " + token}}}}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	return client.Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: e.server.URL, HTTPClient: hc}, nil)
}

func call(t *testing.T, cs *sdk.ClientSession, tool string, args any) (*sdk.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func TestListTools(t *testing.T) {
	cs := newEnv(t).connect(t, testToken)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if len(tool.Description) < 20 {
			t.Errorf("%s: description %q is too short", tool.Name, tool.Description)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil || !strings.Contains(string(schema), `"type":"object"`) {
			t.Errorf("%s: input schema %s, %v", tool.Name, schema, err)
		}
	}
	slices.Sort(names)
	if want := slices.Sorted(slices.Values(mcp.Tools())); !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestToolSchemaDescribesFields(t *testing.T) {
	cs := newEnv(t).connect(t, testToken)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != mcp.ToolCreateGoal {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var s struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(s.Required)), []string{"body", "project", "title"}) {
			t.Errorf("required = %v", s.Required)
		}
		if s.Properties["title"].Description == "" || s.Properties["platforms"].Description == "" {
			t.Errorf("title or platforms has no description: %s", raw)
		}
		continue
	}
	for _, tool := range res.Tools {
		if tool.Name != mcp.ToolDispatchJob {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(raw), `"prompt":{"description":"Instructions for the job`) || strings.Contains(string(raw), `"required":["prompt"`) {
			t.Errorf("dispatch_job schema: %s", raw)
		}
	}
}

func TestRequestChangesRequiresFeedback(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	res, text := call(t, cs, mcp.ToolRequestChanges, map[string]any{"gate": "GATE-1"})
	if !res.IsError || !strings.Contains(text, "feedback") {
		t.Errorf("IsError=%v text=%q", res.IsError, text)
	}
	if res, _ := call(t, cs, mcp.ToolApprove, map[string]any{"gate": "GATE-1"}); res.IsError {
		t.Error("approve should not require feedback")
	}
	if n := len(e.svc.Calls()); n != 1 {
		t.Errorf("service called %d times, want 1 (approve only)", n)
	}
}

func TestCallEachTool(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	proj := domain.Project{ID: domain.NewID(), Name: "app", Repo: "acme/app"}
	e.svc.GetProjectFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error) { return proj, nil }
	e.svc.ListProjectsFunc = func(context.Context, domain.Actor, any) ([]domain.Project, error) { return []domain.Project{proj}, nil }

	tests := []struct {
		tool  string
		args  map[string]any
		input any // expected ToolService input
	}{
		{mcp.ToolOnboardProject, map[string]any{"repo": "acme/app", "name": "App"}, mcp.OnboardProjectInput{Repo: "acme/app", Name: "App"}},
		{mcp.ToolCreateGoal, map[string]any{"project": "app", "title": "Login", "body": "Add login", "platforms": []string{"web", "ios"}}, mcp.CreateGoalInput{Project: "app", Title: "Login", Body: "Add login", Platforms: []string{"web", "ios"}}},
		{mcp.ToolJobStatus, map[string]any{"job": "JOB-1"}, mcp.JobRefInput{Job: "JOB-1"}},
		{mcp.ToolJobLogs, map[string]any{"job": "JOB-1", "after_seq": 5, "limit": 10}, mcp.JobLogsInput{Job: "JOB-1", AfterSeq: 5, Limit: 10}},
		{mcp.ToolListWorkers, nil, nil},
		{mcp.ToolListPendingGates, map[string]any{"project": "app"}, mcp.ProjectFilterInput{Project: "app"}},
		{mcp.ToolApprove, map[string]any{"gate": "GATE-1", "feedback": "ok"}, mcp.GateDecisionInput{Gate: "GATE-1", Feedback: "ok"}},
		{mcp.ToolRequestChanges, map[string]any{"gate": "GATE-1", "feedback": "more tests"}, mcp.GateDecisionInput{Gate: "GATE-1", Feedback: "more tests"}},
		{mcp.ToolReject, map[string]any{"gate": "GATE-1"}, mcp.GateDecisionInput{Gate: "GATE-1"}},
		{mcp.ToolDispatchJob, map[string]any{"project": "app", "role": "reviewer", "needs": []string{"linux"}, "prompt": "review the diff"}, mcp.DispatchJobInput{Project: "app", Role: "reviewer", Needs: []string{"linux"}, Prompt: "review the diff"}},
		{mcp.ToolListProjects, nil, nil},
		{mcp.ToolGetProject, map[string]any{"project": "app"}, mcp.ProjectInput{Project: "app"}},
		{mcp.ToolListGoals, map[string]any{"project": "app", "status": "draft"}, mcp.ListGoalsInput{Project: "app", Status: "draft"}},
		{mcp.ToolGetGoal, map[string]any{"goal": "GOAL-2"}, mcp.GoalRefInput{Goal: "GOAL-2"}},
		{mcp.ToolListJobs, map[string]any{"project": "app", "status": "queued", "limit": 5}, mcp.ListJobsInput{Project: "app", Status: "queued", Limit: 5}},
	}
	if len(tests) != len(mcp.Tools()) {
		t.Fatalf("%d cases for %d tools", len(tests), len(mcp.Tools()))
	}
	for i, tt := range tests {
		var args any = tt.args
		if tt.args == nil {
			args = map[string]any{}
		}
		res, text := call(t, cs, tt.tool, args)
		if res.IsError {
			t.Errorf("%s: error result %q", tt.tool, text)
			continue
		}
		calls := e.svc.Calls()
		if len(calls) != i+1 {
			t.Fatalf("%s: %d calls recorded", tt.tool, len(calls))
		}
		got := calls[i]
		if got.Tool != tt.tool || got.Actor != e.actor || fmt.Sprint(got.Input) != fmt.Sprint(tt.input) {
			t.Errorf("%s: recorded %+v, want input %+v by %v", tt.tool, got, tt.input, e.actor)
		}
		if !json.Valid([]byte(text)) {
			t.Errorf("%s: result %q is not JSON", tt.tool, text)
		}
	}
}

func TestResultIsJSON(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	e.svc.GetProjectFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error) {
		return domain.Project{Name: "app", Repo: "acme/app"}, nil
	}
	_, text := call(t, cs, mcp.ToolGetProject, map[string]any{"project": "app"})
	var p domain.Project
	if err := json.Unmarshal([]byte(text), &p); err != nil || p.Repo != "acme/app" {
		t.Errorf("result %q: %+v, %v", text, p, err)
	}
	if _, text := call(t, cs, mcp.ToolListProjects, map[string]any{}); text != "[]" {
		t.Errorf("empty list = %q, want []", text)
	}
}

func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"missing required", mcp.ToolOnboardProject, map[string]any{}, "repo"},
		{"wrong type", mcp.ToolJobLogs, map[string]any{"job": "JOB-1", "limit": "ten"}, "limit"},
		{"unknown field", mcp.ToolJobStatus, map[string]any{"job": "JOB-1", "extra": 1}, "extra"},
	}
	for _, tt := range tests {
		res, text := call(t, cs, tt.tool, tt.args)
		if !res.IsError || !strings.Contains(text, tt.want) {
			t.Errorf("%s: IsError=%v text=%q, want error mentioning %q", tt.name, res.IsError, text, tt.want)
		}
	}
	if n := len(e.svc.Calls()); n != 0 {
		t.Errorf("service called %d times for invalid input", n)
	}
}

func TestServiceErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"not found", fmt.Errorf("get job JOB-9: %w", mcp.ErrNotFound), "get job JOB-9: not found", ""},
		{"invalid input", fmt.Errorf("gate is required: %w", mcp.ErrInvalidInput), "gate is required", ""},
		{"invalid ref", fmt.Errorf("parse %q: %w", "x", domain.ErrInvalidRef), "invalid ref", ""},
		{"invalid transition", fmt.Errorf("job: %w", domain.ErrInvalidTransition), "invalid state transition", ""},
		{"internal", errors.New("pq: connection refused password=hunter2"), "internal error", "hunter2"},
	}
	for _, tt := range tests {
		e := newEnv(t)
		cs := e.connect(t, testToken)
		e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (domain.Job, error) {
			return domain.Job{}, tt.err
		}
		res, text := call(t, cs, mcp.ToolJobStatus, map[string]any{"job": "JOB-9"})
		if !res.IsError || !strings.Contains(text, tt.want) {
			t.Errorf("%s: IsError=%v text=%q, want %q", tt.name, res.IsError, text, tt.want)
		}
		if tt.notWant != "" {
			if strings.Contains(text, tt.notWant) {
				t.Errorf("%s: client text leaks %q", tt.name, tt.notWant)
			}
			if !strings.Contains(e.logs.String(), tt.notWant) {
				t.Errorf("%s: cause not logged: %s", tt.name, e.logs)
			}
		}
	}
}

func TestGateRulesThroughTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	e.svc.RequestChangesFunc = func(_ context.Context, _ domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
		_, err := mcp.GateDecision(mcp.ToolRequestChanges, domain.GateNeedsYou, in)
		return domain.Gate{}, err
	}
	res, text := call(t, cs, mcp.ToolRequestChanges, map[string]any{"gate": "GATE-1", "feedback": "x"})
	if !res.IsError || !strings.Contains(text, "not allowed on needs_you gates") {
		t.Errorf("IsError=%v text=%q", res.IsError, text)
	}
}

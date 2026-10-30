package tools_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

const e2eToken = "fcm_e2e-token"

// e2e serves the real tool service over MCP and /api/v1, both behind the
// same token.
type e2e struct {
	*fixture
	mcpURL string
	apiURL string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	f := newFixture(t)
	must(t, f.st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: f.account.ID, Name: "e2e", TokenHash: mcp.HashToken(e2eToken)}))
	f.by.Name = "e2e"
	logger := slog.New(slog.DiscardHandler)
	mcpSrv := httptest.NewServer(mcp.NewHandler(f.svc, f.st.MCPTokens(), "test", logger))
	t.Cleanup(mcpSrv.Close)
	apiSrv := httptest.NewServer(api.New(api.Config{Tools: f.svc, Tokens: f.st.MCPTokens(), Accounts: f.st.Accounts(), Logger: logger}).Handler())
	t.Cleanup(apiSrv.Close)
	return &e2e{fixture: f, mcpURL: mcpSrv.URL, apiURL: apiSrv.URL}
}

type bearer struct{}

func (bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+e2eToken)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *e2e) mcpCall(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, out any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s: error result %+v", tool, res.Content)
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		must(t, err)
		must(t, json.Unmarshal(raw, out))
	}
	return res
}

func (e *e2e) apiCall(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.apiURL+path, strings.NewReader(body))
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	data, err := io.ReadAll(resp.Body)
	must(t, err)
	must(t, resp.Body.Close())
	if out != nil && resp.StatusCode < 300 {
		must(t, json.Unmarshal(data, out))
	}
	return resp.StatusCode
}

// TestEndToEndMCP drives the service through a real MCP client.
func TestEndToEndMCP(t *testing.T) {
	e := newE2E(t)
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: e.mcpURL, HTTPClient: &http.Client{Transport: bearer{}}}, nil)
	must(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	var created domain.Goal
	res := e.mcpCall(t, cs, mcp.ToolCreateGoal, map[string]any{"project": "app", "title": "Login", "body": "Add login", "platforms": []string{"web"}}, &created)
	if created.Ref() != "GOAL-1" || created.Status != domain.GoalDraft || created.CreatedBy != "mcp:e2e" {
		t.Errorf("created goal = %+v", created)
	}
	if text := res.Content[0].(*sdk.TextContent).Text; text != "GOAL-1 created: Login (draft)." {
		t.Errorf("text = %q", text)
	}
	if starts := e.playbooks.Starts(); len(starts) != 1 || starts[0].Request.GoalID != created.ID {
		t.Errorf("playbook starts = %+v", starts)
	}

	gate := e.openGate(t, domain.GateSpecApproval, created, domain.Task{})
	var pending struct{ Gates []mcp.GateInfo }
	e.mcpCall(t, cs, mcp.ToolListPendingGates, map[string]any{"project": "app"}, &pending)
	if len(pending.Gates) != 1 || pending.Gates[0].Ref != gate.Ref() || pending.Gates[0].GoalRef != "GOAL-1" || pending.Gates[0].URL != "https://github.com/acme/app/issues/12" {
		t.Errorf("pending = %+v", pending)
	}

	var decided domain.Gate
	e.mcpCall(t, cs, mcp.ToolApprove, map[string]any{"gate": gate.Ref(), "feedback": "go"}, &decided)
	if decided.Status != domain.GateApproved || decided.ResolvedBy != "mcp:e2e" {
		t.Errorf("decided = %+v", decided)
	}
	var detail mcp.GoalDetail
	e.mcpCall(t, cs, mcp.ToolGetGoal, map[string]any{"goal": "GOAL-1"}, &detail)
	if len(detail.Gates) != 1 || detail.Gates[0].Status != domain.GateApproved || detail.Project != "app" {
		t.Errorf("goal detail = %+v", detail)
	}

	bad, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: mcp.ToolApprove, Arguments: map[string]any{"gate": "GATE-99"}})
	must(t, err)
	if !bad.IsError || !strings.Contains(bad.Content[0].(*sdk.TextContent).Text, "not found") {
		t.Errorf("unknown gate = %+v", bad)
	}
}

// TestEndToEndAPI drives the same service through /api/v1.
func TestEndToEndAPI(t *testing.T) {
	e := newE2E(t)
	var created domain.Goal
	if code := e.apiCall(t, "POST", "/api/v1/goals", `{"project":"app","title":"Login","body":"Add login"}`, &created); code != http.StatusCreated {
		t.Fatalf("create goal = %d", code)
	}
	if created.Ref() != "GOAL-1" || created.CreatedBy != "mcp:e2e" {
		t.Errorf("created goal = %+v", created)
	}
	gate := e.openGate(t, domain.GateMergeApproval, created, domain.Task{})

	var pending []mcp.GateInfo
	if code := e.apiCall(t, "GET", "/api/v1/gates?project=app", "", &pending); code != http.StatusOK || len(pending) != 1 || pending[0].Ref != gate.Ref() {
		t.Fatalf("pending = %d %+v", code, pending)
	}
	if code := e.apiCall(t, "POST", "/api/v1/gates/"+gate.Ref()+"/request-changes", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("request changes without feedback = %d", code)
	}
	var decided domain.Gate
	if code := e.apiCall(t, "POST", "/api/v1/gates/"+gate.Ref()+"/request-changes", `{"feedback":"tests"}`, &decided); code != http.StatusOK || decided.Status != domain.GateChangesRequested {
		t.Errorf("request changes = %d %+v", code, decided)
	}
	if code := e.apiCall(t, "POST", "/api/v1/gates/"+gate.Ref()+"/approve", `{}`, nil); code != http.StatusConflict {
		t.Errorf("approve a decided gate = %d", code)
	}
	sigs, err := testkit.NewSignalBus(e.st).Lookup(t.Context(), e.account.ID, domain.GateKey(gate.ID))
	must(t, err)
	if len(sigs) != 1 {
		t.Errorf("%d gate.resolved signals", len(sigs))
	}
	var goals []domain.Goal
	if code := e.apiCall(t, "GET", "/api/v1/goals?status=draft", "", &goals); code != http.StatusOK || len(goals) != 1 {
		t.Errorf("goals = %d %+v", code, goals)
	}
	if got := e.auditActions(t); len(got) != 3 || got[0] != "mcp:e2e goal.created" || got[2] != "mcp:e2e gate.resolved" {
		t.Errorf("audit = %v", got)
	}
}

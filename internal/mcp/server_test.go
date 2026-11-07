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
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/authz"
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
		actor: domain.Actor{AccountID: account.ID, Kind: domain.ActorMCP, Name: "laptop", Credential: domain.Credential{Kind: domain.CredentialToken, ID: tok.ID}}}
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.server = httptest.NewServer(mcp.NewHandler(e.svc, st, authz.New(st, nil), "test", mcp.Resource{}, logger))
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
		{mcp.ToolOnboardProject, map[string]any{"repo": "acme/app", "name": "App", "default_branch": "trunk", "refresh": true}, mcp.OnboardProjectInput{Repo: "acme/app", Name: "App", DefaultBranch: "trunk", Refresh: true}},
		{mcp.ToolCreateGoal, map[string]any{"project": "app", "title": "Login", "body": "Add login", "platforms": []string{"web", "ios"}}, mcp.CreateGoalInput{Project: "app", Title: "Login", Body: "Add login", Platforms: []string{"web", "ios"}}},
		{mcp.ToolReportBug, map[string]any{"project": "app", "title": "Crash", "body": "It crashes", "platforms": []string{"ios"}}, mcp.ReportBugInput{Project: "app", Title: "Crash", Body: "It crashes", Platforms: []string{"ios"}}},
		{mcp.ToolJobStatus, map[string]any{"job": "JOB-1"}, mcp.JobRefInput{Job: "JOB-1"}},
		{mcp.ToolJobLogs, map[string]any{"job": "JOB-1", "after_seq": 5, "limit": 10}, mcp.JobLogsInput{Job: "JOB-1", AfterSeq: 5, Limit: 10}},
		{mcp.ToolListWorkers, nil, nil},
		{mcp.ToolListImages, map[string]any{"name": "linux", "os": "linux", "arch": "amd64", "format": "rootfs", "limit": 5}, mcp.ListImagesInput{Name: "linux", OS: "linux", Arch: "amd64", Format: "rootfs", Limit: 5}},
		{mcp.ToolListPendingGates, map[string]any{"project": "app"}, mcp.ProjectFilterInput{Project: "app"}},
		{mcp.ToolApprove, map[string]any{"gate": "GATE-1", "feedback": "ok"}, mcp.GateDecisionInput{Gate: "GATE-1", Feedback: "ok"}},
		{mcp.ToolRequestChanges, map[string]any{"gate": "GATE-1", "feedback": "more tests"}, mcp.GateDecisionInput{Gate: "GATE-1", Feedback: "more tests"}},
		{mcp.ToolReject, map[string]any{"gate": "GATE-1"}, mcp.GateDecisionInput{Gate: "GATE-1"}},
		{mcp.ToolConfigSchema, map[string]any{"kind": "shape"}, mcp.ConfigKindInput{Kind: "shape"}},
		{mcp.ToolConfigGet, map[string]any{"kind": "team"}, mcp.ConfigGetInput{Kind: "team"}},
		{mcp.ToolConfigPlan, map[string]any{"title": "t", "changes": []any{}}, mcp.ConfigPlanInput{Title: "t", Changes: []mcp.ConfigWanted{}, Source: domain.ConfigFromMCP}},
		{mcp.ToolConfigApply, map[string]any{"plan": "p"}, mcp.ConfigPlanRefInput{Plan: "p"}},
		{mcp.ToolConfigRevert, map[string]any{"plan": "p"}, mcp.ConfigPlanRefInput{Plan: "p", Source: domain.ConfigFromMCP}},
		{mcp.ToolConfigExport, nil, nil},
		{mcp.ToolConfigImport, map[string]any{"yaml": "y"}, mcp.ConfigImportInput{YAML: "y", Source: domain.ConfigFromMCP}},
		{mcp.ToolConfigPlans, map[string]any{"inbox": true}, mcp.ConfigPlansInput{Inbox: true}},
		{mcp.ToolConfigGetPlan, map[string]any{"plan": "p"}, mcp.ConfigPlanRefInput{Plan: "p", Source: domain.ConfigFromMCP}},
		{mcp.ToolConfigCancel, map[string]any{"plan": "p"}, mcp.ConfigPlanRefInput{Plan: "p", Source: domain.ConfigFromMCP}},
		{mcp.ToolAssistantSend, map[string]any{"message": "hi"}, mcp.AssistantSendInput{Message: "hi"}},
		{mcp.ToolAssistantGet, map[string]any{"conversation": "c"}, mcp.AssistantRefInput{Conversation: "c"}},
		{mcp.ToolAssistantList, map[string]any{"limit": 3}, mcp.AssistantListInput{Limit: 3}},
		{mcp.ToolDispatchJob, map[string]any{"project": "app", "role": "reviewer", "needs": []string{"linux"}, "prompt": "review the diff"}, mcp.DispatchJobInput{Project: "app", Role: "reviewer", Needs: []string{"linux"}, Prompt: "review the diff"}},
		{mcp.ToolListProjects, nil, nil},
		{mcp.ToolGetProject, map[string]any{"project": "app"}, mcp.ProjectInput{Project: "app"}},
		{mcp.ToolListGoals, map[string]any{"project": "app", "status": "draft"}, mcp.ListGoalsInput{Project: "app", Status: "draft", WrapUntrusted: true}},
		{mcp.ToolGetGoal, map[string]any{"goal": "GOAL-2"}, mcp.GoalRefInput{Goal: "GOAL-2", WrapUntrusted: true}},
		{mcp.ToolListJobs, map[string]any{"project": "app", "status": "queued", "limit": 5}, mcp.ListJobsInput{Project: "app", Status: "queued", Limit: 5}},
		{mcp.ToolCancelJob, map[string]any{"job": "JOB-1"}, mcp.JobRefInput{Job: "JOB-1"}},
		{mcp.ToolListTokens, nil, nil},
		{mcp.ToolRevokeToken, map[string]any{"token": "old-laptop"}, mcp.RevokeTokenInput{Token: "old-laptop"}},
		{mcp.ToolListArtifacts, map[string]any{"job": "JOB-1"}, mcp.JobRefInput{Job: "JOB-1"}},
		{mcp.ToolListTestRuns, map[string]any{"job": "JOB-1", "sha": "abc"}, mcp.ListTestRunsInput{Job: "JOB-1", SHA: "abc"}},
		{mcp.ToolListAssumptions, map[string]any{"project": "app", "task": "TASK-1", "status": "unverified", "impact": "high", "verify_by": "test"},
			mcp.ListAssumptionsInput{Project: "app", Task: "TASK-1", Status: "unverified", Impact: "high", VerifyBy: "test"}},
		{mcp.ToolResolveAssumption, map[string]any{"assumption": "A-42", "status": "accepted"}, mcp.ResolveAssumptionInput{Assumption: "A-42", Status: "accepted"}},
		{mcp.ToolListDecisions, map[string]any{"project": "app", "goal": "GOAL-1"}, mcp.ListDecisionsInput{Project: "app", Goal: "GOAL-1"}},
		{mcp.ToolSearchKnowledge, map[string]any{"project": "app", "query": "queue", "kind": "note", "limit": 3}, mcp.SearchKnowledgeInput{Project: "app", Query: "queue", Kind: "note", Limit: 3, WrapUntrusted: true}},
		{mcp.ToolDeleteNote, map[string]any{"note": "4bf2c2f4-0000-7000-8000-000000000001"}, mcp.DeleteNoteInput{Note: "4bf2c2f4-0000-7000-8000-000000000001"}},
		{mcp.ToolListKnowledgeProposals, map[string]any{"project": "app", "status": "open", "limit": 5}, mcp.ListKnowledgeProposalsInput{Project: "app", Status: domain.KnowledgeOpen, Limit: 5, WrapUntrusted: true}},
		{mcp.ToolListKnowledge, map[string]any{"project": "app", "section": "vision", "limit": 5}, mcp.ListKnowledgeInput{Project: "app", Section: "vision", Limit: 5, WrapUntrusted: true}},
		{mcp.ToolProposeKnowledge, map[string]any{"project": "app", "section": "lessons", "path": "knowledge/lessons/a.md", "body": "b", "reason": "r"},
			mcp.ProposeKnowledgeInput{Project: "app", Section: "lessons", Path: "knowledge/lessons/a.md", Body: "b", Reason: "r"}},
		{mcp.ToolGetTrace, map[string]any{"ref": "GOAL-1"}, mcp.GetTraceInput{Ref: "GOAL-1"}},
		{mcp.ToolStartCouncil, map[string]any{"project": "app", "question": "Which queue?", "kind": "technical", "roles": []string{"ux"}, "constraints": []string{"c"}},
			mcp.StartCouncilInput{Project: "app", Question: "Which queue?", Kind: "technical", Roles: []string{"ux"}, Constraints: []string{"c"}}},
		{mcp.ToolListCouncils, map[string]any{"project": "app", "status": "decided"}, mcp.ListCouncilsInput{Project: "app", Status: "decided"}},
		{mcp.ToolGetCouncil, map[string]any{"council": "COUNCIL-2"}, mcp.CouncilRefInput{Council: "COUNCIL-2"}},
		{mcp.ToolAddConstraint, map[string]any{"council": "COUNCIL-2", "constraint": "Stay on Postgres"}, mcp.AddCouncilConstraintInput{Council: "COUNCIL-2", Constraint: "Stay on Postgres"}},
		{mcp.ToolCreateToken, map[string]any{"name": "laptop", "scopes": []string{"read"}, "ttl": "24h"}, mcp.CreateTokenInput{Name: "laptop", Scopes: []domain.Scope{domain.ScopeRead}, TTL: "24h"}},
		{mcp.ToolCreateInvitation, map[string]any{"email": "a@b.c", "roles": []map[string]string{{"role": "member"}}}, mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: "member"}}}},
		{mcp.ToolListInvitations, nil, nil},
		{mcp.ToolRevokeInvitation, map[string]any{"invitation": "x"}, mcp.InvitationRefInput{Invitation: "x"}},
		{mcp.ToolListConnectedApps, nil, nil},
		{mcp.ToolRevokeConnectedApp, map[string]any{"app": "x"}, mcp.ConnectedAppInput{App: "x"}},
		{mcp.ToolVerificationReport, map[string]any{"change": "9", "project": "app"}, mcp.VerificationReportInput{Change: "9", Project: "app"}},
		{mcp.ToolListRoles, nil, nil},
		{mcp.ToolListTeams, nil, nil},
		{mcp.ToolCreateTeam, map[string]any{"name": "Core", "slug": "core"}, mcp.CreateTeamInput{Name: "Core", Slug: "core"}},
		{mcp.ToolUpdateTeam, map[string]any{"team": "core", "name": "Base"}, mcp.UpdateTeamInput{Team: "core", Name: "Base"}},
		{mcp.ToolDeleteTeam, map[string]any{"team": "core"}, mcp.TeamRefInput{Team: "core"}},
		{mcp.ToolListTeamMembers, map[string]any{"team": "core"}, mcp.TeamRefInput{Team: "core"}},
		{mcp.ToolAddTeamMember, map[string]any{"team": "core", "user": "u"}, mcp.TeamMemberInput{Team: "core", User: "u"}},
		{mcp.ToolRemoveTeamMember, map[string]any{"team": "core", "user": "u"}, mcp.TeamMemberInput{Team: "core", User: "u"}},
		{mcp.ToolListRoleBindings, map[string]any{"project": "app"}, mcp.ListRoleBindingsInput{Project: "app"}},
		{mcp.ToolCreateRoleBinding, map[string]any{"subject_kind": "team", "subject": "core", "role": "viewer", "project": "app"},
			mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "viewer", Project: "app"}},
		{mcp.ToolDeleteRoleBinding, map[string]any{"binding": "b"}, mcp.RoleBindingRefInput{Binding: "b"}},
		{mcp.ToolListServiceAccounts, nil, nil},
		{mcp.ToolCreateServiceAccount, map[string]any{"name": "ci", "description": "CI"}, mcp.CreateServiceAccountInput{Name: "ci", Description: "CI"}},
		{mcp.ToolUpdateServiceAccount, map[string]any{"service_account": "ci", "name": "build"}, mcp.UpdateServiceAccountInput{ServiceAccount: "ci", Name: "build"}},
		{mcp.ToolSetServiceAccount, map[string]any{"service_account": "ci", "disabled": true}, mcp.SetServiceAccountInput{ServiceAccount: "ci", Disabled: true}},
		{mcp.ToolProjectQuality, map[string]any{"project": "app"}, mcp.ProjectInput{Project: "app"}},
		{mcp.ToolListMachines, map[string]any{"state": "busy", "limit": 5}, mcp.ListMachinesInput{State: "busy", Limit: 5, WrapUntrusted: true}},
		{mcp.ToolListTrackerConnections, nil, nil},
		{mcp.ToolCreateTrackerConnection, map[string]any{"name": "gh", "kind": "github", "triggers": map[string]any{"label": "forecrew"}}, mcp.CreateTrackerConnectionInput{Name: "gh", Kind: "github", Triggers: domain.TrackerTriggers{Label: "forecrew"}}},
		{mcp.ToolDeleteTrackerConnection, map[string]any{"connection": "gh"}, mcp.TrackerConnectionRefInput{Connection: "gh"}},
		{mcp.ToolTestTrackerConnection, map[string]any{"connection": "gh"}, mcp.TrackerConnectionRefInput{Connection: "gh", WrapUntrusted: true}},
		{mcp.ToolListTrackerRoutes, map[string]any{"connection": "gh"}, mcp.TrackerConnectionFilterInput{Connection: "gh", WrapUntrusted: true}},
		{mcp.ToolCreateTrackerRoute, map[string]any{"connection": "gh", "project": "app", "states": map[string]any{"done": "closed"}}, mcp.CreateTrackerRouteInput{Connection: "gh", Project: "app", States: map[domain.TicketState]string{domain.TicketDone: "closed"}}},
		{mcp.ToolDeleteTrackerRoute, map[string]any{"route": "r"}, mcp.TrackerRouteRefInput{Route: "r"}},
		{mcp.ToolListTrackerIdentities, map[string]any{}, mcp.TrackerConnectionFilterInput{}},
		{mcp.ToolMapTrackerIdentity, map[string]any{"connection": "gh", "external_id": "11", "user": "u"}, mcp.MapTrackerIdentityInput{Connection: "gh", ExternalID: "11", User: "u"}},
		{mcp.ToolUnmapTrackerIdentity, map[string]any{"identity": "i"}, mcp.TrackerIdentityRefInput{Identity: "i"}},
		{mcp.ToolListTicketsWaiting, map[string]any{"project": "app"}, mcp.ListTicketsWaitingInput{Project: "app", WrapUntrusted: true}},
		{mcp.ToolAcceptTicket, map[string]any{"ticket": "t", "project": "app"}, mcp.AcceptTicketInput{Ticket: "t", Project: "app"}},
		{mcp.ToolDismissTicket, map[string]any{"ticket": "t", "reason": "no"}, mcp.DismissTicketInput{Ticket: "t", Reason: "no"}},
		{mcp.ToolListShapes, nil, nil},
		{mcp.ToolListProviders, nil, mcp.ListProvidersInput{WrapUntrusted: true}},
		{mcp.ToolFleetOverview, nil, nil},
		{mcp.ToolCreateProvider, map[string]any{"name": "home", "kind": "microvm", "org": "acme"}, mcp.CreateProviderInput{Name: "home", Kind: domain.ProviderMicroVM, Org: "acme"}},
		{mcp.ToolRevokeProvider, map[string]any{"provider": "home"}, mcp.ProviderRefInput{Provider: "home", WrapUntrusted: true}},
		{mcp.ToolCreateJoinCode, map[string]any{"cloud": true}, mcp.CreateJoinCodeInput{Cloud: true}},
		{mcp.ToolListJoinCodes, nil, mcp.ListJoinCodesInput{WrapUntrusted: true}},
		{mcp.ToolConfirmJoin, map[string]any{"join": "j", "name": "home", "facts_hash": "h"}, mcp.ConfirmJoinInput{Join: "j", Name: "home", FactsHash: "h", WrapUntrusted: true}},
		{mcp.ToolListMembers, nil, nil},
		{mcp.ToolSetMemberSuspended, map[string]any{"user": "u", "suspended": true}, mcp.SetMemberInput{User: "u", Suspended: true}},
		{mcp.ToolRemoveMember, map[string]any{"user": "u"}, mcp.MemberRefInput{User: "u"}},
		{mcp.ToolUpdateOrganization, map[string]any{"name": "Acme", "slug": "acme"}, mcp.UpdateOrganizationInput{Name: "Acme", Slug: "acme"}},
		{mcp.ToolListAudit, map[string]any{"action": "member.removed", "limit": 5}, mcp.ListAuditInput{Action: "member.removed", Limit: 5}},
		{mcp.ToolListConnections, nil, nil},
		{mcp.ToolCreateConnection, map[string]any{"kind": "oidc", "name": "okta", "issuer": "https://acme.okta.com", "client_id": "c", "client_secret_ref": "env:FORECREW_CONN_OKTA"},
			mcp.CreateConnectionInput{Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_OKTA"}},
		{mcp.ToolSetConnection, map[string]any{"connection": "okta", "enabled": true}, mcp.SetConnectionInput{Connection: "okta", Enabled: true}},
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
		if text == "" || json.Valid([]byte(text)) {
			t.Errorf("%s: text %q is not a short summary", tt.tool, text)
		}
		if _, ok := res.StructuredContent.(map[string]any); !ok {
			t.Errorf("%s: structured content %#v is not an object", tt.tool, res.StructuredContent)
		}
	}
}

func TestStructuredResult(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	e.svc.GetProjectFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error) {
		return domain.Project{Name: "app", Repo: "acme/app", Status: domain.ProjectOnboarded}, nil
	}
	res, text := call(t, cs, mcp.ToolGetProject, map[string]any{"project": "app"})
	if text != "app (acme/app) is onboarded." {
		t.Errorf("text = %q", text)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var p domain.Project
	if err := json.Unmarshal(raw, &p); err != nil || p.Repo != "acme/app" {
		t.Errorf("structured content %s: %+v, %v", raw, p, err)
	}

	res, text = call(t, cs, mcp.ToolListProjects, map[string]any{})
	if raw, _ := json.Marshal(res.StructuredContent); string(raw) != `{"projects":[]}` || text != "0 projects." {
		t.Errorf("empty list = %s, %q", raw, text)
	}

	e.svc.ListWorkersFunc = func(context.Context, domain.Actor, any) ([]mcp.WorkerInfo, error) {
		return []mcp.WorkerInfo{{Online: true}, {}}, nil
	}
	if _, text := call(t, cs, mcp.ToolListWorkers, map[string]any{}); text != "2 workers, 1 online." {
		t.Errorf("workers text = %q", text)
	}
	e.svc.GetGoalFunc = func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error) {
		return mcp.GoalDetail{Ref: "GOAL-2", Goal: domain.Goal{Title: "Login", Status: domain.GoalInProgress}, CostUSD: 1.5}, nil
	}
	res, text = call(t, cs, mcp.ToolGetGoal, map[string]any{"goal": "GOAL-2"})
	if raw, _ := json.Marshal(res.StructuredContent); !strings.Contains(string(raw), `"tasks":[]`) || !strings.Contains(string(raw), `"gates":[]`) || text != "GOAL-2 Login is in_progress: 0 tasks, $1.50." {
		t.Errorf("goal = %s, %q", raw, text)
	}
	e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Role: "developer", Status: domain.JobQueued, Ref: "JOB-4", Reason: "no matching worker"}, nil
	}
	if _, text := call(t, cs, mcp.ToolJobStatus, map[string]any{"job": "JOB-4"}); text != "JOB-4 (developer) is queued: no matching worker." {
		t.Errorf("job text = %q", text)
	}
}

func TestManagementResults(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	e.svc.CancelJobFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
		return mcp.JobInfo{Ref: "JOB-4", Status: domain.JobCanceled}, nil
	}
	if _, text := call(t, cs, mcp.ToolCancelJob, map[string]any{"job": "JOB-4"}); text != "JOB-4 is canceled." {
		t.Errorf("cancel text = %q", text)
	}
	if res, text := call(t, cs, mcp.ToolListTokens, map[string]any{}); text != "0 tokens, 0 active." {
		t.Errorf("empty tokens text = %q", text)
	} else if raw, _ := json.Marshal(res.StructuredContent); string(raw) != `{"tokens":[]}` {
		t.Errorf("empty tokens = %s", raw)
	}
	e.svc.ListTokensFunc = func(context.Context, domain.Actor, any) ([]mcp.TokenInfo, error) {
		return []mcp.TokenInfo{{Name: "laptop"}, {Name: "old", RevokedAt: time.Now()}}, nil
	}
	if res, text := call(t, cs, mcp.ToolListTokens, map[string]any{}); text != "2 tokens, 1 active." {
		t.Errorf("tokens text = %q", text)
	} else if raw, _ := json.Marshal(res.StructuredContent); strings.Contains(string(raw), "hash") {
		t.Errorf("tokens = %s", raw)
	}
	e.svc.RevokeTokenFunc = func(context.Context, domain.Actor, mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
		return mcp.NewRevokeTokenOutput(mcp.TokenInfo{Name: "old"}, false), nil
	}
	if _, text := call(t, cs, mcp.ToolRevokeToken, map[string]any{"token": "old"}); text != "Token old revoked." {
		t.Errorf("revoke text = %q", text)
	}
	e.svc.RevokeTokenFunc = func(context.Context, domain.Actor, mcp.RevokeTokenInput) (mcp.RevokeTokenOutput, error) {
		return mcp.NewRevokeTokenOutput(mcp.TokenInfo{Name: "laptop"}, true), nil
	}
	if _, text := call(t, cs, mcp.ToolRevokeToken, map[string]any{"token": "laptop"}); !strings.Contains(text, "token you are using") {
		t.Errorf("self revoke text = %q", text)
	}
	if res, _ := call(t, cs, mcp.ToolRevokeToken, map[string]any{}); !res.IsError {
		t.Error("revoke_token without a token should fail")
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
		e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
			return mcp.JobInfo{}, tt.err
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

func TestErrorCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("job: %w", mcp.ErrNotFound), "not_found"},
		{fmt.Errorf("x: %w", mcp.ErrInvalidInput), "invalid_input"},
		{fmt.Errorf("x: %w", domain.ErrInvalidRef), "invalid_input"},
		{fmt.Errorf("x: %w", domain.ErrInvalidDecision), "invalid_input"},
		{fmt.Errorf("x: %w", domain.ErrInvalidTransition), "conflict"},
		{errors.New("boom"), ""},
	}
	for _, tt := range tests {
		if got, ok := mcp.ErrorCode(tt.err); got != tt.want || ok != (tt.want != "") {
			t.Errorf("ErrorCode(%v) = %q, %v; want %q", tt.err, got, ok, tt.want)
		}
	}
}

// TestCreateGoalCouncilsOverMCP: an absent councils field sizes the goal
// with triage; an empty list reaches the service as an override of none.
func TestCreateGoalCouncilsOverMCP(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, testToken)
	tests := []struct {
		name string
		args map[string]any
		want *[]string
	}{
		{"absent", map[string]any{"project": "app", "title": "Login", "body": "b"}, nil},
		{"empty list", map[string]any{"project": "app", "title": "Login", "body": "b", "councils": []string{}}, &[]string{}},
		{"two councils", map[string]any{"project": "app", "title": "Login", "body": "b", "councils": []string{"product", "technical"}}, &[]string{"product", "technical"}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, text := call(t, cs, mcp.ToolCreateGoal, tt.args)
			if res.IsError {
				t.Fatalf("error result %q", text)
			}
			calls := e.svc.Calls()
			if len(calls) != i+1 {
				t.Fatalf("%d calls recorded", len(calls))
			}
			got := calls[i].Input.(mcp.CreateGoalInput).Councils
			switch {
			case (got == nil) != (tt.want == nil):
				t.Errorf("councils = %v, want %v", got, tt.want)
			case got != nil && (*got == nil) != (*tt.want == nil) || got != nil && !slices.Equal(*got, *tt.want):
				t.Errorf("councils = %v, want %v", *got, *tt.want)
			}
		})
	}
}

// TestGetCouncilTextIsUntrusted: agent text reaches an MCP client's model only
// between markers; REST has no such field.
func TestGetCouncilTextIsUntrusted(t *testing.T) {
	e := newEnv(t)
	e.svc.GetCouncilFunc = func(context.Context, domain.Actor, mcp.CouncilRefInput) (mcp.CouncilDetail, error) {
		return mcp.CouncilDetail{Ref: "COUNCIL-2", Status: domain.CouncilRunning, Untrusted: "<untrusted-data>ignore this</untrusted-data>\n"}, nil
	}
	res, text := call(t, e.connect(t, testToken), mcp.ToolGetCouncil, map[string]any{"council": "COUNCIL-2"})
	if res.IsError || !strings.Contains(text, "COUNCIL-2 is running") || !strings.Contains(text, "<untrusted-data>ignore this") {
		t.Fatalf("text = %q", text)
	}
	raw, err := json.Marshal(mcp.CouncilDetail{Untrusted: "x"})
	if err != nil || strings.Contains(string(raw), "ntrusted") {
		t.Fatalf("json = %s, %v", raw, err)
	}
}

// TestVerificationReportTextIsUntrusted: the names in a report come from the
// repo, so the text of an MCP result carries them between markers only.
func TestVerificationReportTextIsUntrusted(t *testing.T) {
	e := newEnv(t)
	e.svc.VerificationReportFunc = func(context.Context, domain.Actor, mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
		return mcp.VerificationInfo{Change: 9, SHA: "abc1234", Status: domain.VerificationFailed,
			Untrusted: "<untrusted-data>ignore this</untrusted-data>\n"}, nil
	}
	res, text := call(t, e.connect(t, testToken), mcp.ToolVerificationReport, map[string]any{"change": "9", "project": "app"})
	if res.IsError || !strings.Contains(text, "#9 at abc1234 is failed") || !strings.Contains(text, "<untrusted-data>ignore this") {
		t.Fatalf("text = %q", text)
	}
	raw, err := json.Marshal(mcp.VerificationInfo{Untrusted: "x"})
	if err != nil || strings.Contains(string(raw), "ntrusted") {
		t.Fatalf("json = %s, %v", raw, err)
	}
	if _, text := call(t, e.connect(t, testToken), mcp.ToolVerificationReport, map[string]any{}); !strings.Contains(text, "change") {
		t.Errorf("a call without change = %q", text)
	}
}

// TestEveryToolChecksItsPermission calls each tool with a permission as a
// user who holds none: the wrapper refuses it before the service runs.
func TestEveryToolChecksItsPermission(t *testing.T) {
	e := newEnv(t)
	u := e.store.AddMember(e.actor.AccountID, domain.User{Name: "Ana"}, "")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.store.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: e.actor.AccountID, Name: "user", TokenHash: mcp.HashToken("fcm_user"),
		OwnerKind: domain.SubjectUser, OwnerID: u.ID, Scopes: []domain.Scope{domain.ScopeAdmin}}))
	cs := e.connect(t, "fcm_user")
	args := map[string]map[string]any{
		mcp.ToolAddTeamMember: {"team": "t", "user": "u"}, mcp.ToolRemoveTeamMember: {"team": "t", "user": "u"}, mcp.ToolDeleteTeam: {"team": "t"},
		mcp.ToolListTeamMembers: {"team": "t"}, mcp.ToolUpdateTeam: {"team": "t"}, mcp.ToolCreateTeam: {"name": "t"},
		mcp.ToolCreateServiceAccount: {"name": "s"}, mcp.ToolUpdateServiceAccount: {"service_account": "s"}, mcp.ToolSetServiceAccount: {"service_account": "s", "disabled": true},
		mcp.ToolGetCouncil: {"council": "c"}, mcp.ToolAddConstraint: {"council": "c", "constraint": "x"}, mcp.ToolStartCouncil: {"project": "p", "question": "q"},
		mcp.ToolCreateGoal: {"project": "p", "title": "t", "body": "b"}, mcp.ToolReportBug: {"project": "p", "title": "t", "body": "b"}, mcp.ToolSearchKnowledge: {"project": "p", "query": "q"},
		mcp.ToolJobLogs: {"job": "j"}, mcp.ToolJobStatus: {"job": "j"}, mcp.ToolCancelJob: {"job": "j"}, mcp.ToolListArtifacts: {"job": "j"},
		mcp.ToolOnboardProject: {"repo": "o/r"}, mcp.ToolResolveAssumption: {"assumption": "a", "status": "accepted"}, mcp.ToolListAssumptions: {"project": "p"},
		mcp.ToolGetProject: {"project": "p"}, mcp.ToolDeleteNote: {"note": "n"}, mcp.ToolDispatchJob: {"project": "p", "role": "r"},
		mcp.ToolGetTrace: {"ref": "GOAL-1"}, mcp.ToolListKnowledgeProposals: {"project": "p"}, mcp.ToolListKnowledge: {"project": "p"}, mcp.ToolGetGoal: {"goal": "g"}, mcp.ToolListDecisions: {"project": "p"}, mcp.ToolVerificationReport: {"change": "9", "project": "p"},
		mcp.ToolProjectQuality:     {"project": "p"},
		mcp.ToolMapTrackerIdentity: {"connection": "c", "external_id": "1", "user": "u"}, mcp.ToolUnmapTrackerIdentity: {"identity": "i"},
		mcp.ToolCreateTrackerConnection: {"name": "c", "kind": "k", "triggers": map[string]any{"label": "l"}}, mcp.ToolCreateTrackerRoute: {"connection": "c", "project": "p"},
		mcp.ToolDeleteTrackerRoute: {"route": "r"}, mcp.ToolDeleteTrackerConnection: {"connection": "c"}, mcp.ToolTestTrackerConnection: {"connection": "c"}, mcp.ToolAcceptTicket: {"ticket": "t"}, mcp.ToolDismissTicket: {"ticket": "t"},
		mcp.ToolProposeKnowledge: {"project": "p", "section": "s", "path": "x", "body": "b", "reason": "r"},
		mcp.ToolCreateInvitation: {"email": "a@b.c", "roles": []map[string]string{{"role": "member"}}}, mcp.ToolRevokeInvitation: {"invitation": "x"},
		mcp.ToolConfigPlan: {"changes": []any{}}, mcp.ToolConfigApply: {"plan": "p"}, mcp.ToolConfigRevert: {"plan": "p"}, mcp.ToolConfigImport: {"yaml": "y"},
		mcp.ToolConfigGetPlan: {"plan": "p"}, mcp.ToolConfigCancel: {"plan": "p"}, mcp.ToolAssistantSend: {"message": "m"}, mcp.ToolAssistantGet: {"conversation": "c"},
		mcp.ToolCreateProvider: {"name": "n", "kind": "aws"}, mcp.ToolRevokeProvider: {"provider": "n"}, mcp.ToolConfirmJoin: {"join": "j", "name": "n", "facts_hash": "h"},
		mcp.ToolSetMemberSuspended: {"user": "u", "suspended": true}, mcp.ToolRemoveMember: {"user": "u"},
		mcp.ToolCreateConnection: {"kind": "oidc", "name": "o", "client_id": "c", "client_secret_ref": "env:X"}, mcp.ToolSetConnection: {"connection": "o", "enabled": true},
	}
	for name, a := range mcp.ToolAccess() {
		if a.Permission == "" || a.Own {
			continue
		}
		in, ok := args[name]
		if !ok {
			in = map[string]any{}
		}
		res, text := call(t, cs, name, in)
		if !res.IsError || !strings.Contains(text, string(a.Permission)) {
			t.Errorf("%s: IsError=%v text=%q", name, res.IsError, text)
		}
	}
	if n := len(e.svc.Calls()); n != 0 {
		t.Errorf("service called %d times", n)
	}
	// The zero-permission tools still reach the service.
	if res, _ := call(t, cs, mcp.ToolListTokens, nil); res.IsError {
		t.Error("list_tokens refused")
	}
	if got := e.svc.Calls(); len(got) != 1 || got[0].Actor.Kind != domain.ActorUser {
		t.Errorf("calls = %+v", got)
	}
}

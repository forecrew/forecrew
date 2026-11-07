package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/api"
	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/testkit"
)

const token = "fcm_test-token"

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	svc     *testkit.ToolService
	images  *fakeImages
	store   *testkit.Store
	server  *httptest.Server
	client  *http.Client // keeps cookies
	logs    *bytes.Buffer
	account domain.Account
	actor   domain.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvFor(t, nil, domain.Account{Name: "acme"})
}

// newEnvFor is newEnv with the store clock and the account given.
func newEnvFor(t *testing.T, now func() time.Time, account domain.Account) *env {
	t.Helper()
	st := testkit.NewStore(now)
	e := &env{svc: &testkit.ToolService{}, images: &fakeImages{}, store: st, logs: &bytes.Buffer{}, account: account}
	must(t, st.Accounts().Create(t.Context(), &e.account))
	laptop := domain.MCPToken{AccountID: e.account.ID, Name: "laptop", TokenHash: mcp.HashToken(token)}
	must(t, st.MCPTokens().Create(t.Context(), &laptop))
	must(t, st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: e.account.ID, Name: "old", TokenHash: mcp.HashToken("fcm_old"), RevokedAt: testkit.NewClock(t0).Now()}))
	e.actor = domain.Actor{AccountID: e.account.ID, Kind: domain.ActorMCP, Name: "laptop", Credential: domain.Credential{Kind: domain.CredentialToken, ID: laptop.ID}}
	srv := api.New(api.Config{
		Tools: e.svc, Artifacts: e.svc, Images: e.images, Store: st, Authz: authz.New(st, nil), Logger: slog.New(slog.NewJSONHandler(e.logs, nil)), TokenLogin: true,
		Auth: auth.New(auth.Config{Store: st, Auth: config.Auth{TokenLogin: true}}),
	})
	e.server = httptest.NewTLSServer(srv.Handler())
	t.Cleanup(e.server.Close)
	jar, err := cookiejar.New(nil)
	must(t, err)
	e.client = e.server.Client()
	e.client.Jar = jar
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type response struct {
	status int
	header http.Header
	body   string
}

// do sends a request; auth is "bearer" or "" (cookie jar only).
func (e *env) do(t *testing.T, method, path, body, auth string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.server.URL+path, rd)
	must(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth == "bearer" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return e.send(t, req)
}

func (e *env) send(t *testing.T, req *http.Request) response {
	t.Helper()
	resp, err := e.client.Do(req)
	must(t, err)
	defer func() { must(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	return response{resp.StatusCode, resp.Header, string(b)}
}

func errorCode(t *testing.T, r response) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal([]byte(r.body), &body); err != nil {
		t.Fatalf("body %q is not an error: %v", r.body, err)
	}
	return body.Error.Code
}

func TestEndpoints(t *testing.T) {
	e := newEnv(t)
	proj := domain.Project{ID: domain.NewID(), Name: "app", Repo: "acme/app"}
	e.svc.GetProjectFunc = func(context.Context, domain.Actor, mcp.ProjectInput) (domain.Project, error) { return proj, nil }

	tests := []struct {
		method, path, body string
		status             int
		tool               string
		input              any
	}{
		{"POST", "/api/v1/projects", `{"repo":"acme/app","name":"App","default_branch":"trunk"}`, 202, mcp.ToolOnboardProject, mcp.OnboardProjectInput{Repo: "acme/app", Name: "App", DefaultBranch: "trunk"}},
		{"POST", "/api/v1/projects", `{"repo":"acme/app","refresh":true}`, 202, mcp.ToolOnboardProject, mcp.OnboardProjectInput{Repo: "acme/app", Refresh: true}},
		{"GET", "/api/v1/projects", "", 200, mcp.ToolListProjects, nil},
		{"GET", "/api/v1/tracker-connections", "", 200, mcp.ToolListTrackerConnections, nil},
		{"POST", "/api/v1/tracker-connections", `{"name":"gh","kind":"github","config":{"repos":["acme/app"]},"triggers":{"label":"forecrew"}}`, 201, mcp.ToolCreateTrackerConnection,
			mcp.CreateTrackerConnectionInput{Name: "gh", Kind: "github", Config: map[string]any{"repos": []any{"acme/app"}}, Triggers: domain.TrackerTriggers{Label: "forecrew"}}},
		{"DELETE", "/api/v1/tracker-connections/gh", "", 200, mcp.ToolDeleteTrackerConnection, mcp.TrackerConnectionRefInput{Connection: "gh"}},
		{"POST", "/api/v1/tracker-connections/gh/discard-writes", "", 200, mcp.ToolDiscardTrackerWrites, mcp.TrackerConnectionRefInput{Connection: "gh"}},
		{"GET", "/api/v1/tracker-routes?connection=gh", "", 200, mcp.ToolListTrackerRoutes, mcp.TrackerConnectionFilterInput{Connection: "gh"}},
		{"POST", "/api/v1/tracker-routes", `{"connection":"gh","project":"app","match":{"labels":["x"]},"states":{"done":"closed"}}`, 201, mcp.ToolCreateTrackerRoute,
			mcp.CreateTrackerRouteInput{Connection: "gh", Project: "app", Match: domain.TrackerMatch{Labels: []string{"x"}}, States: map[domain.TicketState]string{domain.TicketDone: "closed"}}},
		{"DELETE", "/api/v1/tracker-routes/r1", "", 200, mcp.ToolDeleteTrackerRoute, mcp.TrackerRouteRefInput{Route: "r1"}},
		{"GET", "/api/v1/tracker-identities", "", 200, mcp.ToolListTrackerIdentities, mcp.TrackerConnectionFilterInput{}},
		{"POST", "/api/v1/tracker-identities", `{"connection":"gh","external_id":"11","user":"u1"}`, 201, mcp.ToolMapTrackerIdentity, mcp.MapTrackerIdentityInput{Connection: "gh", ExternalID: "11", User: "u1"}},
		{"DELETE", "/api/v1/tracker-identities/i1", "", 200, mcp.ToolUnmapTrackerIdentity, mcp.TrackerIdentityRefInput{Identity: "i1"}},
		{"GET", "/api/v1/tickets/waiting?project=app", "", 200, mcp.ToolListTicketsWaiting, mcp.ListTicketsWaitingInput{Project: "app"}},
		{"POST", "/api/v1/tickets/t1/accept", `{"project":"app"}`, 200, mcp.ToolAcceptTicket, mcp.AcceptTicketInput{Ticket: "t1", Project: "app"}},
		{"POST", "/api/v1/tickets/t1/accept", "", 200, mcp.ToolAcceptTicket, mcp.AcceptTicketInput{Ticket: "t1"}},
		{"POST", "/api/v1/tickets/t1/dismiss", `{"reason":"no"}`, 200, mcp.ToolDismissTicket, mcp.DismissTicketInput{Ticket: "t1", Reason: "no"}},
		{"GET", "/api/v1/projects/acme/app", "", 200, mcp.ToolGetProject, mcp.ProjectInput{Project: "acme/app"}},
		{"POST", "/api/v1/goals", `{"project":"app","title":"Login","body":"b","platforms":["web"]}`, 201, mcp.ToolCreateGoal, mcp.CreateGoalInput{Project: "app", Title: "Login", Body: "b", Platforms: []string{"web"}}},
		{"POST", "/api/v1/bugs", `{"project":"app","title":"Crash","body":"b","platforms":["ios"]}`, 201, mcp.ToolReportBug, mcp.ReportBugInput{Project: "app", Title: "Crash", Body: "b", Platforms: []string{"ios"}}},
		{"GET", "/api/v1/goals?project=app&status=draft", "", 200, mcp.ToolListGoals, mcp.ListGoalsInput{Project: "app", Status: "draft"}},
		{"GET", "/api/v1/goals/GOAL-2", "", 200, mcp.ToolGetGoal, mcp.GoalRefInput{Goal: "GOAL-2"}},
		{"POST", "/api/v1/jobs", `{"project":"app","role":"reviewer","prompt":"look","needs":["linux"]}`, 201, mcp.ToolDispatchJob, mcp.DispatchJobInput{Project: "app", Role: "reviewer", Prompt: "look", Needs: []string{"linux"}}},
		{"GET", "/api/v1/jobs?project=app&goal=GOAL-1&status=queued&limit=5", "", 200, mcp.ToolListJobs, mcp.ListJobsInput{Project: "app", Goal: "GOAL-1", Status: "queued", Limit: 5}},
		{"GET", "/api/v1/jobs/JOB-41", "", 200, mcp.ToolJobStatus, mcp.JobRefInput{Job: "JOB-41"}},
		{"GET", "/api/v1/jobs/JOB-41/logs?after_seq=7&limit=10", "", 200, mcp.ToolJobLogs, mcp.JobLogsInput{Job: "JOB-41", AfterSeq: 7, Limit: 10}},
		{"GET", "/api/v1/workers", "", 200, mcp.ToolListWorkers, nil},
		{"GET", "/api/v1/machines?state=busy&limit=5", "", 200, mcp.ToolListMachines, mcp.ListMachinesInput{State: "busy", Limit: 5}},
		{"GET", "/api/v1/shapes", "", 200, mcp.ToolListShapes, nil},
		{"GET", "/api/v1/providers", "", 200, mcp.ToolListProviders, mcp.ListProvidersInput{}},
		{"GET", "/api/v1/fleet", "", 200, mcp.ToolFleetOverview, nil},
		{"GET", "/api/v1/images?name=linux", "", 200, mcp.ToolListImages, mcp.ListImagesInput{Name: "linux"}},
		{"GET", "/api/v1/config/schema?kind=shape", "", 200, mcp.ToolConfigSchema, mcp.ConfigKindInput{Kind: "shape"}},
		{"GET", "/api/v1/config/resources?kind=team&name=core", "", 200, mcp.ToolConfigGet, mcp.ConfigGetInput{Kind: "team", Name: "core"}},
		{"GET", "/api/v1/config/export", "", 200, mcp.ToolConfigExport, nil},
		{"POST", "/api/v1/config/import", `{"yaml":"y"}`, 201, mcp.ToolConfigImport, mcp.ConfigImportInput{YAML: "y", Source: domain.ConfigFromYAML}},
		{"GET", "/api/v1/config/plans?inbox=true&limit=3", "", 200, mcp.ToolConfigPlans, mcp.ConfigPlansInput{Inbox: true, Limit: 3}},
		{"POST", "/api/v1/config/plans", `{"title":"t","changes":[],"source":"cli"}`, 201, mcp.ToolConfigPlan, mcp.ConfigPlanInput{Title: "t", Changes: []mcp.ConfigWanted{}, Source: domain.ConfigFromCLI}},
		{"GET", "/api/v1/config/plans/p1", "", 200, mcp.ToolConfigGetPlan, mcp.ConfigPlanRefInput{Plan: "p1"}},
		{"POST", "/api/v1/config/plans/p1/request-approval", "", 200, mcp.ToolConfigApply, mcp.ConfigPlanRefInput{Plan: "p1"}},
		{"POST", "/api/v1/config/plans/p1/revert", "", 201, mcp.ToolConfigRevert, mcp.ConfigPlanRefInput{Plan: "p1"}},
		{"POST", "/api/v1/config/plans/p1/cancel", "", 200, mcp.ToolConfigCancel, mcp.ConfigPlanRefInput{Plan: "p1"}},
		{"POST", "/api/v1/config/plans/p1/apply", `{"word":"acme"}`, 200, "config_apply_confirmed", mcp.ConfigApplyInput{Plan: "p1", Word: "acme"}},
		{"PUT", "/api/v1/config/plans/p1/secrets/conn-okta", `{"value":"v"}`, 200, "config_put_secret", mcp.ConfigSecretInput{Plan: "p1", Secret: "conn-okta"}},
		{"POST", "/api/v1/assistant/messages", `{"message":"hi"}`, 202, mcp.ToolAssistantSend, mcp.AssistantSendInput{Message: "hi"}},
		{"GET", "/api/v1/assistant/conversations?limit=2", "", 200, mcp.ToolAssistantList, mcp.AssistantListInput{Limit: 2}},
		{"GET", "/api/v1/assistant/conversations/c1", "", 200, mcp.ToolAssistantGet, mcp.AssistantRefInput{Conversation: "c1"}},
		{"POST", "/api/v1/providers", `{"name":"home","kind":"microvm","org":"acme"}`, 201, mcp.ToolCreateProvider, mcp.CreateProviderInput{Name: "home", Kind: domain.ProviderMicroVM, Org: "acme"}},
		{"POST", "/api/v1/providers/home/revoke", "", 200, mcp.ToolRevokeProvider, mcp.ProviderRefInput{Provider: "home"}},
		{"POST", "/api/v1/join-codes", `{"cloud":true}`, 201, mcp.ToolCreateJoinCode, mcp.CreateJoinCodeInput{Cloud: true}},
		{"GET", "/api/v1/join-codes", "", 200, mcp.ToolListJoinCodes, mcp.ListJoinCodesInput{}},
		{"POST", "/api/v1/join-codes/abc/confirm", `{"name":"home","facts_hash":"h"}`, 200, mcp.ToolConfirmJoin, mcp.ConfirmJoinInput{Join: "abc", Name: "home", FactsHash: "h"}},
		{"GET", "/api/v1/gates?project=app", "", 200, mcp.ToolListPendingGates, mcp.ProjectFilterInput{Project: "app"}},
		{"POST", "/api/v1/gates/GATE-7/approve", `{"feedback":"ok"}`, 200, mcp.ToolApprove, mcp.GateDecisionInput{Gate: "GATE-7", Feedback: "ok"}},
		{"POST", "/api/v1/gates/GATE-7/approve", "", 200, mcp.ToolApprove, mcp.GateDecisionInput{Gate: "GATE-7"}},
		{"POST", "/api/v1/gates/GATE-7/approve", `{"feedback":"keep it","save_as_lesson":true}`, 200, mcp.ToolApprove, mcp.GateDecisionInput{Gate: "GATE-7", Feedback: "keep it", SaveAsLesson: true}},
		{"POST", "/api/v1/gates/GATE-7/request-changes", `{"feedback":"more tests"}`, 200, mcp.ToolRequestChanges, mcp.GateDecisionInput{Gate: "GATE-7", Feedback: "more tests"}},
		{"POST", "/api/v1/gates/GATE-7/reject", `{}`, 200, mcp.ToolReject, mcp.GateDecisionInput{Gate: "GATE-7"}},
		{"POST", "/api/v1/jobs/JOB-41/cancel", "", 200, mcp.ToolCancelJob, mcp.JobRefInput{Job: "JOB-41"}},
		{"GET", "/api/v1/tokens", "", 200, mcp.ToolListTokens, nil},
		{"GET", "/api/v1/jobs/JOB-41/artifacts", "", 200, mcp.ToolListArtifacts, mcp.JobRefInput{Job: "JOB-41"}},
		{"GET", "/api/v1/test-runs?job=JOB-41&project=app&change=7&sha=abc&limit=5", "", 200, mcp.ToolListTestRuns,
			mcp.ListTestRunsInput{Job: "JOB-41", Project: "app", Change: 7, SHA: "abc", Limit: 5}},
		{"GET", "/api/v1/knowledge?project=app&q=queue&section=architecture&kind=note&provider=repo&limit=3", "", 200, mcp.ToolSearchKnowledge,
			mcp.SearchKnowledgeInput{Project: "app", Query: "queue", Section: "architecture", Kind: "note", Provider: "repo", Limit: 3}},
		{"DELETE", "/api/v1/knowledge/notes/4bf2c2f4-0000-7000-8000-000000000001", "", 200, mcp.ToolDeleteNote, mcp.DeleteNoteInput{Note: "4bf2c2f4-0000-7000-8000-000000000001"}},
		{"GET", "/api/v1/knowledge/proposals?project=app&goal=GOAL-1&status=open&limit=5", "", 200, mcp.ToolListKnowledgeProposals,
			mcp.ListKnowledgeProposalsInput{Project: "app", Goal: "GOAL-1", Status: domain.KnowledgeOpen, Limit: 5}},
		{"GET", "/api/v1/knowledge/entries?project=app&section=vision&kind=entry&provider=repo&goal=GOAL-1&limit=5", "", 200, mcp.ToolListKnowledge,
			mcp.ListKnowledgeInput{Project: "app", Section: "vision", Kind: "entry", Provider: "repo", Goal: "GOAL-1", Limit: 5}},
		{"POST", "/api/v1/knowledge/proposals", `{"project": "app", "section": "lessons", "path": "a.md", "body": "b", "reason": "r", "action": "add"}`, 201, mcp.ToolProposeKnowledge,
			mcp.ProposeKnowledgeInput{Project: "app", Section: "lessons", Path: "a.md", Body: "b", Reason: "r", Action: domain.KnowledgeAdd}},
		{"GET", "/api/v1/trace?ref=acme%2Fapp%2312", "", 200, mcp.ToolGetTrace, mcp.GetTraceInput{Ref: "acme/app#12"}},
		{"POST", "/api/v1/tokens/old-laptop/revoke", "", 200, mcp.ToolRevokeToken, mcp.RevokeTokenInput{Token: "old-laptop"}},
		{"GET", "/api/v1/proposals?project=app&status=snoozed&category=ci", "", 200, mcp.ToolListProposals, mcp.ListProposalsInput{Project: "app", Status: "snoozed", Category: "ci"}},
		{"POST", "/api/v1/proposals/PROP-3/decide", `{"decision":"snooze","reason":"later","until":"2030-01-02T03:04:05Z"}`, 200, mcp.ToolDecideProposal,
			mcp.DecideProposalInput{Proposal: "PROP-3", Decision: "snooze", Reason: "later", Until: "2030-01-02T03:04:05Z"}},
		{"GET", "/api/v1/assumptions?project=app&goal=GOAL-1&task=TASK-2&status=unverified&impact=high&verify_by=test", "", 200, mcp.ToolListAssumptions,
			mcp.ListAssumptionsInput{Project: "app", Goal: "GOAL-1", Task: "TASK-2", Status: "unverified", Impact: "high", VerifyBy: "test"}},
		{"POST", "/api/v1/assumptions/A-42/resolve", `{"status":"invalidated","evidence":"it scales out"}`, 200, mcp.ToolResolveAssumption,
			mcp.ResolveAssumptionInput{Assumption: "A-42", Status: "invalidated", Evidence: "it scales out"}},
		{"GET", "/api/v1/decisions?project=app&goal=GOAL-1&council=0192f5a0-0000-7000-8000-00000000000c", "", 200, mcp.ToolListDecisions,
			mcp.ListDecisionsInput{Project: "app", Goal: "GOAL-1", Council: "0192f5a0-0000-7000-8000-00000000000c"}},
		{"POST", "/api/v1/councils", `{"project":"app","question":"Which queue?","kind":"technical","roles":["ux"]}`, 202, mcp.ToolStartCouncil,
			mcp.StartCouncilInput{Project: "app", Question: "Which queue?", Kind: "technical", Roles: []string{"ux"}}},
		{"GET", "/api/v1/councils?project=app&status=running", "", 200, mcp.ToolListCouncils, mcp.ListCouncilsInput{Project: "app", Status: "running"}},
		{"GET", "/api/v1/councils/COUNCIL-2", "", 200, mcp.ToolGetCouncil, mcp.CouncilRefInput{Council: "COUNCIL-2"}},
		{"POST", "/api/v1/councils/COUNCIL-2/constraints", `{"constraint":"Stay on Postgres"}`, 200, mcp.ToolAddConstraint, mcp.AddCouncilConstraintInput{Council: "COUNCIL-2", Constraint: "Stay on Postgres"}},
		{"POST", "/api/v1/tokens", `{"name":"ci","scopes":["read"],"ttl":"24h","service_account":"bot"}`, 201, mcp.ToolCreateToken,
			mcp.CreateTokenInput{Name: "ci", Scopes: []domain.Scope{domain.ScopeRead}, TTL: "24h", ServiceAccount: "bot"}},
		{"POST", "/api/v1/invitations", `{"email":"a@b.c","roles":[{"role":"viewer","project":"app"}]}`, 201, mcp.ToolCreateInvitation,
			mcp.CreateInvitationInput{Email: "a@b.c", Roles: []mcp.RoleGrant{{Role: "viewer", Project: "app"}}}},
		{"GET", "/api/v1/invitations", "", 200, mcp.ToolListInvitations, nil},
		{"POST", "/api/v1/invitations/some-id/revoke", "", 200, mcp.ToolRevokeInvitation, mcp.InvitationRefInput{Invitation: "some-id"}},
		{"GET", "/api/v1/connected-apps", "", 200, mcp.ToolListConnectedApps, nil},
		{"POST", "/api/v1/connected-apps/some-id/revoke", "", 200, mcp.ToolRevokeConnectedApp, mcp.ConnectedAppInput{App: "some-id"}},
		{"GET", "/api/v1/changes/9/verification?project=app", "", 200, mcp.ToolVerificationReport, mcp.VerificationReportInput{Change: "9", Project: "app"}},
		{"GET", "/api/v1/changes/TASK-12/verification", "", 200, mcp.ToolVerificationReport, mcp.VerificationReportInput{Change: "TASK-12"}},
		{"GET", "/api/v1/roles", "", 200, mcp.ToolListRoles, nil},
		{"GET", "/api/v1/teams", "", 200, mcp.ToolListTeams, nil},
		{"POST", "/api/v1/teams", `{"name":"Core","slug":"core"}`, 201, mcp.ToolCreateTeam, mcp.CreateTeamInput{Name: "Core", Slug: "core"}},
		{"PATCH", "/api/v1/teams/core", `{"name":"Base"}`, 200, mcp.ToolUpdateTeam, mcp.UpdateTeamInput{Team: "core", Name: "Base"}},
		{"DELETE", "/api/v1/teams/core", "", 200, mcp.ToolDeleteTeam, mcp.TeamRefInput{Team: "core"}},
		{"GET", "/api/v1/teams/core/members", "", 200, mcp.ToolListTeamMembers, mcp.TeamRefInput{Team: "core"}},
		{"POST", "/api/v1/teams/core/members", `{"user":"u"}`, 201, mcp.ToolAddTeamMember, mcp.TeamMemberInput{Team: "core", User: "u"}},
		{"DELETE", "/api/v1/teams/core/members/u", "", 200, mcp.ToolRemoveTeamMember, mcp.TeamMemberInput{Team: "core", User: "u"}},
		{"GET", "/api/v1/role-bindings?project=app&subject_kind=team&subject=core", "", 200, mcp.ToolListRoleBindings,
			mcp.ListRoleBindingsInput{Project: "app", SubjectKind: domain.SubjectTeam, Subject: "core"}},
		{"POST", "/api/v1/role-bindings", `{"subject_kind":"team","subject":"core","role":"viewer","project":"app"}`, 201, mcp.ToolCreateRoleBinding,
			mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "viewer", Project: "app"}},
		{"DELETE", "/api/v1/role-bindings/b", "", 200, mcp.ToolDeleteRoleBinding, mcp.RoleBindingRefInput{Binding: "b"}},
		{"GET", "/api/v1/service-accounts", "", 200, mcp.ToolListServiceAccounts, nil},
		{"POST", "/api/v1/service-accounts", `{"name":"ci"}`, 201, mcp.ToolCreateServiceAccount, mcp.CreateServiceAccountInput{Name: "ci"}},
		{"PATCH", "/api/v1/service-accounts/ci", `{"name":"build"}`, 200, mcp.ToolUpdateServiceAccount, mcp.UpdateServiceAccountInput{ServiceAccount: "ci", Name: "build"}},
		{"POST", "/api/v1/service-accounts/ci/disable", "", 200, mcp.ToolSetServiceAccount, mcp.SetServiceAccountInput{ServiceAccount: "ci", Disabled: true}},
		{"POST", "/api/v1/service-accounts/ci/enable", "", 200, mcp.ToolSetServiceAccount, mcp.SetServiceAccountInput{ServiceAccount: "ci"}},
		{"GET", "/api/v1/changes/https%3A%2F%2Fgithub.com%2Facme%2Fapp%2Fpull%2F9/verification", "", 200, mcp.ToolVerificationReport, mcp.VerificationReportInput{Change: "https://github.com/acme/app/pull/9"}},
		{"GET", "/api/v1/quality?project=app", "", 200, mcp.ToolProjectQuality, mcp.ProjectInput{Project: "app"}},
		{"GET", "/api/v1/members", "", 200, mcp.ToolListMembers, nil},
		{"POST", "/api/v1/members/u1/suspend", "", 200, mcp.ToolSetMemberSuspended, mcp.SetMemberInput{User: "u1", Suspended: true}},
		{"POST", "/api/v1/members/u1/resume", "", 200, mcp.ToolSetMemberSuspended, mcp.SetMemberInput{User: "u1"}},
		{"DELETE", "/api/v1/members/u1", "", 200, mcp.ToolRemoveMember, mcp.MemberRefInput{User: "u1"}},
		{"GET", "/api/v1/audit?actor=user%3Ax&action=team.created&subject_type=team&subject=s&since=2026-01-01T00%3A00%3A00Z&until=2026-02-01T00%3A00%3A00Z&limit=5", "", 200, mcp.ToolListAudit,
			mcp.ListAuditInput{Actor: "user:x", Action: "team.created", SubjectType: "team", Subject: "s", Since: "2026-01-01T00:00:00Z", Until: "2026-02-01T00:00:00Z", Limit: 5}},
		{"GET", "/api/v1/connections", "", 200, mcp.ToolListConnections, nil},
		{"POST", "/api/v1/connections", `{"kind":"oidc","name":"okta","issuer":"https://acme.okta.com","client_id":"c","client_secret_ref":"env:FORECREW_CONN_OKTA"}`, 201, mcp.ToolCreateConnection,
			mcp.CreateConnectionInput{Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://acme.okta.com", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_OKTA"}},
		{"POST", "/api/v1/connections/okta/enable", "", 200, mcp.ToolSetConnection, mcp.SetConnectionInput{Connection: "okta", Enabled: true}},
		{"POST", "/api/v1/connections/okta/disable", "", 200, mcp.ToolSetConnection, mcp.SetConnectionInput{Connection: "okta"}},
		{"PATCH", "/api/v1/organization", `{"name":"Acme Inc","slug":"acme-inc"}`, 200, mcp.ToolUpdateOrganization, mcp.UpdateOrganizationInput{Name: "Acme Inc", Slug: "acme-inc"}},
	}
	seen := map[string]bool{}
	for i, tt := range tests {
		r := e.do(t, tt.method, tt.path, tt.body, "bearer")
		if r.status != tt.status || !json.Valid([]byte(r.body)) || r.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d %q (%s)", tt.method, tt.path, r.status, r.body, r.header.Get("Content-Type"))
			continue
		}
		seen[tt.tool] = true
		calls := e.svc.Calls()
		if len(calls) != i+1 {
			t.Fatalf("%s %s: %d calls recorded", tt.method, tt.path, len(calls))
		}
		if got := calls[i]; got.Tool != tt.tool || got.Actor != e.actor || fmt.Sprint(got.Input) != fmt.Sprint(tt.input) {
			t.Errorf("%s %s: recorded %+v, want %s %+v by %v", tt.method, tt.path, got, tt.tool, tt.input, e.actor)
		}
	}
	for _, tool := range mcp.Tools() {
		if !seen[tool] {
			t.Errorf("no endpoint test for tool %s", tool)
		}
	}
}

func TestListsAreArrays(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/projects", "/api/v1/workers", "/api/v1/machines", "/api/v1/shapes", "/api/v1/providers", "/api/v1/gates", "/api/v1/goals", "/api/v1/jobs", "/api/v1/tokens", "/api/v1/jobs/JOB-1/artifacts", "/api/v1/test-runs?sha=abc", "/api/v1/assumptions?project=app", "/api/v1/decisions?project=app", "/api/v1/knowledge?project=app&q=x", "/api/v1/members", "/api/v1/audit", "/api/v1/connections"} {
		if r := e.do(t, "GET", path, "", "bearer"); strings.TrimSpace(r.body) != "[]" {
			t.Errorf("GET %s = %q, want []", path, r.body)
		}
	}
	e.svc.GetGoalFunc = func(context.Context, domain.Actor, mcp.GoalRefInput) (mcp.GoalDetail, error) {
		return mcp.GoalDetail{Ref: "GOAL-2", Goal: domain.Goal{Title: "Login"}}, nil
	}
	r := e.do(t, "GET", "/api/v1/goals/GOAL-2", "", "bearer")
	var d mcp.GoalDetail
	if err := json.Unmarshal([]byte(r.body), &d); err != nil || d.Ref != "GOAL-2" || d.Goal.Title != "Login" {
		t.Errorf("goal = %q, %v", r.body, err)
	}
}

func TestToolErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
		notIn  string
	}{
		{"not found", fmt.Errorf("job JOB-9: %w", mcp.ErrNotFound), 404, "not_found", ""},
		{"invalid input", fmt.Errorf("gate is required: %w", mcp.ErrInvalidInput), 400, "invalid_input", ""},
		{"invalid ref", fmt.Errorf("x: %w", domain.ErrInvalidRef), 400, "invalid_input", ""},
		{"invalid decision", fmt.Errorf("x: %w", domain.ErrInvalidDecision), 400, "invalid_input", ""},
		{"conflict", fmt.Errorf("gate: %w", domain.ErrInvalidTransition), 409, "conflict", ""},
		{"unavailable", fmt.Errorf("no evidence storage: %w", mcp.ErrUnavailable), 503, "unavailable", ""},
		{"internal", errors.New("pq: connection refused password=hunter2"), 500, "internal", "hunter2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.svc.JobStatusFunc = func(context.Context, domain.Actor, mcp.JobRefInput) (mcp.JobInfo, error) {
				return mcp.JobInfo{}, tt.err
			}
			r := e.do(t, "GET", "/api/v1/jobs/JOB-9", "", "bearer")
			if r.status != tt.status || errorCode(t, r) != tt.code {
				t.Errorf("%d %s, want %d %s", r.status, r.body, tt.status, tt.code)
			}
			if tt.notIn != "" {
				if strings.Contains(r.body, tt.notIn) {
					t.Errorf("response leaks %q: %s", tt.notIn, r.body)
				}
				if !strings.Contains(e.logs.String(), tt.notIn) {
					t.Errorf("cause not logged: %s", e.logs)
				}
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"malformed json", "POST", "/api/v1/goals", `{"project":`, 400, "bad_request"},
		{"unknown field", "POST", "/api/v1/goals", `{"project":"a","title":"t","extra":1}`, 400, "bad_request"},
		{"trailing data", "POST", "/api/v1/projects", `{"repo":"a/b"} {}`, 400, "bad_request"},
		{"wrong type", "POST", "/api/v1/goals", `{"project":1}`, 400, "bad_request"},
		{"resolve with a body field for the path", "POST", "/api/v1/assumptions/A-1/resolve", `{"assumption":"A-2"}`, 400, "bad_request"},
		{"gate field in body", "POST", "/api/v1/gates/GATE-1/approve", `{"gate":"GATE-2"}`, 400, "bad_request"},
		{"bad limit", "GET", "/api/v1/jobs?limit=ten", "", 400, "bad_request"},
		{"bad test-runs limit", "GET", "/api/v1/test-runs?limit=x", "", 400, "bad_request"},
		{"bad constraint", "POST", "/api/v1/councils/COUNCIL-2/constraints", `{"text":"x"}`, 400, "bad_request"},
		{"bad test-runs change", "GET", "/api/v1/test-runs?change=x", "", 400, "bad_request"},
		{"bad after_seq", "GET", "/api/v1/jobs/JOB-1/logs?after_seq=x", "", 400, "bad_request"},
		{"bad logs limit", "GET", "/api/v1/jobs/JOB-1/logs?limit=x", "", 400, "bad_request"},
		{"too large", "POST", "/api/v1/goals", `{"project":"` + strings.Repeat("a", api.MaxBodyBytes) + `"}`, 413, "too_large"},
		{"no such endpoint", "GET", "/api/v1/nope", "", 404, "not_found"},
	}
	for _, tt := range tests {
		r := e.do(t, tt.method, tt.path, tt.body, "bearer")
		if r.status != tt.status || errorCode(t, r) != tt.code {
			t.Errorf("%s: %d %.80s, want %d %s", tt.name, r.status, r.body, tt.status, tt.code)
		}
	}
	if n := len(e.svc.Calls()); n != 0 {
		t.Errorf("service called %d times for bad requests", n)
	}
}

func TestBearerAuth(t *testing.T) {
	e := newEnv(t)
	tests := []struct{ name, header string }{
		{"missing", ""}, {"not bearer", "Basic abc"}, {"unknown", "Bearer fcm_nope"}, {"revoked", "Bearer fcm_old"},
	}
	for _, tt := range tests {
		req, err := http.NewRequestWithContext(t.Context(), "GET", e.server.URL+"/api/v1/workers", nil)
		must(t, err)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		if r := e.send(t, req); r.status != 401 || errorCode(t, r) != "unauthorized" {
			t.Errorf("%s: %d %s", tt.name, r.status, r.body)
		}
	}
	if n := len(e.svc.Calls()); n != 0 {
		t.Errorf("service called %d times without a valid token", n)
	}
}

func TestMe(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "GET", "/api/v1/me", "", "bearer")
	var s api.Session
	if err := json.Unmarshal([]byte(r.body), &s); err != nil || r.status != 200 || s.Token != "laptop" || s.Account.ID != e.account.ID || s.Account.Name != "acme" {
		t.Errorf("me = %d %s, %v", r.status, r.body, err)
	}
}

func TestMeWithMissingAccount(t *testing.T) {
	e := newEnv(t)
	other := domain.Account{ID: domain.NewID(), Name: "gone"}
	must(t, e.store.Accounts().Create(t.Context(), &other))
	st := testkit.NewStore(nil) // tokens know an account that this store lacks
	must(t, st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: domain.NewID(), Name: "t", TokenHash: mcp.HashToken("fcm_t")}))
	srv := httptest.NewServer(api.New(api.Config{Tools: e.svc, Store: st, Authz: authz.New(st, nil), Logger: slog.New(slog.DiscardHandler), TokenLogin: true}).Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/api/v1/me", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer fcm_t")
	resp, err := srv.Client().Do(req)
	must(t, err)
	must(t, resp.Body.Close())
	if resp.StatusCode != 500 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func sessionCookieOf(t *testing.T, r response) *http.Cookie {
	t.Helper()
	resp := http.Response{Header: r.header}
	for _, c := range resp.Cookies() {
		if c.Name == api.CookieName {
			return c
		}
	}
	return nil
}

func TestLoginSetsCookie(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	if r.status != 200 {
		t.Fatalf("login = %d %s", r.status, r.body)
	}
	c := sessionCookieOf(t, r)
	if c == nil || c.Value != token || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", c)
	}
	var s api.Session
	if err := json.Unmarshal([]byte(r.body), &s); err != nil || s.Token != "laptop" || s.Account.Name != "acme" {
		t.Errorf("body = %s, %v", r.body, err)
	}
}

func TestCookieRequestsWork(t *testing.T) {
	e := newEnv(t)
	if r := e.do(t, "GET", "/api/v1/workers", "", ""); r.status != 401 {
		t.Fatalf("before login = %d", r.status)
	}
	if r := e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, ""); r.status != 200 {
		t.Fatalf("login = %d %s", r.status, r.body)
	}
	if r := e.do(t, "GET", "/api/v1/me", "", ""); r.status != 200 {
		t.Errorf("GET /me with cookie = %d %s", r.status, r.body)
	}
	if r := e.do(t, "POST", "/api/v1/gates/GATE-1/approve", `{"feedback":"ok"}`, ""); r.status != 200 {
		t.Errorf("JSON POST with cookie = %d %s", r.status, r.body)
	}
	calls := e.svc.Calls()
	if len(calls) != 1 || calls[0].Actor != e.actor {
		t.Errorf("calls = %+v", calls)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	e := newEnv(t)
	e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	r := e.do(t, "DELETE", "/api/v1/session", "{}", "")
	c := sessionCookieOf(t, r)
	if r.status != 204 || c == nil || c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("logout = %d, cookie %+v", r.status, c)
	}
	if r := e.do(t, "GET", "/api/v1/me", "", ""); r.status != 401 {
		t.Errorf("after logout = %d", r.status)
	}
}

func TestLoginRefusals(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name, body string
		status     int
	}{
		{"bad token", `{"token":"fcm_nope"}`, 401},
		{"revoked token", `{"token":"fcm_old"}`, 401},
		{"no token", `{}`, 400},
		{"not json", `token=abc`, 400},
	}
	for _, tt := range tests {
		r := e.do(t, "POST", "/api/v1/session", tt.body, "")
		if r.status != tt.status || sessionCookieOf(t, r) != nil {
			t.Errorf("%s: %d %s cookie=%v", tt.name, r.status, r.body, sessionCookieOf(t, r))
		}
	}
}

func TestLoginRequiresJSON(t *testing.T) {
	e := newEnv(t)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		// What a cross-site form can send: a body that is also valid JSON.
		req, err := http.NewRequestWithContext(t.Context(), "POST", e.server.URL+"/api/v1/session", strings.NewReader(`{"token":"`+token+`","x":"="}`))
		must(t, err)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		r := e.send(t, req)
		if r.status != 415 || errorCode(t, r) != "unsupported_media_type" || sessionCookieOf(t, r) != nil {
			t.Errorf("content type %q: %d %s", ct, r.status, r.body)
		}
	}
}

func TestSessionHygiene(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("login Cache-Control = %q", got)
	}
	if got := e.do(t, "GET", "/api/v1/me", "", "").header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("me Cache-Control = %q", got)
	}
	if got := e.do(t, "DELETE", "/api/v1/session", "", "").header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("logout Cache-Control = %q", got)
	}

	// A cookie whose token was revoked is cleared by the 401.
	e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	tok, err := e.store.MCPTokens().GetByTokenHash(t.Context(), mcp.HashToken(token))
	must(t, err)
	must(t, e.store.MCPTokens().Revoke(t.Context(), tok.AccountID, tok.ID))
	r = e.do(t, "GET", "/api/v1/me", "", "")
	c := sessionCookieOf(t, r)
	if r.status != 401 || c == nil || c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("revoked cookie: %d, cookie %+v", r.status, c)
	}
	if r = e.do(t, "GET", "/api/v1/me", "", ""); r.status != 401 || sessionCookieOf(t, r) != nil {
		t.Errorf("after clearing: %d, cookie %+v", r.status, sessionCookieOf(t, r))
	}
	// A bad bearer header does not touch cookies.
	if r := e.do(t, "GET", "/api/v1/me", "", "bad"); sessionCookieOf(t, r) != nil {
		t.Error("401 without a cookie sets one")
	}
}

func TestCookieRequiresJSONForChanges(t *testing.T) {
	e := newEnv(t)
	e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", "multipart/form-data; boundary=x", ""} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", e.server.URL+"/api/v1/gates/GATE-1/approve", strings.NewReader("feedback=ok"))
		must(t, err)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if r := e.send(t, req); r.status != 415 || errorCode(t, r) != "unsupported_media_type" {
			t.Errorf("content type %q: %d %s", ct, r.status, r.body)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.server.URL+"/api/v1/gates/GATE-1/approve", strings.NewReader(`{}`))
	must(t, err)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if r := e.send(t, req); r.status != 200 {
		t.Errorf("json with charset = %d %s", r.status, r.body)
	}
	if n := len(e.svc.Calls()); n != 1 {
		t.Errorf("service called %d times, want 1", n)
	}
}

func TestBearerHeaderBeatsCookie(t *testing.T) {
	e := newEnv(t)
	e.do(t, "POST", "/api/v1/session", `{"token":"`+token+`"}`, "")
	req, err := http.NewRequestWithContext(t.Context(), "GET", e.server.URL+"/api/v1/me", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer fcm_nope")
	if r := e.send(t, req); r.status != 401 {
		t.Errorf("bad header with good cookie = %d", r.status)
	}
	// A header skips the media type rule: no cookie is involved.
	req, err = http.NewRequestWithContext(t.Context(), "POST", e.server.URL+"/api/v1/gates/GATE-1/approve", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if r := e.send(t, req); r.status != 200 {
		t.Errorf("bearer POST without body = %d %s", r.status, r.body)
	}
}

// TestCreateGoalCouncilsOverREST: councils: [] reaches the service as an
// override of none; an absent field stays nil (triage sizes the goal).
func TestCreateGoalCouncilsOverREST(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, http.MethodPost, "/api/v1/goals", `{"project":"app","title":"Login","body":"b","councils":[]}`, "bearer")
	if r.status != http.StatusCreated {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	r = e.do(t, http.MethodPost, "/api/v1/goals", `{"project":"app","title":"Login","body":"b"}`, "bearer")
	if r.status != http.StatusCreated {
		t.Fatalf("status = %d %s", r.status, r.body)
	}
	calls := e.svc.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls recorded", len(calls))
	}
	if got := calls[0].Input.(mcp.CreateGoalInput).Councils; got == nil || len(*got) != 0 {
		t.Errorf("empty councils = %v", got)
	}
	if got := calls[1].Input.(mcp.CreateGoalInput).Councils; got != nil {
		t.Errorf("absent councils = %v", *got)
	}
}

// TestApplyIsNotFromTheBrowserWithAToken: a token never counts as a person in
// the web UI, in a header or in the cookie of token_login, and nothing in the
// body can say so; a source of a model is refused.
func TestApplyIsNotFromTheBrowserWithAToken(t *testing.T) {
	e := newEnv(t)
	var got []mcp.ConfigApplyInput
	e.svc.ConfigApplyFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
		got = append(got, in)
		return mcp.ConfigApplied{}, nil
	}
	post := func(body string, cookie bool) int {
		req, err := http.NewRequestWithContext(t.Context(), "POST", e.server.URL+"/api/v1/config/plans/p/apply", strings.NewReader(body))
		must(t, err)
		req.Header.Set("Content-Type", "application/json")
		if cookie {
			req.AddCookie(&http.Cookie{Name: api.CookieName, Value: token})
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return e.send(t, req).status
	}
	if code := post(`{}`, false); code != 200 || got[0].Browser {
		t.Errorf("token: %d %+v", code, got)
	}
	if code := post(`{}`, true); code != 200 || got[1].Browser {
		t.Errorf("token in the cookie: %d %+v", code, got)
	}
	if code := post(`{"browser":true}`, false); code != 400 {
		t.Errorf("a body claiming the browser: %d", code)
	}
	if r := e.do(t, "POST", "/api/v1/config/plans", `{"changes":[],"source":"assistant"}`, "bearer"); r.status != 400 {
		t.Errorf("source assistant over REST: %d", r.status)
	}
}

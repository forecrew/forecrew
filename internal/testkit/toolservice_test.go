package testkit

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestToolServiceDefaults(t *testing.T) {
	s := &ToolService{}
	ctx := t.Context()
	by := domain.Actor{Kind: domain.ActorMCP, Name: "laptop"}
	calls := []func() error{
		func() error {
			_, err := s.OnboardProject(ctx, by, mcp.OnboardProjectInput{Repo: "acme/app"})
			return err
		},
		func() error { _, err := s.CreateGoal(ctx, by, mcp.CreateGoalInput{}); return err },
		func() error { _, err := s.ReportBug(ctx, by, mcp.ReportBugInput{}); return err },
		func() error { _, err := s.JobStatus(ctx, by, mcp.JobRefInput{}); return err },
		func() error { _, err := s.JobLogs(ctx, by, mcp.JobLogsInput{}); return err },
		func() error { _, err := s.ListWorkers(ctx, by); return err },
		func() error { _, err := s.ListPendingGates(ctx, by, mcp.ProjectFilterInput{}); return err },
		func() error { _, err := s.Approve(ctx, by, mcp.GateDecisionInput{}); return err },
		func() error { _, err := s.RequestChanges(ctx, by, mcp.GateDecisionInput{}); return err },
		func() error { _, err := s.Reject(ctx, by, mcp.GateDecisionInput{}); return err },
		func() error { _, err := s.DispatchJob(ctx, by, mcp.DispatchJobInput{}); return err },
		func() error { _, err := s.ListProjects(ctx, by); return err },
		func() error { _, err := s.GetProject(ctx, by, mcp.ProjectInput{}); return err },
		func() error { _, err := s.ListGoals(ctx, by, mcp.ListGoalsInput{}); return err },
		func() error { _, err := s.GetGoal(ctx, by, mcp.GoalRefInput{}); return err },
		func() error { _, err := s.ListJobs(ctx, by, mcp.ListJobsInput{}); return err },
		func() error { _, err := s.CancelJob(ctx, by, mcp.JobRefInput{}); return err },
		func() error { _, err := s.ListTokens(ctx, by); return err },
		func() error { _, err := s.RevokeToken(ctx, by, mcp.RevokeTokenInput{}); return err },
		func() error { _, err := s.ListArtifacts(ctx, by, mcp.JobRefInput{}); return err },
		func() error { _, err := s.ListTestRuns(ctx, by, mcp.ListTestRunsInput{}); return err },
		func() error { _, err := s.ListAssumptions(ctx, by, mcp.ListAssumptionsInput{}); return err },
		func() error { _, err := s.ResolveAssumption(ctx, by, mcp.ResolveAssumptionInput{}); return err },
		func() error { _, err := s.ListDecisions(ctx, by, mcp.ListDecisionsInput{}); return err },
		func() error { _, err := s.SearchKnowledge(ctx, by, mcp.SearchKnowledgeInput{}); return err },
		func() error { _, err := s.DeleteNote(ctx, by, mcp.DeleteNoteInput{}); return err },
		func() error { _, err := s.ProposeKnowledge(ctx, by, mcp.ProposeKnowledgeInput{}); return err },
		func() error { _, err := s.StartCouncil(ctx, by, mcp.StartCouncilInput{}); return err },
		func() error { _, err := s.ListCouncils(ctx, by, mcp.ListCouncilsInput{}); return err },
		func() error { _, err := s.GetCouncil(ctx, by, mcp.CouncilRefInput{}); return err },
		func() error { _, err := s.AddCouncilConstraint(ctx, by, mcp.AddCouncilConstraintInput{}); return err },
		func() error { _, err := s.VerificationReport(ctx, by, mcp.VerificationReportInput{}); return err },
		func() error { _, err := s.ProjectQuality(ctx, by, mcp.ProjectInput{}); return err },
		func() error { _, err := s.CreateToken(ctx, by, mcp.CreateTokenInput{}); return err },
		func() error { _, err := s.CreateInvitation(ctx, by, mcp.CreateInvitationInput{}); return err },
		func() error { _, err := s.ListInvitations(ctx, by); return err },
		func() error { _, err := s.RevokeInvitation(ctx, by, mcp.InvitationRefInput{}); return err },
		func() error { _, err := s.ListRoles(ctx, by); return err },
		func() error { _, err := s.ListTeams(ctx, by); return err },
		func() error { _, err := s.CreateTeam(ctx, by, mcp.CreateTeamInput{}); return err },
		func() error { _, err := s.UpdateTeam(ctx, by, mcp.UpdateTeamInput{}); return err },
		func() error { _, err := s.DeleteTeam(ctx, by, mcp.TeamRefInput{}); return err },
		func() error { _, err := s.ListTeamMembers(ctx, by, mcp.TeamRefInput{}); return err },
		func() error { _, err := s.AddTeamMember(ctx, by, mcp.TeamMemberInput{}); return err },
		func() error { _, err := s.RemoveTeamMember(ctx, by, mcp.TeamMemberInput{}); return err },
		func() error { _, err := s.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{}); return err },
		func() error { _, err := s.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{}); return err },
		func() error { _, err := s.DeleteRoleBinding(ctx, by, mcp.RoleBindingRefInput{}); return err },
		func() error { _, err := s.ListServiceAccounts(ctx, by); return err },
		func() error { _, err := s.CreateServiceAccount(ctx, by, mcp.CreateServiceAccountInput{}); return err },
		func() error { _, err := s.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{}); return err },
		func() error { _, err := s.SetServiceAccountDisabled(ctx, by, mcp.SetServiceAccountInput{}); return err },
		func() error { _, err := s.ListMachines(ctx, by, mcp.ListMachinesInput{}); return err },
		func() error { _, err := s.ListShapes(ctx, by); return err },
		func() error { _, err := s.ListProviders(ctx, by, mcp.ListProvidersInput{}); return err },
		func() error { _, err := s.FleetOverview(ctx, by); return err },
		func() error { _, err := s.ListTrackerConnections(ctx, by); return err },
		func() error {
			_, err := s.CreateTrackerConnection(ctx, by, mcp.CreateTrackerConnectionInput{})
			return err
		},
		func() error {
			_, err := s.DeleteTrackerConnection(ctx, by, mcp.TrackerConnectionRefInput{})
			return err
		},
		func() error {
			_, err := s.DiscardTrackerWrites(ctx, by, mcp.TrackerConnectionRefInput{})
			return err
		},
		func() error { _, err := s.ListTrackerRoutes(ctx, by, mcp.TrackerConnectionFilterInput{}); return err },
		func() error { _, err := s.CreateTrackerRoute(ctx, by, mcp.CreateTrackerRouteInput{}); return err },
		func() error { _, err := s.DeleteTrackerRoute(ctx, by, mcp.TrackerRouteRefInput{}); return err },
		func() error {
			_, err := s.ListTrackerIdentities(ctx, by, mcp.TrackerConnectionFilterInput{})
			return err
		},
		func() error { _, err := s.MapTrackerIdentity(ctx, by, mcp.MapTrackerIdentityInput{}); return err },
		func() error { _, err := s.UnmapTrackerIdentity(ctx, by, mcp.TrackerIdentityRefInput{}); return err },
		func() error { _, err := s.ListTicketsWaiting(ctx, by, mcp.ListTicketsWaitingInput{}); return err },
		func() error { _, err := s.AcceptTicket(ctx, by, mcp.AcceptTicketInput{}); return err },
		func() error { _, err := s.DismissTicket(ctx, by, mcp.DismissTicketInput{}); return err },
		func() error { _, err := s.GetTrace(ctx, by, mcp.GetTraceInput{}); return err },
		func() error {
			_, err := s.ListKnowledgeProposals(ctx, by, mcp.ListKnowledgeProposalsInput{})
			return err
		},
		func() error { _, err := s.ListKnowledge(ctx, by, mcp.ListKnowledgeInput{}); return err },
		func() error { _, err := s.ListImages(ctx, by, mcp.ListImagesInput{}); return err },
		func() error { _, err := s.ConfigSchema(ctx, by, mcp.ConfigKindInput{}); return err },
		func() error { _, err := s.ConfigGet(ctx, by, mcp.ConfigGetInput{}); return err },
		func() error { _, err := s.ConfigPlan(ctx, by, mcp.ConfigPlanInput{}); return err },
		func() error { _, err := s.ConfigRequestApproval(ctx, by, mcp.ConfigPlanRefInput{}); return err },
		func() error { _, err := s.ConfigRevert(ctx, by, mcp.ConfigPlanRefInput{}); return err },
		func() error { _, err := s.ConfigExport(ctx, by); return err },
		func() error { _, err := s.ConfigImport(ctx, by, mcp.ConfigImportInput{}); return err },
		func() error { _, err := s.ConfigPlans(ctx, by, mcp.ConfigPlansInput{}); return err },
		func() error { _, err := s.ConfigGetPlan(ctx, by, mcp.ConfigPlanRefInput{}); return err },
		func() error { _, err := s.ConfigCancel(ctx, by, mcp.ConfigPlanRefInput{}); return err },
		func() error { _, err := s.AssistantSend(ctx, by, mcp.AssistantSendInput{}); return err },
		func() error { _, err := s.AssistantGet(ctx, by, mcp.AssistantRefInput{}); return err },
		func() error { _, err := s.AssistantList(ctx, by, mcp.AssistantListInput{}); return err },
		func() error { _, err := s.CreateProvider(ctx, by, mcp.CreateProviderInput{}); return err },
		func() error { _, err := s.RevokeProvider(ctx, by, mcp.ProviderRefInput{}); return err },
		func() error { _, err := s.CreateJoinCode(ctx, by, mcp.CreateJoinCodeInput{}); return err },
		func() error { _, err := s.ListJoinCodes(ctx, by, mcp.ListJoinCodesInput{}); return err },
		func() error { _, err := s.ConfirmJoin(ctx, by, mcp.ConfirmJoinInput{}); return err },
		func() error { _, err := s.ListMembers(ctx, by); return err },
		func() error { _, err := s.SetMemberSuspended(ctx, by, mcp.SetMemberInput{}); return err },
		func() error { _, err := s.RemoveMember(ctx, by, mcp.MemberRefInput{}); return err },
		func() error { _, err := s.UpdateOrganization(ctx, by, mcp.UpdateOrganizationInput{}); return err },
		func() error { _, err := s.ListAudit(ctx, by, mcp.ListAuditInput{}); return err },
		func() error { _, err := s.ListConnections(ctx, by); return err },
		func() error { _, err := s.CreateConnection(ctx, by, mcp.CreateConnectionInput{}); return err },
		func() error { _, err := s.SetConnectionEnabled(ctx, by, mcp.SetConnectionInput{}); return err },
		func() error { _, err := s.ListConnectedApps(ctx, by); return err },
		func() error { _, err := s.RevokeConnectedApp(ctx, by, mcp.ConnectedAppInput{}); return err },
	}
	for _, call := range calls {
		must(t, call())
	}
	want := mcp.Tools()
	got := s.Calls()
	if len(got) != len(want) {
		t.Fatalf("recorded %d calls, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.Tool != want[i] || c.Actor != by {
			t.Errorf("call %d = %+v, want tool %s", i, c, want[i])
		}
	}
	if in, ok := got[0].Input.(mcp.OnboardProjectInput); !ok || in.Repo != "acme/app" {
		t.Errorf("input = %#v", got[0].Input)
	}
}

func TestToolServiceFuncs(t *testing.T) {
	boom := errors.New("boom")
	s := &ToolService{
		ApproveFunc: func(_ context.Context, _ domain.Actor, in mcp.GateDecisionInput) (domain.Gate, error) {
			return domain.Gate{Number: 7, Feedback: in.Feedback}, nil
		},
		ListProjectsFunc: func(context.Context, domain.Actor, any) ([]domain.Project, error) { return nil, boom },
	}
	g, err := s.Approve(t.Context(), domain.Actor{}, mcp.GateDecisionInput{Gate: "GATE-7", Feedback: "lgtm"})
	if err != nil || g.Ref() != "GATE-7" || g.Feedback != "lgtm" {
		t.Fatalf("Approve = %+v, %v", g, err)
	}
	_, err = s.ListProjects(t.Context(), domain.Actor{})
	wantErr(t, err, boom)
}

// gateTools opens one pending gate of kind and returns a tool service
// resolving gates through the testkit gate service.
func gateTools(t *testing.T, kind domain.GateKind) (*ToolService, domain.Actor, domain.Gate, fixture) {
	t.Helper()
	f := newFixture(t)
	svc := NewGates(f.st, NewSignalBus(f.st), f.clock.Now)
	g, err := svc.Open(t.Context(), gates.OpenRequest{AccountID: f.account, ProjectID: f.project.ID, Kind: kind})
	must(t, err)
	by := domain.Actor{AccountID: f.account, Kind: domain.ActorMCP, Name: "laptop"}
	return &ToolService{Gates: svc}, by, g, f
}

type gateTool func(*ToolService, context.Context, domain.Actor, mcp.GateDecisionInput) (domain.Gate, error)

func TestToolServiceGateDecisions(t *testing.T) {
	tools := []struct {
		name string
		call gateTool
		want domain.GateStatus
	}{
		{mcp.ToolApprove, (*ToolService).Approve, domain.GateApproved},
		{mcp.ToolRequestChanges, (*ToolService).RequestChanges, domain.GateChangesRequested},
		{mcp.ToolReject, (*ToolService).Reject, domain.GateRejected},
	}
	for _, kind := range domain.GateKinds() {
		for _, tt := range tools {
			t.Run(string(kind)+"/"+tt.name, func(t *testing.T) {
				checkGateTool(t, kind, tt.name, tt.call, tt.want)
			})
		}
	}
}

// checkGateTool runs a gate tool on a new gate of kind: request_changes on
// needs_you must fail and leave the gate pending; everything else must record
// want and publish gate.resolved.
func checkGateTool(t *testing.T, kind domain.GateKind, name string, call gateTool, want domain.GateStatus) {
	t.Helper()
	s, by, g, f := gateTools(t, kind)
	got, err := call(s, t.Context(), by, mcp.GateDecisionInput{Gate: g.Ref(), Feedback: "why"})
	if !kind.AllowsDecision(domain.GateChangesRequested) && name == mcp.ToolRequestChanges {
		wantErr(t, err, mcp.ErrInvalidInput)
		if stored, _ := f.st.Gates().Get(t.Context(), f.account, g.ID); stored.Status != domain.GatePending {
			t.Fatalf("gate resolved anyway: %s", stored.Status)
		}
		return
	}
	if err != nil || got.Status != want || got.Feedback != "why" {
		t.Fatalf("%s = %s, feedback %q, %v; want %s, feedback \"why\"", name, got.Status, got.Feedback, err, want)
	}
	sigs, _ := f.st.Signals().ListByKey(t.Context(), f.account, domain.GateKey(g.ID))
	if len(sigs) != 1 {
		t.Fatalf("signals = %v", sigs)
	}
	p, err := sigs[0].Decode()
	must(t, err)
	if r := p.(*domain.GateResolved); r.Decision != want || r.Feedback != "why" {
		t.Fatalf("gate.resolved payload = %+v", r)
	}
}

func TestToolServiceGateErrors(t *testing.T) {
	s, by, g, _ := gateTools(t, domain.GateSpecApproval)
	ctx := t.Context()
	_, err := s.Reject(ctx, by, mcp.GateDecisionInput{})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = s.Reject(ctx, by, mcp.GateDecisionInput{Gate: "JOB-1"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = s.Reject(ctx, by, mcp.GateDecisionInput{Gate: "GATE-99"})
	wantErr(t, err, gates.ErrNotFound)
	_, err = s.RequestChanges(ctx, by, mcp.GateDecisionInput{Gate: g.Ref()})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err := s.Approve(ctx, by, mcp.GateDecisionInput{Gate: g.ID.String()})
	if err != nil || got.Status != domain.GateApproved {
		t.Fatalf("Approve by UUID = %+v, %v", got, err)
	}
	_, err = s.Reject(ctx, by, mcp.GateDecisionInput{Gate: g.Ref()})
	wantErr(t, err, domain.ErrInvalidTransition)
}

func TestToolServiceOpenArtifact(t *testing.T) {
	s := &ToolService{}
	by := domain.Actor{Kind: domain.ActorMCP, Name: "laptop"}
	if _, _, err := s.OpenArtifact(t.Context(), by, "id"); !errors.Is(err, mcp.ErrNotFound) {
		t.Fatalf("without a func: %v", err)
	}
	s.OpenArtifactFunc = func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error) {
		return domain.Artifact{Name: "a"}, nil, nil
	}
	if a, _, err := s.OpenArtifact(t.Context(), by, "id"); err != nil || a.Name != "a" {
		t.Fatalf("with a func: %+v, %v", a, err)
	}
}

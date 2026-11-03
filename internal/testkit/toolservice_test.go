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
	if kind == domain.GateNeedsYou && name == mcp.ToolRequestChanges {
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

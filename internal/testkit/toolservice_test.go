package testkit

import (
	"context"
	"errors"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
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
		func() error { _, err := s.DispatchJob(ctx, by, mcp.DispatchJobInput{}); return err },
		func() error { _, err := s.ListProjects(ctx, by); return err },
		func() error { _, err := s.GetProject(ctx, by, mcp.ProjectInput{}); return err },
		func() error { _, err := s.ListGoals(ctx, by, mcp.ListGoalsInput{}); return err },
		func() error { _, err := s.GetGoal(ctx, by, mcp.GoalRefInput{}); return err },
		func() error { _, err := s.ListJobs(ctx, by, mcp.ListJobsInput{}); return err },
	}
	for _, call := range calls {
		must(t, call())
	}
	want := []string{
		mcp.ToolOnboardProject, mcp.ToolCreateGoal, mcp.ToolJobStatus, mcp.ToolJobLogs, mcp.ToolListWorkers,
		mcp.ToolListPendingGates, mcp.ToolApprove, mcp.ToolRequestChanges, mcp.ToolDispatchJob, mcp.ToolListProjects,
		mcp.ToolGetProject, mcp.ToolListGoals, mcp.ToolGetGoal, mcp.ToolListJobs,
	}
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

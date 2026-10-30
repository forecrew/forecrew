package testkit

import (
	"errors"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestPlaybooks(t *testing.T) {
	p := &Playbooks{}
	req := workflow.StartRequest{AccountID: domain.NewID(), GoalID: domain.NewID()}
	run, err := p.Start(t.Context(), "goal", req)
	if err != nil || run.Playbook != "goal" || run.GoalID != req.GoalID || run.Status != domain.RunRunning {
		t.Fatalf("run = %+v, %v", run, err)
	}
	p.Err = errors.New("boom")
	if _, err := p.Start(t.Context(), "onboarding", req); err == nil {
		t.Fatal("want the configured error")
	}
	starts := p.Starts()
	if len(starts) != 2 || starts[0].Playbook != "goal" || starts[1].Playbook != "onboarding" || starts[0].Request.AccountID != req.AccountID {
		t.Fatalf("starts = %+v", starts)
	}
}

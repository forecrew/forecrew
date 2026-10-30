package testkit

import (
	"context"
	"slices"
	"sync"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// PlaybookStart is one recorded Playbooks.Start call.
type PlaybookStart struct {
	Playbook string
	Request  workflow.StartRequest
}

// Playbooks records playbook starts instead of running them: a stand-in for
// workflow.Engine.Start. Err, when set, is returned instead of a run.
type Playbooks struct {
	Err error

	mu     sync.Mutex
	starts []PlaybookStart
}

// Start records the call and returns a running run.
func (p *Playbooks) Start(_ context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, PlaybookStart{playbook, req})
	if p.Err != nil {
		return domain.WorkflowRun{}, p.Err
	}
	return domain.WorkflowRun{
		ID: domain.NewID(), AccountID: req.AccountID, ProjectID: req.ProjectID, GoalID: req.GoalID, TaskID: req.TaskID,
		Playbook: playbook, Status: domain.RunRunning,
	}, nil
}

// Starts returns the recorded calls in order.
func (p *Playbooks) Starts() []PlaybookStart {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.starts)
}

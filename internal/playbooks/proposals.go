package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/workflow"
)

// proposalBatch is the with of the knowledge.* actions of a job's proposals.
type proposalBatch struct {
	JobID    uuid.UUID `json:"job_id"`
	Provider string    `json:"provider"`
}

func (a *actions) proposalBatch(call workflow.ActionCall, with *proposalBatch) (proposals.Batch, error) {
	if a.Proposals == nil {
		return proposals.Batch{}, errors.New("this forecrewd has no knowledge base")
	}
	if with.JobID == uuid.Nil() || with.Provider == "" {
		return proposals.Batch{}, errors.New("with: job_id and provider are required")
	}
	return proposals.Batch{AccountID: call.Run.AccountID, ProjectID: call.Run.ProjectID, JobID: with.JobID, Provider: with.Provider}, nil
}

// knowledgePropose is the action knowledge.propose: it sends the standalone
// proposals of with.job_id to with.provider. Idempotent: what was sent is
// not sent again.
func (a *actions) knowledgePropose(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalBatch
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.proposalBatch(call, &with)
	if err != nil {
		return nil, err
	}
	return a.Proposals.Send(ctx, b)
}

// knowledgeAssess is the action knowledge.assess: it opens the
// knowledge_proposal gate when a human has to decide, with the step run's
// key as the gate's ID, and outputs it in if_gate. with.reviews is the
// output of the foreach of review jobs.
func (a *actions) knowledgeAssess(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		proposalBatch
		Reviews []map[string]json.RawMessage `json:"reviews"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.proposalBatch(call, &with.proposalBatch)
	if err != nil {
		return nil, err
	}
	key, err := uuid.Parse(call.Key)
	if err != nil {
		return nil, err
	}
	verdicts := make([]proposals.Verdict, len(with.Reviews))
	for i, r := range with.Reviews {
		var step struct {
			Output proposals.Verdict `json:"output"`
		}
		// A review that cannot be read has no verdict, so a human decides.
		_ = json.Unmarshal(r["review"], &step)
		verdicts[i] = step.Output
	}
	gates, err := a.Proposals.Assess(ctx, b, key, verdicts)
	if err != nil {
		return nil, err
	}
	return map[string][]string{"if_gate": gates}, nil
}

// knowledgeDecide is the action knowledge.decide: it applies the human's
// decision and outputs how the PR gets merged (if_merge, if_follow).
func (a *actions) knowledgeDecide(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalBatch
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.proposalBatch(call, &with)
	if err != nil {
		return nil, err
	}
	return a.Proposals.Decide(ctx, b)
}

// knowledgeSettle is the action knowledge.settle, the last step of the run
// and its finally step: a run that fails or is canceled rejects what is
// still in review and closes the PR.
func (a *actions) knowledgeSettle(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalBatch
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.proposalBatch(call, &with)
	if err != nil {
		return nil, err
	}
	failed := call.Run.Status == domain.RunFailed || call.Run.Status == domain.RunCanceled
	return map[string]bool{"failed": failed}, a.Proposals.Settle(ctx, b, failed)
}

// knowledgeAccessPR is the action knowledge.access_pr: when the
// access_request gate was approved it opens the PR that changes the policy.
func (a *actions) knowledgeAccessPR(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GateID  uuid.UUID `json:"gate_id"`
		Section string    `json:"section"`
		Level   string    `json:"level"`
		Role    string    `json:"role"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if a.Proposals == nil {
		return nil, errors.New("this forecrewd has no knowledge base")
	}
	return a.Proposals.GrantPR(ctx, proposals.Grant{AccountID: call.Run.AccountID, GateID: with.GateID, Section: with.Section, Level: with.Level, Role: with.Role})
}

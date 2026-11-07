package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/advisor"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// advisorRole is the role of the advisor's jobs.
const advisorRole = "advisor"

// advisor returns the service, or the error of an action that needs it.
func (a *actions) advisor() (*advisor.Service, error) {
	if a.Advisor == nil {
		return nil, errors.New("this forecrewd has no advisor")
	}
	return a.Advisor, nil
}

// adviseOutput is the output of advise.prepare.
type adviseOutput struct {
	Text    string           `json:"text"`
	Signals []advisor.Signal `json:"signals"`
	Room    int              `json:"room"`
	// IfAdvise holds one item when the advisor has signals to answer and
	// room to write, and none otherwise, so a foreach over it skips the job.
	IfAdvise []string `json:"if_advise"`
}

// advisePrepare is the action advise.prepare: the upkeep of the project's
// proposals and the advisor's brief (advisor.Service.Prepare). A forecrewd
// with no runtime for the advisor role does the upkeep and starts no job.
func (a *actions) advisePrepare(ctx context.Context, call workflow.ActionCall) (any, error) {
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	brief, err := svc.Prepare(ctx, p)
	if err != nil {
		return nil, err
	}
	out := adviseOutput{Text: brief.Text, Signals: slices.Clip(brief.Signals), Room: brief.Room, IfAdvise: []string{}}
	if out.Signals == nil {
		out.Signals = []advisor.Signal{}
	}
	configured := a.Config == nil
	if !configured {
		_, configured = a.Config.Roles[advisorRole]
	}
	if len(brief.Signals) > 0 && brief.Room > 0 && configured {
		out.IfAdvise = []string{advisorRole}
	}
	return out, nil
}

// adviseFile is the action advise.file: the advisor's proposals become
// proposals, gates and runs (advisor.Service.File). with.jobs is the output
// of the foreach over the advisor job, with.signals the signals it was given.
func (a *actions) adviseFile(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Jobs    []map[string]json.RawMessage `json:"jobs"`
		Signals []advisor.Signal             `json:"signals"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	var drafts []advisor.Draft
	for _, j := range with.Jobs {
		var step struct {
			Output struct {
				Proposals []advisor.Draft `json:"proposals"`
			} `json:"output"`
		}
		if err := json.Unmarshal(j[advisorRole], &step); err != nil {
			continue // a job that left no output has nothing to file
		}
		drafts = append(drafts, step.Output.Proposals...)
	}
	return svc.File(ctx, p, call.Key, with.Signals, drafts)
}

// adviseStart is the action advise.start, the last step of the learn
// steps: it starts the advise playbook for the goal, unless a run of it is
// going on. with.gate is the learn gate (empty when the learn step opened
// none): a learn step a human rejected starts no advisor. Advice is never
// worth failing a retro: an error is logged.
func (a *actions) adviseStart(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
		Gate   string    `json:"gate"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	no := map[string]bool{"started": false}
	if a.Advisor == nil {
		return no, nil
	}
	if with.Gate != "" {
		id, err := uuid.Parse(with.Gate)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", with.Gate, err)
		}
		g, err := a.Store.Gates().Get(ctx, call.Run.AccountID, id)
		if err != nil {
			return nil, fmt.Errorf("get the learn gate: %w", err)
		}
		if g.Status != domain.GateApproved {
			return no, nil
		}
	}
	started, err := a.Advisor.StartAfterGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		a.Logger.ErrorContext(ctx, "start the advisor", "goal_id", with.GoalID, "error", err)
	}
	return map[string]bool{"started": started}, nil
}

// proposalWith is the input of the proposal.* actions.
type proposalWith struct {
	ProposalID uuid.UUID `json:"proposal_id"`
}

func (a *actions) proposalRound(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalWith
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	gate, err := svc.Round(ctx, call.Run.AccountID, with.ProposalID)
	if err != nil {
		return nil, err
	}
	return map[string]string{"gate_id": gate.String()}, nil
}

func (a *actions) proposalAssess(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalWith
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	return svc.Assess(ctx, call.Run.AccountID, with.ProposalID)
}

// proposalReopen is the action proposal.reopen: after a request for changes
// the advisor's revision opens the next gate; a settled proposal has none.
func (a *actions) proposalReopen(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		proposalWith
		Jobs    []map[string]json.RawMessage `json:"jobs"`
		Settled bool                         `json:"settled"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Settled {
		return map[string]bool{"reopened": false}, nil
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	var draft *advisor.Draft
	for _, j := range with.Jobs {
		var step struct {
			Output struct {
				Proposal *advisor.Draft `json:"proposal"`
			} `json:"output"`
		}
		if json.Unmarshal(j[advisorRole], &step) == nil && step.Output.Proposal != nil {
			draft = step.Output.Proposal
		}
	}
	if err := svc.Reopen(ctx, call.Key, call.Run.AccountID, with.ProposalID, draft); err != nil {
		return nil, err
	}
	return map[string]bool{"reopened": true}, nil
}

func (a *actions) proposalCarry(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalWith
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	return svc.Carry(ctx, call.Run.AccountID, with.ProposalID)
}

// proposalPREnded is the action proposal.pr_ended; with.signal is the wait's.
func (a *actions) proposalPREnded(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		proposalWith
		Signal domain.SignalKind `json:"signal"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	return map[string]string{"signal": string(with.Signal)}, svc.PREnded(ctx, call.Run.AccountID, with.ProposalID, with.Signal)
}

// proposalAbandon is the action proposal.abandon, the finally step.
func (a *actions) proposalAbandon(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with proposalWith
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	svc, err := a.advisor()
	if err != nil {
		return nil, err
	}
	return map[string]bool{"abandoned": true}, svc.Abandon(ctx, call.Run.AccountID, with.ProposalID)
}

package playbooks

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
	"github.com/forecrew/forecrew/internal/workflow"
)

// adrPaths are the files the architect may write in the job that records an
// architectural decision.
var adrPaths = []string{"docs/adr/**"}

// adrBranch is the branch of the goal's ADR.
func adrBranch(g domain.Goal) string { return "agent/adr-" + g.Ref() }

// adrOutput is the output of goal.adr.
type adrOutput struct {
	// WritePaths, Pool and Branch go to the architect's job: the paths the
	// ADR goes under, a pool that gets a write token and the ADR's branch.
	// All three are empty for a goal whose decision is not architectural,
	// which leaves the job read-only in its usual pool.
	WritePaths []string `json:"write_paths"`
	Pool       string   `json:"pool"`
	Branch     string   `json:"branch"`
}

// goalADR is the action goal.adr. The decision of a goal is architectural
// when its technical council decided: that council weighs the design, so its
// brief is the ADR's subject. Then the architect's job may write docs/adr/**
// on the branch agent/adr-GOAL-n, in the build pool. The output is for the
// job's with; it reads only.
func (a *actions) goalADR(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	cs, err := a.Store.Councils().List(ctx, g.AccountID, store.CouncilFilter{GoalID: g.ID, Status: domain.CouncilDecided})
	if err != nil {
		return nil, fmt.Errorf("list the councils of %s: %w", g.Ref(), err)
	}
	for _, c := range cs {
		if c.Kind == domain.CouncilTechnical {
			return adrOutput{WritePaths: adrPaths, Pool: domain.PoolBuild, Branch: adrBranch(g)}, nil
		}
	}
	return adrOutput{WritePaths: []string{}}, nil
}

// adrReady is the output of goal.adr_ready. IfADR holds the branch for a
// foreach that skips the ADR's steps when the architect wrote none.
type adrReady struct {
	IfADR []string `json:"if_adr"`
	SHA   string   `json:"sha"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
}

// adrReadyAction is the action goal.adr_ready: it finds the ADR the
// architect pushed, the head of agent/adr-GOAL-n, and writes the PR text. A
// goal without the branch has no ADR.
func (a *actions) adrReadyAction(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	g, err := a.loadGoal(ctx, call.Run, with.GoalID)
	if err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project of %s: %w", g.Ref(), err)
	}
	sha, err := a.Tracker.HeadSHA(ctx, p.Repo, adrBranch(g))
	if errors.Is(err, github.ErrNotFound) {
		return adrReady{IfADR: []string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", adrBranch(g), p.Repo, err)
	}
	return adrReady{
		IfADR: []string{adrBranch(g)}, SHA: sha,
		Title: "Add the ADR of " + g.Ref() + ": " + contextpack.OneLine(g.Title),
		Body:  "The architecture decision record the technical council's decision for " + g.Ref() + " leads to. Merging it is a human's decision.",
	}, nil
}

// productBriefNote links the brief of the goal's decided product council in
// the text of the spec gate, so that the human sees what the spec was written
// from. It is empty for a goal without one. The recommendation is the
// facilitator's text, cut to one line.
func (a *actions) productBriefNote(ctx context.Context, g domain.Goal) (string, error) {
	cs, err := a.Store.Councils().List(ctx, g.AccountID, store.CouncilFilter{GoalID: g.ID, Status: domain.CouncilDecided})
	if err != nil {
		return "", fmt.Errorf("list the councils of %s: %w", g.Ref(), err)
	}
	for _, c := range cs {
		if c.Kind == domain.CouncilProduct && c.Brief != nil {
			return fmt.Sprintf("\n\nThe spec follows the brief of %s, the product council (get_council %s has it; it recommended: %s).",
				c.Ref(), c.Ref(), testreport.Clip(contextpack.OneLine(c.Brief.Recommendation), 200)), nil
		}
	}
	return "", nil
}

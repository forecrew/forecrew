package playbooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/roles"
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
		if c.Kind != domain.CouncilTechnical {
			continue
		}
		writable, err := a.adrWritable(ctx, g)
		if err != nil {
			return nil, err
		}
		if writable {
			return adrOutput{WritePaths: adrPaths, Pool: domain.PoolBuild, Branch: adrBranch(g)}, nil
		}
	}
	return adrOutput{WritePaths: []string{}}, nil
}

// adrWritable reports whether the architect's job may write an ADR under the
// project's knowledge policy: a section that is protected, or whose globs
// reach beyond the ADR folder, keeps its files out of the job's write paths
// (knowledge.PathRules), and then the architect stays read-only, since a job
// that cannot write its ADR would fail at the end. Without a knowledge base
// nothing is curated.
func (a *actions) adrWritable(ctx context.Context, g domain.Goal) (bool, error) {
	if a.Proposals == nil || a.Proposals.Policy == nil {
		return true, nil
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return false, fmt.Errorf("get project of %s: %w", g.Ref(), err)
	}
	policy, err := a.Proposals.Policy(ctx, p)
	if err != nil {
		return false, fmt.Errorf("read the knowledge policy of %s: %w", p.Name, err)
	}
	paths := slices.Clone(adrPaths)
	for _, d := range policy.PathRules(adrRole, false).DeniedFor(adrPaths) {
		paths = append(paths, "!"+d)
	}
	return roles.PathAllowed(paths, adrFile), nil
}

// adrRole is the role that writes the ADR, and adrFile a file of the folder
// it writes to.
const (
	adrRole = "architect"
	adrFile = "docs/adr/0001-record.md"
)

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
			return fmt.Sprintf("\n\nThe spec follows the brief of %s, the product council (get_council %s has it). It recommended:\n\n%s",
				c.Ref(), c.Ref(), contextpack.WrapUntrusted("recommendation of "+c.Ref(), testreport.Clip(contextpack.OneLine(c.Brief.Recommendation), 200))), nil
		}
	}
	return "", nil
}

// adrRoundOutput is the output of goal.adr_round.
type adrRoundOutput struct {
	// IfRework holds the human's feedback when the last round of the ADR ended
	// with changes requested, for a foreach that reruns the architect.
	IfRework []string `json:"if_rework"`
	// Done is set when the ADR needs nothing more: none was written, or the
	// human approved it.
	Done bool `json:"done"`
}

// adrRoundAction is the action goal.adr_round, which reads how a round of
// the ADR's review ended: with.decision is the decision of the round's gate,
// empty when the architect wrote no ADR, and with.feedback what the human
// wrote. Changes requested are not done and give the feedback (a default
// when the human wrote none) to the architect; approved and no gate are done.
func (a *actions) adrRoundAction(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Decision domain.GateStatus `json:"decision"`
		Feedback string            `json:"feedback"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	out := adrRoundOutput{IfRework: []string{}, Done: with.Decision == "" || with.Decision == domain.GateApproved}
	if with.Decision == domain.GateChangesRequested {
		out.IfRework = []string{cmp.Or(strings.TrimSpace(with.Feedback), "The reviewer asked for changes to the ADR.")}
	}
	return out, nil
}

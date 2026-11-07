package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testreport"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Bounds of the history shown to the retro job: it reads what people and
// agents wrote, so each text is cut and each list limited.
const (
	retroMaxGates    = 30
	retroMaxReviews  = 20
	retroMaxFailures = 20
	retroMaxNotes    = 40
	retroText        = 1500
	retroComment     = 300
	retroComments    = 8
)

// retroOutput is the output of goal.retro_context.
type retroOutput struct {
	// Text is the goal's history for the retro job.
	Text  string `json:"text"`
	Notes int    `json:"notes"` // goal notes the retro may promote
}

// retroContext is the action goal.retro_context. It writes the history of
// with.goal_id for the knowledge curator's retro job: the spec revisions, what
// humans answered at the goal's gates, what the reviewer asked to change, the
// jobs that failed, what the work cost and the notes the agents left each
// other. Everything a person or an agent wrote is marked as data. It reads
// only, so repeating it is harmless.
func (a *actions) retroContext(ctx context.Context, call workflow.ActionCall) (any, error) {
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
	specs, err := a.Store.Specs().ListByGoal(ctx, g.AccountID, g.ID)
	if err != nil {
		return nil, fmt.Errorf("list the specs of %s: %w", g.Ref(), err)
	}
	gs, err := a.Store.Gates().List(ctx, g.AccountID, store.GateFilter{ProjectID: g.ProjectID})
	if err != nil {
		return nil, fmt.Errorf("list the gates of %s: %w", g.Ref(), err)
	}
	jobs, err := a.Store.Jobs().List(ctx, g.AccountID, store.JobFilter{GoalID: g.ID})
	if err != nil {
		return nil, fmt.Errorf("list the jobs of %s: %w", g.Ref(), err)
	}
	notes, err := a.Store.Knowledge().List(ctx, g.AccountID, store.KnowledgeFilter{ProjectID: g.ProjectID, Provider: domain.ProviderInternal})
	if err != nil {
		return nil, fmt.Errorf("list the notes of %s: %w", g.Ref(), err)
	}
	notes = slices.DeleteFunc(notes, func(n domain.KnowledgeEntry) bool { return n.GoalID != g.ID })

	var b strings.Builder
	fmt.Fprintf(&b, "## The goal\n\n%s %q is %s.\n", g.Ref(), contextpack.OneLine(g.Title), g.Status)
	b.WriteString(retroSpecs(specs))
	b.WriteString(retroGates(gs, g.ID))
	b.WriteString(retroReviews(jobs))
	b.WriteString(retroFailures(jobs))
	b.WriteString(retroCosts(jobs))
	b.WriteString(retroNotes(notes))
	if a.Advisor != nil {
		outcomes, err := a.Advisor.RetroText(ctx, g.AccountID, g.ProjectID)
		if err != nil {
			return nil, err
		}
		b.WriteString(outcomes)
	}
	return retroOutput{Text: b.String(), Notes: min(len(notes), retroMaxNotes)}, nil
}

func retroSpecs(specs []domain.Spec) string {
	if len(specs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Spec revisions\n\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "- %s, revision %d\n", s.Ref(), s.Revision)
	}
	if len(specs) > 1 {
		fmt.Fprintf(&b, "\nThe spec needed %d revisions; the feedback that asked for them is under the gates.\n", len(specs))
	}
	return b.String()
}

// retroGates lists what the humans answered at the gates of goal.
func retroGates(gs []domain.Gate, goal uuid.UUID) string {
	var b strings.Builder
	n := 0
	for _, g := range gs {
		if g.GoalID != goal || g.Status == domain.GatePending || strings.TrimSpace(g.Feedback) == "" {
			continue
		}
		if n++; n > retroMaxGates {
			fmt.Fprintf(&b, "\n(more gates left out)\n")
			break
		}
		fmt.Fprintf(&b, "\n%s, a %s gate, %s by %s: %s\n", g.Ref(), g.Kind, g.Status, contextpack.OneLine(g.ResolvedBy), contextpack.OneLine(g.Title))
		b.WriteString(contextpack.WrapUntrusted(g.Ref()+" feedback", testreport.Clip(g.Feedback, retroText)))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n## What humans answered at gates\n" + b.String()
}

// reviewJob is the part of a reviewer's output the retro reads.
type reviewJob struct {
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Comments []struct {
		Path     *string `json:"path"`
		Severity string  `json:"severity"`
		Body     string  `json:"body"`
	} `json:"comments"`
}

// retroReviews lists the reviews that asked for changes or commented: the
// rework the goal needed.
func retroReviews(jobs []domain.Job) string {
	var b strings.Builder
	n := 0
	for _, j := range jobs {
		if j.Role != "reviewer" || j.Status != domain.JobSucceeded {
			continue
		}
		var r reviewJob
		if json.Unmarshal(j.Output, &r) != nil || (r.Verdict == "approve" && len(r.Comments) == 0) {
			continue
		}
		if n++; n > retroMaxReviews {
			b.WriteString("\n(more reviews left out)\n")
			break
		}
		fmt.Fprintf(&b, "\n%s, the reviewer said %s:\n", j.Ref(), contextpack.OneLine(r.Verdict))
		var text strings.Builder
		text.WriteString(testreport.Clip(r.Summary, retroText))
		for i, c := range r.Comments {
			if i == retroComments {
				text.WriteString("\n(more comments left out)")
				break
			}
			where := ""
			if c.Path != nil {
				where = " " + contextpack.OneLine(*c.Path)
			}
			fmt.Fprintf(&text, "\n- %s%s: %s", contextpack.OneLine(c.Severity), where, testreport.Clip(contextpack.OneLine(c.Body), retroComment))
		}
		b.WriteString(contextpack.WrapUntrusted(j.Ref()+" review", text.String()))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n## What the reviewer asked to change\n" + b.String()
}

func retroFailures(jobs []domain.Job) string {
	var b strings.Builder
	n := 0
	for _, j := range jobs {
		if j.Status != domain.JobFailed && j.Status != domain.JobCanceled {
			continue
		}
		if n++; n > retroMaxFailures {
			b.WriteString("\n(more jobs left out)\n")
			break
		}
		fmt.Fprintf(&b, "\n%s (%s) %s after %d attempts:\n", j.Ref(), j.Role, j.Status, j.Attempts)
		b.WriteString(contextpack.WrapUntrusted(j.Ref()+" error", testreport.Clip(j.Error, retroComment)))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n## Jobs that failed or were canceled\n" + b.String()
}

func retroCosts(jobs []domain.Job) string {
	if len(jobs) == 0 {
		return ""
	}
	type tally struct {
		n    int
		cost float64
	}
	by := map[string]*tally{}
	total := 0.0
	for _, j := range jobs {
		t := by[j.Role]
		if t == nil {
			t = &tally{}
			by[j.Role] = t
		}
		t.n++
		t.cost += j.CostUSD
		total += j.CostUSD
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Cost\n\n%d jobs, $%.2f in all.\n", len(jobs), total)
	for _, role := range slices.Sorted(maps.Keys(by)) {
		fmt.Fprintf(&b, "- %s: %d %s, $%.2f\n", role, by[role].n, plural(by[role].n, "job"), by[role].cost)
	}
	return b.String()
}

// retroNotes lists the goal's notes by id, so that the retro can cite the
// ones it promotes (note:<id>).
func retroNotes(notes []domain.KnowledgeEntry) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Notes the agents left each other\n\nCite a note you promote as note:<id> in the reason of the proposal that carries it.\n")
	for i, n := range notes {
		if i == retroMaxNotes {
			b.WriteString("\n(more notes left out; kb_search finds them)\n")
			break
		}
		fmt.Fprintf(&b, "\nnote:%s:\n", n.ID)
		b.WriteString(contextpack.WrapUntrusted("note "+n.ID.String(), noteText(n)))
	}
	return b.String()
}

// learnBatch is the with of the goal.learn* actions.
func (a *actions) learnBatch(ctx context.Context, call workflow.ActionCall, goalID uuid.UUID) (proposals.LearnBatch, error) {
	if a.Proposals == nil {
		return proposals.LearnBatch{}, errors.New("this forecrewd has no knowledge base")
	}
	g, err := a.loadGoal(ctx, call.Run, goalID)
	if err != nil {
		return proposals.LearnBatch{}, err
	}
	return proposals.LearnBatch{AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.ID}, nil
}

// learnGoal is the action goal.learn: it writes, in one PR agent/learn-GOAL-n,
// the goal's assumptions and decisions (registers.Export) and the per_goal
// knowledge proposals of the goal for the repo, sends those for other
// providers to them (or to the gate, when a provider has no review flow) and
// opens the knowledge_proposal gate that asks a human to approve the lot,
// with the step run's key as its ID. Idempotent for its key.
func (a *actions) learnGoal(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.learnBatch(ctx, call, with.GoalID)
	if err != nil {
		return nil, err
	}
	key, err := uuid.Parse(call.Key)
	if err != nil {
		return nil, err
	}
	files, err := a.regs.Export(ctx, b.AccountID, b.GoalID)
	if err != nil {
		return nil, fmt.Errorf("export the registers of the goal: %w", err)
	}
	return a.Proposals.Learn(ctx, b, key, files)
}

// learnDecide is the action goal.learn_decide: it applies the human's
// decision on with.gate (the gate of goal.learn, "" when it opened none) and
// outputs how the PR gets merged (if_merge, if_follow).
func (a *actions) learnDecide(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
		Gate   string    `json:"gate"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	b, err := a.learnBatch(ctx, call, with.GoalID)
	if err != nil {
		return nil, err
	}
	return a.Proposals.LearnDecide(ctx, b, with.Gate)
}

// learnSettle is the action goal.learn_settle, the last step of the learn
// run and its finally step: a run that fails or is canceled closes the learn
// PR and rejects what is still in review or queued. with.skipped (the goal
// playbook after a human skipped a failed learn step) does the same in a run
// that goes on.
func (a *actions) learnSettle(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID `json:"goal_id"`
		// Skipped is set by the goal playbook after its learn step: true
		// settles it as failed, false (the step finished) settles nothing.
		Skipped *bool `json:"skipped"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Skipped != nil && !*with.Skipped {
		return map[string]bool{"failed": false, "settled": true}, nil
	}
	b, err := a.learnBatch(ctx, call, with.GoalID)
	if err != nil {
		return nil, err
	}
	failed := with.Skipped != nil || call.Run.Status == domain.RunFailed || call.Run.Status == domain.RunCanceled
	return map[string]bool{"failed": failed, "settled": true}, a.Proposals.LearnSettle(ctx, b, failed)
}

package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// runningCouncil is loadCouncil for the actions between a council's jobs. A
// council that ended, because its goal was canceled, fails them: the jobs of
// the stage under way finish and the run ends without starting another.
func (a *actions) runningCouncil(ctx context.Context, run domain.WorkflowRun, id uuid.UUID) (domain.Council, error) {
	c, err := a.loadCouncil(ctx, run, id)
	if err == nil && c.Status.Terminal() {
		return c, fmt.Errorf("%s is %s", c.Ref(), c.Status)
	}
	return c, err
}

// cancelCouncils cancels the running councils of a goal, so that they stop
// spending tokens with it.
func (a *actions) cancelCouncils(ctx context.Context, g domain.Goal) error {
	running, err := a.Store.Councils().List(ctx, g.AccountID, store.CouncilFilter{GoalID: g.ID, Status: domain.CouncilRunning})
	if err != nil {
		return fmt.Errorf("list the councils of %s: %w", g.Ref(), err)
	}
	for _, c := range running {
		if err := a.endCouncil(ctx, c, domain.CouncilCanceled); err != nil {
			return err
		}
	}
	return nil
}

// loadCouncil returns the council of a run's project.
func (a *actions) loadCouncil(ctx context.Context, run domain.WorkflowRun, id uuid.UUID) (domain.Council, error) {
	c, err := a.Store.Councils().Get(ctx, run.AccountID, id)
	if err != nil {
		return domain.Council{}, fmt.Errorf("get council %s: %w", id, err)
	}
	if c.ProjectID != run.ProjectID {
		return domain.Council{}, fmt.Errorf("council %s belongs to another project", c.Ref())
	}
	return c, nil
}

// councilRun is the action council.run: it starts a council as a child run
// of a goal playbook, which waits for it with a wait step on council.decided.
// The council's ID is the call's key, so a repeated call finds it.
func (a *actions) councilRun(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Kind        domain.CouncilKind `json:"kind"`
		Question    string             `json:"question"`
		Constraints []string           `json:"constraints"`
		Roles       []string           `json:"roles"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(call.Key)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	c, err := a.Councils.Start(ctx, a.Store, a.Runs, councils.Request{
		ID: id, AccountID: call.Run.AccountID, ProjectID: call.Run.ProjectID, GoalID: call.Run.GoalID,
		Kind: with.Kind, Question: with.Question, Constraints: with.Constraints, Roles: with.Roles,
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"council_id": c.ID.String(), "ref": c.Ref()}, nil
}

// startOutput is the output of council.start.
type startOutput struct {
	Facilitator string            `json:"facilitator"`
	Members     []councils.Member `json:"members"`
	Commit      string            `json:"commit"`
}

// models is what council.start asks which model a role runs with.
func (a *actions) models() CouncilModels {
	if a.Models != nil {
		return a.Models
	}
	return fileModels{a.Config}
}

// councilStart is the action council.start, the first step of the council
// playbook: it pins the members to runtimes and takes the commit of the
// default branch, which every job checks out and every path is checked at.
// It only reads, so it is idempotent.
func (a *actions) councilStart(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID uuid.UUID `json:"council_id"`
		Roles     []string  `json:"roles"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.loadCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	pack, err := a.Councils.Seated(c.Kind, with.Roles)
	if err != nil {
		return nil, err
	}
	project, err := a.Store.Projects().Get(ctx, c.AccountID, c.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project of %s: %w", c.Ref(), err)
	}
	commit, err := a.Tracker.HeadSHA(ctx, project.Repo, project.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", project.DefaultBranch, project.Repo, err)
	}
	workers, err := a.Store.Workers().List(ctx, c.AccountID)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	now := a.Now()
	offered := func(s councils.Seat, runtime string) bool {
		if _, err := a.models().ModelOn(ctx, c.AccountID, c.ProjectID, s.Role, runtime); err != nil {
			return false
		}
		pool := domain.OrDefaultPool(a.Councils.Pool(s.Role))
		return slices.ContainsFunc(workers, func(w domain.Worker) bool {
			return w.Online(now) && w.Pool == pool && slices.Contains(w.Labels, domain.RuntimeLabel(runtime))
		})
	}
	usual := func(role string) string {
		adapter, err := a.models().Usual(ctx, c.AccountID, c.ProjectID, role)
		if err != nil {
			return ""
		}
		return adapter
	}
	return startOutput{Facilitator: pack.Facilitator, Members: pack.Assign(offered, usual), Commit: commit}, nil
}

// contributeOutput is the output of council.contribute.
type contributeOutput struct {
	Valid bool `json:"valid"`
	// Errors says what is wrong with an invalid output, as the job's
	// feedback: it quotes agent text, so it is marked untrusted.
	Errors         string    `json:"errors"`
	ContributionID uuid.UUID `json:"contribution_id,omitzero"`
	// Moving is what a check job says.
	Moving bool `json:"moving"`
	// IfGate is [1] after a synthesis the policy does not accept, for the
	// foreach around the gate, and [] otherwise.
	IfGate []int `json:"if_gate"`
}

// councilContribute is the action council.contribute: it checks the output
// of a council job (councils.Set.CheckOutput) and stores it. An invalid output
// is not an error but valid: false, for the rerun. Member stages and the
// synthesis become contributions, once per job; the runtime is the job's. A
// synthesis also becomes the council's brief, noting a budget that ran out
// whatever the facilitator wrote.
func (a *actions) councilContribute(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID       uuid.UUID       `json:"council_id"`
		Step            councils.Step   `json:"step"`
		Round           int             `json:"round"`
		JobID           uuid.UUID       `json:"job_id"`
		Output          json.RawMessage `json:"output"`
		BudgetExhausted bool            `json:"budget_exhausted"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.runningCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	out := contributeOutput{IfGate: []int{}}
	if err := a.Councils.CheckOutput(with.Step, with.Output); err != nil {
		if !errors.Is(err, councils.ErrInvalid) {
			return nil, err
		}
		out.Errors = "Your output was refused. Answer again, without these problems:\n\n" + contextpack.WrapUntrusted("the output check", err.Error())
		return out, nil
	}
	out.Valid = true
	switch with.Step {
	case councils.StepFrame:
		return out, nil
	case councils.StepCheck:
		var doc struct {
			Moving bool `json:"positions_moving"`
		}
		if err := json.Unmarshal(with.Output, &doc); err != nil {
			return nil, fmt.Errorf("with: output: %w", err)
		}
		out.Moving = doc.Moving
		return out, nil
	}
	job, err := a.Store.Jobs().Get(ctx, c.AccountID, with.JobID)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", with.JobID, err)
	}
	if out.ContributionID, err = a.addContribution(ctx, c, job, domain.CouncilStage(with.Step), with.Round, with.Output); err != nil {
		return nil, err
	}
	if with.Step == councils.StepSynthesis {
		accepted, err := a.storeBrief(ctx, c, with.Output, with.BudgetExhausted)
		if err != nil {
			return nil, err
		}
		if !accepted {
			out.IfGate = []int{1}
		}
	}
	return out, nil
}

// addContribution stores the output of job, or finds it when it is stored.
func (a *actions) addContribution(ctx context.Context, c domain.Council, job domain.Job, stage domain.CouncilStage, round int, output []byte) (uuid.UUID, error) {
	k := domain.CouncilContribution{
		AccountID: c.AccountID, CouncilID: c.ID, Role: job.Role, Runtime: job.Runtime, Stage: stage, Round: round, JobID: job.ID, Content: output,
	}
	if err := k.Validate(); err != nil {
		return uuid.Nil(), err
	}
	err := a.Store.Councils().AddContribution(ctx, &k)
	if err == nil {
		return k.ID, nil
	}
	if !errors.Is(err, store.ErrConflict) {
		return uuid.Nil(), fmt.Errorf("store the %s of %s: %w", stage, job.Role, err)
	}
	all, err := a.Store.Councils().Contributions(ctx, c.AccountID, c.ID, stage)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("list the contributions of %s: %w", c.Ref(), err)
	}
	for _, x := range all {
		if x.Role == k.Role && x.Runtime == k.Runtime && x.Round == round {
			return x.ID, nil
		}
	}
	return uuid.Nil(), fmt.Errorf("no contribution of %s to round %d of %s", job.Role, round, stage)
}

// storeBrief makes output the council's brief and reports whether the
// policy accepts its recommendation (policyAccepts). A brief written after a
// human's request for changes goes back to that human.
func (a *actions) storeBrief(ctx context.Context, c domain.Council, output []byte, exhausted bool) (bool, error) {
	brief, err := councils.ParseBrief(output)
	if err != nil {
		return false, err
	}
	brief.BudgetExhausted = brief.BudgetExhausted || exhausted
	err = a.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		cur, err := tx.Councils().GetForUpdate(ctx, c.AccountID, c.ID)
		if err != nil {
			return err
		}
		c = cur
		c.Brief = &brief
		return tx.Councils().Update(ctx, &c)
	})
	if err != nil {
		return false, fmt.Errorf("store the brief of %s: %w", c.Ref(), err)
	}
	return a.policyAccepts(c), nil
}

// policyAccepts reports whether the policy may decide for c without a human:
// the config names its kind, no human was asked yet, the brief says the
// decision is reversible, and nobody but the devil's advocate dissents.
// Reversible is the facilitator's claim, which nothing here can verify.
func (a *actions) policyAccepts(c domain.Council) bool {
	return c.Brief != nil && c.Brief.Reversible && c.GateID == uuid.Nil() && councils.Undisputed(*c.Brief) &&
		slices.Contains(a.Config.Policy.AcceptRecommendations, c.Kind)
}

// stageOutput is the output of council.round_done.
type stageOutput struct {
	// Exhausted says the token or time budget ran out.
	Exhausted bool `json:"exhausted"`
	// Members run the next stage; empty skips it.
	Members []councils.Member `json:"members"`
	// IfCheck is [round] when the facilitator checks whether positions
	// still move, else [].
	IfCheck []int `json:"if_check"`
	// Done ends the debate.
	Done bool `json:"done"`
	// Round is the round of the stage to run.
	Round int `json:"round"`
}

// councilRoundDone is the action council.round_done, which ends a stage and
// says what runs next, given the budget:
//   - proposal: members, for the critiques;
//   - critique: if_check, while round is within the pack's max_rounds;
//   - check: members and round when positions move, else done;
//   - synthesis: the round of the next brief.
//
// A stage that started finishes: the budget is looked at between stages. It
// only reads, so it is idempotent.
func (a *actions) councilRoundDone(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID uuid.UUID         `json:"council_id"`
		Stage     councils.Step     `json:"stage"`
		Round     int               `json:"round"`
		Moving    bool              `json:"moving"`
		Members   []councils.Member `json:"members"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.runningCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	pack, err := a.Councils.Seated(c.Kind, nil) // members do not change rounds or budget
	if err != nil {
		return nil, err
	}
	out := stageOutput{Members: []councils.Member{}, IfCheck: []int{}, Round: with.Round}
	if out.Exhausted, err = a.budgetSpent(ctx, call.Run, c, pack.Budget); err != nil {
		return nil, err
	}
	switch with.Stage {
	case councils.StepProposal:
		if !out.Exhausted {
			out.Members = with.Members
		}
	case councils.StepCritique:
		if !out.Exhausted && with.Round <= pack.MaxRounds {
			out.IfCheck = []int{with.Round}
		}
	case councils.StepCheck:
		if with.Moving && !out.Exhausted {
			out.Members = with.Members
		} else {
			out.Done = true
		}
	case councils.StepSynthesis:
		briefs, err := a.Store.Councils().Contributions(ctx, c.AccountID, c.ID, domain.StageSynthesis)
		if err != nil {
			return nil, fmt.Errorf("list the briefs of %s: %w", c.Ref(), err)
		}
		out.Round = len(briefs) + 1
	default:
		return nil, fmt.Errorf("with: unknown stage %q", with.Stage)
	}
	return out, nil
}

// budgetSpent reports whether the council's budget ran out: the tokens of
// every job of its run, retries included, and the time since it started. The
// time a human takes to answer the gate does not count.
func (a *actions) budgetSpent(ctx context.Context, run domain.WorkflowRun, c domain.Council, b councils.Budget) (bool, error) {
	if c.GateID == uuid.Nil() && !a.Now().Before(c.CreatedAt.Add(b.Time)) {
		return true, nil
	}
	steps, err := a.Store.StepRuns().ListByRun(ctx, c.AccountID, run.ID)
	if err != nil {
		return false, fmt.Errorf("list the steps of %s: %w", c.Ref(), err)
	}
	var tokens int64
	for _, s := range steps {
		if s.JobID == uuid.Nil() {
			continue
		}
		j, err := a.Store.Jobs().Get(ctx, c.AccountID, s.JobID)
		if err != nil {
			return false, fmt.Errorf("get job %s: %w", s.JobID, err)
		}
		tokens += j.Usage.InputTokens + j.Usage.OutputTokens
	}
	return tokens >= b.Tokens, nil
}

// councilOpenGate is the action council.open_gate: it opens the
// council_decision gate with the brief as its summary and records it on the
// council in the same transaction. The gate's ID is the call's key, so a
// repeated call finds it.
func (a *actions) councilOpenGate(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID uuid.UUID `json:"council_id"`
		JobID     uuid.UUID `json:"job_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.runningCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	if c.Brief == nil {
		return nil, fmt.Errorf("%s has no brief", c.Ref())
	}
	project, err := a.Store.Projects().Get(ctx, c.AccountID, c.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project of %s: %w", c.Ref(), err)
	}
	id, err := uuid.Parse(call.Key)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	g, err := a.Gates.Open(ctx, gates.OpenRequest{
		ID: id, AccountID: c.AccountID, ProjectID: c.ProjectID, Kind: domain.GateCouncilDecision,
		Title: c.Ref() + ": " + contextpack.FirstLine(c.Question, 120), Summary: briefSummary(c), SubjectRepo: project.Repo, GoalID: c.GoalID, JobID: with.JobID,
		// In the gate's transaction: its decision must find the council.
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			cur, err := tx.Councils().GetForUpdate(ctx, c.AccountID, c.ID)
			if err != nil {
				return err
			}
			cur.GateID = g.ID
			return tx.Councils().Update(ctx, &cur)
		},
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"gate_id": g.ID.String()}, nil
}

// briefSummary is the text of the council_decision gate. The brief is agent
// text, so it is marked untrusted.
func briefSummary(c domain.Council) string {
	b := c.Brief
	var t strings.Builder
	fmt.Fprintf(&t, "Recommendation: %s\n", b.Recommendation)
	if b.BudgetExhausted {
		t.WriteString("\nThe council's budget ran out: this brief was written from what existed.\n")
	}
	if !b.Reversible {
		t.WriteString("\nThe facilitator says this decision is hard to undo.\n")
	}
	t.WriteString("\nOptions:\n")
	for _, o := range b.Options {
		fmt.Fprintf(&t, "- %s (cost %s, risk %s): %s\n", o.Name, o.Cost, o.Risk, o.Summary)
	}
	if len(b.Votes) > 0 {
		t.WriteString("\nVotes:\n")
	}
	for _, v := range b.Votes {
		fmt.Fprintf(&t, "- %s: %s (%s)\n", v.Member, v.Option, v.Reason)
	}
	if len(b.Dissent) > 0 {
		t.WriteString("\nDissent:\n")
	}
	for _, d := range b.Dissent {
		fmt.Fprintf(&t, "- %s: %s\n", d.Member, d.Position)
	}
	if len(b.Assumptions) > 0 {
		t.WriteString("\nAssumptions:\n")
	}
	for _, x := range b.Assumptions {
		fmt.Fprintf(&t, "- %s (impact %s, confidence %s)\n", x.Statement, x.Impact, x.Confidence)
	}
	if len(b.OpenQuestions) > 0 {
		t.WriteString("\nOpen questions:\n")
	}
	for _, q := range b.OpenQuestions {
		fmt.Fprintf(&t, "- %s\n", q)
	}
	return "The brief of " + c.Ref() + ". It is agent text. Approve to accept the recommendation, request changes to add your feedback as a constraint for one more round, reject to fail the council.\n\n" +
		contextpack.WrapUntrusted("facilitator of "+c.Ref(), t.String())
}

// decideOutput is the output of council.decide.
type decideOutput struct {
	// Done ends the council's decision loop.
	Done bool `json:"done"`
	// Members and Round are the human's revision round, when changes were
	// requested.
	Members []councils.Member `json:"members"`
	Round   int               `json:"round"`
}

// councilDecide is the action council.decide, after the gate or, when the
// policy accepted the recommendation, without one. It is the guard of the
// decision, so it believes nothing it is told: without a gate it asks the
// policy again, and with one it reads the gate, which must be the council's
// and have the decision it was given, feedback included. An accepted
// recommendation moves the council to decided and publishes council.decided
// with the decision: the policy's, which it records, or the one the register
// stored when the human resolved the gate. Changes requested add the feedback
// as a constraint and output the human's round; a rejection fails the council
// and the step. Each step finds what a call before a crash did, in this order:
// the decision comes first, so a council that is decided always has one, and a
// repeated call records nothing twice and publishes the same signal.
func (a *actions) councilDecide(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID uuid.UUID         `json:"council_id"`
		GateID    uuid.UUID         `json:"gate_id"`
		Decision  domain.GateStatus `json:"decision"`
		Members   []councils.Member `json:"members"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.loadCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	if with.GateID == uuid.Nil() {
		return a.decideByPolicy(ctx, c, with.Decision)
	}
	g, err := a.councilGate(ctx, c, with.GateID, with.Decision)
	if err != nil {
		return nil, err
	}
	switch g.Status {
	case domain.GateChangesRequested:
		return a.humanRound(ctx, c, g.Feedback, with.Members)
	case domain.GateRejected:
		if err := a.endCouncil(ctx, c, domain.CouncilFailed); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("the brief of %s was rejected", c.Ref())
	}
	d, err := a.gateDecision(ctx, c, g.ID)
	if err != nil {
		return nil, fmt.Errorf("the decision of %s: %w", c.Ref(), err)
	}
	return a.decided(ctx, c, d)
}

// decideByPolicy accepts the recommendation of c when the policy allows it.
// A repeated call for a decided council finds the decision, whatever the
// config says by then.
func (a *actions) decideByPolicy(ctx context.Context, c domain.Council, decision domain.GateStatus) (any, error) {
	if decision != "" {
		return nil, fmt.Errorf("%s: decision %q without a gate", c.Ref(), decision)
	}
	if c.Status != domain.CouncilDecided && !a.policyAccepts(c) {
		return nil, fmt.Errorf("the policy does not accept the recommendation of %s", c.Ref())
	}
	d, err := a.regs.AcceptRecommendation(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("the decision of %s: %w", c.Ref(), err)
	}
	return a.decided(ctx, c, d)
}

// councilGate returns the gate with id, which must be the gate of c and
// resolved as decision.
func (a *actions) councilGate(ctx context.Context, c domain.Council, id uuid.UUID, decision domain.GateStatus) (domain.Gate, error) {
	if !slices.Contains([]domain.GateStatus{domain.GateApproved, domain.GateChangesRequested, domain.GateRejected}, decision) {
		return domain.Gate{}, fmt.Errorf("%s: unknown decision %q", c.Ref(), decision)
	}
	if id != c.GateID {
		return domain.Gate{}, fmt.Errorf("gate %s is not the gate of %s", id, c.Ref())
	}
	g, err := a.Gates.Get(ctx, c.AccountID, id)
	if err != nil {
		return domain.Gate{}, err
	}
	if g.Status != decision {
		return domain.Gate{}, fmt.Errorf("%s is %s, not %s", g.Ref(), g.Status, decision)
	}
	return g, nil
}

// decided moves c to decided, unless a call before a crash did, and
// publishes its end.
func (a *actions) decided(ctx context.Context, c domain.Council, d domain.Decision) (any, error) {
	if c.Status != domain.CouncilDecided {
		if err := a.Store.Councils().UpdateStatus(ctx, c.AccountID, c.ID, c.Status, domain.CouncilDecided); err != nil {
			return nil, fmt.Errorf("end %s: %w", c.Ref(), err)
		}
	}
	if err := a.publishCouncil(ctx, c, domain.CouncilDecided, d.ID); err != nil {
		return nil, err
	}
	return decideOutput{Done: true, Members: []councils.Member{}}, nil
}

// gateDecision finds the decision a human made on the council's gate, which
// the register stored with the gate's resolution.
func (a *actions) gateDecision(ctx context.Context, c domain.Council, gateID uuid.UUID) (domain.Decision, error) {
	all, err := a.Store.Decisions().List(ctx, c.AccountID, store.DecisionFilter{CouncilID: c.ID})
	if err != nil {
		return domain.Decision{}, err
	}
	i := slices.IndexFunc(all, func(d domain.Decision) bool { return d.GateID == gateID })
	if i < 0 {
		return domain.Decision{}, fmt.Errorf("gate %s has no decision", gateID)
	}
	return all[i], nil
}

// humanRound adds a human's feedback to the constraints, once, and returns
// the human's revision round, numbered after the last round run.
func (a *actions) humanRound(ctx context.Context, c domain.Council, feedback string, members []councils.Member) (decideOutput, error) {
	all, err := a.Store.Councils().Contributions(ctx, c.AccountID, c.ID, "")
	if err != nil {
		return decideOutput{}, fmt.Errorf("list the contributions of %s: %w", c.Ref(), err)
	}
	round := 1
	for _, k := range all {
		if k.Stage == domain.StageRevision {
			round = max(round, k.Round+1)
		}
		if k.Stage == domain.StageSynthesis && k.Round >= domain.MaxSynthesisRounds {
			return decideOutput{}, fmt.Errorf("%s had its human round", c.Ref())
		}
	}
	err = a.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		cur, err := tx.Councils().GetForUpdate(ctx, c.AccountID, c.ID)
		if err != nil || slices.Contains(cur.Constraints, feedback) {
			return err
		}
		cur.Constraints = append(cur.Constraints, feedback)
		return tx.Councils().Update(ctx, &cur)
	})
	if err != nil {
		return decideOutput{}, fmt.Errorf("add the feedback to %s: %w", c.Ref(), err)
	}
	return decideOutput{Members: members, Round: round}, nil
}

// endCouncil moves a running council to to and publishes council.decided. A
// council that ended already keeps its status.
func (a *actions) endCouncil(ctx context.Context, c domain.Council, to domain.CouncilStatus) error {
	err := a.Store.Councils().UpdateStatus(ctx, c.AccountID, c.ID, domain.CouncilRunning, to)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("end %s: %w", c.Ref(), err)
	}
	cur, err := a.Store.Councils().Get(ctx, c.AccountID, c.ID)
	if err != nil {
		return fmt.Errorf("get %s: %w", c.Ref(), err)
	}
	if cur.Status == domain.CouncilDecided {
		return nil // its decision was published with it
	}
	return a.publishCouncil(ctx, c, cur.Status, uuid.Nil())
}

func (a *actions) publishCouncil(ctx context.Context, c domain.Council, status domain.CouncilStatus, decision uuid.UUID) error {
	sig, err := domain.NewSignal(c.AccountID, c.ProjectID, &domain.CouncilDecidedSignal{CouncilID: c.ID, Status: status, DecisionID: decision}, a.Now())
	if err != nil {
		return err
	}
	if _, err := a.Signals.Publish(ctx, sig); err != nil {
		return fmt.Errorf("publish the end of %s: %w", c.Ref(), err)
	}
	return nil
}

// councilSettle is the action council.settle, the finally step of the
// council playbook: a council whose run stopped early fails, or is canceled
// when a human stopped the run, and whoever waits for it is told.
func (a *actions) councilSettle(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID uuid.UUID `json:"council_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.loadCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	to := domain.CouncilFailed
	if call.Run.Status == domain.RunCanceled {
		to = domain.CouncilCanceled
	}
	if err := a.endCouncil(ctx, c, to); err != nil {
		return nil, err
	}
	return map[string]string{"status": string(to)}, nil
}

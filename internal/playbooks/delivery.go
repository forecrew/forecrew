package playbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// assessOutput is the output of goal.assess.
type assessOutput struct {
	// Done is set when the round needs no rework: the PR is ready for the
	// merge gate or a human merged it.
	Done   bool `json:"done"`
	Merged bool `json:"merged"`
	// IfReady holds the PR number when the merge gate must open, and nothing
	// otherwise, for a foreach that skips steps.
	IfReady []int `json:"if_ready"`
	// Feedback is the developer's rework instructions when Done is false.
	Feedback string `json:"feedback"`
	// GateSummary is the text of the merge_approval gate.
	GateSummary string `json:"gate_summary"`
	// SHA is the commit the gate is about, with.sha, when the merge gate
	// opens: the head github.merge_pr can be held to.
	SHA string `json:"sha,omitempty"`
}

// assess is the action goal.assess, run after the checks and the review of
// a round. It looks at PR with.number of with.repo again: a PR merged on
// GitHub meanwhile ends the round, a PR closed there fails the action. Else
// with.checks, the conclusion of the checks of the PR head (success, failure
// or none, which means the repo has no checks), and with.review, the
// reviewer's output, decide: failing checks or a request for changes give
// the rework feedback, anything else opens the merge gate, with a summary of
// the checks, the verdict, the criteria and the assumptions of the task, for
// commit with.sha.
//
// The assumption register can keep the merge gate closed: while a
// high-impact assumption the task (with.task_id) rests on is unverified, or
// one it rests on was invalidated after the developer job with.job_id
// finished, the round is not done and the feedback says which assumptions.
// Without with.task_id the register is not read.
//
// With with.require_verification the verification report of the PR at
// with.sha decides too: a failed report gives the rework feedback with what
// blocks, a passed one puts its status and link in the gate summary, and no
// report or an incomplete one is an error, since the playbook must finish
// the verification (and ask a human about missing workers) before it assesses.
func (a *actions) assess(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string          `json:"repo"`
		Number int             `json:"number"`
		Checks string          `json:"checks"`
		Review json.RawMessage `json:"review"`
		Result json.RawMessage `json:"result"`
		// SHA is the commit the developer pushed, the one the checks and the
		// review are about.
		SHA string `json:"sha"`
		// TaskID and JobID are the task and the developer job of the round.
		TaskID uuid.UUID `json:"task_id"`
		JobID  uuid.UUID `json:"job_id"`
		// RequireVerification makes the verification report of the commit
		// part of the decision.
		RequireVerification bool `json:"require_verification"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	pr, err := a.Tracker.GetPR(ctx, with.Repo, with.Number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s#%d: %w", with.Repo, with.Number, err)
	}
	switch {
	case pr.Merged:
		return assessOutput{Done: true, Merged: true, IfReady: []int{}}, nil
	case pr.State != "open":
		return nil, fmt.Errorf("pull request %s#%d was closed on GitHub: not going on", with.Repo, with.Number)
	}
	var review reviewDoc
	if err := parseOutput(with.Review, &review); err != nil {
		return nil, fmt.Errorf("with: review: %w", err)
	}
	var result devResult
	if err := parseOutput(with.Result, &result); err != nil {
		return nil, fmt.Errorf("with: result: %w", err)
	}
	switch with.Checks {
	case "success", "failure", "none":
	default:
		return nil, fmt.Errorf("with: checks %q must be success, failure or none", with.Checks)
	}
	var feedback []string
	if with.Checks == "failure" || review.Verdict != "approve" {
		feedback = append(feedback, reworkFeedback(pr.URL, with.Checks, review))
	}
	var verified string
	var unlocked bool
	if with.RequireVerification {
		var failed string
		if with.TaskID != uuid.Nil() {
			if unlocked, err = a.ranUnlocked(ctx, call.Run, with.TaskID); err != nil {
				return nil, err
			}
		}
		if verified, failed, err = a.verificationForGate(ctx, call.Run.AccountID, with.Repo, pr, with.SHA); err != nil {
			return nil, err
		}
		if failed != "" {
			feedback = append(feedback, failed)
		}
	}
	var rests []domain.Assumption
	if with.TaskID != uuid.Nil() {
		var held string
		if rests, held, err = a.assumptionFeedback(ctx, call.Run, with.TaskID, with.JobID); err != nil {
			return nil, err
		}
		if held != "" {
			feedback = append(feedback, held)
		}
	}
	if len(feedback) > 0 {
		return assessOutput{IfReady: []int{}, Feedback: strings.Join(feedback, "\n\n")}, nil
	}
	return assessOutput{Done: true, IfReady: []int{pr.Number}, SHA: with.SHA, GateSummary: gateSummary(pr.URL, with.SHA, with.Checks, verified, unlocked, review, result, rests)}, nil
}

// ranUnlocked reports whether a developer job of the task ran without the
// lock on the tests of the test engineer, which a human approved: some of its
// jobs have no locked paths although the test engineer wrote files.
func (a *actions) ranUnlocked(ctx context.Context, run domain.WorkflowRun, taskID uuid.UUID) (bool, error) {
	t, err := a.loadTask(ctx, run, taskID)
	if err != nil {
		return false, err
	}
	docs, err := a.testerOutputs(ctx, t)
	if err != nil {
		return false, err
	}
	if !slices.ContainsFunc(docs, func(d testMapDoc) bool { return len(d.Files) > 0 || len(d.Tests) > 0 }) {
		return false, nil
	}
	jobs, err := a.Store.Jobs().List(ctx, t.AccountID, store.JobFilter{TaskID: t.ID})
	if err != nil {
		return false, fmt.Errorf("list jobs of %s: %w", t.Ref(), err)
	}
	return slices.ContainsFunc(jobs, func(j domain.Job) bool {
		return j.Role == "developer" && j.Status != domain.JobCanceled && len(j.Spec.LockedPaths) == 0
	}), nil
}

// verificationForGate reads the report of the PR's head sha. It returns the
// text for the merge gate (status, summary and where the report is) when the
// report passed, and the developer's rework feedback when it failed. A head
// without a report and an incomplete report are errors.
func (a *actions) verificationForGate(ctx context.Context, accountID uuid.UUID, repo string, pr github.PullRequest, sha string) (gate, rework string, err error) {
	rep, found, err := a.headReport(ctx, accountID, repo, pr.Number, sha)
	switch {
	case err != nil:
		return "", "", err
	case !found:
		return "", "", fmt.Errorf("%s#%d has no verification report at %s: build it before assessing", repo, pr.Number, sha)
	case rep.Status == domain.VerificationIncomplete:
		return "", "", fmt.Errorf("the verification of %s#%d at %s is incomplete: finish it before assessing", repo, pr.Number, sha)
	case rep.Status == domain.VerificationFailed:
		var b strings.Builder
		fmt.Fprintf(&b, "The verification of %s at %s failed: %s\n", pr.URL, sha, rep.Summary())
		b.WriteString("\nWhat blocks the merge, from the test results of the repo; fix the code, never the tests that were written from the spec:\n")
		b.WriteString(contextpack.WrapUntrusted("verification report", strings.Join(rep.Blocking, "\n")))
		return "", strings.TrimSpace(b.String()), nil
	}
	return fmt.Sprintf("%s Report: %s (the comment and the check `forecrew/verification`).", rep.Summary(), pr.URL), "", nil
}

// assumptionFeedback returns the assumptions the task rests on and, when
// they keep its merge gate closed, the feedback that says which: the
// high-impact ones still unverified, and the ones invalidated since the
// developer job jobID finished.
func (a *actions) assumptionFeedback(ctx context.Context, run domain.WorkflowRun, taskID, jobID uuid.UUID) ([]domain.Assumption, string, error) {
	t, err := a.loadTask(ctx, run, taskID)
	if err != nil {
		return nil, "", err
	}
	rests, err := a.regs.OfTask(ctx, t.AccountID, t)
	if err != nil {
		return nil, "", err
	}
	// What was invalidated since the job was created is not in what the
	// developer was told, whether before it started or while it ran: compare
	// with its creation, not its start or end.
	var since time.Time
	if jobID != uuid.Nil() {
		j, err := a.Store.Jobs().Get(ctx, t.AccountID, jobID)
		if err != nil {
			return nil, "", fmt.Errorf("get job %s: %w", jobID, err)
		}
		since = j.CreatedAt
	}
	blocking := registers.Blocking(rests)
	asked, err := a.regs.AskedFor(ctx, t.AccountID, t.ProjectID, blocking)
	if err != nil {
		return nil, "", err
	}
	var b strings.Builder
	var open []string
	for _, as := range blocking {
		item := fmt.Sprintf("%s (verify by %s):\n%s", as.Ref(), as.VerifyBy, contextpack.WrapUntrusted("assumption "+as.Ref(), as.Statement))
		if f := asked[as.Number]; f != "" {
			item += "\nA human asked for this: " + f
		}
		open = append(open, item)
	}
	section(&b, "High-impact assumptions are not resolved, so the merge gate stays closed. Check each one if you can (a test that proves it) and say what you found; a human accepts or verifies it", open, "- ")
	var invalidated []string
	for _, as := range rests {
		if as.Status == domain.AssumptionInvalidated && as.ResolvedAt.After(since) {
			invalidated = append(invalidated, fmt.Sprintf("%s was invalidated. Evidence:\n%s%s", as.Ref(), contextpack.WrapUntrusted("evidence "+as.Ref(), as.Evidence), contextpack.WrapUntrusted("assumption "+as.Ref(), as.Statement)))
		}
	}
	section(&b, "Assumptions your change rests on were invalidated since your last run: rework it without them", invalidated, "- ")
	return rests, strings.TrimSpace(b.String()), nil
}

// holdOutput is the output of goal.hold.
type holdOutput struct {
	// Gates are the IDs of the pending assumption_review gates a human has
	// to resolve before the task's merge gate opens, for a foreach of wait
	// steps.
	Gates       []uuid.UUID `json:"gates"`
	Assumptions []string    `json:"assumptions"`
	// Clear is set when there is nothing to wait for: no review is pending,
	// or the pull request was merged or closed on GitHub, which goal.assess
	// deals with.
	Clear bool `json:"clear"`
}

// hold is the action goal.hold, run before goal.assess in a loop that ends
// when it outputs clear. It outputs the pending assumption_review gates of
// the high-impact assumptions with.task_id rests on that are still
// unverified, so that the playbook waits for a human to accept or reject
// them instead of spending rework rounds; a human who asks for changes
// opens a new review, and the loop waits again. It is read-only.
func (a *actions) hold(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID uuid.UUID `json:"task_id"`
		Repo   string    `json:"repo"`
		Number int       `json:"number"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	pr, err := a.Tracker.GetPR(ctx, with.Repo, with.Number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s#%d: %w", with.Repo, with.Number, err)
	}
	out := holdOutput{Gates: []uuid.UUID{}, Assumptions: []string{}}
	if pr.Merged || pr.State != "open" {
		out.Clear = true
		return out, nil
	}
	rests, err := a.regs.OfTask(ctx, t.AccountID, t)
	if err != nil {
		return nil, err
	}
	blocking := registers.Blocking(rests)
	gs, err := a.regs.ReviewGates(ctx, t.AccountID, t.ProjectID, blocking)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		out.Gates = append(out.Gates, g.ID)
	}
	for _, as := range blocking {
		out.Assumptions = append(out.Assumptions, as.Ref())
	}
	out.Clear = len(out.Gates) == 0
	return out, nil
}

// reworkFeedback tells the developer what to fix: the failing checks and the
// reviewer's summary, comments and unmet criteria.
func reworkFeedback(prURL, checks string, review reviewDoc) string {
	var b strings.Builder
	if checks == "failure" {
		fmt.Fprintf(&b, "The checks of %s failed. Open them, find out why and fix it.\n", prURL)
	}
	if review.Verdict != "approve" {
		fmt.Fprintf(&b, "\nThe reviewer asks for changes: %s\n", strings.TrimSpace(review.Summary))
		var lines []string
		for _, c := range review.Comments {
			lines = append(lines, commentLine(c))
		}
		section(&b, "Review comments", lines, "- ")
		var unmet []string
		for _, c := range review.CriteriaCheck {
			if c.Status != "met" {
				unmet = append(unmet, fmt.Sprintf("%s is %s: %s", c.Criterion, strings.ReplaceAll(c.Status, "_", " "), c.Evidence))
			}
		}
		section(&b, "Criteria not met", unmet, "- ")
	}
	return strings.TrimSpace(b.String())
}

// commentLine is a review comment in one line: its place, severity and text.
func commentLine(c reviewComment) string {
	place := ""
	switch {
	case c.Path != nil && c.Line != nil:
		place = fmt.Sprintf("`%s:%d` ", *c.Path, *c.Line)
	case c.Path != nil:
		place = fmt.Sprintf("`%s` ", *c.Path)
	}
	return fmt.Sprintf("%s**%s**: %s", place, c.Severity, c.Body)
}

// gateSummary is the text of the merge_approval gate.
func gateSummary(prURL, sha, checks, verified string, unlocked bool, review reviewDoc, result devResult, rests []domain.Assumption) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at %s.\n\n", prURL, sha)
	if checks == "none" {
		b.WriteString("Checks: none, the repo has no checks.\n")
	} else {
		fmt.Fprintf(&b, "Checks: %s.\n", checks)
	}
	if verified != "" {
		fmt.Fprintf(&b, "Verification: %s\n", verified)
	}
	if unlocked {
		b.WriteString("Tests: a round of the developer ran without the lock on the tests written from the spec, as a human approved. Read the diff of the test files.\n")
	}
	fmt.Fprintf(&b, "Review: %s. %s\n", strings.ReplaceAll(review.Verdict, "_", " "), strings.TrimSpace(review.Summary))
	var checked []string
	for _, c := range review.CriteriaCheck {
		checked = append(checked, c.line())
	}
	section(&b, "Criteria", checked, "- ")
	var risky, open []string
	for _, as := range rests {
		switch {
		case as.Impact == domain.LevelHigh:
			risky = append(risky, registerLine(as))
		case as.Impact == domain.LevelMedium && as.Status.Open():
			open = append(open, registerLine(as))
		}
	}
	if len(rests) == 0 {
		var stated []domain.StatedAssumption
		for _, as := range append(append([]domain.StatedAssumption{}, result.Assumptions...), review.Assumptions...) {
			if as.Impact == domain.LevelHigh {
				stated = append(stated, as)
			}
		}
		risky = assumptionLines(stated)
	}
	section(&b, "High-impact assumptions", risky, "- ")
	section(&b, "Open assumptions", open, "- ")
	b.WriteString("\nApprove to squash-merge it. Merging it on GitHub approves, closing it rejects.")
	return b.String()
}

// registerLine is a registered assumption in one line: its ref, status and
// statement.
func registerLine(a domain.Assumption) string {
	return fmt.Sprintf("%s %s (impact %s): %s", a.Ref(), a.Status, a.Impact, contextpack.OneLine(a.Statement))
}

// ifApproved is the action goal.if_approved. It outputs if_approved: [number]
// when with.decision is approved and [] otherwise, for a foreach that merges
// only after an approval.
func (a *actions) ifApproved(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Decision domain.GateStatus `json:"decision"`
		Number   int               `json:"number"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	out := map[string][]int{"if_approved": {}}
	if with.Decision == domain.GateApproved {
		out["if_approved"] = []int{with.Number}
	}
	return out, nil
}

// doneOutput is the output of goal.task_done.
type doneOutput struct {
	Done     bool   `json:"done"`
	Merged   bool   `json:"merged"`
	SHA      string `json:"sha,omitempty"`
	Feedback string `json:"feedback"`
}

// taskDone is the action goal.task_done, the last step of a delivery. When
// PR with.number of with.repo is merged, by the playbook or by a human, it
// moves with.task_id to merged and outputs done. Otherwise the human asked
// for changes at the merge gate: it outputs with.feedback for the next
// delivery. A PR closed without merging fails the action.
func (a *actions) taskDone(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID   uuid.UUID `json:"task_id"`
		Repo     string    `json:"repo"`
		Number   int       `json:"number"`
		Feedback string    `json:"feedback"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	pr, err := a.Tracker.GetPR(ctx, with.Repo, with.Number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s#%d: %w", with.Repo, with.Number, err)
	}
	switch {
	case pr.Merged:
		if _, err := a.moveTask(ctx, t, domain.TaskMerged); err != nil {
			return nil, err
		}
		return doneOutput{Done: true, Merged: true, SHA: pr.MergeSHA}, nil
	case pr.State != "open":
		return nil, fmt.Errorf("pull request %s#%d was closed without merging", with.Repo, with.Number)
	}
	feedback := strings.TrimSpace(with.Feedback)
	if feedback == "" {
		feedback = "A human asked for changes at the merge gate without saying which."
	}
	return doneOutput{Feedback: feedback}, nil
}

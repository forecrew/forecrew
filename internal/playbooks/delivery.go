package playbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
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
}

// assess is the action goal.assess, run after the checks and the review of
// a round. It looks at PR with.number of with.repo again: a PR merged on
// GitHub meanwhile ends the round, a PR closed there fails the action. Else
// with.checks, the conclusion of the checks of the PR head (success, failure
// or none, which means the repo has no checks), and with.review, the
// reviewer's output, decide: failing checks or a request for changes give
// the rework feedback, anything else opens the merge gate, with a summary of
// the checks, the verdict, the criteria and the high-impact assumptions of
// with.result (the developer's output) and the review, for commit with.sha.
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
	if with.Checks == "failure" || review.Verdict != "approve" {
		return assessOutput{IfReady: []int{}, Feedback: reworkFeedback(pr.URL, with.Checks, review)}, nil
	}
	return assessOutput{Done: true, IfReady: []int{pr.Number}, GateSummary: gateSummary(pr.URL, with.SHA, with.Checks, review, result)}, nil
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
func gateSummary(prURL, sha, checks string, review reviewDoc, result devResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at %s.\n\n", prURL, sha)
	if checks == "none" {
		b.WriteString("Checks: none, the repo has no checks.\n")
	} else {
		fmt.Fprintf(&b, "Checks: %s.\n", checks)
	}
	fmt.Fprintf(&b, "Review: %s. %s\n", strings.ReplaceAll(review.Verdict, "_", " "), strings.TrimSpace(review.Summary))
	var checked []string
	for _, c := range review.CriteriaCheck {
		checked = append(checked, c.line())
	}
	section(&b, "Criteria", checked, "- ")
	var risky []assumption
	for _, as := range append(append([]assumption{}, result.Assumptions...), review.Assumptions...) {
		if as.Impact == "high" {
			risky = append(risky, as)
		}
	}
	section(&b, "High-impact assumptions", assumptionLines(risky), "- ")
	b.WriteString("\nApprove to squash-merge it. Merging it on GitHub approves, closing it rejects.")
	return b.String()
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

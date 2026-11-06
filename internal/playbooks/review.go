package playbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

// devResult is the output of the developer role (packs/schemas/developer-result.json).
type devResult struct {
	Summary     string                    `json:"summary"`
	Assumptions []domain.StatedAssumption `json:"assumptions"`
	Escalation  *string                   `json:"escalation"`
}

// reviewComment is a comment of the reviewer.
type reviewComment struct {
	Path     *string `json:"path"`
	Line     *int    `json:"line"`
	Severity string  `json:"severity"`
	Body     string  `json:"body"`
}

// reviewDoc is the output of the reviewer role (packs/schemas/review-verdict.json).
type reviewDoc struct {
	Verdict       string                    `json:"verdict"` // approve or request_changes
	Summary       string                    `json:"summary"`
	Comments      []reviewComment           `json:"comments"`
	CriteriaCheck []criterionCheck          `json:"criteria_check"`
	Assumptions   []domain.StatedAssumption `json:"assumptions"`
}

// criterionCheck is the reviewer's verdict on one acceptance criterion.
type criterionCheck struct {
	Criterion string `json:"criterion"`
	Status    string `json:"status"` // met, not_met or unclear
	Evidence  string `json:"evidence"`
}

// line is the check in one markdown line.
func (c criterionCheck) line() string {
	return fmt.Sprintf("**%s** %s: %s", c.Criterion, strings.ReplaceAll(c.Status, "_", " "), c.Evidence)
}

// prTextOutput is the output of goal.pr_text.
type prTextOutput struct {
	// Ready is false when the developer reported an escalation: there is no
	// PR text then.
	Ready bool   `json:"ready"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// IfEscalated holds the developer's escalation, quoted as untrusted
	// data, when there is one, and nothing otherwise: a foreach over it opens
	// a gate that shows it.
	IfEscalated []string `json:"if_escalated"`
}

// prText is the action goal.pr_text. It writes the title and body of the PR
// of with.task_id from with.result, the developer's output: the agent's
// summary, the task with its criteria, the assumptions and "Closes" for the
// task's issue. When the developer reports an escalation it comments the
// reason on the task's issue, once per call, and outputs ready: false so
// that the playbook's loop asks a human.
func (a *actions) prText(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID uuid.UUID       `json:"task_id"`
		Result json.RawMessage `json:"result"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var result devResult
	if err := parseOutput(with.Result, &result); err != nil {
		return nil, fmt.Errorf("with: result: %w", err)
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	project, err := a.Store.Projects().Get(ctx, t.AccountID, t.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if e := result.Escalation; e != nil && strings.TrimSpace(*e) != "" {
		marker := fmt.Sprintf("<!-- forecrew:escalation:%s -->", call.Key)
		if err := a.commentOnce(ctx, project.Repo, t.IssueNumber, marker, marker+"\nThe developer stopped and needs a human: "+strings.TrimSpace(*e)); err != nil {
			return nil, err
		}
		return prTextOutput{IfEscalated: []string{contextpack.WrapUntrusted("developer escalation", strings.TrimSpace(*e))}}, nil
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(result.Summary) + "\n")
	section(&b, "Task "+t.Ref()+": "+t.Title, []string{strings.TrimSpace(t.Body)}, "")
	lines := assumptionLines(result.Assumptions)
	if rests, err := a.regs.OfTask(ctx, t.AccountID, t); err != nil {
		return nil, err
	} else if len(rests) > 0 {
		lines = lines[:0]
		for _, as := range rests {
			lines = append(lines, registerLine(as))
		}
	}
	section(&b, "Assumptions", lines, "- ")
	if t.IssueNumber > 0 {
		fmt.Fprintf(&b, "\nCloses #%d\n", t.IssueNumber)
	}
	return prTextOutput{Ready: true, Title: t.Ref() + ": " + t.Title, Body: b.String(), IfEscalated: []string{}}, nil
}

// reviewMarker marks a review posted by the action call with key.
func reviewMarker(key string) string { return fmt.Sprintf("<!-- forecrew:review:%s -->", key) }

// postReview is the action goal.review_post. It posts the reviewer's output
// with.review on PR with.number of with.repo as a review with the COMMENT
// event: the verdict is a recommendation for the human who merges, and the
// reviewer never approves or requests changes on GitHub. Comments on a file
// and line are inline (GitHub puts those outside the diff into the body);
// the others go into the body with the summary and the criteria check. A
// call whose review exists already posts nothing.
func (a *actions) postReview(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo   string          `json:"repo"`
		Number int             `json:"number"`
		Review json.RawMessage `json:"review"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var review reviewDoc
	if err := parseOutput(with.Review, &review); err != nil {
		return nil, fmt.Errorf("with: review: %w", err)
	}
	reviews, err := a.Tracker.ListReviews(ctx, with.Repo, with.Number)
	if err != nil {
		return nil, fmt.Errorf("list the reviews of %s#%d: %w", with.Repo, with.Number, err)
	}
	marker := reviewMarker(call.Key)
	for _, r := range reviews {
		if strings.Contains(r.Body, marker) {
			return map[string]any{"posted": false}, nil
		}
	}
	body, inline := review.render(marker)
	_, err = a.Tracker.CreateReview(ctx, with.Repo, with.Number, github.ReviewInput{Event: github.ReviewComment, Body: body, Comments: inline})
	if err != nil {
		return nil, fmt.Errorf("review %s#%d: %w", with.Repo, with.Number, err)
	}
	return map[string]any{"posted": true}, nil
}

// render returns the body of the review and its inline comments.
func (r reviewDoc) render(marker string) (string, []github.InlineComment) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n**Verdict: %s** (a recommendation: a human decides)\n\n%s\n", marker, strings.ReplaceAll(r.Verdict, "_", " "), strings.TrimSpace(r.Summary))
	var checks []string
	for _, c := range r.CriteriaCheck {
		checks = append(checks, c.line())
	}
	section(&b, "Acceptance criteria", checks, "- ")
	var inline []github.InlineComment
	var general []string
	for _, c := range r.Comments {
		text := fmt.Sprintf("**%s**: %s", c.Severity, c.Body)
		switch {
		case c.Path != nil && c.Line != nil:
			inline = append(inline, github.InlineComment{Path: *c.Path, Line: *c.Line, Body: text})
		case c.Path != nil:
			general = append(general, fmt.Sprintf("`%s`: %s", *c.Path, text))
		default:
			general = append(general, text)
		}
	}
	section(&b, "Comments", general, "- ")
	return b.String(), inline
}

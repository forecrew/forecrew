package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

// criterion is an acceptance criterion of a spec.
type criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// specDoc is the output of the product-owner role (packs/schemas/spec.json).
type specDoc struct {
	Summary            string      `json:"summary"`
	AcceptanceCriteria []criterion `json:"acceptance_criteria"`
	Scope              struct {
		In  []string `json:"in"`
		Out []string `json:"out"`
	} `json:"scope"`
	Assumptions   []domain.StatedAssumption `json:"assumptions"`
	OpenQuestions []string                  `json:"open_questions"`
}

// check reports why the spec cannot be used, if so.
func (s specDoc) check() error {
	if len(s.AcceptanceCriteria) == 0 {
		return fmt.Errorf("the spec has no acceptance criteria")
	}
	seen := map[string]bool{}
	for _, c := range s.AcceptanceCriteria {
		if c.ID == "" || seen[c.ID] {
			return fmt.Errorf("the spec has an empty or repeated criterion id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

// markdown renders the spec: criteria, scope, assumptions with their impact
// and open questions.
func (s specDoc) markdown(revision int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Spec (revision %d)\n\n%s\n", revision, strings.TrimSpace(s.Summary))
	var criteria []string
	for _, c := range s.AcceptanceCriteria {
		criteria = append(criteria, fmt.Sprintf("**%s** %s", c.ID, c.Text))
	}
	section(&b, "Acceptance criteria", criteria, "- ")
	section(&b, "In scope", s.Scope.In, "- ")
	section(&b, "Out of scope", s.Scope.Out, "- ")
	section(&b, "Assumptions", assumptionLines(s.Assumptions), "- ")
	section(&b, "Open questions", s.OpenQuestions, "- ")
	return b.String()
}

// specOutput is the output of goal.spec.
type specOutput struct {
	SpecID   uuid.UUID `json:"spec_id"`
	Revision int       `json:"revision"`
	// Summary is the text of the spec_approval gate.
	Summary string `json:"summary"`
}

// goalSpec is the action goal.spec. It stores with.spec, the output of
// product-owner job with.job_id, as the next revision of the goal's spec
// (once per job), renders it into the goal issue under the goal's own text
// and moves the goal from draft to spec_review. It outputs the spec's ID and
// revision and the summary the approval gate shows.
func (a *actions) goalSpec(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		GoalID uuid.UUID       `json:"goal_id"`
		JobID  uuid.UUID       `json:"job_id"`
		Spec   json.RawMessage `json:"spec"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.JobID == uuid.Nil() {
		return nil, fmt.Errorf("with: job_id is required")
	}
	var doc specDoc
	if err := parseOutput(with.Spec, &doc); err != nil {
		return nil, fmt.Errorf("with: spec: %w", err)
	}
	if err := doc.check(); err != nil {
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
	var spec domain.Spec
	for _, s := range specs {
		if s.JobID == with.JobID {
			spec = s
		}
	}
	if spec.ID == uuid.Nil() {
		spec = domain.Spec{
			AccountID: g.AccountID, ProjectID: g.ProjectID, GoalID: g.ID, Revision: len(specs) + 1,
			Body: doc.markdown(len(specs) + 1), JobID: with.JobID,
		}
		if err := a.Store.Specs().Create(ctx, &spec); err != nil {
			return nil, fmt.Errorf("store the spec of %s: %w", g.Ref(), err)
		}
	}
	p, err := a.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	body := strings.TrimSpace(g.Body + "\n\n---\n\n" + spec.Body)
	issue, err := a.Tracker.UpdateIssue(ctx, p.Repo, g.IssueNumber, github.IssueUpdate{Body: &body})
	if err != nil {
		return nil, fmt.Errorf("write the spec into issue %s#%d: %w", p.Repo, g.IssueNumber, err)
	}
	switch g.Status {
	case domain.GoalDraft:
		if err := a.Store.Goals().UpdateStatus(ctx, g.AccountID, g.ID, domain.GoalDraft, domain.GoalSpecReview); err != nil {
			return nil, fmt.Errorf("move %s to spec review: %w", g.Ref(), err)
		}
	case domain.GoalSpecReview:
	default:
		return nil, fmt.Errorf("%s is %s, not in spec review", g.Ref(), g.Status)
	}
	return specOutput{SpecID: spec.ID, Revision: spec.Revision, Summary: doc.gateSummary(issue.URL)}, nil
}

// gateSummary is the text of the spec_approval gate.
func (s specDoc) gateSummary(issueURL string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(s.Summary) + "\n")
	fmt.Fprintf(&b, "\n%d acceptance criteria, %d assumptions.\n", len(s.AcceptanceCriteria), len(s.Assumptions))
	section(&b, "Open questions", s.OpenQuestions, "- ")
	fmt.Fprintf(&b, "\nRead the whole spec in %s. Approve to plan the tasks; request changes to get a new revision.", issueURL)
	return b.String()
}

// parseOutput decodes the structured output of a job into v. Unknown fields
// are ignored: v holds only what the playbook uses.
func parseOutput(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return errors.New("is empty")
	}
	return json.Unmarshal(raw, v)
}

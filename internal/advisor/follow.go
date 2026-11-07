package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

var _ gates.Recorder = (*Service)(nil)

// GateResolved implements gates.Recorder: the decision on the gate of a
// proposal is the decision on the proposal, in the same transaction. A
// request for changes leaves it proposed: its run has the advisor revise it.
func (s *Service) GateResolved(ctx context.Context, tx store.Store, by domain.Actor, g domain.Gate) (func(context.Context) error, error) {
	if g.Kind != domain.GateImprovementProposal {
		return nil, nil
	}
	found, err := tx.Proposals().List(ctx, g.AccountID, store.ProposalFilter{GateID: g.ID})
	if err != nil || len(found) == 0 || found[0].Status.Final() {
		return nil, err
	}
	p := found[0]
	from := p.Status
	switch g.Status {
	case domain.GateApproved:
		err = p.Decide(by, domain.ProposalAccept, g.Feedback, time.Time{}, s.Now())
	case domain.GateRejected:
		err = p.Decide(by, domain.ProposalReject, g.Feedback, time.Time{}, s.Now())
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, tx.Proposals().Update(ctx, &p, from)
}

// Round returns the gate a proposal waits on now.
func (s *Service) Round(ctx context.Context, accountID, id uuid.UUID) (uuid.UUID, error) {
	p, err := s.Store.Proposals().Get(ctx, accountID, id)
	return p.GateID, err
}

// Assessment says how a resolved gate leaves a proposal.
type Assessment struct {
	// Settled means the human decided: no further round.
	Settled bool `json:"settled"`
	// IfRevise holds the advisor's brief when the human asked for changes,
	// so a foreach over it runs the revision only then.
	IfRevise []string `json:"if_revise"`
}

// Assess reads the gate of proposal id.
func (s *Service) Assess(ctx context.Context, accountID, id uuid.UUID) (Assessment, error) {
	p, err := s.Store.Proposals().Get(ctx, accountID, id)
	if err != nil {
		return Assessment{}, err
	}
	g, err := s.Gates.Get(ctx, accountID, p.GateID)
	if err != nil {
		return Assessment{}, err
	}
	if g.Status != domain.GateChangesRequested {
		return Assessment{Settled: true, IfRevise: []string{}}, nil
	}
	current, err := json.MarshalIndent(struct {
		Key       string                  `json:"dedupe_key"`
		Scope     domain.ProposalScope    `json:"scope"`
		Category  domain.ProposalCategory `json:"category"`
		Title     string                  `json:"title"`
		Rationale string                  `json:"rationale"`
		Benefit   string                  `json:"benefit"`
		Effort    domain.Effort           `json:"effort"`
		Risk      domain.Level            `json:"risk"`
		Action    domain.ProposalAction   `json:"action"`
	}{p.DedupeKey, p.Scope, p.Category, p.Title, p.Rationale, p.Benefit, p.Effort, p.Risk, p.Action}, "", "  ")
	if err != nil {
		return Assessment{}, err
	}
	var b strings.Builder
	b.WriteString("## The proposal\n\n")
	b.WriteString(contextpack.WrapUntrusted(p.Ref(), string(current)))
	b.WriteString("\n## The human's feedback\n\n")
	b.WriteString(contextpack.WrapUntrusted(g.Ref()+" feedback", g.Feedback))
	b.WriteString("\n## Tool catalog\n\n")
	b.WriteString(s.Catalog.text())
	return Assessment{IfRevise: []string{b.String()}}, nil
}

// Reopen applies the advisor's revision, when it passes, and opens the next
// gate of proposal id. key is the action's. A revision that breaks a rule is
// left out and the gate says why, so the human decides on what stands.
func (s *Service) Reopen(ctx context.Context, key string, accountID, id uuid.UUID, d *Draft) error {
	p, err := s.Store.Proposals().Get(ctx, accountID, id)
	if err != nil {
		return err
	}
	project, err := s.Store.Projects().Get(ctx, accountID, p.ProjectID)
	if err != nil {
		return err
	}
	revised, note := p, "The advisor left no revision; this is the proposal as it stood."
	if d != nil {
		note = "Revised after your feedback."
		if why := s.apply(&revised, *d); why != "" {
			revised, note = p, "The revision was refused ("+contextpack.OneLine(why)+"); this is the proposal as it stood."
		}
	}
	_, err = s.Gates.Open(ctx, gates.OpenRequest{
		ID: deriveID("gate", key), AccountID: accountID, ProjectID: p.ProjectID, Kind: domain.GateImprovementProposal,
		Title: revised.Title, Summary: gateSummary(revised, note), SubjectRepo: project.Repo,
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			revised.GateID = g.ID
			return tx.Proposals().Update(ctx, &revised, p.Status)
		},
	})
	return err
}

// Carried says what accepting a proposal started.
type Carried struct {
	// IfPR holds the PR of a config_pr action, for a wait on its merge.
	IfPR    []int  `json:"if_pr"`
	GoalRef string `json:"goal_ref,omitempty"`
}

// Carry starts the action of proposal id once it is accepted: a goal action
// creates and starts the goal, a config_pr action opens the PR, and a setup
// action is the human's to do and mark. Anything else does nothing.
// Repeating it finds what the first call made.
func (s *Service) Carry(ctx context.Context, accountID, id uuid.UUID) (Carried, error) {
	out := Carried{IfPR: []int{}}
	x, err := s.Store.Proposals().Get(ctx, accountID, id)
	if err != nil || x.Status != domain.ProposalAccepted {
		return out, err
	}
	p, err := s.Store.Projects().Get(ctx, accountID, x.ProjectID)
	if err != nil {
		return out, err
	}
	switch {
	case x.Action.Goal != nil:
		out.GoalRef, err = s.startGoal(ctx, p, x)
	case x.Action.ConfigPR != nil:
		var n int
		if n, err = s.openPR(ctx, p, x); err == nil {
			out.IfPR = append(out.IfPR, n)
		}
	}
	return out, err
}

// startGoal creates the goal of x, linked to it, and starts it. forecrewd
// makes the goal from what the advisor wrote, so the body is marked as data.
func (s *Service) startGoal(ctx context.Context, p domain.Project, x domain.Proposal) (string, error) {
	a := x.Action.Goal
	g := domain.Goal{
		ID: deriveID("goal", x.ID.String()), AccountID: x.AccountID, ProjectID: x.ProjectID, Kind: domain.GoalFeature, Title: contextpack.OneLine(a.Title),
		Body:      fmt.Sprintf("%s was accepted by %s.\n\n%s", x.Ref(), x.DecidedBy, contextpack.WrapUntrusted("proposal "+x.Ref(), a.Body)),
		Platforms: domain.NormalizeLabels(a.Platforms), Status: domain.GoalDraft, CreatedBy: x.DecidedBy,
	}
	if x.GoalID == uuid.Nil() {
		err := s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := tx.Goals().Create(ctx, &g); err != nil {
				return err
			}
			from := x.Status
			x.GoalID = g.ID
			return tx.Proposals().Update(ctx, &x, from)
		})
		if err != nil {
			return "", fmt.Errorf("create the goal of %s: %w", x.Ref(), err)
		}
	} else {
		var err error
		if g, err = s.Store.Goals().Get(ctx, x.AccountID, x.GoalID); err != nil {
			return "", err
		}
	}
	_, err := s.Runs.Start(ctx, s.GoalPlaybook(p), workflow.StartRequest{
		ID: g.ID, AccountID: x.AccountID, ProjectID: x.ProjectID, GoalID: g.ID,
		Inputs: map[string]any{"repo": p.Repo, "goal_id": g.ID.String(), "platforms": g.Platforms},
	})
	if err != nil {
		return "", fmt.Errorf("start %s: %w", g.Ref(), err)
	}
	return g.Ref(), nil
}

// openPR writes the files of x on agent/prop-<n> and opens the PR, which a
// human merges. The files passed check when x was filed; a policy or a
// profile may have moved on, which the human reads in the PR.
func (s *Service) openPR(ctx context.Context, p domain.Project, x domain.Proposal) (int, error) {
	branch := fmt.Sprintf("agent/prop-%d", x.Number)
	head, err := s.GitHub.HeadSHA(ctx, p.Repo, p.DefaultBranch)
	if err != nil {
		return 0, err
	}
	if err := s.GitHub.CreateBranch(ctx, p.Repo, branch, head); err != nil {
		return 0, err
	}
	for _, f := range x.Action.ConfigPR.Files {
		if _, err := s.GitHub.WriteFile(ctx, p.Repo, branch, f.Path, []byte(f.Content), x.Ref()+": "+x.Title); err != nil {
			return 0, err
		}
	}
	pr, err := s.GitHub.FindPR(ctx, p.Repo, branch)
	if errors.Is(err, github.ErrNotFound) {
		body := fmt.Sprintf("%s was accepted by %s. A human merges this.\n\n%s", x.Ref(), x.DecidedBy, contextpack.WrapUntrusted("proposal "+x.Ref(), x.Rationale))
		pr, err = s.GitHub.CreatePR(ctx, p.Repo, github.PRInput{Title: x.Title, Body: body, Head: branch, Base: p.DefaultBranch})
	}
	if err != nil {
		return 0, fmt.Errorf("open the pull request of %s: %w", x.Ref(), err)
	}
	c := domain.Change{AccountID: x.AccountID, ProjectID: x.ProjectID, Repo: p.Repo, Number: pr.Number, Branch: branch, BaseRef: p.DefaultBranch,
		HeadSHA: pr.HeadSHA, State: domain.ChangeOpen, URL: pr.URL}
	if err := s.Store.Changes().Upsert(ctx, &c); err != nil {
		return 0, fmt.Errorf("record pull request %s#%d: %w", p.Repo, pr.Number, err)
	}
	return pr.Number, nil
}

// finish moves an accepted proposal to done, or to superseded when what
// carried it out ended without result. Anything else is left alone.
func (s *Service) finish(ctx context.Context, x domain.Proposal, done bool, reason string) error {
	if x.Status != domain.ProposalAccepted {
		return nil
	}
	x.Status = domain.ProposalDone
	if !done {
		x.Status, x.Reason = domain.ProposalSuperseded, reason
	}
	if err := s.Store.Proposals().Update(ctx, &x, domain.ProposalAccepted); err != nil {
		return fmt.Errorf("finish %s: %w", x.Ref(), err)
	}
	return nil
}

// PREnded settles a config_pr proposal: signal is the wait's, pr.merged or
// pr.closed.
func (s *Service) PREnded(ctx context.Context, accountID, id uuid.UUID, signal domain.SignalKind) error {
	x, err := s.Store.Proposals().Get(ctx, accountID, id)
	if err != nil {
		return err
	}
	return s.finish(ctx, x, signal == domain.SignalPRMerged, "the pull request was closed without merging")
}

// GoalEnded settles the proposal linked to g, which is done or canceled.
func (s *Service) GoalEnded(ctx context.Context, g domain.Goal) error {
	found, err := s.Store.Proposals().List(ctx, g.AccountID, store.ProposalFilter{GoalID: g.ID})
	if err != nil {
		return fmt.Errorf("find the proposal of %s: %w", g.Ref(), err)
	}
	for _, x := range found {
		if err := s.finish(ctx, x, g.Status == domain.GoalDone, g.Ref()+" was canceled"); err != nil {
			return err
		}
	}
	return nil
}

// Abandon ends a proposal whose run failed or was canceled: nothing follows
// it any more, so it is superseded and its pending gate closed. A proposal
// that is final stays.
func (s *Service) Abandon(ctx context.Context, accountID, id uuid.UUID) error {
	x, err := s.Store.Proposals().Get(ctx, accountID, id)
	if err != nil {
		return err
	}
	if x.Status.Final() {
		return nil
	}
	from := x.Status
	x.Status, x.SnoozedUntil, x.Reason = domain.ProposalSuperseded, time.Time{}, "its run ended"
	if err := s.Store.Proposals().Update(ctx, &x, from); err != nil {
		return fmt.Errorf("abandon %s: %w", x.Ref(), err)
	}
	g, err := s.Gates.Get(ctx, accountID, x.GateID)
	if err != nil || g.Status != domain.GatePending {
		return err
	}
	_, err = s.Gates.Resolve(ctx, domain.SystemActor(accountID, domain.ForecrewdName), g.ID, domain.GateRejected, "The run of "+x.Ref()+" ended.")
	return err
}

// RetroText lists the outcomes measured lately for a goal's retro, "" when
// there are none.
func (s *Service) RetroText(ctx context.Context, accountID, projectID uuid.UUID) (string, error) {
	done, err := s.Store.Proposals().List(ctx, accountID, store.ProposalFilter{ProjectID: projectID, Status: domain.ProposalDone})
	if err != nil {
		return "", fmt.Errorf("list the finished proposals: %w", err)
	}
	var b strings.Builder
	for _, x := range done {
		if x.Outcome == nil || x.Measure == nil || s.Now().After(x.Outcome.MeasuredAt.Add(retroWindow)) {
			continue
		}
		fmt.Fprintf(&b, "\n%s, measured by %s on %s: %.1f before, %.1f after.\n", x.Ref(), x.Measure.Detector, x.Outcome.MeasuredAt.Format(time.DateOnly), x.Outcome.Before, x.Outcome.After)
		b.WriteString(contextpack.WrapUntrusted(x.Ref()+" title", x.Title))
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "\n## Improvements measured lately\n" + b.String(), nil
}

// StartAfterGoal starts the advise playbook for a goal that was just learned
// from, unless a run of it is going on for the project.
func (s *Service) StartAfterGoal(ctx context.Context, run domain.WorkflowRun, goal uuid.UUID) (bool, error) {
	runs, err := s.Store.WorkflowRuns().ListActive(ctx, run.AccountID)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(runs, func(r domain.WorkflowRun) bool { return r.Playbook == PlaybookAdvise && r.ProjectID == run.ProjectID }) {
		return false, nil
	}
	p, err := s.Store.Projects().Get(ctx, run.AccountID, run.ProjectID)
	if err != nil {
		return false, err
	}
	_, err = s.Runs.Start(ctx, PlaybookAdvise, workflow.StartRequest{
		ID: deriveID("advise", goal.String()), AccountID: p.AccountID, ProjectID: p.ID, Inputs: map[string]any{"repo": p.Repo},
	})
	return err == nil, err
}

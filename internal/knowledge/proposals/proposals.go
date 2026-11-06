// Package proposals is the flow of changes to curated knowledge
// (docs/contracts/knowledge.md, Proposals): agents and humans propose, the
// policy decides what is allowed and who approves, and each provider's own
// review flow carries the change. It sits above internal/knowledge because
// it opens gates and writes pull requests.
package proposals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
)

// Playbooks of the flow.
const (
	// PlaybookProposal takes the inputs repo, job_id and provider.
	PlaybookProposal = "knowledge-proposal"
	// PlaybookAccess takes the inputs gate_id, section, level and role.
	PlaybookAccess = "knowledge-access"
)

// Errors of the service.
var (
	// ErrForbidden means the policy does not allow the call. What was
	// refused is in the audit log.
	ErrForbidden = errors.New("not allowed by the knowledge policy")
	// ErrInvalid means the input breaks a rule.
	ErrInvalid = errors.New("invalid knowledge proposal")
)

// Service runs the flow.
type Service struct {
	Store store.Store
	// Policy returns the policy of a project; the Syncer's Policy.
	Policy func(context.Context, domain.Project) (knowledge.Policy, error)
	GitHub github.Tracker
	Gates  gates.Service
	Roles  roles.Resolver
	Log    *slog.Logger // defaults to discarding
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// proposer returns the review flow of a provider, nil for one without its
// own: a knowledge_proposal gate stands in. Only the repo has one so far;
// the external providers join here (4.19).
func (s *Service) proposer(p knowledge.Policy, provider string) knowledge.Proposer {
	if p.Providers[provider].Kind == domain.ProviderKindRepo {
		return &RepoProposer{Store: s.Store, GitHub: s.GitHub}
	}
	return nil
}

// Input is a proposed change.
type Input struct {
	Section string
	Path    string
	Action  domain.KnowledgeAction
	Body    string
	Reason  string
}

// Submit records a proposal of job j. What the policy refuses is dropped
// and audited, and wraps ErrForbidden. A proposal of a goal's job to a
// section that is not protected waits for the goal's learn step (per_goal);
// the others go out when the job completes (standalone). Calling it again
// with the same change, as a retried attempt does, returns the same row.
func (s *Service) Submit(ctx context.Context, j domain.Job, in Input) (domain.KnowledgeProposal, error) {
	project, err := s.Store.Projects().Get(ctx, j.AccountID, j.ProjectID)
	if err != nil {
		return domain.KnowledgeProposal{}, fmt.Errorf("project of %s: %w", j.Ref(), err)
	}
	policy, err := s.Policy(ctx, project)
	if err != nil {
		return domain.KnowledgeProposal{}, err
	}
	t, err := policy.Target(in.Section, in.Path, j.Role)
	if errors.Is(err, knowledge.ErrRefused) {
		if aerr := s.drop(ctx, s.Store, j.AccountID, j.ProjectID, "job", j.ID, in, j.Role, err); aerr != nil {
			return domain.KnowledgeProposal{}, aerr
		}
		return domain.KnowledgeProposal{}, fmt.Errorf("%w: %w", ErrForbidden, err)
	}
	if err != nil {
		return domain.KnowledgeProposal{}, err
	}
	if err := s.checkAction(ctx, project, t, in); err != nil {
		return domain.KnowledgeProposal{}, err
	}
	if k, ok, err := s.findSame(ctx, j, in); ok || err != nil {
		return k, err
	}
	k := domain.KnowledgeProposal{
		AccountID: j.AccountID, ProjectID: j.ProjectID, Provider: t.Provider, Section: in.Section, Path: in.Path, Action: in.Action,
		Body: in.Body, Reason: in.Reason, ProposedBy: j.Role, JobID: j.ID, GoalID: j.GoalID, Batch: domain.BatchStandalone, Status: domain.KnowledgeQueued,
	}
	if j.GoalID != uuid.Nil() && !t.Protected {
		k.Batch = domain.BatchPerGoal
	}
	if err := k.Validate(); err != nil {
		return domain.KnowledgeProposal{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	err = s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.KnowledgeProposals().Create(ctx, &k); err != nil {
			return err
		}
		return audit(ctx, tx, k, domain.SystemActor(k.AccountID, domain.ForecrewdName).String(), "knowledge.proposed", map[string]any{"job": j.Ref(), "role": j.Role})
	})
	return k, err
}

// checkAction refuses what cannot be applied: a change without text, text
// on a retirement, and an edit or retirement of an entry the index does not
// have (a retirement of a file that is not there would make an empty PR).
func (s *Service) checkAction(ctx context.Context, p domain.Project, t knowledge.Target, in Input) error {
	if !slices.Contains(domain.KnowledgeActions(), in.Action) {
		return fmt.Errorf("%w: action %q must be add, edit or retire", ErrInvalid, in.Action)
	}
	if (in.Action == domain.KnowledgeRetire) != (strings.TrimSpace(in.Body) == "") {
		return fmt.Errorf("%w: body is required, and empty only to retire", ErrInvalid)
	}
	if in.Action == domain.KnowledgeAdd {
		return nil
	}
	_, err := s.Store.Knowledge().GetByPath(ctx, p.AccountID, p.ID, t.Provider, in.Path)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: there is no entry at %s to %s", ErrInvalid, in.Path, in.Action)
	}
	return err
}

func (s *Service) findSame(ctx context.Context, j domain.Job, in Input) (domain.KnowledgeProposal, bool, error) {
	have, err := s.Store.KnowledgeProposals().List(ctx, j.AccountID, store.KnowledgeProposalFilter{JobID: j.ID})
	if err != nil {
		return domain.KnowledgeProposal{}, false, err
	}
	for _, k := range have {
		if k.Section == in.Section && k.Path == in.Path && k.Action == in.Action && k.Body == in.Body && k.Status != domain.KnowledgeRejected {
			return k, true, nil
		}
	}
	return domain.KnowledgeProposal{}, false, nil
}

// drop writes the audit entry of a refused proposal.
func (s *Service) drop(ctx context.Context, st store.Store, accountID, projectID uuid.UUID, subject string, id uuid.UUID, in Input, by string, why error) error {
	raw, err := json.Marshal(map[string]string{"section": in.Section, "path": in.Path, "action": string(in.Action), "proposed_by": by, "reason": why.Error()})
	if err != nil {
		return err
	}
	s.log().WarnContext(ctx, "knowledge proposal dropped", "section", in.Section, "proposed_by", by, "reason", why)
	return st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: accountID, ProjectID: projectID, Actor: domain.SystemActor(accountID, domain.ForecrewdName).String(),
		Action: "knowledge.proposal_dropped", SubjectType: subject, SubjectID: id, Data: raw,
	})
}

func audit(ctx context.Context, st store.Store, k domain.KnowledgeProposal, actor, action string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["proposal"] = k.Ref()
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: k.AccountID, ProjectID: k.ProjectID, Actor: actor, Action: action, SubjectType: "knowledge_proposal", SubjectID: k.ID, Data: raw,
	})
}

// HumanResult is what a human's proposal became.
type HumanResult struct {
	Proposal domain.KnowledgeProposal
	Number   int
	URL      string
}

// Human sends a human's proposal straight to a PR: no job, no gate. For a
// protected section by is recorded as approver. The PR merges on GitHub
// like any other; PRClosed settles the proposal. The action defaults to edit
// when the entry exists, else add. A provider without a review flow is
// changed in the provider itself, so it wraps ErrInvalid. check is told, once
// the section is placed and before anything is stored, whether it is
// protected; its error stops the proposal, because the caller of a protected
// one is recorded as its approver.
func (s *Service) Human(ctx context.Context, by domain.Actor, project domain.Project, in Input, check func(protected bool) error) (HumanResult, error) {
	policy, err := s.Policy(ctx, project)
	if err != nil {
		return HumanResult{}, err
	}
	t, err := policy.Target(in.Section, in.Path, "")
	if errors.Is(err, knowledge.ErrRefused) {
		return HumanResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err != nil {
		return HumanResult{}, err
	}
	if err := check(t.Protected); err != nil {
		return HumanResult{}, err
	}
	proposer := s.proposer(policy, t.Provider)
	if proposer == nil {
		return HumanResult{}, fmt.Errorf("%w: provider %s has no review flow: change the entry there", ErrInvalid, t.Provider)
	}
	if in.Action == "" {
		in.Action = domain.KnowledgeAdd
		if _, err := s.Store.Knowledge().GetByPath(ctx, project.AccountID, project.ID, t.Provider, in.Path); err == nil {
			in.Action = domain.KnowledgeEdit
		}
	}
	if err := s.checkAction(ctx, project, t, in); err != nil {
		return HumanResult{}, err
	}
	k := domain.KnowledgeProposal{
		AccountID: project.AccountID, ProjectID: project.ID, Provider: t.Provider, Section: in.Section, Path: in.Path, Action: in.Action,
		Body: in.Body, Reason: in.Reason, ProposedBy: by.String(), Batch: domain.BatchStandalone, Status: domain.KnowledgeQueued,
	}
	if err := k.Validate(); err != nil {
		return HumanResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	err = s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.KnowledgeProposals().Create(ctx, &k); err != nil {
			return err
		}
		return audit(ctx, tx, k, by.String(), "knowledge.proposed", nil)
	})
	if err != nil {
		return HumanResult{}, err
	}
	if err := s.send(ctx, proposer, &k); err != nil {
		return HumanResult{}, errors.Join(err, s.finish(ctx, []domain.KnowledgeProposal{k}, domain.KnowledgeRejected, "the provider refused it"))
	}
	number, _ := strconv.Atoi(k.ExternalRef)
	pr, err := s.GitHub.GetPR(ctx, project.Repo, number)
	if err != nil {
		return HumanResult{}, fmt.Errorf("get pull request of %s: %w", k.Ref(), err)
	}
	if t.Protected {
		if err := audit(ctx, s.Store, k, by.String(), "knowledge.proposal_approved", map[string]any{"sections": t.Sections}); err != nil {
			return HumanResult{}, err
		}
		note := fmt.Sprintf("%s changes a protected section (%s). Approved by %s, who proposed it.", k.Ref(), strings.Join(t.Sections, ", "), by.String())
		if _, err := s.GitHub.CreateComment(ctx, project.Repo, number, note); err != nil {
			return HumanResult{}, fmt.Errorf("record the approver of %s: %w", k.Ref(), err)
		}
	}
	return HumanResult{Proposal: k, Number: number, URL: pr.URL}, nil
}

// send gives k to its provider and records the external ref and the change.
func (s *Service) send(ctx context.Context, p knowledge.Proposer, k *domain.KnowledgeProposal) error {
	ref, err := p.Propose(ctx, *k)
	if err != nil {
		return fmt.Errorf("send %s to provider %s: %w", k.Ref(), k.Provider, err)
	}
	from := k.Status
	k.Status, k.ExternalRef = domain.KnowledgeOpen, ref
	if _, isRepo := p.(*RepoProposer); isRepo {
		project, err := s.Store.Projects().Get(ctx, k.AccountID, k.ProjectID)
		if err != nil {
			return err
		}
		number, _ := strconv.Atoi(ref) // RepoProposer's refs are numbers
		c, err := s.Store.Changes().GetByPR(ctx, k.AccountID, project.Repo, number)
		if err != nil {
			return fmt.Errorf("change of %s: %w", k.Ref(), err)
		}
		k.ChangeID = c.ID
	}
	return s.Store.KnowledgeProposals().Update(ctx, k, from)
}

// finish moves proposals still in review to status, accepted or rejected,
// and audits it. why goes into the audit entry.
func (s *Service) finish(ctx context.Context, ks []domain.KnowledgeProposal, status domain.KnowledgeProposalStatus, why string) error {
	action := "knowledge.proposal_accepted"
	if status == domain.KnowledgeRejected {
		action = "knowledge.proposal_rejected"
	}
	return s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		for i := range ks {
			k := ks[i]
			from := k.Status
			if !from.CanTransition(status) {
				continue
			}
			k.Status = status
			if err := tx.KnowledgeProposals().Update(ctx, &k, from); err != nil {
				return err
			}
			if err := audit(ctx, tx, k, domain.SystemActor(k.AccountID, domain.ForecrewdName).String(), action, map[string]any{"why": why}); err != nil {
				return err
			}
		}
		return nil
	})
}

// PRClosed settles the proposals a closed PR carried: merged accepts them,
// closed rejects them. A PR that is not a proposal's is ignored. It is
// idempotent, for the webhook and the poller.
func (s *Service) PRClosed(ctx context.Context, accountID uuid.UUID, repo string, number int, merged bool) error {
	c, err := s.Store.Changes().GetByPR(ctx, accountID, repo, number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	open, err := s.Store.KnowledgeProposals().List(ctx, accountID, store.KnowledgeProposalFilter{ChangeID: c.ID, Status: domain.KnowledgeOpen})
	if err != nil {
		return fmt.Errorf("list proposals of %s#%d: %w", repo, number, err)
	}
	status, why := domain.KnowledgeRejected, "the pull request was closed"
	if merged {
		status, why = domain.KnowledgeAccepted, "the pull request was merged"
	}
	return s.finish(ctx, open, status, why)
}

// JobCompleted implements jobs.Registrar: in the transaction that finishes
// j it starts one run per provider that has standalone proposals of j, so
// no crash can lose them. The engine's sweep picks the runs up.
func (s *Service) JobCompleted(ctx context.Context, tx store.Store, j domain.Job) error {
	queued, err := tx.KnowledgeProposals().List(ctx, j.AccountID, store.KnowledgeProposalFilter{
		JobID: j.ID, Batch: domain.BatchStandalone, Status: domain.KnowledgeQueued,
	})
	if err != nil {
		return fmt.Errorf("list proposals of %s: %w", j.Ref(), err)
	}
	var providers []string
	for _, k := range queued {
		providers = append(providers, k.Provider)
	}
	slices.Sort(providers)
	for _, provider := range slices.Compact(providers) {
		err := startRun(ctx, tx, PlaybookProposal, j.AccountID, j.ProjectID, j.GoalID, j.TaskID,
			map[string]any{"repo": j.Spec.Repo, "job_id": j.ID, "provider": provider})
		if err != nil {
			return fmt.Errorf("start the proposals of %s: %w", j.Ref(), err)
		}
	}
	return nil
}

// startRun stores a run for the engine's sweep to advance. It works in a
// caller's transaction, which Engine.Start does not.
func startRun(ctx context.Context, tx store.Store, playbook string, accountID, projectID, goalID, taskID uuid.UUID, inputs map[string]any) error {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	return tx.WorkflowRuns().Create(ctx, &domain.WorkflowRun{
		AccountID: accountID, ProjectID: projectID, GoalID: goalID, TaskID: taskID, Playbook: playbook, Status: domain.RunRunning, Inputs: raw,
	})
}

// refs returns the human refs of ks.
func refs(ks []domain.KnowledgeProposal) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.Ref()
	}
	return out
}

// sectionNames lists the distinct sections of ks.
func sectionNames(ks []domain.KnowledgeProposal) string {
	var out []string
	for _, k := range ks {
		out = append(out, k.Section)
	}
	slices.Sort(out)
	return strings.Join(slices.Compact(out), ", ")
}

package proposals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
)

// Batch names what one run of PlaybookProposal carries: the standalone
// proposals of a job to one provider.
type Batch struct {
	AccountID uuid.UUID
	ProjectID uuid.UUID
	JobID     uuid.UUID
	Provider  string
}

// load returns the proposals of b still in review, with their project and
// the PR they share (0 when they have none).
func (s *Service) load(ctx context.Context, b Batch) (domain.Project, []domain.KnowledgeProposal, int, error) {
	p, err := s.Store.Projects().Get(ctx, b.AccountID, b.ProjectID)
	if err != nil {
		return domain.Project{}, nil, 0, err
	}
	all, err := s.Store.KnowledgeProposals().List(ctx, b.AccountID, store.KnowledgeProposalFilter{JobID: b.JobID, Batch: domain.BatchStandalone})
	if err != nil {
		return domain.Project{}, nil, 0, err
	}
	ks := slices.DeleteFunc(all, func(k domain.KnowledgeProposal) bool {
		return k.Provider != b.Provider || k.Status == domain.KnowledgeAccepted || k.Status == domain.KnowledgeRejected
	})
	number := 0
	for _, k := range ks {
		if k.ChangeID != uuid.Nil() {
			number, _ = strconv.Atoi(k.ExternalRef)
		}
	}
	return p, ks, number, nil
}

// route is who has to approve a set of proposals.
type route struct {
	human      bool     // a human gate: a protected or human-owned section, or no review flow
	codeowners bool     // GitHub's reviewers approve and a human merges
	reviewers  []string // roles that review; none when a human decides anyway
}

func (s *Service) route(ctx context.Context, p domain.Project, policy knowledge.Policy, ks []domain.KnowledgeProposal) (route, error) {
	var r route
	for _, k := range ks {
		t, err := policy.Target(k.Section, k.Path, "")
		if errors.Is(err, knowledge.ErrRefused) || s.proposer(policy, k.Provider) == nil {
			r.human = true // the policy moved on, or the flow is a gate: a human looks
			continue
		}
		if err != nil {
			return route{}, err
		}
		for _, owner := range t.Owners {
			switch owner {
			case knowledge.OwnerHuman:
				r.human = true
			case knowledge.OwnerCodeowners:
				r.codeowners = true
			default:
				_, err := s.Roles.Resolve(ctx, p.AccountID, p.ID, owner)
				switch {
				case errors.Is(err, roles.ErrUnknownRole):
					r.human = true
				case err != nil:
					return route{}, err
				default:
					r.reviewers = append(r.reviewers, owner)
				}
			}
		}
	}
	slices.Sort(r.reviewers)
	r.reviewers = slices.Compact(r.reviewers)
	if r.human {
		r.reviewers = nil
	}
	return r, nil
}

// Sent is the output of knowledge.propose.
type Sent struct {
	Proposals []string `json:"proposals"`
	Number    int      `json:"number"` // the PR, 0 for a provider without a review flow
	Branch    string   `json:"branch"`
	HeadSHA   string   `json:"head_sha"`
	// Reviewers are the owner roles that review the change.
	Reviewers []string `json:"reviewers"`
	// ReviewText tells a reviewing job what to look at.
	ReviewText string `json:"review_text"`
}

// Send gives the proposals of b to their provider. It checks them against
// the policy again, which may have changed since the job proposed them: what
// the proposer may no longer propose is dropped and audited.
func (s *Service) Send(ctx context.Context, b Batch) (Sent, error) {
	p, ks, _, err := s.load(ctx, b)
	if err != nil {
		return Sent{}, err
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return Sent{}, err
	}
	var live []domain.KnowledgeProposal
	for _, k := range ks {
		_, err := policy.Target(k.Section, k.Path, k.ProposedBy)
		if errors.Is(err, knowledge.ErrRefused) {
			in := Input{Section: k.Section, Path: k.Path, Action: k.Action}
			if err := errors.Join(s.drop(ctx, s.Store, k.AccountID, k.ProjectID, "knowledge_proposal", k.ID, in, k.ProposedBy, err),
				s.finish(ctx, []domain.KnowledgeProposal{k}, domain.KnowledgeRejected, err.Error())); err != nil {
				return Sent{}, err
			}
			continue
		}
		if err != nil {
			return Sent{}, err
		}
		live = append(live, k)
	}
	out := Sent{Proposals: refs(live), Reviewers: []string{}}
	proposer := s.proposer(policy, b.Provider)
	if len(live) == 0 || proposer == nil {
		return out, nil
	}
	for i := range live {
		if live[i].Status == domain.KnowledgeQueued {
			if err := s.send(ctx, proposer, &live[i]); err != nil {
				return Sent{}, err
			}
		}
	}
	c, err := s.Store.Changes().Get(ctx, b.AccountID, live[0].ChangeID)
	if err != nil {
		return Sent{}, fmt.Errorf("change of %s: %w", live[0].Ref(), err)
	}
	r, err := s.route(ctx, p, policy, live)
	if err != nil {
		return Sent{}, err
	}
	out.Number, out.Branch, out.HeadSHA = c.Number, c.Branch, c.HeadSHA
	out.Reviewers = append(out.Reviewers, r.reviewers...)
	out.ReviewText = reviewText(live, c)
	return out, nil
}

// reviewText is what a reviewing job is shown. Reasons are the proposer's
// text, so they are marked; the change itself is on the branch.
func reviewText(ks []domain.KnowledgeProposal, c domain.Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The change is on branch %s (pull request #%d).\n\n", c.Branch, c.Number)
	for _, k := range ks {
		fmt.Fprintf(&b, "%s: %s %s in section %s, proposed by %s.\n", k.Ref(), k.Action, contextpack.OneLine(k.Path), k.Section, k.ProposedBy)
		b.WriteString(contextpack.WrapUntrusted(k.Ref()+" reason", k.Reason))
	}
	return b.String()
}

// Verdict is the output of a reviewing job, as far as the flow reads it.
type Verdict struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
}

// Assess opens the knowledge_proposal gate when a human has to decide: a
// protected or human-owned section, a provider without a review flow, a
// verdict other than approve. key makes it idempotent. It returns the gates
// to wait for. A PR merged or closed meanwhile needs no gate.
func (s *Service) Assess(ctx context.Context, b Batch, key uuid.UUID, verdicts []Verdict) ([]string, error) {
	p, ks, number, err := s.load(ctx, b)
	if err != nil || len(ks) == 0 {
		return []string{}, err
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return nil, err
	}
	r, err := s.route(ctx, p, policy, ks)
	if err != nil {
		return nil, err
	}
	disputed := slices.ContainsFunc(verdicts, func(v Verdict) bool { return v.Verdict != "approve" })
	if !r.human && !disputed {
		return []string{}, nil
	}
	if number > 0 {
		pr, err := s.GitHub.GetPR(ctx, p.Repo, number)
		if err != nil {
			return nil, fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
		}
		if pr.State != "open" {
			return []string{}, nil
		}
	}
	g, err := s.Gates.Open(ctx, gates.OpenRequest{
		ID: key, AccountID: b.AccountID, ProjectID: b.ProjectID, Kind: domain.GateKnowledgeProposal,
		Title:   fmt.Sprintf("%s: change to %s", strings.Join(refs(ks), ", "), sectionNames(ks)),
		Summary: gateSummary(ks, number, verdicts), SubjectRepo: p.Repo, SubjectNumber: number, GoalID: ks[0].GoalID, JobID: b.JobID,
		// In the gate's transaction: its decision must find the proposals.
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			for _, k := range ks {
				from := k.Status
				k.Status, k.GateID = domain.KnowledgeOpen, g.ID
				if err := tx.KnowledgeProposals().Update(ctx, &k, from); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return []string{g.ID.String()}, nil
}

// gateSummary shows the change to a human. With a PR the diff is there;
// without one the summary is what the human copies across. All of it is
// agent text, so all of it is marked.
func gateSummary(ks []domain.KnowledgeProposal, number int, verdicts []Verdict) string {
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, "### %s: %s %s (section %s, provider %s)\n\nProposed by %s.\n\n", k.Ref(), k.Action, contextpack.OneLine(k.Path), k.Section, k.Provider, k.ProposedBy)
		b.WriteString(contextpack.WrapUntrusted(k.Ref()+" reason", k.Reason))
		if number == 0 && k.Body != "" {
			b.WriteString("\nThis provider has no review flow here: approving says you copied the change across.\n\n")
			b.WriteString(contextpack.WrapUntrusted(k.Ref()+" body", k.Body))
		}
	}
	for _, v := range verdicts {
		fmt.Fprintf(&b, "\nThe owner's review says %s:\n\n", contextpack.OneLine(v.Verdict))
		b.WriteString(contextpack.WrapUntrusted("owner review", v.Summary))
	}
	return b.String()
}

// Decision is the output of knowledge.decide: lists for foreach steps.
type Decision struct {
	// IfMerge holds the PR when forecrewd merges it.
	IfMerge []int `json:"if_merge"`
	// IfFollow holds the PR when its reviewers and a human merge it.
	IfFollow []int `json:"if_follow"`
}

// Decide applies the gate's decision. Anything but an approval rejects the
// proposals, with the human's feedback in the audit log, and closes the PR.
// Then it says how an approved PR gets merged: owner codeowners leaves that
// to GitHub, the others to forecrewd.
func (s *Service) Decide(ctx context.Context, b Batch) (Decision, error) {
	out := Decision{IfMerge: []int{}, IfFollow: []int{}}
	p, ks, number, err := s.load(ctx, b)
	if err != nil || len(ks) == 0 {
		return out, err
	}
	if id := gateOf(ks); id != uuid.Nil() {
		g, err := s.Gates.Get(ctx, b.AccountID, id)
		if err != nil {
			return out, err
		}
		if g.Status != domain.GateApproved {
			return out, s.reject(ctx, p, ks, number, "rejected at "+g.Ref()+": "+g.Feedback)
		}
	}
	if number == 0 {
		return out, nil
	}
	pr, err := s.GitHub.GetPR(ctx, p.Repo, number)
	if err != nil {
		return out, fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
	}
	if pr.State != "open" {
		return out, nil
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return out, err
	}
	r, err := s.route(ctx, p, policy, ks)
	if err != nil {
		return out, err
	}
	if r.codeowners {
		out.IfFollow = []int{number}
	} else {
		out.IfMerge = []int{number}
	}
	return out, nil
}

// GateProtected reports whether a knowledge_proposal gate is about a protected
// section: one of the proposals linked to it is, by the policy of now. A gate
// without proposals, or with one the policy no longer places, counts as
// protected, because nothing shows that it is not.
func (s *Service) GateProtected(ctx context.Context, g domain.Gate) (bool, error) {
	ks, err := s.Store.KnowledgeProposals().List(ctx, g.AccountID, store.KnowledgeProposalFilter{GateID: g.ID})
	if err != nil {
		return true, err
	}
	learn := strings.HasPrefix(g.Title, learnTitlePrefix) && g.GoalID != uuid.Nil()
	if len(ks) == 0 && !learn {
		return true, nil
	}
	p, err := s.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return true, err
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return true, err
	}
	// A learn gate also asks for the register files it carries.
	if protected, _ := registerTarget(policy, registerSamples()); learn && protected {
		return true, nil
	}
	for _, k := range ks {
		t, err := policy.Target(k.Section, k.Path, "")
		if errors.Is(err, knowledge.ErrRefused) {
			return true, nil
		}
		if err != nil {
			return true, err
		}
		if t.Protected {
			return true, nil
		}
	}
	return false, nil
}

func gateOf(ks []domain.KnowledgeProposal) uuid.UUID {
	for _, k := range ks {
		if k.GateID != uuid.Nil() {
			return k.GateID
		}
	}
	return uuid.Nil()
}

// reject closes the PR, if it is still open, and rejects ks.
func (s *Service) reject(ctx context.Context, p domain.Project, ks []domain.KnowledgeProposal, number int, why string) error {
	if number > 0 {
		pr, err := s.GitHub.GetPR(ctx, p.Repo, number)
		if err != nil {
			return fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
		}
		if pr.Merged {
			return s.finish(ctx, ks, domain.KnowledgeAccepted, "the pull request was merged")
		}
		if pr.State == "open" {
			if _, err := s.GitHub.CreateComment(ctx, p.Repo, number, strings.Join(refs(ks), ", ")+" rejected: closing."); err != nil {
				return err
			}
			closed := "closed"
			if _, err := s.GitHub.UpdatePR(ctx, p.Repo, number, github.PRUpdate{State: &closed}); err != nil {
				return fmt.Errorf("close pull request %s#%d: %w", p.Repo, number, err)
			}
		}
	}
	return s.finish(ctx, ks, domain.KnowledgeRejected, why)
}

// Settle records how the review of b ended. failed is set when the run did
// not get to the end: what is still in review is rejected and its PR closed.
// Otherwise a merged PR or an approved gate accepts the proposals and a
// closed PR rejects them.
func (s *Service) Settle(ctx context.Context, b Batch, failed bool) error {
	p, ks, number, err := s.load(ctx, b)
	if err != nil || len(ks) == 0 {
		return err
	}
	if failed {
		return s.reject(ctx, p, ks, number, "the review did not finish")
	}
	if number == 0 {
		return s.finish(ctx, ks, domain.KnowledgeAccepted, "a human copied the change across")
	}
	pr, err := s.GitHub.GetPR(ctx, p.Repo, number)
	if err != nil {
		return fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
	}
	switch {
	case pr.Merged:
		return s.finish(ctx, ks, domain.KnowledgeAccepted, "the pull request was merged")
	case pr.State != "open":
		return s.finish(ctx, ks, domain.KnowledgeRejected, "the pull request was closed")
	}
	return fmt.Errorf("pull request %s#%d of %s is still open", p.Repo, number, ks[0].Ref())
}

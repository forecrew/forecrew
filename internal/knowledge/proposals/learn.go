package proposals

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

// PlaybookLearn takes the inputs repo and goal_id.
const PlaybookLearn = "learn"

// LearnBatch names what a goal's learn step carries: the per_goal proposals
// of the goal and the files of its registers.
type LearnBatch struct {
	AccountID uuid.UUID
	ProjectID uuid.UUID
	GoalID    uuid.UUID
}

// Learned is the output of goal.learn.
type Learned struct {
	Proposals []string `json:"proposals"` // refs of the proposals sent
	Files     []string `json:"files"`     // register files written to the PR
	Number    int      `json:"number"`    // the learn PR, 0 when there is none
	Branch    string   `json:"branch"`
	HeadSHA   string   `json:"head_sha"`
	// Gate is the knowledge_proposal gate, "" when there is nothing to
	// decide; IfGate holds it for a foreach.
	Gate   string   `json:"gate"`
	IfGate []string `json:"if_gate"`
}

// learnTitlePrefix starts the title of a learn gate.
const learnTitlePrefix = "Learnings of "

// registerSamples are paths of the files the learn step writes, one for each
// folder. The policy places a folder by a sample: the files differ only in name.
func registerSamples() []string {
	return []string{registers.AssumptionsDir + "/A-1.yaml", registers.DecisionsDir + "/DEC-1.md"}
}

// registerTarget merges what the policy says of the paths: whether any is in a
// protected section, and whether a section owned by codeowners holds one.
func registerTarget(policy knowledge.Policy, paths []string) (protected, codeowners bool) {
	for _, path := range paths {
		t := policy.PathTarget(path)
		protected = protected || t.Protected
		codeowners = codeowners || slices.Contains(t.Owners, knowledge.OwnerCodeowners)
	}
	return protected, codeowners
}

func learnMarker(g domain.Goal) string { return "<!-- forecrew:learn:" + g.Ref() + " -->" }

// Learn sends what the goal learned. Of the per_goal proposals of the goal
// that are still queued it drops what the policy no longer allows (and what
// a section turned protected since: those need a human's own proposal), then
// gives the rest to their providers. The repo provider's go in the learn PR
// agent/learn-GOAL-n, which also gets files, the goal's register files
// (registers.Export). A knowledge_proposal gate, with key as its ID, asks a
// human to approve the PR and the proposals of providers that have no review
// flow, which approving says were copied across. Nothing to send opens
// nothing. It is idempotent for key.
func (s *Service) Learn(ctx context.Context, b LearnBatch, key uuid.UUID, files map[string][]byte) (Learned, error) {
	out := Learned{Proposals: []string{}, Files: []string{}, IfGate: []string{}}
	goal, err := s.Store.Goals().Get(ctx, b.AccountID, b.GoalID)
	if err != nil {
		return out, fmt.Errorf("get goal: %w", err)
	}
	p, err := s.Store.Projects().Get(ctx, b.AccountID, b.ProjectID)
	if err != nil {
		return out, err
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return out, err
	}
	live, err := s.livePerGoal(ctx, goal, policy)
	if err != nil {
		return out, err
	}
	need, err := s.writeRegisters(ctx, p, goal, files)
	if err != nil {
		return out, err
	}
	if need {
		out.Files = slices.Sorted(maps.Keys(files))
	}
	if err := s.sendPerGoal(ctx, policy, live); err != nil {
		return out, err
	}
	out.Proposals = refs(live)
	// The PR is there when the register files or a proposal opened it.
	pr, err := s.GitHub.FindPR(ctx, p.Repo, LearnBranch(goal))
	switch {
	case err == nil:
		out.Number, out.Branch, out.HeadSHA = pr.Number, pr.Head, pr.HeadSHA
	case !errors.Is(err, github.ErrNotFound):
		return out, fmt.Errorf("find the learn PR of %s: %w", goal.Ref(), err)
	}
	if out.Number == 0 && len(live) == 0 {
		return out, nil
	}
	g, err := s.Gates.Open(ctx, gates.OpenRequest{
		ID: key, AccountID: b.AccountID, ProjectID: b.ProjectID, Kind: domain.GateKnowledgeProposal, GoalID: goal.ID,
		Title:   learnTitlePrefix + goal.Ref(),
		Summary: learnSummary(goal, out, live, files, need), SubjectRepo: p.Repo, SubjectNumber: out.Number,
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error { return gateProposals(ctx, tx, live, g) },
	})
	if err != nil {
		return out, err
	}
	out.Gate, out.IfGate = g.ID.String(), []string{g.ID.String()}
	return out, nil
}

// sendPerGoal gives the queued proposals to their providers' review flows;
// the ones of a provider without one wait for the gate.
func (s *Service) sendPerGoal(ctx context.Context, policy knowledge.Policy, live []domain.KnowledgeProposal) error {
	for i := range live {
		k := &live[i]
		if proposer := s.proposer(policy, k.Provider); proposer != nil && k.Status == domain.KnowledgeQueued {
			if err := s.send(ctx, proposer, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// gateProposals puts the proposals in review at gate g, in the transaction
// that stores it: its decision must find them.
func gateProposals(ctx context.Context, tx store.Store, ks []domain.KnowledgeProposal, g domain.Gate) error {
	for _, k := range ks {
		from := k.Status
		k.Status, k.GateID = domain.KnowledgeOpen, g.ID
		if err := tx.KnowledgeProposals().Update(ctx, &k, from); err != nil {
			return err
		}
	}
	return nil
}

// livePerGoal returns the per_goal proposals of the goal that wait for its
// learn step (queued), or were sent by an earlier try of it (open), after the
// policy of this moment was applied again: the ones it no longer allows, or
// whose section is protected now, are rejected and audited.
func (s *Service) livePerGoal(ctx context.Context, goal domain.Goal, policy knowledge.Policy) ([]domain.KnowledgeProposal, error) {
	all, err := s.Store.KnowledgeProposals().List(ctx, goal.AccountID, store.KnowledgeProposalFilter{GoalID: goal.ID, Batch: domain.BatchPerGoal})
	if err != nil {
		return nil, err
	}
	var live []domain.KnowledgeProposal
	for _, k := range all {
		if k.Status != domain.KnowledgeQueued && k.Status != domain.KnowledgeOpen {
			continue
		}
		t, err := policy.Target(k.Section, k.Path, k.ProposedBy)
		if err == nil && t.Protected && k.Status == domain.KnowledgeQueued {
			err = fmt.Errorf("section %s is protected now, and its changes go to a human on their own: %w", k.Section, knowledge.ErrRefused)
		}
		if errors.Is(err, knowledge.ErrRefused) {
			in := Input{Section: k.Section, Path: k.Path, Action: k.Action}
			if err := errors.Join(s.drop(ctx, s.Store, k.AccountID, k.ProjectID, "knowledge_proposal", k.ID, in, k.ProposedBy, err),
				s.finish(ctx, []domain.KnowledgeProposal{k}, domain.KnowledgeRejected, err.Error())); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		live = append(live, k)
	}
	return live, nil
}

// writeRegisters writes the register files that differ from the default
// branch to the learn branch and opens the learn PR for them; it says whether
// there were any. Without such files there is no PR of this step's own: the
// repo proposals open it themselves.
func (s *Service) writeRegisters(ctx context.Context, p domain.Project, goal domain.Goal, files map[string][]byte) (bool, error) {
	need := false
	for _, path := range slices.Sorted(maps.Keys(files)) {
		old, err := s.GitHub.GetFile(ctx, p.Repo, p.DefaultBranch, path)
		switch {
		case errors.Is(err, github.ErrNotFound):
			need = true
		case err != nil:
			return false, err
		case !bytes.Equal(old, files[path]):
			need = true
		}
	}
	if !need {
		return false, nil
	}
	branch := LearnBranch(goal)
	head, err := s.GitHub.HeadSHA(ctx, p.Repo, p.DefaultBranch)
	if err != nil {
		return true, err
	}
	if err := s.GitHub.CreateBranch(ctx, p.Repo, branch, head); err != nil {
		return true, err
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if _, err := s.GitHub.WriteFile(ctx, p.Repo, branch, path, files[path], "Record what "+goal.Ref()+" decided and assumed: "+path); err != nil {
			return true, err
		}
	}
	text := "Written by forecrewd when " + goal.Ref() + " was delivered: the assumptions and decisions of the goal as files, and what the retro proposes for the knowledge base. Merging records them."
	pr, err := openPR(ctx, s.GitHub, p.Repo, github.PRInput{Title: learnTitlePrefix + goal.Ref(), Head: branch, Base: p.DefaultBranch}, learnMarker(goal), text)
	if err != nil {
		return true, err
	}
	c := domain.Change{
		AccountID: p.AccountID, ProjectID: p.ID, Repo: p.Repo, Number: pr.Number, Branch: branch, BaseRef: p.DefaultBranch,
		HeadSHA: pr.HeadSHA, State: domain.ChangeOpen, URL: pr.URL,
	}
	if err := s.Store.Changes().Upsert(ctx, &c); err != nil {
		return true, fmt.Errorf("record pull request %s#%d: %w", p.Repo, pr.Number, err)
	}
	return true, nil
}

// learnSummary shows a human what the learn step carries. The proposals'
// reasons and bodies are agents' text, marked as data.
func learnSummary(goal domain.Goal, out Learned, ks []domain.KnowledgeProposal, files map[string][]byte, need bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The learn step of %s: ", goal.Ref())
	if out.Number > 0 {
		fmt.Fprintf(&b, "approving merges pull request #%d. ", out.Number)
	}
	b.WriteString("Rejecting closes it and rejects its proposals.\n\n")
	if need {
		b.WriteString("### Registers\n\nFiles of the goal's assumptions and decisions:\n\n")
		for _, path := range slices.Sorted(maps.Keys(files)) {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
		b.WriteString("\n")
	}
	for _, k := range ks {
		fmt.Fprintf(&b, "### %s: %s %s (section %s, provider %s)\n\nProposed by %s.\n\n", k.Ref(), k.Action, contextpack.OneLine(k.Path), k.Section, k.Provider, k.ProposedBy)
		b.WriteString(contextpack.WrapUntrusted(k.Ref()+" reason", k.Reason))
		if k.ChangeID == uuid.Nil() && k.Body != "" {
			b.WriteString("\nThis provider has no review flow here: approving says you copied the change across.\n\n")
			b.WriteString(contextpack.WrapUntrusted(k.Ref()+" body", k.Body))
		}
	}
	return b.String()
}

// LearnDecision is the output of goal.learn_decide.
type LearnDecision struct {
	// IfMerge holds the PR when forecrewd merges it; IfFollow when its
	// CODEOWNERS reviewers and a human do.
	IfMerge  []int `json:"if_merge"`
	IfFollow []int `json:"if_follow"`
}

// learnProposals returns the per_goal proposals of the goal in review, and
// its learn PR, 0 when it has none.
func (s *Service) learnProposals(ctx context.Context, b LearnBatch) (domain.Project, domain.Goal, []domain.KnowledgeProposal, int, error) {
	p, err := s.Store.Projects().Get(ctx, b.AccountID, b.ProjectID)
	if err != nil {
		return p, domain.Goal{}, nil, 0, err
	}
	goal, err := s.Store.Goals().Get(ctx, b.AccountID, b.GoalID)
	if err != nil {
		return p, goal, nil, 0, fmt.Errorf("get goal: %w", err)
	}
	ks, err := s.Store.KnowledgeProposals().List(ctx, b.AccountID, store.KnowledgeProposalFilter{GoalID: goal.ID, Batch: domain.BatchPerGoal, Status: domain.KnowledgeOpen})
	if err != nil {
		return p, goal, nil, 0, err
	}
	number, err := s.learnNumber(ctx, p, goal, ks)
	if err != nil {
		return p, goal, nil, 0, err
	}
	return p, goal, ks, number, nil
}

// learnNumber returns the number of the learn PR of goal, open, merged or
// closed, 0 when it has none: the PR of a proposal in review, else the PR
// recorded for the learn branch (a PR of the register files only).
func (s *Service) learnNumber(ctx context.Context, p domain.Project, goal domain.Goal, ks []domain.KnowledgeProposal) (int, error) {
	for _, k := range ks {
		if k.ChangeID != uuid.Nil() {
			c, err := s.Store.Changes().Get(ctx, k.AccountID, k.ChangeID)
			if err != nil {
				return 0, fmt.Errorf("change of %s: %w", k.Ref(), err)
			}
			return c.Number, nil
		}
	}
	cs, err := s.Store.Changes().List(ctx, p.AccountID, store.ChangeFilter{ProjectID: p.ID})
	if err != nil {
		return 0, fmt.Errorf("list the pull requests of %s: %w", p.Name, err)
	}
	number := 0
	for _, c := range cs {
		if c.Branch == LearnBranch(goal) {
			number = max(number, c.Number)
		}
	}
	return number, nil
}

// LearnDecide applies the human's decision on the learn gate: anything but an
// approval closes the learn PR and rejects the proposals; an approval accepts
// the proposals of providers that have no review flow (a human copied them
// across) and says how the PR gets merged, like Decide. gate is the gate of
// Learn, empty when it opened none.
func (s *Service) LearnDecide(ctx context.Context, b LearnBatch, gate string) (LearnDecision, error) {
	out := LearnDecision{IfMerge: []int{}, IfFollow: []int{}}
	p, goal, ks, number, err := s.learnProposals(ctx, b)
	if err != nil {
		return out, err
	}
	if gate != "" {
		id, err := uuid.Parse(gate)
		if err != nil {
			return out, fmt.Errorf("gate %q: %w", gate, err)
		}
		g, err := s.Gates.Get(ctx, b.AccountID, id)
		if err != nil {
			return out, err
		}
		if g.Status != domain.GateApproved {
			return out, s.rejectLearn(ctx, p, goal, ks, number, "rejected at "+g.Ref()+": "+g.Feedback)
		}
	}
	var copied, repoKs []domain.KnowledgeProposal
	for _, k := range ks {
		if k.ChangeID == uuid.Nil() {
			copied = append(copied, k)
		} else {
			repoKs = append(repoKs, k)
		}
	}
	if err := s.finish(ctx, copied, domain.KnowledgeAccepted, "a human copied the change across"); err != nil {
		return out, err
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
	r, err := s.route(ctx, p, policy, repoKs)
	if err != nil {
		return out, err
	}
	// The register files go through the same sections as any change.
	if _, owned := registerTarget(policy, registerSamples()); owned {
		r.codeowners = true
	}
	if r.codeowners {
		out.IfFollow = []int{number}
	} else {
		out.IfMerge = []int{number}
	}
	return out, nil
}

// rejectLearn closes the learn PR if it is open and rejects ks. A PR that was
// merged meanwhile accepts them.
func (s *Service) rejectLearn(ctx context.Context, p domain.Project, goal domain.Goal, ks []domain.KnowledgeProposal, number int, why string) error {
	if number > 0 {
		pr, err := s.GitHub.GetPR(ctx, p.Repo, number)
		if err != nil {
			return fmt.Errorf("get pull request %s#%d: %w", p.Repo, number, err)
		}
		if pr.Merged {
			return s.finish(ctx, ks, domain.KnowledgeAccepted, "the pull request was merged")
		}
		if pr.State == "open" {
			if _, err := s.GitHub.CreateComment(ctx, p.Repo, number, "The learn step of "+goal.Ref()+" was rejected: closing."); err != nil {
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

// LearnSettle records how the learn step ended. failed is set when the run did
// not get to the end: the PR is closed and what is still in review rejected.
// Otherwise a merged PR accepts the proposals and a closed one rejects them.
func (s *Service) LearnSettle(ctx context.Context, b LearnBatch, failed bool) error {
	p, goal, ks, number, err := s.learnProposals(ctx, b)
	if err != nil {
		return err
	}
	if failed {
		return s.rejectLearn(ctx, p, goal, ks, number, "the learn step did not finish")
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
	return fmt.Errorf("pull request %s#%d of %s is still open", p.Repo, number, goal.Ref())
}

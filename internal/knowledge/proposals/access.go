package proposals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
)

// Access levels of request_access.
const (
	levelRead  = "read"
	levelWrite = "write"
)

// AccessRequest is what a role asks for.
type AccessRequest struct {
	Section string
	Level   string // read, propose or write
	Reason  string
}

// RequestAccess opens an access_request gate for the role of j, and in the
// same transaction the run that turns an approval into a policy PR. The
// policy itself is never touched. Asking again while the gate is pending
// returns it. A job whose lease has ended is refused with store.ErrLeaseLost. A section the policy does not have, a level the role has, and
// what the policy could not grant (write on a protected section, more than
// read on a reserved one) wrap ErrInvalid.
func (s *Service) RequestAccess(ctx context.Context, j domain.Job, in AccessRequest) (domain.Gate, error) {
	p, err := s.Store.Projects().Get(ctx, j.AccountID, j.ProjectID)
	if err != nil {
		return domain.Gate{}, fmt.Errorf("project of %s: %w", j.Ref(), err)
	}
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return domain.Gate{}, err
	}
	section, known := policy.Sections[in.Section]
	switch {
	case knowledge.Reserved(in.Section) && in.Level != levelRead:
		return domain.Gate{}, fmt.Errorf("%w: section %s is only ever read", ErrInvalid, in.Section)
	case !known:
		return domain.Gate{}, fmt.Errorf("%w: the policy has no section %s", ErrInvalid, in.Section)
	case section.Protected && in.Level == levelWrite:
		return domain.Gate{}, fmt.Errorf("%w: protected section %s changes only through proposals", ErrInvalid, in.Section)
	case policy.Has(in.Section, in.Level, j.Role):
		return domain.Gate{}, fmt.Errorf("%w: role %s already has %s access to %s", ErrInvalid, j.Role, in.Level, in.Section)
	}
	title := fmt.Sprintf("%s (%s) asks for %s access to %s", j.Ref(), j.Role, in.Level, in.Section)
	pending, err := s.Store.Gates().List(ctx, j.AccountID, store.GateFilter{ProjectID: j.ProjectID, Kind: domain.GateAccessRequest, Status: domain.GatePending})
	if err != nil {
		return domain.Gate{}, err
	}
	if i := slices.IndexFunc(pending, func(g domain.Gate) bool { return g.JobID == j.ID && g.Title == title }); i >= 0 {
		return pending[i], nil
	}
	return s.Gates.Open(ctx, gates.OpenRequest{
		ID: domain.NewID(), AccountID: j.AccountID, ProjectID: j.ProjectID, Kind: domain.GateAccessRequest, Title: title,
		Summary:     "Approving opens a pull request that changes the knowledge policy.\n\n" + contextpack.WrapUntrusted(j.Ref()+" reason", in.Reason),
		SubjectRepo: p.Repo, GoalID: j.GoalID, TaskID: j.TaskID, JobID: j.ID,
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			// The job's lock, as completing it takes: the request commits
			// before the job finishes, or is refused.
			if err := tx.Jobs().LockLeased(ctx, j.AccountID, j.ID, j.WorkerID); err != nil {
				return err
			}
			return startRun(ctx, tx, PlaybookAccess, j.AccountID, j.ProjectID, j.GoalID, j.TaskID,
				map[string]any{"gate_id": g.ID, "section": in.Section, "level": in.Level, "role": j.Role})
		},
	})
}

// PolicyPR is the output of knowledge.access_pr.
type PolicyPR struct {
	Opened bool   `json:"opened"`
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Grant is what an approved request changes.
type Grant struct {
	AccountID uuid.UUID
	GateID    uuid.UUID
	Section   string
	Level     string
	Role      string
}

// GrantPR opens the PR that gives the role its access, when the gate was
// approved and the policy at the default branch does not grant it yet. A
// human merges it like any PR. The gate's ID names the branch, so a second
// call finds the same PR.
func (s *Service) GrantPR(ctx context.Context, in Grant) (PolicyPR, error) {
	g, err := s.Gates.Get(ctx, in.AccountID, in.GateID)
	if err != nil || g.Status != domain.GateApproved {
		return PolicyPR{}, err
	}
	p, err := s.Store.Projects().Get(ctx, g.AccountID, g.ProjectID)
	if err != nil {
		return PolicyPR{}, err
	}
	head, err := s.GitHub.HeadSHA(ctx, p.Repo, p.DefaultBranch)
	if err != nil {
		return PolicyPR{}, err
	}
	current, err := s.GitHub.GetFile(ctx, p.Repo, head, knowledge.PolicyFile)
	if errors.Is(err, github.ErrNotFound) {
		current, err = s.renderPolicy(ctx, p)
	}
	if err != nil {
		return PolicyPR{}, fmt.Errorf("read the policy of %s: %w", p.Repo, err)
	}
	updated, changed, err := grant(current, in.Section, in.Level, in.Role)
	if err != nil || !changed {
		return PolicyPR{}, err
	}
	if _, err := knowledge.ParsePolicy(updated); err != nil {
		return PolicyPR{}, fmt.Errorf("grant of %s: %w", g.Ref(), err)
	}
	branch := "agent/kb-access-" + g.Ref()
	if err := s.GitHub.CreateBranch(ctx, p.Repo, branch, head); err != nil {
		return PolicyPR{}, err
	}
	title := fmt.Sprintf("Give %s %s access to %s", in.Role, in.Level, in.Section)
	if _, err := s.GitHub.WriteFile(ctx, p.Repo, branch, knowledge.PolicyFile, updated, title); err != nil {
		return PolicyPR{}, err
	}
	text := fmt.Sprintf("%s was approved by %s. This changes the knowledge policy: a human merges it.\n\n%s", g.Ref(), g.ResolvedBy, g.Summary)
	pr, err := openPR(ctx, s.GitHub, p.Repo, github.PRInput{Title: title, Head: branch, Base: p.DefaultBranch}, "<!-- forecrew:"+g.Ref()+" -->", text)
	if err != nil {
		return PolicyPR{}, err
	}
	c := domain.Change{
		AccountID: g.AccountID, ProjectID: g.ProjectID, Repo: p.Repo, Number: pr.Number, Branch: branch, BaseRef: p.DefaultBranch,
		HeadSHA: pr.HeadSHA, State: domain.ChangeOpen, URL: pr.URL,
	}
	if err := s.Store.Changes().Upsert(ctx, &c); err != nil {
		return PolicyPR{}, fmt.Errorf("record pull request %s#%d: %w", p.Repo, pr.Number, err)
	}
	return PolicyPR{Opened: true, Number: pr.Number, URL: pr.URL}, nil
}

// renderPolicy writes the policy a repo without a file has in effect: a
// file replaces the default policy whole, so it must say all of it.
func (s *Service) renderPolicy(ctx context.Context, p domain.Project) ([]byte, error) {
	policy, err := s.Policy(ctx, p)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return yaml.Marshal(doc)
}

// grant adds role to sections.<section>.access.<level> of a policy file,
// keeping the rest of it, comments included. It reports false when the role
// has the level already: an access list that is absent means every role for
// read and propose.
func grant(data []byte, section, level, role string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", knowledge.PolicyFile, err)
	}
	if len(doc.Content) != 1 {
		return nil, false, fmt.Errorf("%s is empty", knowledge.PolicyFile)
	}
	node := doc.Content[0]
	for _, key := range []string{"sections", section, "access"} {
		node = child(node, key, yaml.MappingNode)
	}
	if level != levelWrite && lookup(node, level) == nil {
		return nil, false, nil
	}
	list := child(node, level, yaml.SequenceNode)
	for _, n := range list.Content {
		if n.Value == role || n.Value == knowledge.Everyone {
			return nil, false, nil
		}
	}
	list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: role})
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return out.Bytes(), true, nil
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// child returns the value of key in the mapping m, added as an empty node of
// kind when it is missing.
func child(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	if n := lookup(m, key); n != nil {
		return n
	}
	tag := "!!map"
	if kind == yaml.SequenceNode {
		tag = "!!seq"
	}
	n := &yaml.Node{Kind: kind, Tag: tag}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
	return n
}

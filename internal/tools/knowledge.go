package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// MaxSearchLimit is the most results search_knowledge returns.
const MaxSearchLimit = 50

// SearchKnowledge implements mcp.ToolService.
func (s *Service) SearchKnowledge(ctx context.Context, by domain.Actor, in mcp.SearchKnowledgeInput) ([]knowledge.Result, error) {
	if s.cfg.Search == nil {
		return nil, fmt.Errorf("the knowledge base is not configured: %w", mcp.ErrUnavailable)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	if err := s.allow(ctx, by, domain.PermProjectRead, p.ID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, fmt.Errorf("query is required: %w", mcp.ErrInvalidInput)
	}
	q := knowledge.Query{Project: p, Text: in.Query, Limit: in.Limit, AllGoals: true}
	if in.Section != "" {
		q.Sections = []string{in.Section}
	}
	if in.Provider != "" {
		q.Providers = []string{in.Provider}
	}
	if k := in.Kind; k != "" {
		if k != domain.KnowledgeEntryKind && k != domain.KnowledgeNoteKind && k != domain.KnowledgeGeneratedKind {
			return nil, fmt.Errorf("kind %q must be entry, note or generated: %w", k, mcp.ErrInvalidInput)
		}
		q.Kinds = []domain.KnowledgeKind{k}
	}
	if in.Limit < 0 || in.Limit > MaxSearchLimit {
		return nil, fmt.Errorf("limit %d must be between 1 and %d: %w", in.Limit, MaxSearchLimit, mcp.ErrInvalidInput)
	}
	res, err := s.cfg.Search.Run(ctx, q)
	if errors.Is(err, knowledge.ErrPolicyUnknown) {
		return nil, fmt.Errorf("%w: %w", err, mcp.ErrUnavailable)
	}
	if in.WrapUntrusted {
		for i := range res {
			res[i] = contextpack.WrapKnowledge(res[i])
		}
	}
	return res, err
}

// ProposeKnowledge implements mcp.ToolService.
func (s *Service) ProposeKnowledge(ctx context.Context, by domain.Actor, in mcp.ProposeKnowledgeInput) (mcp.ProposeKnowledgeOutput, error) {
	if s.cfg.Proposals == nil {
		return mcp.ProposeKnowledgeOutput{}, fmt.Errorf("the knowledge base is not configured: %w", mcp.ErrUnavailable)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return mcp.ProposeKnowledgeOutput{}, err
	}
	if err := s.allow(ctx, by, domain.PermKnowledgePropose, p.ID); err != nil {
		return mcp.ProposeKnowledgeOutput{}, err
	}
	res, err := s.cfg.Proposals.Human(ctx, by, p, proposals.Input{Section: in.Section, Path: in.Path, Action: in.Action, Body: in.Body, Reason: in.Reason},
		func(protected bool) error {
			if !protected {
				return nil
			}
			return s.authz.AuthorizeProtectedKnowledge(ctx, by, p.ID)
		})
	switch {
	case errors.Is(err, proposals.ErrInvalid):
		return mcp.ProposeKnowledgeOutput{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
	case errors.Is(err, knowledge.ErrPolicyUnknown):
		return mcp.ProposeKnowledgeOutput{}, fmt.Errorf("%w: %w", err, mcp.ErrUnavailable)
	case err != nil:
		return mcp.ProposeKnowledgeOutput{}, err
	}
	return mcp.ProposeKnowledgeOutput{Proposal: res.Proposal.Ref(), Status: res.Proposal.Status, Number: res.Number, URL: res.URL}, nil
}

// Limits of list_knowledge.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListKnowledge implements mcp.ToolService.
func (s *Service) ListKnowledge(ctx context.Context, by domain.Actor, in mcp.ListKnowledgeInput) (mcp.KnowledgeListing, error) {
	if s.cfg.Search == nil {
		return mcp.KnowledgeListing{}, fmt.Errorf("the knowledge base is not configured: %w", mcp.ErrUnavailable)
	}
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return mcp.KnowledgeListing{}, err
	}
	if err := s.allow(ctx, by, domain.PermProjectRead, p.ID); err != nil {
		return mcp.KnowledgeListing{}, err
	}
	if in.Limit < 0 || in.Limit > MaxListLimit {
		return mcp.KnowledgeListing{}, fmt.Errorf("limit %d must be between 1 and %d: %w", in.Limit, MaxListLimit, mcp.ErrInvalidInput)
	}
	if k := in.Kind; k != "" && k != domain.KnowledgeEntryKind && k != domain.KnowledgeNoteKind && k != domain.KnowledgeGeneratedKind {
		return mcp.KnowledgeListing{}, fmt.Errorf("kind %q must be entry, note or generated: %w", k, mcp.ErrInvalidInput)
	}
	q, err := s.listQuery(ctx, by, p, in)
	if err != nil {
		return mcp.KnowledgeListing{}, err
	}
	policy, err := s.cfg.Search.Policy(ctx, p)
	if errors.Is(err, knowledge.ErrPolicyUnknown) {
		return mcp.KnowledgeListing{}, fmt.Errorf("%w: %w", err, mcp.ErrUnavailable)
	}
	if err != nil {
		return mcp.KnowledgeListing{}, err
	}
	entries, err := s.cfg.Search.List(ctx, q)
	if err != nil {
		return mcp.KnowledgeListing{}, err
	}
	if in.WrapUntrusted {
		for i, e := range entries {
			entries[i].Result = contextpack.WrapKnowledge(e.Result)
			if e.Untrusted {
				entries[i].Body = contextpack.WrapUntrusted("knowledge "+string(e.Kind)+" "+e.Provider, e.Body)
			}
		}
	}
	return mcp.KnowledgeListing{Sections: sectionInfos(policy), Entries: mcp.NonNil(entries)}, nil
}

// listQuery turns the filters of list_knowledge into a knowledge query.
func (s *Service) listQuery(ctx context.Context, by domain.Actor, p domain.Project, in mcp.ListKnowledgeInput) (knowledge.Query, error) {
	q := knowledge.Query{Project: p, Limit: cmp.Or(in.Limit, DefaultListLimit)}
	if in.Section != "" {
		q.Sections = []string{in.Section}
	}
	if in.Provider != "" {
		q.Providers = []string{in.Provider}
	}
	if in.Kind != "" {
		q.Kinds = []domain.KnowledgeKind{in.Kind}
	}
	if in.Goal != "" {
		g, err := s.goal(ctx, by.AccountID, in.Goal)
		if err != nil {
			return q, err
		}
		q.GoalID = g.ID
	}
	return q, nil
}

// sectionInfos lists the sections of a policy by name, with the two
// reserved ones: notes and generated.
func sectionInfos(policy knowledge.Policy) []mcp.KnowledgeSectionInfo {
	out := []mcp.KnowledgeSectionInfo{}
	for _, name := range slices.Sorted(maps.Keys(policy.Sections)) {
		sec := policy.Sections[name]
		if knowledge.Reserved(name) {
			continue
		}
		pr := policy.Providers[sec.Provider]
		out = append(out, mcp.KnowledgeSectionInfo{Name: name, Provider: sec.Provider, ProviderKind: pr.Kind, Instance: pr.Instance,
			Paths: nonNil(sec.Paths), Owner: sec.Owner, Protected: sec.Protected})
	}
	return append(out,
		mcp.KnowledgeSectionInfo{Name: domain.SectionNotes, Provider: domain.ProviderInternal, ProviderKind: domain.ProviderInternal, Paths: []string{}},
		mcp.KnowledgeSectionInfo{Name: domain.SectionGenerated, Paths: []string{}})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Limits of list_knowledge_proposals.
const (
	DefaultProposalLimit = 50
	MaxProposalLimit     = 200
)

// ListKnowledgeProposals implements mcp.ToolService.
func (s *Service) ListKnowledgeProposals(ctx context.Context, by domain.Actor, in mcp.ListKnowledgeProposalsInput) ([]mcp.KnowledgeProposalInfo, error) {
	p, err := s.project(ctx, by.AccountID, in.Project)
	if err != nil {
		return nil, err
	}
	if err := s.allow(ctx, by, domain.PermProjectRead, p.ID); err != nil {
		return nil, err
	}
	sees, err := s.readsGates(ctx, by, p.ID)
	if err != nil {
		return nil, err
	}
	if in.Limit < 0 || in.Limit > MaxProposalLimit {
		return nil, fmt.Errorf("limit %d must be between 1 and %d: %w", in.Limit, MaxProposalLimit, mcp.ErrInvalidInput)
	}
	if in.Status != "" && !slices.Contains(domain.KnowledgeProposalStatuses(), in.Status) {
		return nil, fmt.Errorf("unknown status %q: %w", in.Status, mcp.ErrInvalidInput)
	}
	st := s.cfg.Store
	f := store.KnowledgeProposalFilter{ProjectID: p.ID, Status: in.Status}
	if in.Goal != "" {
		g, err := s.goal(ctx, by.AccountID, in.Goal)
		if err != nil {
			return nil, err
		}
		f.GoalID = g.ID
	}
	list, err := st.KnowledgeProposals().List(ctx, by.AccountID, f)
	if err != nil {
		return nil, err
	}
	slices.Reverse(list)
	list = list[:min(len(list), cmp.Or(in.Limit, DefaultProposalLimit))]
	out := make([]mcp.KnowledgeProposalInfo, len(list))
	for i, k := range list {
		if out[i], err = s.proposalInfo(ctx, by, k, sees); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// proposalInfo adds the ref, the goal, the pull request and, for a caller
// with gate.read, the gate to a proposal.
func (s *Service) proposalInfo(ctx context.Context, by domain.Actor, k domain.KnowledgeProposal, sees bool) (mcp.KnowledgeProposalInfo, error) {
	st := s.cfg.Store
	info := mcp.KnowledgeProposalInfo{KnowledgeProposal: k, Ref: k.Ref()}
	if k.GoalID != uuid.Nil() {
		g, err := st.Goals().Get(ctx, by.AccountID, k.GoalID)
		if err != nil {
			return info, err
		}
		info.GoalRef = g.Ref()
	}
	if k.ChangeID != uuid.Nil() {
		c, err := st.Changes().Get(ctx, by.AccountID, k.ChangeID)
		if err != nil {
			return info, err
		}
		info.URL = c.URL
	}
	if !sees {
		// The gate is part of the inbox: only gate.read shows it.
		info.GateID = uuid.Nil()
		return info, nil
	}
	if k.GateID != uuid.Nil() {
		g, err := st.Gates().Get(ctx, by.AccountID, k.GateID)
		if err != nil {
			return info, err
		}
		gi, err := s.gateInfos(ctx, by, []domain.Gate{g})
		if err != nil {
			return info, err
		}
		info.Gate = &gi[0]
	}
	return info, nil
}

// DeleteNote implements mcp.ToolService.
func (s *Service) DeleteNote(ctx context.Context, by domain.Actor, in mcp.DeleteNoteInput) (mcp.DeleteNoteOutput, error) {
	if s.cfg.Notes == nil {
		return mcp.DeleteNoteOutput{}, fmt.Errorf("the knowledge base is not configured: %w", mcp.ErrUnavailable)
	}
	id, err := uuid.Parse(in.Note)
	if err != nil {
		return mcp.DeleteNoteOutput{}, fmt.Errorf("%q is not a note UUID: %w", in.Note, mcp.ErrInvalidInput)
	}
	e, err := s.cfg.Store.Knowledge().Get(ctx, by.AccountID, id)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.DeleteNoteOutput{}, fmt.Errorf("note %s: %w", id, mcp.ErrNotFound)
	}
	if err != nil {
		return mcp.DeleteNoteOutput{}, err
	}
	if err := s.allow(ctx, by, domain.PermKnowledgePropose, e.ProjectID); err != nil {
		return mcp.DeleteNoteOutput{}, err
	}
	if err := s.cfg.Notes.Delete(ctx, by.AccountID, id); err != nil {
		if errors.Is(err, knowledge.ErrNotNote) {
			return mcp.DeleteNoteOutput{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
		}
		return mcp.DeleteNoteOutput{}, err
	}
	if err := s.audit(ctx, s.cfg.Store, by, e.ProjectID, "knowledge.note_deleted", "knowledge_entry", id, map[string]any{"title": e.Title, "scope": e.Scope}); err != nil {
		return mcp.DeleteNoteOutput{}, err
	}
	return mcp.DeleteNoteOutput{Note: id, Message: "Note " + e.Title + " deleted."}, nil
}

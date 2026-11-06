package tools

import (
	"context"
	"errors"
	"fmt"
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

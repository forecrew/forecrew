package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// ListArtifacts implements mcp.ToolService.
func (s *Service) ListArtifacts(ctx context.Context, by domain.Actor, in mcp.JobRefInput) ([]domain.Artifact, error) {
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return nil, err
	}
	if err := s.allow(ctx, by, domain.PermJobRead, j.ProjectID); err != nil {
		return nil, err
	}
	return s.cfg.Store.Artifacts().ListByJob(ctx, by.AccountID, j.ID)
}

// OpenArtifact returns the artifact with the given UUID of the actor's
// account and its content, which the caller closes. It is not an MCP tool:
// the content is served as a download by GET /api/v1/artifacts/{id}.
func (s *Service) OpenArtifact(ctx context.Context, by domain.Actor, id string) (domain.Artifact, io.ReadCloser, error) {
	aid, err := uuid.Parse(id)
	if err != nil {
		return domain.Artifact{}, nil, fmt.Errorf("%q is not an artifact UUID: %w", id, mcp.ErrInvalidInput)
	}
	a, err := s.cfg.Store.Artifacts().Get(ctx, by.AccountID, aid)
	if errors.Is(err, store.ErrNotFound) {
		return a, nil, fmt.Errorf("artifact %s: %w", id, mcp.ErrNotFound)
	}
	if err != nil {
		return a, nil, err
	}
	if err := s.allow(ctx, by, domain.PermJobRead, a.ProjectID); err != nil {
		return a, nil, err
	}
	if s.cfg.Evidence == nil {
		return a, nil, fmt.Errorf("no evidence storage is configured: %w", mcp.ErrUnavailable)
	}
	body, err := s.cfg.Evidence.Get(ctx, a.StorageKey)
	if errors.Is(err, evidence.ErrNotFound) {
		return a, nil, fmt.Errorf("artifact %s: its content is gone from storage: %w", id, mcp.ErrNotFound)
	}
	return a, body, err
}

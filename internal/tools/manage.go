package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// CancelJob implements mcp.ToolService.
func (s *Service) CancelJob(ctx context.Context, by domain.Actor, in mcp.JobRefInput) (mcp.JobInfo, error) {
	j, err := s.job(ctx, by.AccountID, in.Job)
	if err != nil {
		return mcp.JobInfo{}, err
	}
	if _, err := s.cfg.Jobs.Cancel(ctx, by, by.AccountID, j.ID); errors.Is(err, domain.ErrInvalidTransition) {
		return mcp.JobInfo{}, fmt.Errorf("%s is %s, only queued or running jobs can be canceled: %w", j.Ref(), j.Status, err)
	} else if err != nil {
		return mcp.JobInfo{}, err
	}
	return s.JobStatus(ctx, by, mcp.JobRefInput{Job: j.ID.String()})
}

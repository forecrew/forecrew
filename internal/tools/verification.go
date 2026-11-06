package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
)

var (
	prNumber = regexp.MustCompile(`^[0-9]{1,9}$`)
	// prPath is the path of a pull request URL: /owner/name/pull/9.
	prPath = regexp.MustCompile(`^/([^/]+/[^/]+)/pull/([0-9]{1,9})/?$`)
)

// VerificationReport implements mcp.ToolService.
func (s *Service) VerificationReport(ctx context.Context, by domain.Actor, in mcp.VerificationReportInput) (mcp.VerificationInfo, error) {
	c, err := s.changeOf(ctx, by.AccountID, in)
	if err != nil {
		return mcp.VerificationInfo{}, err
	}
	if err := s.allow(ctx, by, domain.PermJobRead, c.ProjectID); err != nil {
		return mcp.VerificationInfo{}, err
	}
	return s.verificationOf(ctx, by.AccountID, c)
}

// verificationOf is the verification_report of a resolved change.
func (s *Service) verificationOf(ctx context.Context, accountID uuid.UUID, c domain.Change) (mcp.VerificationInfo, error) {
	r, current, err := s.reportOf(ctx, accountID, c)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.VerificationInfo{}, fmt.Errorf("pull request #%d of %s has no verification report yet: %w", c.Number, c.Repo, mcp.ErrNotFound)
	}
	if err != nil {
		return mcp.VerificationInfo{}, err
	}
	out := mcp.VerificationInfo{VerificationReport: r, Change: c.Number, URL: c.URL, Current: current}
	rep, err := verification.Parse(r.Report)
	if err != nil {
		return mcp.VerificationInfo{}, fmt.Errorf("verification report %s: %w", r.ID, err)
	}
	out.Untrusted = contextpack.WrapUntrusted("verification report of "+c.Repo+"#"+strconv.Itoa(c.Number), verification.Render(rep))
	return out, nil
}

// reportOf returns the verification report of the change's head commit, else
// its latest (current false); store.ErrNotFound when it has none.
func (s *Service) reportOf(ctx context.Context, accountID uuid.UUID, c domain.Change) (r domain.VerificationReport, current bool, err error) {
	repo := s.cfg.Store.VerificationReports()
	r, err = repo.GetBySHA(ctx, accountID, c.ID, strings.ToLower(c.HeadSHA))
	if errors.Is(err, store.ErrNotFound) {
		r, err = repo.Latest(ctx, accountID, c.ID)
		return r, false, err
	}
	return r, true, err
}

// changeOf resolves the pull request of a verification_report input.
func (s *Service) changeOf(ctx context.Context, accountID uuid.UUID, in mcp.VerificationReportInput) (domain.Change, error) {
	ref := strings.TrimSpace(in.Change)
	var c domain.Change
	var err error
	switch {
	case ref == "":
		return c, fmt.Errorf("change is required: a pull request number, a pull request URL or a task ref: %w", mcp.ErrInvalidInput)
	case prNumber.MatchString(ref):
		n, _ := strconv.Atoi(ref)
		var p domain.Project
		if p, err = s.project(ctx, accountID, in.Project); err != nil {
			return c, err
		}
		c, err = s.cfg.Store.Changes().GetByPR(ctx, accountID, p.Repo, n)
	case strings.Contains(ref, "://"):
		repo, n, ok := parsePullURL(ref)
		if !ok {
			return c, fmt.Errorf("%q is not a pull request URL like https://github.com/owner/app/pull/9: %w", ref, mcp.ErrInvalidInput)
		}
		c, err = s.cfg.Store.Changes().GetByPR(ctx, accountID, repo, n)
	default:
		return s.newestChange(ctx, accountID, ref, in.Project)
	}
	if errors.Is(err, store.ErrNotFound) {
		return c, fmt.Errorf("pull request %s: %w", ref, mcp.ErrNotFound)
	}
	return c, err
}

// parsePullURL returns the repo and number of a pull request URL.
func parsePullURL(raw string) (repo string, number int, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", 0, false
	}
	m := prPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", 0, false
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], n, true
}

// newestChange returns the newest pull request of the task with the ref.
func (s *Service) newestChange(ctx context.Context, accountID uuid.UUID, ref, project string) (domain.Change, error) {
	t, err := s.task(ctx, accountID, ref)
	if err != nil {
		return domain.Change{}, err
	}
	if project != "" {
		p, err := s.project(ctx, accountID, project)
		if err != nil {
			return domain.Change{}, err
		}
		if p.ID != t.ProjectID {
			return domain.Change{}, fmt.Errorf("task %s: %w", ref, mcp.ErrNotFound)
		}
	}
	changes, err := s.cfg.Store.Changes().List(ctx, accountID, store.ChangeFilter{TaskID: t.ID})
	if err != nil {
		return domain.Change{}, fmt.Errorf("changes of %s: %w", t.Ref(), err)
	}
	if len(changes) == 0 {
		return domain.Change{}, fmt.Errorf("task %s has no pull request: %w", t.Ref(), mcp.ErrNotFound)
	}
	return slices.MaxFunc(changes, func(a, b domain.Change) int { return a.CreatedAt.Compare(b.CreatedAt) }), nil
}

// verificationRef links a task to the verification of its newest pull request
// that has a report; nil when none has.
func (s *Service) verificationRef(ctx context.Context, t domain.Task, changes []domain.Change) (*mcp.VerificationRef, error) {
	var best *mcp.VerificationRef
	var bestAt time.Time
	for _, c := range changes {
		r, current, err := s.reportOf(ctx, t.AccountID, c)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("verification of %s#%d: %w", c.Repo, c.Number, err)
		}
		if best == nil || r.UpdatedAt.After(bestAt) {
			best, bestAt = &mcp.VerificationRef{Status: r.Status, SHA: r.SHA, Current: current, URL: "/api/v1/changes/" + t.Ref() + "/verification"}, r.UpdatedAt
		}
	}
	return best, nil
}

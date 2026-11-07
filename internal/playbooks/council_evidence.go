package playbooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/netguard"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Limits of the URLs fetched: agents choose them, and every fetch holds the
// run's engine worker and may archive councils.DefaultFetchSize bytes.
const (
	MaxCheckedURLs = 10 // new URLs of one contribution
	MaxCouncilURLs = 40 // URLs of one council
)

// notFetched starts the detail of a URL over the limit, which a later
// contribution may still get fetched.
const notFetched = "not fetched"

// sourceChecker checks the sources of one contribution.
type sourceChecker struct {
	a       *actions
	council domain.Council
	repo    string
	commit  string
	job     uuid.UUID
	// seen holds the checks of the council's URLs so far: a URL is fetched
	// once per council.
	seen    map[string]domain.SourceCheck
	fetched int
}

// councilCheckEvidence is the action council.check_evidence: it checks every
// source a contribution cites and stores the result on it. A checked
// contribution is left alone, so a repeated call does nothing.
func (a *actions) councilCheckEvidence(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		CouncilID      uuid.UUID `json:"council_id"`
		ContributionID uuid.UUID `json:"contribution_id"`
		Commit         string    `json:"commit"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	c, err := a.loadCouncil(ctx, call.Run, with.CouncilID)
	if err != nil {
		return nil, err
	}
	all, err := a.Store.Councils().Contributions(ctx, c.AccountID, c.ID, "")
	if err != nil {
		return nil, fmt.Errorf("list the contributions of %s: %w", c.Ref(), err)
	}
	i := slices.IndexFunc(all, func(k domain.CouncilContribution) bool { return k.ID == with.ContributionID })
	if i < 0 {
		return nil, fmt.Errorf("%s has no contribution %s", c.Ref(), with.ContributionID)
	}
	k := all[i]
	if k.EvidenceCheck != nil {
		return map[string]int{"problems": problems(k.EvidenceCheck)}, nil
	}
	project, err := a.Store.Projects().Get(ctx, c.AccountID, c.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project of %s: %w", c.Ref(), err)
	}
	s := sourceChecker{a: a, council: c, repo: project.Repo, commit: with.Commit, job: k.JobID, seen: map[string]domain.SourceCheck{}}
	for _, other := range all {
		for _, check := range other.EvidenceCheck {
			if check.Kind == domain.SourceURL && !strings.HasPrefix(check.Detail, notFetched) {
				s.seen[check.Source] = check
			}
		}
	}
	checks := []domain.SourceCheck{}
	for _, src := range councils.Sources(k.Content) {
		checks = append(checks, s.check(ctx, src))
	}
	if err := a.Store.Councils().SetEvidenceCheck(ctx, c.AccountID, k.ID, checks); err != nil {
		return nil, fmt.Errorf("store the evidence check of %s: %w", c.Ref(), err)
	}
	return map[string]int{"problems": problems(checks)}, nil
}

// problems counts the sources that did not check out.
func problems(checks []domain.SourceCheck) int {
	n := 0
	for _, c := range checks {
		if c.Status != domain.SourceOK {
			n++
		}
	}
	return n
}

func (s *sourceChecker) check(ctx context.Context, src councils.Source) domain.SourceCheck {
	out := domain.SourceCheck{Source: src.Source, Kind: src.Kind, Status: domain.SourceOK}
	switch src.Kind {
	case domain.SourcePath, domain.SourceKB:
		s.file(ctx, &out)
	case domain.SourceJob:
		s.jobRef(ctx, &out)
	case domain.SourceURL:
		s.url(ctx, &out)
	}
	return out
}

// file checks a repo path or knowledge entry at the council's commit. A
// directory counts as missing: sources name files. A read that fails says
// nothing about the file, so it is unreachable.
func (s *sourceChecker) file(ctx context.Context, out *domain.SourceCheck) {
	path, _, _ := strings.Cut(out.Source, ":") // a source may name lines
	_, err := s.a.Tracker.GetFile(ctx, s.repo, s.commit, path)
	switch {
	case errors.Is(err, github.ErrNotFound):
		out.Status, out.Detail = domain.SourceMissing, "not in the repo at "+s.commit
	case err != nil:
		out.Status, out.Detail = domain.SourceUnreachable, "the repo could not be read"
	}
}

// jobRef checks a job ref against the jobs of the council's project.
func (s *sourceChecker) jobRef(ctx context.Context, out *domain.SourceCheck) {
	missing := func() { out.Status, out.Detail = domain.SourceMissing, "no such job in this project" }
	kind, number, err := domain.ParseRef(out.Source)
	if err != nil || kind != domain.RefJob {
		missing()
		return
	}
	if j, err := s.a.Store.Jobs().GetByNumber(ctx, s.council.AccountID, number); err != nil || j.ProjectID != s.council.ProjectID {
		missing()
	}
}

// url fetches a URL once per council and archives it as an artifact of the
// job that first cited it. Without evidence storage the check still tells
// whether it answered.
func (s *sourceChecker) url(ctx context.Context, out *domain.SourceCheck) {
	if prior, ok := s.seen[out.Source]; ok {
		*out = prior
		return
	}
	if s.fetched >= MaxCheckedURLs {
		out.Status, out.Detail = domain.SourceUnreachable, fmt.Sprintf("%s: over the limit of %d URLs per contribution", notFetched, MaxCheckedURLs)
		return
	}
	if len(s.seen) >= MaxCouncilURLs {
		out.Status, out.Detail = domain.SourceUnreachable, fmt.Sprintf("%s: over the limit of %d URLs per council", notFetched, MaxCouncilURLs)
		return
	}
	s.fetched++
	defer func() { s.seen[out.Source] = *out }()
	page, err := s.a.Fetcher.Fetch(ctx, out.Source)
	switch {
	case errors.Is(err, netguard.ErrRefused):
		// Not the error: members must not learn what a name resolves to here.
		out.Status, out.Detail = domain.SourceRefused, "only public http(s) addresses are fetched"
		return
	case err != nil:
		out.Status, out.Detail = domain.SourceUnreachable, err.Error()
		return
	}
	if s.a.Evidence == nil {
		out.Detail = "not archived: no evidence storage"
		return
	}
	id, err := s.archive(ctx, out.Source, page)
	if err != nil {
		s.a.Logger.ErrorContext(ctx, "archive a council source", "account_id", s.council.AccountID, "council", s.council.Ref(), "error", err)
		out.Detail = "not archived: the evidence storage failed"
		return
	}
	out.ArtifactID = id
}

// archive stores a fetched page as an artifact of the contribution's job.
func (s *sourceChecker) archive(ctx context.Context, source string, page councils.Page) (uuid.UUID, error) {
	acc := s.council.AccountID
	urlSum, bodySum := sha256.Sum256([]byte(source)), sha256.Sum256(page.Body)
	name := "council/url-" + hex.EncodeToString(urlSum[:16])
	key := evidence.NewKey(acc, s.job, name)
	if err := s.a.Evidence.Put(ctx, key, bytes.NewReader(page.Body), int64(len(page.Body)), page.ContentType); err != nil {
		return uuid.Nil(), err
	}
	art := domain.Artifact{
		AccountID: acc, ProjectID: s.council.ProjectID, JobID: s.job, Name: name, ContentType: page.ContentType,
		Size: int64(len(page.Body)), SHA256: hex.EncodeToString(bodySum[:]), StorageKey: key,
	}
	if err := s.a.Store.Artifacts().Put(ctx, &art); err != nil {
		return uuid.Nil(), err
	}
	return art.ID, nil
}

package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// CheckName is the name of the check run of the report.
const CheckName = "forecrew/verification"

// Tracker is what Publisher needs from GitHub; github.Tracker implements it.
type Tracker interface {
	Bot(ctx context.Context) (string, error)
	ListComments(ctx context.Context, repo string, number int, since time.Time) ([]github.Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) (github.Comment, error)
	UpdateComment(ctx context.Context, repo string, id int64, body string) (github.Comment, error)
	SetCheckRun(ctx context.Context, repo string, in github.CheckRunInput) error
}

// Publisher stores reports and shows them on the PR.
type Publisher struct {
	Store store.Store
	// Tracker publishes the comment and the check run; nil keeps the report
	// in the store only.
	Tracker Tracker
	// Evidence stores the rendered report as an artifact; nil stores none.
	Evidence evidence.Store
	Logger   *slog.Logger // defaults to discarding

	warnedChecks atomic.Bool
}

// conclusion maps a status to the conclusion of the check run: an incomplete
// report is neutral, so that it blocks no one while cells are still running.
func conclusion(s domain.VerificationStatus) github.CheckConclusion {
	switch s {
	case domain.VerificationPassed:
		return github.ConclusionSuccess
	case domain.VerificationFailed:
		return github.ConclusionFailure
	}
	return github.ConclusionNeutral
}

// Publish stores the report of (change, head sha), replacing the one a
// previous build stored, renders it as an artifact, updates the PR comment in
// place (or creates it) and sets the check run forecrew/verification on the
// sha. It can be called again for the same report. An App without the Checks
// permission gets a warning in the log, once, and keeps the comment.
func (p *Publisher) Publish(ctx context.Context, change domain.Change, rep Report) (domain.VerificationReport, error) {
	raw, err := rep.JSON()
	if err != nil {
		return domain.VerificationReport{}, err
	}
	md := Render(rep)
	prev, err := p.Store.VerificationReports().GetBySHA(ctx, change.AccountID, change.ID, rep.SHA)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return domain.VerificationReport{}, fmt.Errorf("read the stored report: %w", err)
	}
	art, replaced, err := p.artifact(ctx, change, rep.SHA, md, prev.ArtifactID)
	if err != nil {
		return domain.VerificationReport{}, err
	}
	out := domain.VerificationReport{
		AccountID: change.AccountID, ProjectID: change.ProjectID, ChangeID: change.ID, SHA: rep.SHA,
		Status: rep.Status, Report: raw, ArtifactID: art,
	}
	if err := p.Store.VerificationReports().Upsert(ctx, &out); err != nil {
		return domain.VerificationReport{}, fmt.Errorf("store the report: %w", err)
	}
	p.dropArtifact(ctx, replaced)
	if p.Tracker == nil {
		return out, nil
	}
	// The check run comes first: when the comment fails, the sha does not
	// keep the conclusion of an older build.
	err = p.Tracker.SetCheckRun(ctx, change.Repo, github.CheckRunInput{
		Name: CheckName, SHA: rep.SHA, Conclusion: conclusion(rep.Status), Title: rep.Title(),
		Summary: strings.TrimPrefix(md, Marker+"\n"),
	})
	switch {
	case errors.Is(err, github.ErrForbidden):
		if !p.warnedChecks.Swap(true) {
			p.log().WarnContext(ctx, "the GitHub App may not write check runs: the verification is published as a comment only; grant Checks: write",
				"account_id", change.AccountID, "repo", change.Repo)
		}
	case err != nil:
		return out, fmt.Errorf("set the %s check run: %w", CheckName, err)
	}
	// The comment shows the head of the PR: a report of an older sha leaves
	// the comment of the newer one alone.
	head, err := p.Store.Changes().Get(ctx, change.AccountID, change.ID)
	if err != nil {
		return out, fmt.Errorf("read the pull request: %w", err)
	}
	if !strings.EqualFold(head.HeadSHA, rep.SHA) {
		return out, nil
	}
	return out, p.comment(ctx, change, md)
}

func (p *Publisher) log() *slog.Logger {
	if p.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return p.Logger
}

// artifact stores md as the artifact of the report, unless the previous
// build's artifact holds the same content. It returns the artifact to link
// and the one it replaces, to drop once the report points at the new one.
func (p *Publisher) artifact(ctx context.Context, change domain.Change, sha, md string, prev uuid.UUID) (uuid.UUID, *domain.Artifact, error) {
	if p.Evidence == nil {
		return prev, nil, nil
	}
	sum := sha256.Sum256([]byte(md))
	hexSum := hex.EncodeToString(sum[:])
	var old *domain.Artifact
	if prev != uuid.Nil() {
		switch a, err := p.Store.Artifacts().Get(ctx, change.AccountID, prev); {
		case err == nil && a.SHA256 == hexSum:
			return prev, nil, nil
		case err == nil:
			old = &a
		case !errors.Is(err, store.ErrNotFound):
			return uuid.Nil(), nil, fmt.Errorf("read the stored artifact: %w", err)
		}
	}
	name := "verification/" + sha + ".md"
	key := evidence.NewKey(change.AccountID, change.ID, name)
	const contentType = "text/markdown; charset=utf-8"
	if err := p.Evidence.Put(ctx, key, strings.NewReader(md), int64(len(md)), contentType); err != nil {
		return uuid.Nil(), nil, fmt.Errorf("store the rendered report: %w", err)
	}
	a := domain.Artifact{
		AccountID: change.AccountID, ProjectID: change.ProjectID, Name: name, ContentType: contentType,
		Size: int64(len(md)), SHA256: hexSum, StorageKey: key,
	}
	if err := p.Store.Artifacts().Put(ctx, &a); err != nil {
		_ = p.Evidence.Delete(ctx, key)
		return uuid.Nil(), nil, fmt.Errorf("record the rendered report: %w", err)
	}
	return a.ID, old, nil
}

// dropArtifact removes a replaced artifact and its content. A failure is
// only logged: the reconciliation of the bucket removes what is left.
func (p *Publisher) dropArtifact(ctx context.Context, a *domain.Artifact) {
	if a == nil {
		return
	}
	if err := p.Store.Artifacts().Delete(ctx, a.AccountID, a.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		p.log().WarnContext(ctx, "delete a replaced verification artifact", "account_id", a.AccountID, "artifact_id", a.ID, "error", err)
		return
	}
	if err := p.Evidence.Delete(ctx, a.StorageKey); err != nil {
		p.log().WarnContext(ctx, "delete the content of a replaced verification artifact", "account_id", a.AccountID, "artifact_id", a.ID, "error", err)
	}
}

// comment updates the App's comment that starts with the marker, or creates
// it. Anyone can post the marker, and a token with write access may be able to
// edit their comments, so only the bot's own count.
func (p *Publisher) comment(ctx context.Context, change domain.Change, md string) error {
	bot, err := p.Tracker.Bot(ctx)
	if err != nil {
		return fmt.Errorf("read the App's login: %w", err)
	}
	comments, err := p.Tracker.ListComments(ctx, change.Repo, change.Number, time.Time{})
	if err != nil {
		return fmt.Errorf("list the comments of %s#%d: %w", change.Repo, change.Number, err)
	}
	for _, comment := range slices.Backward(comments) {
		if comment.Author != bot || !strings.HasPrefix(comment.Body, Marker) {
			continue
		}
		if comment.Body == md {
			return nil
		}
		_, err := p.Tracker.UpdateComment(ctx, change.Repo, comment.ID, md)
		if err == nil {
			return nil
		}
		if !errors.Is(err, github.ErrNotFound) { // deleted meanwhile: comment anew
			return fmt.Errorf("update the verification comment: %w", err)
		}
		break
	}
	if _, err := p.Tracker.CreateComment(ctx, change.Repo, change.Number, md); err != nil {
		return fmt.Errorf("comment on %s#%d: %w", change.Repo, change.Number, err)
	}
	return nil
}

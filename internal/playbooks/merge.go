package playbooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// mergeOutput is the output of github.merge_pr.
type mergeOutput struct {
	Merged        bool   `json:"merged"`
	AlreadyMerged bool   `json:"already_merged"`
	SHA           string `json:"sha"` // merge commit
}

// mergePR is the action github.merge_pr. It merges with.repo's pull request
// with.number with a squash merge unless it is already merged. Only a head
// whose checks succeeded, or that has none, is merged. The action fails when
// the PR was closed without merging, when the checks of its head failed or
// are still running, when with.check_profile is set and the profile file at
// the head is invalid, and when GitHub refuses the merge; a step with
// on_failure: escalate then lets a human fix it and retry. After merging it
// records the change as merged and publishes pr.merged, as the webhook will
// when GitHub reports the same fact.
func (a *actions) mergePR(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo         string `json:"repo"`
		Number       int    `json:"number"`
		CheckProfile bool   `json:"check_profile"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	pr, err := a.Tracker.GetPR(ctx, with.Repo, with.Number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s#%d: %w", with.Repo, with.Number, err)
	}
	switch {
	case pr.Merged:
		return mergeOutput{Merged: true, AlreadyMerged: true, SHA: pr.MergeSHA}, nil
	case pr.State != "open":
		return nil, fmt.Errorf("pull request %s#%d was closed without merging", with.Repo, with.Number)
	}
	checks, err := a.Tracker.CheckStatus(ctx, with.Repo, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("get checks of %s@%s: %w", with.Repo, pr.HeadSHA, err)
	}
	switch checks.State {
	case github.ChecksFailure:
		return nil, fmt.Errorf("checks failed on %s#%d (%s): fix them and approve again", with.Repo, with.Number, pr.HeadSHA)
	case github.ChecksPending:
		return nil, fmt.Errorf("checks are still running on %s#%d (%s): approve again when they are done", with.Repo, with.Number, pr.HeadSHA)
	}
	if with.CheckProfile {
		problem, err := a.profileFileProblem(ctx, with.Repo, pr.HeadSHA)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			return nil, fmt.Errorf("not merging %s#%d: %s: fix it and approve again", with.Repo, with.Number, problem)
		}
	}
	sha, err := a.Tracker.MergePR(ctx, with.Repo, with.Number, github.MergeInput{Method: github.MergeSquash, SHA: pr.HeadSHA})
	if err != nil {
		return nil, fmt.Errorf("merge pull request %s#%d: %w", with.Repo, with.Number, err)
	}
	if err := a.recordMerged(ctx, call.Run, with.Repo, with.Number, sha); err != nil {
		return nil, err
	}
	return mergeOutput{Merged: true, SHA: sha}, nil
}

// recordMerged marks the change of a merged PR as merged and publishes
// pr.merged; both are idempotent.
func (a *actions) recordMerged(ctx context.Context, run domain.WorkflowRun, repo string, number int, sha string) error {
	c, err := a.Store.Changes().GetByPR(ctx, run.AccountID, repo, number)
	switch {
	case err == nil:
		c.State = domain.ChangeMerged
		if err := a.Store.Changes().Upsert(ctx, &c); err != nil {
			return fmt.Errorf("record merge of %s#%d: %w", repo, number, err)
		}
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	sig, err := domain.NewSignal(run.AccountID, run.ProjectID, domain.PRMerged{Repo: repo, Number: number, SHA: sha}, a.Now())
	if err == nil {
		_, err = a.Signals.Publish(ctx, sig)
	}
	if err != nil {
		return fmt.Errorf("publish merge of %s#%d: %w", repo, number, err)
	}
	return nil
}

package playbooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/workflow"
)

// prOutput is the output of github.open_or_update_pr.
type prOutput struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"head_sha"`
	// Merged is set, with the merge commit, when the previous round's PR was
	// merged on GitHub: the action then opens and updates nothing.
	Merged   bool   `json:"merged"`
	MergeSHA string `json:"merge_sha,omitempty"`
	// IfOpen holds the PR's number, or nothing when it was merged. A foreach
	// over it runs its steps only for a PR that is still open.
	IfOpen []int `json:"if_open"`
}

// openOrUpdatePR is the action github.open_or_update_pr. It opens a pull
// request from with.head to with.base in with.repo, or updates the title and
// body of the open one from that head, and upserts the change (the run's
// project and with.task_id or else the run's task, the PR number and its
// head SHA). It outputs the PR's number, URL and head SHA. See with.previous
// for a PR merged meanwhile.
func (a *actions) openOrUpdatePR(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo  string `json:"repo"`
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		// Previous is the number of the PR an earlier round opened, if any.
		// When a human closed it the action fails instead of opening
		// another PR; when a human merged it the action outputs it as
		// merged and changes nothing, so the human's decision stands.
		Previous int `json:"previous"`
		// TaskID is the task the PR delivers; it defaults to the run's.
		TaskID uuid.UUID `json:"task_id"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"repo": with.Repo, "title": with.Title, "head": with.Head, "base": with.Base} {
		if v == "" {
			return nil, fmt.Errorf("with: %s is required", name)
		}
	}
	if with.Previous > 0 {
		prev, err := a.Tracker.GetPR(ctx, with.Repo, with.Previous)
		if err != nil {
			return nil, fmt.Errorf("get pull request %s#%d: %w", with.Repo, with.Previous, err)
		}
		switch {
		case prev.Merged:
			return prOutput{Number: prev.Number, URL: prev.URL, HeadSHA: prev.HeadSHA, Merged: true, MergeSHA: prev.MergeSHA, IfOpen: []int{}}, nil
		case prev.State != "open":
			return nil, fmt.Errorf("pull request %s#%d was closed on GitHub: not opening another", with.Repo, with.Previous)
		}
	}
	pr, err := a.Tracker.FindPR(ctx, with.Repo, with.Head)
	switch {
	case err == nil:
		pr, err = a.Tracker.UpdatePR(ctx, with.Repo, pr.Number, github.PRUpdate{Title: &with.Title, Body: &with.Body})
		if err != nil {
			return nil, fmt.Errorf("update pull request %s#%d: %w", with.Repo, pr.Number, err)
		}
	case errors.Is(err, github.ErrNotFound):
		pr, err = a.Tracker.CreatePR(ctx, with.Repo, github.PRInput{Title: with.Title, Body: with.Body, Head: with.Head, Base: with.Base})
		if err != nil {
			return nil, fmt.Errorf("open pull request %s -> %s in %s: %w", with.Head, with.Base, with.Repo, err)
		}
	default:
		return nil, fmt.Errorf("find pull request of %s in %s: %w", with.Head, with.Repo, err)
	}
	c := domain.Change{
		AccountID: call.Run.AccountID, ProjectID: call.Run.ProjectID, TaskID: cmp.Or(with.TaskID, call.Run.TaskID),
		Repo: with.Repo, Number: pr.Number, Branch: with.Head, BaseRef: with.Base, HeadSHA: pr.HeadSHA,
		State: domain.ChangeOpen, URL: pr.URL,
	}
	if err := a.Store.Changes().Upsert(ctx, &c); err != nil {
		return nil, fmt.Errorf("record pull request %s#%d: %w", with.Repo, pr.Number, err)
	}
	return prOutput{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA, IfOpen: []int{pr.Number}}, nil
}

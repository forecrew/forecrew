package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
)

// noChecksAfter is how long a PR head may stay without any check, counted from
// the last update of the change, before it counts as having none.
const noChecksAfter = 10 * time.Minute

// checkEvent looks at the open PRs whose head is the SHA of a check_suite,
// check_run or status event.
func (e *engine) checkEvent(ctx context.Context, ev github.Event) error {
	if ev.SHA == "" {
		return nil
	}
	return e.eachProject(ctx, ev.Repo, func(p domain.Project) error {
		changes, err := e.Store.Changes().List(ctx, p.AccountID, store.ChangeFilter{ProjectID: p.ID, State: domain.ChangeOpen})
		if err != nil {
			return fmt.Errorf("list changes of %s: %w", p.Name, err)
		}
		for _, c := range changes {
			if strings.EqualFold(c.Repo, ev.Repo) && strings.EqualFold(c.HeadSHA, ev.SHA) {
				if err := e.checkChange(ctx, c); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// checkChange publishes checks.completed when the checks of the head of c are
// final: success or failure, or none once noChecksAfter has passed since c was
// last updated. Pending checks publish nothing.
func (e *engine) checkChange(ctx context.Context, c domain.Change) error {
	st, err := e.Tracker.CheckStatus(ctx, c.Repo, c.HeadSHA)
	if err != nil {
		return fmt.Errorf("check status of %s#%d: %w", c.Repo, c.Number, err)
	}
	var conclusion domain.CheckConclusion
	switch st.State {
	case github.ChecksSuccess:
		conclusion = domain.ChecksSuccess
	case github.ChecksFailure:
		conclusion = domain.ChecksFailure
	case github.ChecksNone:
		if e.Now().Sub(c.UpdatedAt) < noChecksAfter {
			return nil
		}
		conclusion = domain.ChecksNone
	default:
		return nil
	}
	_, err = e.publish(ctx, c.AccountID, c.ProjectID, domain.ChecksCompleted{Repo: c.Repo, Number: c.Number, SHA: c.HeadSHA, Conclusion: conclusion})
	return err
}

package proposals

import (
	"context"
	"regexp"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// maxCitedNotes is how many notes one proposal may promote.
const maxCitedNotes = 10

// noteRef is how a proposal's reason cites an agent note: note:<id>.
var noteRef = regexp.MustCompile(`\bnote:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)

// citedNotes returns the notes a reason cites, at most maxCitedNotes.
func citedNotes(reason string) []uuid.UUID {
	var out []uuid.UUID
	for _, m := range noteRef.FindAllStringSubmatch(reason, -1) {
		id, err := uuid.Parse(m[1])
		if err == nil && !slices.Contains(out, id) && len(out) < maxCitedNotes {
			out = append(out, id)
		}
	}
	return out
}

// promote marks the notes the reason of a proposal of job j cites as
// promoted: a note of the job's project that the job could see (a project
// note, or a note of its own goal). Anything else a reason names is ignored,
// since a reason is free text. A promoted note does not expire, so what the
// proposal rests on stays while it is in review.
func promote(ctx context.Context, tx store.Store, j domain.Job, reason string) error {
	var ids []uuid.UUID
	for _, id := range citedNotes(reason) {
		n, err := tx.Knowledge().Get(ctx, j.AccountID, id)
		switch {
		case err != nil:
			continue // not there, or not an id of this account
		case n.Provider != domain.ProviderInternal || n.ProjectID != j.ProjectID:
		case n.Scope == domain.NoteScopeGoal && n.GoalID != j.GoalID:
		default:
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Knowledge().MarkPromoted(ctx, j.AccountID, ids)
}

// release takes back the promotion of the notes the rejected proposals cite,
// except those another proposal that is not rejected still cites: a note is
// kept for the sake of a proposal that can still be accepted.
func release(ctx context.Context, tx store.Store, rejected []domain.KnowledgeProposal) error {
	if len(rejected) == 0 {
		return nil
	}
	all, err := tx.KnowledgeProposals().List(ctx, rejected[0].AccountID, store.KnowledgeProposalFilter{ProjectID: rejected[0].ProjectID})
	if err != nil {
		return err
	}
	kept := map[uuid.UUID]bool{}
	for _, k := range all {
		if k.Status != domain.KnowledgeRejected {
			for _, id := range citedNotes(k.Reason) {
				kept[id] = true
			}
		}
	}
	var ids []uuid.UUID
	for _, k := range rejected {
		for _, id := range citedNotes(k.Reason) {
			if !kept[id] && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.Knowledge().ClearPromoted(ctx, rejected[0].AccountID, ids)
}

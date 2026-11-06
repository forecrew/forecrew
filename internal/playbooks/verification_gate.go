package playbooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

// headReport returns the stored verification report of the pull request
// repo#number at the commit sha, and false when there is none: no change, or
// no report for that commit, which is what a push after the report looks
// like. The gates decide on the report's status, never on the check run: an
// incomplete report is a neutral check run, which GitHub counts as passed.
func (a *actions) headReport(ctx context.Context, accountID uuid.UUID, repo string, number int, sha string) (verification.Report, bool, error) {
	ch, err := a.Store.Changes().GetByPR(ctx, accountID, repo, number)
	if errors.Is(err, store.ErrNotFound) {
		return verification.Report{}, false, nil
	}
	if err != nil {
		return verification.Report{}, false, fmt.Errorf("get change %s#%d: %w", repo, number, err)
	}
	stored, err := a.Store.VerificationReports().GetBySHA(ctx, accountID, ch.ID, sha)
	if errors.Is(err, store.ErrNotFound) {
		return verification.Report{}, false, nil
	}
	if err != nil {
		return verification.Report{}, false, fmt.Errorf("get the verification report of %s#%d at %s: %w", repo, number, sha, err)
	}
	rep, err := verification.Parse(stored.Report)
	if err != nil {
		return verification.Report{}, false, fmt.Errorf("verification report %s: %w", stored.ID, err)
	}
	return rep, true, nil
}

// blockingLines returns up to five reasons that block a report, one line
// each, and how many there are besides.
func blockingLines(rep verification.Report) (lines []string, more int) {
	for i, b := range rep.Blocking {
		if i == 5 {
			return lines, len(rep.Blocking) - 5
		}
		lines = append(lines, contextpack.OneLine(b))
	}
	return lines, 0
}

// requireVerified returns an error unless the head sha of repo#number has a
// passed verification report. The text is shown to the human whose approval
// the merge refuses, so the reasons, which come from the repo's tests, are one
// line each and quoted as untrusted data.
func (a *actions) requireVerified(ctx context.Context, accountID uuid.UUID, repo string, number int, sha string) error {
	rep, found, err := a.headReport(ctx, accountID, repo, number, sha)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("not merging %s#%d: its head %s has no verification report: verify it and approve again", repo, number, sha)
	case rep.Status == domain.VerificationPassed:
		return nil
	}
	lines, more := blockingLines(rep)
	reason := strings.Join(lines, "\n")
	if more > 0 {
		reason += fmt.Sprintf("\nand %d more", more)
	}
	return fmt.Errorf("not merging %s#%d: the verification of %s is %s: what blocks it, from the test results of the repo:\n%s\nfix it and approve again", repo, number, sha, rep.Status, contextpack.WrapUntrusted("verification report", reason))
}

// verificationReadOutput is the output of verification.read.
type verificationReadOutput struct {
	Status   domain.VerificationStatus `json:"status"`
	Blocking []string                  `json:"blocking"`
	Summary  string                    `json:"summary"`
	// Complete is false while required results are still missing.
	Complete bool `json:"complete"`
	// IfIncomplete holds the commit while the report is incomplete, for a
	// foreach that asks a human.
	IfIncomplete []string `json:"if_incomplete"`
	// Text tells a job (the reviewer) what the report found, with what comes
	// from the repo quoted as untrusted data.
	Text string `json:"text"`
}

// verificationRead is the action verification.read. It reads the stored
// report of with.change at the head with.sha and outputs its status, what
// blocks, its one-line summary and text for the reviewer. It never builds
// anything, so it is idempotent. A head without a report is an error.
func (a *actions) verificationRead(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Change int    `json:"change"`
		SHA    string `json:"sha"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	if with.Change <= 0 || with.SHA == "" {
		return nil, errors.New("with: change and sha are required")
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	rep, found, err := a.headReport(ctx, call.Run.AccountID, p.Repo, with.Change, with.SHA)
	switch {
	case err != nil:
		return nil, err
	case !found:
		return nil, fmt.Errorf("%s#%d has no verification report at %s", p.Repo, with.Change, with.SHA)
	}
	out := verificationReadOutput{
		Status: rep.Status, Blocking: append([]string{}, rep.Blocking...), Summary: rep.Summary(), Complete: rep.Status != domain.VerificationIncomplete,
		IfIncomplete: []string{},
	}
	if !out.Complete {
		out.IfIncomplete = append(out.IfIncomplete, with.SHA)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Verification report of %s at %s: %s\n", p.Repo, with.SHA, out.Summary)
	if len(rep.Blocking) > 0 {
		b.WriteString("\nWhat blocks the merge, from the test results of the repo:\n")
		b.WriteString(contextpack.WrapUntrusted("verification report", strings.Join(rep.Blocking, "\n")))
	}
	out.Text = strings.TrimSpace(b.String())
	return out, nil
}

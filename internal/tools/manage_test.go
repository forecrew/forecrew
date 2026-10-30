package tools_test

import (
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestCancelJob(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.newGoal(t, "one")
	first, second := f.newJob(t, "reviewer", g, domain.Task{}, 0), f.newJob(t, "reviewer", g, domain.Task{}, 0)
	w := domain.Worker{AccountID: f.account.ID, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, f.st.Workers().Create(ctx, &w))
	leased, err := f.st.Jobs().LeaseNext(ctx, f.account.ID, w.ID, []string{"runtime:claude-code"}, domain.LeaseDuration)
	must(t, err)

	if leased.ID != first.ID || leased.Status != domain.JobRunning {
		t.Fatalf("leased %s (%s), want %s running", leased.Ref(), leased.Status, first.Ref())
	}
	// One job runs, the other is queued.
	for _, j := range []domain.Job{first, second} {
		info, err := f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: j.Ref()})
		must(t, err)
		if info.Ref != j.Ref() || info.Status != domain.JobCanceled || info.FinishedAt.IsZero() {
			t.Errorf("canceled %s = %+v", j.Ref(), info)
		}
	}
	// A finished job cannot be canceled again.
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: second.Ref()})
	wantErr(t, err, domain.ErrInvalidTransition)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: "JOB-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)
	other, _, otherJob := f.otherAccount(t)
	_, err = f.svc.CancelJob(ctx, f.by, mcp.JobRefInput{Job: otherJob.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	if got, _ := f.st.Jobs().Get(ctx, other.AccountID, otherJob.ID); got.Status != domain.JobQueued {
		t.Errorf("job of another account is %s", got.Status)
	}

	want := []string{"mcp:laptop job.canceled", "mcp:laptop job.canceled"}
	if got := f.auditActions(t); !slices.Equal(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

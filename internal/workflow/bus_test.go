package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestBus(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	now := time.Now().UTC()

	bus := workflow.NewBus(st.Signals())
	ctx, cancel := context.WithCancel(t.Context())
	sub := bus.Subscribe(ctx)
	job := domain.NewID()
	sig, err := domain.NewSignal(acc.ID, p.ID, &domain.JobCompleted{JobID: job, Status: domain.JobSucceeded}, now)
	must(t, err)
	if ok, err := bus.Publish(t.Context(), sig); !ok || err != nil {
		t.Fatalf("Publish = %v, %v", ok, err)
	}
	if ok, err := bus.Publish(t.Context(), sig); ok || err != nil {
		t.Fatalf("duplicate Publish = %v, %v", ok, err)
	}
	if got := <-sub; got.Key != domain.JobKey(job) {
		t.Fatalf("received %+v", got)
	}
	stored, err := bus.Lookup(t.Context(), acc.ID, domain.JobKey(job))
	if err != nil || len(stored) != 1 {
		t.Fatalf("Lookup = %v, %v", stored, err)
	}
	// A full subscriber does not block publishers.
	for i := range workflow.SubscriberBuffer + 1 {
		s, _ := domain.NewSignal(acc.ID, p.ID, &domain.PRClosed{Repo: "acme/app", Number: i + 1}, now)
		if _, err := bus.Publish(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	n := 0
	for range sub {
		n++
	}
	if n != workflow.SubscriberBuffer {
		t.Fatalf("drained %d signals, want %d", n, workflow.SubscriberBuffer)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

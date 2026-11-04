package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

// openPR opens a PR in the tracker and stores it as a change.
func (e *env) openPR() domain.Change {
	e.t.Helper()
	must(e.t, e.tracker.AddBranch(repo, "agent/task-1"))
	pr, err := e.tracker.CreatePR(e.t.Context(), repo, github.PRInput{Title: "x", Head: "agent/task-1", Base: "main"})
	must(e.t, err)
	return e.addChange(pr.Number, pr.HeadSHA)
}

func (e *env) poller() *Poller { return &Poller{e: e.eng, interval: pollEvery} }

func TestReconcileMergedPR(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	g := e.openGate(domain.GateMergeApproval, c.Number)
	_, err := e.tracker.MergePR(t.Context(), repo, c.Number, github.MergeInput{})
	must(t, err)

	must(t, e.poller().Reconcile(t.Context()))

	key := domain.PRKey(repo, c.Number)
	if got := e.signals(key); len(got) != 1 || got[0].Kind != domain.SignalPRMerged {
		t.Fatalf("signals = %+v", got)
	}
	if e.gate(g).Status != domain.GateApproved {
		t.Fatalf("gate = %s", e.gate(g).Status)
	}
	if e.change(c.Number).State != domain.ChangeMerged {
		t.Fatalf("change = %+v", e.change(c.Number))
	}
	// The next round has nothing left to do.
	must(t, e.poller().Reconcile(t.Context()))
	if len(e.signals(key)) != 1 {
		t.Fatal("second round published again")
	}
}

func TestReconcileClosedPR(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	g := e.openGate(domain.GateProfileApproval, c.Number)
	must(t, e.tracker.ClosePR(repo, c.Number))
	must(t, e.poller().Reconcile(t.Context()))
	if got := e.signals(domain.PRKey(repo, c.Number)); len(got) != 1 || got[0].Kind != domain.SignalPRClosed {
		t.Fatalf("signals = %+v", got)
	}
	if e.gate(g).Status != domain.GateRejected || e.change(c.Number).State != domain.ChangeClosed {
		t.Fatal("gate or change not updated")
	}
}

// The webhook and the poller see the same merge in either order.
func TestWebhookAndPollerAgree(t *testing.T) {
	for _, webhookFirst := range []bool{true, false} {
		e := newEnv(t)
		c := e.openPR()
		_, err := e.tracker.MergePR(t.Context(), repo, c.Number, github.MergeInput{})
		must(t, err)
		payload := []byte(`{"action":"closed","number":1,"pull_request":{"number":1,"merged":true,"head":{"sha":"merge-sha"}},"repository":{"full_name":"Acme/App"}}`)
		if webhookFirst {
			e.send("d1", "pull_request", payload)
			must(t, e.poller().Reconcile(t.Context()))
		} else {
			must(t, e.poller().Reconcile(t.Context()))
			// The change is merged already; the late webhook still dedupes.
			e.send("d1", "pull_request", payload)
		}
		if got := e.signals(domain.PRKey(repo, c.Number)); len(got) != 1 {
			t.Fatalf("webhookFirst=%v: signals = %+v", webhookFirst, got)
		}
	}
}

func TestReconcileChecks(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	key := domain.ChecksKey(repo, c.HeadSHA)

	// Pending checks, then no signal.
	must(t, e.tracker.SetCheckStatus(repo, c.HeadSHA, github.ChecksPending, 1))
	must(t, e.poller().Reconcile(t.Context()))
	if len(e.signals(key)) != 0 {
		t.Fatal("signal while pending")
	}
	// The webhook for the end of the checks was lost.
	must(t, e.tracker.SetCheckStatus(repo, c.HeadSHA, github.ChecksFailure, 1))
	must(t, e.poller().Reconcile(t.Context()))
	must(t, e.poller().Reconcile(t.Context()))
	if got := e.signals(key); len(got) != 1 {
		t.Fatalf("signals = %+v", got)
	}
}

func TestReconcileNoChecks(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	e.clock.Advance(noChecksAfter - time.Minute)
	must(t, e.poller().Reconcile(t.Context()))
	if len(e.signals(domain.ChecksKey(repo, c.HeadSHA))) != 0 {
		t.Fatal("none signalled too early")
	}
	e.clock.Advance(time.Minute)
	must(t, e.poller().Reconcile(t.Context()))
	got := e.signals(domain.ChecksKey(repo, c.HeadSHA))
	if len(got) != 1 {
		t.Fatalf("signals = %+v", got)
	}
	if p, _ := got[0].Decode(); p.(*domain.ChecksCompleted).Conclusion != domain.ChecksNone {
		t.Fatalf("payload = %+v", p)
	}
}

func TestReconcileNewHead(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	e.clock.Advance(noChecksAfter)
	must(t, e.tracker.SetHeadSHA(repo, c.Number, "pushed"))
	must(t, e.tracker.SetCheckStatus(repo, "pushed", github.ChecksSuccess, 1))
	must(t, e.poller().Reconcile(t.Context()))
	if got := e.change(c.Number); got.HeadSHA != "pushed" || !got.UpdatedAt.Equal(t0.Add(noChecksAfter)) {
		t.Fatalf("change = %+v", got)
	}
	if got := e.signals(domain.ChecksKey(repo, "pushed")); len(got) != 1 {
		t.Fatalf("signals = %+v", got)
	}
	if len(e.signals(domain.ChecksKey(repo, c.HeadSHA))) != 0 {
		t.Fatal("signal for the old head")
	}
}

func TestReconcileKeepsGoingAfterAnError(t *testing.T) {
	e := newEnv(t)
	e.addChange(99, "ghost") // not a PR in the tracker
	c := e.openPR()
	must(t, e.tracker.SetCheckStatus(repo, c.HeadSHA, github.ChecksSuccess, 1))
	if err := e.poller().Reconcile(t.Context()); err == nil {
		t.Fatal("no error for the missing PR")
	}
	if len(e.signals(domain.ChecksKey(repo, c.HeadSHA))) != 1 {
		t.Fatal("the healthy change was skipped")
	}
}

func TestPollerRun(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	must(t, e.tracker.SetCheckStatus(repo, c.HeadSHA, github.ChecksSuccess, 1))
	p := NewPoller(e.deps())
	if p.interval != 5*time.Minute {
		t.Fatalf("interval = %v", p.interval)
	}
	p.interval = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	sub := e.bus.Subscribe(ctx)
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case s := <-sub:
		if s.Kind != domain.SignalChecksCompleted {
			t.Fatalf("signal = %s", s.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal")
	}
	cancel()
	<-done
}

// The profile PR is not a stored change; a missed merge must still resolve its gate.
func TestReconcileGateOfUnstoredPR(t *testing.T) {
	e := newEnv(t)
	must(t, e.tracker.AddBranch(repo, "agent/onboard"))
	pr, err := e.tracker.CreatePR(t.Context(), repo, github.PRInput{Title: "profile", Head: "agent/onboard", Base: "main"})
	must(t, err)
	profile := e.openGate(domain.GateProfileApproval, pr.Number)
	stillOpen := e.openGate(domain.GateMergeApproval, pr.Number)
	issueGate := e.openGate(domain.GateSpecApproval, pr.Number)
	_, err = e.tracker.MergePR(t.Context(), repo, pr.Number, github.MergeInput{})
	must(t, err)

	must(t, e.poller().Reconcile(t.Context()))

	for _, g := range []domain.Gate{profile, stillOpen} {
		if e.gate(g).Status != domain.GateApproved {
			t.Fatalf("%s = %s", g.Ref(), e.gate(g).Status)
		}
	}
	if e.gate(issueGate).Status != domain.GatePending {
		t.Fatal("spec gate was decided")
	}
	if got := e.signals(domain.PRKey(repo, pr.Number)); len(got) != 1 || got[0].Kind != domain.SignalPRMerged {
		t.Fatalf("signals = %+v", got)
	}
}

func TestReconcileGateOfOpenOrMissingPR(t *testing.T) {
	e := newEnv(t)
	must(t, e.tracker.AddBranch(repo, "h"))
	open, err := e.tracker.CreatePR(t.Context(), repo, github.PRInput{Title: "x", Head: "h", Base: "main"})
	must(t, err)
	g := e.openGate(domain.GateProfileApproval, open.Number)
	missing := e.openGate(domain.GateMergeApproval, 99)
	if err := e.poller().Reconcile(t.Context()); err == nil {
		t.Fatal("no error for the missing PR")
	}
	if e.gate(g).Status != domain.GatePending || e.gate(missing).Status != domain.GatePending {
		t.Fatal("gate decided while the PR is open or unknown")
	}
}

// A failure while handling the webhook leaves the change open; the next
// Reconcile finishes the work.
func TestReconcileFinishesAfterFailedWebhook(t *testing.T) {
	e := newEnv(t)
	c := e.openPR()
	g := e.openGate(domain.GateMergeApproval, c.Number)
	_, err := e.tracker.MergePR(t.Context(), repo, c.Number, github.MergeInput{})
	must(t, err)
	good := e.eng.Deps
	e.eng.Gates = failingGates{e.gates}
	ev := github.Event{Kind: "pull_request", Action: "closed", Repo: repo, Number: c.Number, Merged: true, SHA: "x"}
	if err := e.eng.handle(t.Context(), ev); err == nil {
		t.Fatal("no error")
	}
	if e.change(c.Number).State != domain.ChangeOpen {
		t.Fatal("change closed")
	}
	e.eng.Deps = good
	must(t, e.poller().Reconcile(t.Context()))
	if e.change(c.Number).State != domain.ChangeMerged || e.gate(g).Status != domain.GateApproved {
		t.Fatal("poller did not finish")
	}
	if len(e.signals(domain.PRKey(repo, c.Number))) != 1 {
		t.Fatal("signal duplicated")
	}
}

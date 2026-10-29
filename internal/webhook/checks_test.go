package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

func (e *env) checksSignals(sha string) []domain.Signal {
	e.t.Helper()
	return e.signals(domain.ChecksKey(repo, sha))
}

func TestChecksWaitForTheLastSuite(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	suite := fixture(t, "check_suite_completed.json")

	// A first suite finished, a second one is still running.
	must(t, e.tracker.SetCheckStatus(repo, "abc123", github.ChecksPending, 2))
	e.send("d1", "check_suite", suite)
	if got := e.checksSignals("abc123"); len(got) != 0 {
		t.Fatalf("signal while pending: %+v", got)
	}

	// The second one finishes: one signal, however many events follow.
	must(t, e.tracker.SetCheckStatus(repo, "abc123", github.ChecksSuccess, 2))
	e.send("d2", "check_suite", suite)
	e.send("d3", "check_suite", suite)
	e.send("d4", "status", fixture(t, "status.json"))
	got := e.checksSignals("abc123")
	if len(got) != 1 || got[0].DedupeKey != "checks.completed:acme/app@abc123" {
		t.Fatalf("signals = %+v", got)
	}
	p, err := got[0].Decode()
	must(t, err)
	if want := (&domain.ChecksCompleted{Repo: repo, Number: 7, SHA: "abc123", Conclusion: domain.ChecksSuccess}); *p.(*domain.ChecksCompleted) != *want {
		t.Fatalf("payload = %+v", p)
	}
}

func TestChecksFailure(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	must(t, e.tracker.SetCheckStatus(repo, "abc123", github.ChecksFailure, 3))
	e.send("d1", "check_run", fixture(t, "check_run_completed.json"))
	got := e.checksSignals("abc123")
	if len(got) != 1 {
		t.Fatalf("signals = %+v", got)
	}
	p, _ := got[0].Decode()
	if p.(*domain.ChecksCompleted).Conclusion != domain.ChecksFailure {
		t.Fatalf("payload = %+v", p)
	}
}

func TestChecksIgnoreOtherCommits(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "other")
	closed := e.addChange(8, "abc123")
	closed.State = domain.ChangeMerged
	must(t, e.st.Changes().Upsert(t.Context(), &closed))
	must(t, e.tracker.SetCheckStatus(repo, "abc123", github.ChecksSuccess, 1))
	e.send("d1", "check_suite", fixture(t, "check_suite_completed.json"))
	if len(e.checksSignals("abc123"))+len(e.checksSignals("other")) != 0 {
		t.Fatal("signal for a commit that is not the head of an open PR")
	}
	// A suite without a SHA is skipped.
	e.send("d2", "check_suite", []byte(`{"action":"completed","check_suite":{},"repository":{"full_name":"Acme/App"}}`))
}

func TestNoChecksAfterTimeout(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	suite := fixture(t, "check_suite_completed.json")

	e.clock.Advance(noChecksAfter - time.Second)
	e.send("d1", "check_suite", suite)
	if len(e.checksSignals("abc123")) != 0 {
		t.Fatal("none signalled too early")
	}
	e.clock.Advance(time.Second)
	e.send("d2", "check_suite", suite)
	got := e.checksSignals("abc123")
	if len(got) != 1 {
		t.Fatalf("signals = %+v", got)
	}
	p, _ := got[0].Decode()
	if p.(*domain.ChecksCompleted).Conclusion != domain.ChecksNone {
		t.Fatalf("payload = %+v", p)
	}
}

func TestPushRestartsTheWait(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	e.clock.Advance(noChecksAfter)
	e.send("d1", "pull_request", fixture(t, "pull_request_synchronize.json"))
	c := e.change(7)
	if c.HeadSHA != "def456" || c.State != domain.ChangeOpen || !c.UpdatedAt.Equal(t0.Add(noChecksAfter)) {
		t.Fatalf("change = %+v", c)
	}
	// Unknown PRs are not tracked.
	e.send("d2", "pull_request", []byte(`{"action":"reopened","number":9,"pull_request":{"number":9,"head":{"sha":"x"}},"repository":{"full_name":"Acme/App"}}`))
	if _, err := e.st.Changes().GetByPR(t.Context(), e.account, repo, 9); err == nil {
		t.Fatal("unknown PR was stored")
	}
}

type failingTracker struct{ github.Tracker }

func (failingTracker) CheckStatus(context.Context, string, string) (github.CheckStatus, error) {
	return github.CheckStatus{}, errors.New("boom")
}

func TestChecksTrackerError(t *testing.T) {
	e := newEnv(t)
	c := e.addChange(7, "abc123")
	e.eng.Tracker = failingTracker{e.tracker}
	if err := e.eng.checkChange(t.Context(), c); err == nil {
		t.Fatal("no error")
	}
	if err := e.eng.handle(t.Context(), github.Event{Kind: "status", Repo: repo, SHA: "abc123"}); err == nil {
		t.Fatal("no error from handle")
	}
}

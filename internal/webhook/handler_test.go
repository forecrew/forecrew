package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestPullRequestMerged(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	profile := e.openGate(domain.GateProfileApproval, 7)
	merge := e.openGate(domain.GateMergeApproval, 7)
	other := e.openGate(domain.GateMergeApproval, 8)
	needsYou := e.openGate(domain.GateNeedsYou, 7)

	e.send("d1", "pull_request", fixture(t, "pull_request_merged.json"))

	merged := e.signals(domain.PRKey(repo, 7))
	if len(merged) != 1 || merged[0].Kind != domain.SignalPRMerged || merged[0].DedupeKey != "pr.merged:acme/app#7" {
		t.Fatalf("pr signals = %+v", merged)
	}
	if merged[0].ProjectID != e.project.ID {
		t.Fatalf("project = %v", merged[0].ProjectID)
	}
	for _, g := range []domain.Gate{profile, merge} {
		got, err := e.gates.Get(t.Context(), e.account, g.ID)
		must(t, err)
		if got.Status != domain.GateApproved || got.ResolvedBy != "github:github" {
			t.Fatalf("%s = %s by %s", g.Ref(), got.Status, got.ResolvedBy)
		}
		if len(e.signals(domain.GateKey(g.ID))) != 1 {
			t.Fatalf("no gate.resolved for %s", g.Ref())
		}
	}
	for _, g := range []domain.Gate{other, needsYou} {
		got, _ := e.gates.Get(t.Context(), e.account, g.ID)
		if got.Status != domain.GatePending {
			t.Fatalf("%s = %s, want pending", g.Ref(), got.Status)
		}
	}
	if c := e.change(7); c.State != domain.ChangeMerged {
		t.Fatalf("change state = %s", c.State)
	}
}

func TestPullRequestClosed(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	g := e.openGate(domain.GateMergeApproval, 7)

	e.send("d1", "pull_request", fixture(t, "pull_request_closed.json"))

	closed := e.signals(domain.PRKey(repo, 7))
	if len(closed) != 1 || closed[0].Kind != domain.SignalPRClosed || closed[0].DedupeKey != "pr.closed:acme/app#7" {
		t.Fatalf("pr signals = %+v", closed)
	}
	got, _ := e.gates.Get(t.Context(), e.account, g.ID)
	if got.Status != domain.GateRejected {
		t.Fatalf("gate = %s", got.Status)
	}
	if c := e.change(7); c.State != domain.ChangeClosed {
		t.Fatalf("change state = %s", c.State)
	}
}

func TestPullRequestUnknownAndOtherActions(t *testing.T) {
	e := newEnv(t)
	e.send("d1", "pull_request", fixture(t, "pull_request_opened.json"))
	if len(e.signals(domain.PRKey(repo, 7))) != 0 {
		t.Fatal("opened PR produced a signal")
	}
	// A merged PR without a stored change still produces its signal.
	e.send("d2", "pull_request", fixture(t, "pull_request_merged.json"))
	if len(e.signals(domain.PRKey(repo, 7))) != 1 {
		t.Fatal("no signal for an unknown PR")
	}
}

func TestUntrackedRepoIgnored(t *testing.T) {
	e := newEnv(t)
	payload := []byte(`{"action":"closed","number":3,"pull_request":{"number":3,"merged":true,"head":{"sha":"x"}},"repository":{"full_name":"other/repo"}}`)
	e.send("d1", "pull_request", payload)
	if len(e.signals(domain.PRKey("other/repo", 3))) != 0 {
		t.Fatal("signal for an untracked repo")
	}
}

func TestDuplicateDeliveryIgnored(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	e.send("d1", "pull_request", fixture(t, "pull_request_merged.json"))
	// A change that is open again would be merged again by a second run.
	c := e.change(7)
	c.State = domain.ChangeOpen
	must(t, e.st.Changes().Upsert(t.Context(), &c))
	e.send("d1", "pull_request", fixture(t, "pull_request_merged.json"))
	if e.change(7).State != domain.ChangeOpen {
		t.Fatal("redelivery was processed")
	}
	e.send("d2", "pull_request", fixture(t, "pull_request_merged.json"))
	if e.change(7).State != domain.ChangeMerged || len(e.signals(domain.PRKey(repo, 7))) != 1 {
		t.Fatal("a new delivery was not processed")
	}
}

type failingDeliveries struct{ store.WebhookDeliveryRepo }

func (failingDeliveries) Record(context.Context, domain.WebhookDelivery) (bool, error) {
	return false, errors.New("boom")
}

// failingStore fails the delivery log.
type failingStore struct{ store.Store }

func (failingStore) WebhookDeliveries() store.WebhookDeliveryRepo { return failingDeliveries{} }

func TestDeliveryStorageError(t *testing.T) {
	e := newEnv(t)
	e.eng.Store = failingStore{e.st}
	e.send("d1", "pull_request", fixture(t, "pull_request_merged.json"))
	if len(e.signals(domain.PRKey(repo, 7))) != 0 {
		t.Fatal("event processed without a delivery record")
	}
}

func TestBadSignature(t *testing.T) {
	e := newEnv(t)
	payload := fixture(t, "pull_request_merged.json")
	for name, sig := range map[string]string{"missing": "", "wrong": "sha256=00", "other body": sign([]byte("x"))} {
		if code := e.deliverSigned("d-"+name, "pull_request", payload, sig); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	e.process()
	if len(e.signals(domain.PRKey(repo, 7))) != 0 {
		t.Fatal("unsigned delivery was processed")
	}
	// A rejected delivery is not remembered: the signed retry goes through.
	if code := e.deliver("d-missing", "pull_request", payload); code != http.StatusAccepted {
		t.Fatalf("signed retry = %d", code)
	}
}

func TestDeliveryErrors(t *testing.T) {
	e := newEnv(t)
	if code := e.deliver("", "pull_request", fixture(t, "pull_request_merged.json")); code != http.StatusBadRequest {
		t.Errorf("no delivery id = %d", code)
	}
	if code := e.deliver("d1", "ping", fixture(t, "ping.json")); code != http.StatusAccepted {
		t.Errorf("ping = %d", code)
	}
	if code := e.deliver("d2", "pull_request", []byte("{")); code != http.StatusBadRequest {
		t.Errorf("bad payload = %d", code)
	}
	if code := e.deliver("d3", "pull_request", []byte{}); code != http.StatusBadRequest {
		t.Errorf("empty payload = %d", code)
	}
	if len(e.h.queue) != 0 {
		t.Fatal("queued something")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhooks/github", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET = %d %v", rec.Code, rec.Header())
	}
}

func TestQueueFull(t *testing.T) {
	e := newEnv(t)
	for range queueSize {
		e.h.queue <- delivery{id: "filler"}
	}
	payload := fixture(t, "pull_request_merged.json")
	if code := e.deliver("busy", "pull_request", payload); code != http.StatusServiceUnavailable {
		t.Fatalf("full queue = %d", code)
	}
	for range queueSize {
		<-e.h.queue
	}
	// GitHub redelivers the 503: it is accepted and processed.
	e.send("busy", "pull_request", payload)
	if len(e.signals(domain.PRKey(repo, 7))) != 1 {
		t.Fatal("redelivery after 503 was not processed")
	}
}

func TestRunProcessesQueue(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.h.Run(ctx); close(done) }()
	sub := e.bus.Subscribe(ctx)
	if code := e.deliver("d1", "pull_request", fixture(t, "pull_request_merged.json")); code != http.StatusAccepted {
		t.Fatal(code)
	}
	select {
	case s := <-sub:
		if s.Kind != domain.SignalPRMerged {
			t.Fatalf("signal = %s", s.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal")
	}
	cancel()
	<-done
}

type failingGates struct{ gates.Service }

func (failingGates) Resolve(context.Context, domain.Actor, uuid.UUID, domain.GateStatus, string) (domain.Gate, error) {
	return domain.Gate{}, errors.New("boom")
}

type failingBus struct{ workflow.SignalBus }

func (failingBus) Publish(context.Context, domain.Signal) (bool, error) {
	return false, errors.New("boom")
}

// A human approves in the UI, the merge follows, then pr.merged arrives.
func TestMergeAfterGateAlreadyResolved(t *testing.T) {
	e := newEnv(t)
	e.addChange(7, "abc123")
	g := e.openGate(domain.GateMergeApproval, 7)
	must(t, e.resolveDirect(g))
	e.send("d1", "pull_request", fixture(t, "pull_request_merged.json"))
	got := e.gate(g)
	if got.Status != domain.GateApproved || got.ResolvedBy != "user:ana" {
		t.Fatalf("gate = %s by %s", got.Status, got.ResolvedBy)
	}
	if len(e.signals(domain.PRKey(repo, 7))) != 1 || e.change(7).State != domain.ChangeMerged {
		t.Fatal("merge not recorded")
	}
}

func TestPRClosedFailuresLeaveChangeOpen(t *testing.T) {
	ev := github.Event{Kind: "pull_request", Action: "closed", Repo: repo, Number: 7, Merged: true, SHA: "abc123"}
	for name, fail := range map[string]func(e *env){
		"gate":    func(e *env) { e.eng.Gates = failingGates{e.gates} },
		"publish": func(e *env) { e.eng.Signals = failingBus{e.bus} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.addChange(7, "abc123")
			g := e.openGate(domain.GateMergeApproval, 7)
			good := e.eng.Deps
			fail(e)
			if err := e.eng.handle(t.Context(), ev); err == nil {
				t.Fatal("no error")
			}
			if e.change(7).State != domain.ChangeOpen {
				t.Fatal("change closed despite the failure")
			}
			// Once the failure is gone the same event finishes the work.
			e.eng.Deps = good
			must(t, e.eng.handle(t.Context(), ev))
			if e.change(7).State != domain.ChangeMerged || e.gate(g).Status != domain.GateApproved {
				t.Fatal("retry did not finish")
			}
		})
	}
}

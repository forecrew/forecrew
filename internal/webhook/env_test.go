package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/testkit"
)

var (
	t0     = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	secret = []byte("s3cret")
)

const repo = "acme/app"

// env is a project in a store with fake signal bus, gates and tracker.
type env struct {
	t       *testing.T
	clock   *testkit.Clock
	st      *testkit.Store
	bus     *testkit.SignalBus
	gates   *testkit.Gates
	tracker *testkit.Tracker
	account uuid.UUID
	project domain.Project
	h       *Handler
	eng     *engine
	allowed map[string]bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	bus := testkit.NewSignalBus(st)
	e := &env{
		t: t, clock: clock, st: st, bus: bus,
		gates:   testkit.NewGates(st, bus, clock.Now),
		tracker: testkit.NewTracker(clock.Now),
		allowed: map[string]bool{"octocat": true},
	}
	e.tracker.Install(repo)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	e.account = acc.ID
	e.project = domain.Project{AccountID: acc.ID, Name: "app", Repo: "Acme/App", DefaultBranch: "main"}
	must(t, st.Projects().Create(t.Context(), &e.project))
	e.h = NewHandler(secret, e.deps())
	e.eng = e.h.e
	return e
}

func (e *env) deps() Deps {
	return Deps{
		Store: e.st, Signals: e.bus, Gates: e.gates, Tracker: e.tracker,
		Allowed: func(l string) bool { return e.allowed[l] },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)), Now: e.clock.Now,
	}
}

// openGate opens a pending gate about a PR or issue of the project.
func (e *env) openGate(kind domain.GateKind, number int) domain.Gate {
	e.t.Helper()
	g, err := e.gates.Open(e.t.Context(), gates.OpenRequest{
		AccountID: e.account, ProjectID: e.project.ID, Kind: kind, Title: "t", SubjectRepo: "Acme/App", SubjectNumber: number,
	})
	must(e.t, err)
	return g
}

// signals returns the stored signals with a key.
func (e *env) signals(key string) []domain.Signal {
	e.t.Helper()
	s, err := e.bus.Lookup(e.t.Context(), e.account, key)
	must(e.t, err)
	return s
}

// addChange stores an open PR as a change.
func (e *env) addChange(number int, sha string) domain.Change {
	e.t.Helper()
	c := domain.Change{AccountID: e.account, ProjectID: e.project.ID, Repo: repo, Number: number, HeadSHA: sha, State: domain.ChangeOpen}
	must(e.t, e.st.Changes().Upsert(e.t.Context(), &c))
	return c
}

func (e *env) change(number int) domain.Change {
	e.t.Helper()
	c, err := e.st.Changes().GetByPR(e.t.Context(), e.account, repo, number)
	must(e.t, err)
	return c
}

// deliver posts a signed delivery and returns the status code.
func (e *env) deliver(id, kind string, payload []byte) int {
	return e.deliverSigned(id, kind, payload, sign(payload))
}

func (e *env) deliverSigned(id, kind string, payload []byte, sig string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", kind)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code
}

// process handles every queued event.
func (e *env) process() {
	for {
		select {
		case d := <-e.h.queue:
			e.h.process(e.t.Context(), d)
		default:
			return
		}
	}
}

// send delivers an event and processes it.
func (e *env) send(id, kind string, payload []byte) {
	e.t.Helper()
	if code := e.deliver(id, kind, payload); code != http.StatusAccepted {
		e.t.Fatalf("deliver %s = %d", id, code)
	}
	e.process()
}

func sign(payload []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(payload)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) gate(g domain.Gate) domain.Gate {
	e.t.Helper()
	got, err := e.gates.Get(e.t.Context(), e.account, g.ID)
	must(e.t, err)
	return got
}

// resolveDirect approves a gate outside the webhook.
func (e *env) resolveDirect(g domain.Gate) error {
	_, err := e.gates.Resolve(e.t.Context(), domain.Actor{AccountID: e.account, Kind: domain.ActorUser, Name: "ana"}, g.ID, domain.GateApproved, "")
	return err
}

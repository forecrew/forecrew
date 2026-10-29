package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestClock(t *testing.T) {
	c := NewClock(t0.In(time.FixedZone("x", 3600)))
	if c.Now().Location() != time.UTC || !c.Now().Equal(t0) {
		t.Fatalf("Now = %v", c.Now())
	}
	c.Advance(time.Second)
	if !c.Now().Equal(t0.Add(time.Second)) {
		t.Fatalf("Now = %v", c.Now())
	}
	if orNow(nil)().Location() != time.UTC {
		t.Fatal("default clock is not UTC")
	}
}

func TestAdapter(t *testing.T) {
	a := &Adapter{
		AdapterName: "claude-code",
		Events:      []domain.JobEvent{{Type: domain.EventMessage, Text: "hi"}, {Type: domain.EventLog, Text: "done"}},
		Result:      runtime.Result{Output: json.RawMessage(`{"ok":true}`), Turns: 2},
	}
	if a.Name() != "claude-code" {
		t.Fatalf("Name = %s", a.Name())
	}
	_, err := a.Detect(t.Context())
	wantErr(t, err, runtime.ErrNotInstalled)
	a.Version = "2.0.0"
	if v, err := a.Detect(t.Context()); err != nil || v != "2.0.0" {
		t.Fatalf("Detect = %s, %v", v, err)
	}
	var got []string
	res, err := a.Run(t.Context(), runtime.RunSpec{Prompt: "p"}, func(e domain.JobEvent) { got = append(got, e.Text) })
	if err != nil || res.Turns != 2 || !slices.Equal(got, []string{"hi", "done"}) {
		t.Fatalf("Run = %+v, %v, events %v", res, err, got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = a.Run(ctx, runtime.RunSpec{}, func(domain.JobEvent) { t.Error("emitted after cancel") })
	wantErr(t, err, context.Canceled)
	if specs := a.Specs(); len(specs) != 2 || specs[0].Prompt != "p" {
		t.Fatalf("Specs = %+v", specs)
	}
}

func TestAnalyzer(t *testing.T) {
	a := &Analyzer{Facts: analyzer.Facts{Manifests: []string{"go.mod"}}}
	facts, err := a.Analyze(t.Context(), "/repo")
	if err != nil || facts.Manifests[0] != "go.mod" || a.Dirs()[0] != "/repo" {
		t.Fatalf("Analyze = %+v, %v", facts, err)
	}
}

func TestRoles(t *testing.T) {
	r := Roles{"reviewer": {Name: "reviewer", Runtime: "codex"}}
	role, err := r.Resolve(t.Context(), domain.NewID(), domain.NewID(), "reviewer")
	if err != nil || role.Runtime != "codex" || !role.ReadOnly() {
		t.Fatalf("Resolve = %+v, %v", role, err)
	}
	_, err = r.Resolve(t.Context(), domain.NewID(), domain.NewID(), "nobody")
	wantErr(t, err, roles.ErrUnknownRole)
}

func TestContextBuilder(t *testing.T) {
	goal := domain.NewID()
	b := &ContextBuilder{}
	pack, err := b.Build(t.Context(), workflow.JobRequest{Role: "developer", Repo: "acme/app", GoalID: goal, Feedback: "add tests"},
		roles.Role{Instructions: "You are a developer."})
	must(t, err)
	for _, want := range []string{"You are a developer.", "role: developer", "goal: " + goal.String(), "feedback: add tests"} {
		if !strings.Contains(pack.Prompt, want) {
			t.Errorf("prompt %q lacks %q", pack.Prompt, want)
		}
	}
	if strings.Contains(pack.Prompt, "task:") {
		t.Errorf("prompt mentions unset task: %q", pack.Prompt)
	}
	boom := errors.New("boom")
	b.Err = boom
	_, err = b.Build(t.Context(), workflow.JobRequest{}, roles.Role{})
	wantErr(t, err, boom)
}

func TestSignalBus(t *testing.T) {
	f := newFixture(t)
	bus := NewSignalBus(f.st)
	ctx, cancel := context.WithCancel(t.Context())
	sub := bus.Subscribe(ctx)
	job := domain.NewID()
	sig, err := domain.NewSignal(f.account, f.project.ID, &domain.JobCompleted{JobID: job, Status: domain.JobSucceeded}, t0)
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
	stored, err := bus.Lookup(t.Context(), f.account, domain.JobKey(job))
	if err != nil || len(stored) != 1 {
		t.Fatalf("Lookup = %v, %v", stored, err)
	}
	// A full subscriber does not block publishers.
	for i := range subscriberBuffer + 1 {
		s, _ := domain.NewSignal(f.account, f.project.ID, &domain.PRClosed{Repo: "acme/app", Number: i + 1}, t0)
		if _, err := bus.Publish(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	n := 0
	for range sub {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("drained %d signals, want %d", n, subscriberBuffer)
	}
}

func TestGates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	bus := NewSignalBus(f.st)
	svc := NewGates(f.st, bus, f.clock.Now)
	g, err := svc.Open(ctx, gates.OpenRequest{AccountID: f.account, ProjectID: f.project.ID, Kind: domain.GateNeedsYou, Title: "stuck"})
	if err != nil || g.Status != domain.GatePending || g.Number != 1 {
		t.Fatalf("Open = %+v, %v", g, err)
	}
	pending, _ := svc.ListPending(ctx, f.account, uuid.Nil())
	if len(pending) != 1 {
		t.Fatalf("pending = %v", pending)
	}
	by := domain.Actor{AccountID: f.account, Kind: domain.ActorMCP, Name: "laptop"}
	_, err = svc.Resolve(ctx, by, g.ID, domain.GateChangesRequested, "x")
	wantErr(t, err, domain.ErrInvalidDecision)
	got, err := svc.Resolve(ctx, by, g.ID, domain.GateApproved, "retry")
	if err != nil || got.Status != domain.GateApproved || got.ResolvedBy != "mcp:laptop" {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	_, err = svc.Resolve(ctx, by, g.ID, domain.GateRejected, "")
	wantErr(t, err, domain.ErrInvalidTransition)
	sigs, _ := bus.Lookup(ctx, f.account, domain.GateKey(g.ID))
	if len(sigs) != 1 {
		t.Fatalf("signals = %v", sigs)
	}
	p, err := sigs[0].Decode()
	if err != nil || p.(*domain.GateResolved).Feedback != "retry" {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	_, err = svc.Get(ctx, f.account, domain.NewID())
	wantErr(t, err, gates.ErrNotFound)
	_, err = svc.Resolve(ctx, by, domain.NewID(), domain.GateApproved, "")
	wantErr(t, err, gates.ErrNotFound)
	if entries, _ := f.st.Audit().List(ctx, f.account, store.AuditFilter{}); len(entries) != 2 {
		t.Fatalf("audit = %v", entries)
	}
}

func TestJobCreator(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	req := workflow.JobRequest{AccountID: f.account, ProjectID: f.project.ID, Repo: "acme/app", Role: "architect", Needs: []string{"go"}}

	plain := &JobCreator{}
	if id, err := plain.Create(ctx, req); err != nil || id == uuid.Nil() {
		t.Fatalf("Create = %v, %v", id, err)
	}
	stored := &JobCreator{Store: f.st}
	id, err := stored.Create(ctx, req)
	must(t, err)
	j, err := f.st.Jobs().Get(ctx, f.account, id)
	if err != nil || j.Role != "architect" || j.Spec.Repo != "acme/app" || j.Status != domain.JobQueued {
		t.Fatalf("job = %+v, %v", j, err)
	}
	boom := errors.New("boom")
	failing := &JobCreator{Err: boom}
	_, err = failing.Create(ctx, req)
	wantErr(t, err, boom)
	if len(failing.Requests()) != 1 || failing.Requests()[0].Role != "architect" {
		t.Fatalf("requests = %+v", failing.Requests())
	}
}

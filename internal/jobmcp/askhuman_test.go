package jobmcp_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/store"
)

var assumption = map[string]any{"statement": "Dates are shown in UTC.", "confidence": "medium", "impact": "low", "verify_by": "ask-human"}

func ask(timeout string) map[string]any {
	return map[string]any{"question": "Which time zone?\nThe spec does not say.", "timeout": timeout, "assumption": assumption}
}

// asked starts an ask_human call and returns its result channel and the
// question gate once it is open.
func (e *env) asked(t *testing.T, cs *sdk.ClientSession, args map[string]any) (<-chan map[string]any, domain.Gate) {
	t.Helper()
	done := make(chan map[string]any, 1)
	go func() {
		res, err := cs.CallTool(context.WithoutCancel(t.Context()), &sdk.CallToolParams{Name: "ask_human", Arguments: args})
		if err != nil {
			done <- map[string]any{"call_error": err.Error()}
			return
		}
		_, out := structured(res)
		done <- out
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gs, err := e.st.Gates().List(t.Context(), e.project.AccountID, store.GateFilter{Status: domain.GatePending})
		must(t, err)
		if len(gs) == 1 {
			return done, gs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no question gate opened")
	return nil, domain.Gate{}
}

func structured(res *sdk.CallToolResult) (*sdk.CallToolResult, map[string]any) {
	out := map[string]any{}
	if m, ok := res.StructuredContent.(map[string]any); ok {
		out = m
	}
	if res.IsError {
		out["is_error"] = true
	}
	return res, out
}

func wait(t *testing.T, done <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("ask_human did not return")
		return nil
	}
}

func TestAskHumanAnswered(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	done, g := e.asked(t, cs, ask("10m"))
	if g.Kind != domain.GateNeedsYou || !g.AsksHuman() || g.JobID != j.ID || g.TaskID != e.task.ID || g.SubjectNumber != 42 ||
		g.SubjectRepo != "acme/app" || g.Title != j.Ref()+" asks: Which time zone?" ||
		!strings.Contains(g.Summary, "<untrusted-data") || !strings.Contains(g.Summary, "The spec does not say.") {
		t.Fatalf("gate = %+v", g)
	}
	// One question at a time.
	res, out := call(t, cs, "ask_human", ask("1m"))
	if !res.IsError || errorCode(out) != jobmcp.CodeConflict {
		t.Fatalf("second question = %v", out)
	}
	by := domain.Actor{AccountID: j.AccountID, Kind: domain.ActorUser, Name: "ana"}
	_, err := e.gates.Resolve(t.Context(), by, g.ID, domain.GateApproved, "Europe/Rome")
	must(t, err)
	out = wait(t, done)
	if out["status"] != "answered" || out["answer"] != "Europe/Rome" || out["gate"] != g.Ref() || out["assumption"] != nil {
		t.Fatalf("answer = %v", out)
	}
	if e.timeouts[0] != 10*time.Minute || len(e.records) != 0 {
		t.Fatalf("timeouts %v, records %v", e.timeouts, e.records)
	}
}

func TestAskHumanNoAnswer(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	done, g := e.asked(t, cs, ask("90s"))
	e.fire <- t0 // the fake timer: the timeout passes with nobody answering
	out := wait(t, done)
	if out["status"] != "no_answer" || out["gate"] != g.Ref() || out["assumption"] != "A-1" || out["answer"] != nil {
		t.Fatalf("no answer = %v", out)
	}
	got, err := e.st.Gates().Get(t.Context(), g.AccountID, g.ID)
	if err != nil || got.Status != domain.GateRejected || got.ResolvedBy != "system:forecrewd" || !strings.Contains(got.Feedback, "1m30s") {
		t.Fatalf("gate = %+v, %v", got, err)
	}
	if len(e.records) != 1 || e.records[0].Statement != "Dates are shown in UTC." {
		t.Fatalf("records = %+v", e.records)
	}
	events, _ := e.st.JobEvents().List(t.Context(), j.AccountID, j.ID, 0, 0)
	if len(events) != 1 || !strings.Contains(events[0].Text, "going on with the assumption: Dates are shown in UTC.") {
		t.Fatalf("events = %+v", events)
	}

	// A human declining is no answer too, and a new question may follow.
	done, g = e.asked(t, cs, ask("5m"))
	by := domain.Actor{AccountID: j.AccountID, Kind: domain.ActorUser, Name: "ana"}
	_, err = e.gates.Resolve(t.Context(), by, g.ID, domain.GateRejected, "not now")
	must(t, err)
	if out := wait(t, done); out["status"] != "no_answer" || out["assumption"] != "A-1" {
		t.Fatalf("declined = %v", out)
	}
}

func TestAskHumanWithoutRegister(t *testing.T) {
	e := newEnv(t, func(c *jobmcp.Config) { c.Assumptions = nil; c.Bus = nil })
	_, tok := e.lease(t, e.w, allTools...)
	done, _ := e.asked(t, e.connect(t, tok, nil), ask("1m"))
	e.fire <- t0
	if out := wait(t, done); out["status"] != "no_answer" || out["assumption"] != nil {
		t.Fatalf("no answer = %v", out)
	}
}

func TestAskHumanInvalidTimeout(t *testing.T) {
	e := newEnv(t)
	_, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	for _, timeout := range []string{"31m", "0s", "2h"} {
		res, out := call(t, cs, "ask_human", ask(timeout))
		if !res.IsError || errorCode(out) != jobmcp.CodeInvalidInput {
			t.Errorf("timeout %s = %v", timeout, out)
		}
	}
	if gs, _ := e.st.Gates().List(t.Context(), e.project.AccountID, store.GateFilter{}); len(gs) != 0 {
		t.Fatalf("gates = %+v", gs)
	}
}

// TestAskHumanLeaseEnds: when the job stops running the wait ends and the
// question is closed.
func TestAskHumanLeaseEnds(t *testing.T) {
	e := newEnv(t)
	j, tok := e.lease(t, e.w, allTools...)
	done, g := e.asked(t, e.connect(t, tok, nil), ask("10m"))
	_, err := e.st.Jobs().Cancel(t.Context(), j.AccountID, j.ID)
	must(t, err)
	if out := wait(t, done); out["is_error"] != true || errorCode(out) != jobmcp.CodeUnavailable {
		t.Fatalf("result = %v", out)
	}
	got, _ := e.st.Gates().Get(t.Context(), g.AccountID, g.ID)
	if got.Status != domain.GateRejected || !strings.Contains(got.Feedback, "stopped") {
		t.Fatalf("gate = %+v", got)
	}
}

func TestAskHumanProgress(t *testing.T) {
	e := newEnv(t, func(c *jobmcp.Config) { c.ProgressEvery = 5 * time.Millisecond })
	_, tok := e.lease(t, e.w, allTools...)
	var notes atomic.Int32
	cs := e.connect(t, tok, &sdk.ClientOptions{ProgressNotificationHandler: func(context.Context, *sdk.ProgressNotificationClientRequest) {
		notes.Add(1)
	}})
	done := make(chan error, 1)
	go func() {
		params := &sdk.CallToolParams{Name: "ask_human", Arguments: ask("1m")}
		params.SetProgressToken("p1")
		_, err := cs.CallTool(context.WithoutCancel(t.Context()), params)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for notes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if notes.Load() < 2 {
		t.Fatal("no progress notifications")
	}
	e.fire <- t0
	must(t, <-done)
}

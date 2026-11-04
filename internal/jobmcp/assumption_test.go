package jobmcp_test

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/registers"
	"github.com/forecrew/forecrew/internal/store"
)

func TestAssumptionRecord(t *testing.T) {
	var regs *registers.Service
	e := newEnv(t, func(c *jobmcp.Config) {
		regs = registers.New(c.Store, nil, c.Now)
		c.Assumptions = regs
	})
	ctx := t.Context()
	j, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	args := map[string]any{"statement": "The queue never holds more than 1000 jobs", "confidence": "medium", "impact": "medium", "verify_by": "test"}

	res, out := call(t, cs, "assumption_record", args)
	if res.IsError || out["assumption"] != "A-1" || out["duplicate"] != false || out["status"] != "unverified" {
		t.Fatalf("first call: %+v %v", res, out)
	}
	args["statement"] = "the queue never holds more than 1000 jobs."
	args["impact"] = "high"
	_, out = call(t, cs, "assumption_record", args)
	if out["assumption"] != "A-1" || out["duplicate"] != true {
		t.Fatalf("duplicate call: %v", out)
	}
	as, err := e.st.Assumptions().List(ctx, j.AccountID, store.AssumptionFilter{})
	must(t, err)
	if len(as) != 1 || as[0].Impact != domain.LevelHigh || as[0].JobID != j.ID || as[0].MadeBy != "developer" || as[0].GoalID != j.GoalID ||
		len(as[0].Dependents) != 2 || as[0].Dependents[1].ID != e.task.Ref() {
		t.Fatalf("assumptions = %+v", as)
	}
	events, err := e.st.JobEvents().List(ctx, j.AccountID, j.ID, 0, 10)
	must(t, err)
	if len(events) != 2 || events[0].Type != domain.EventLog {
		t.Fatalf("events = %+v", events)
	}

	// Stating the item again at a higher impact raised it and opened its
	// review gate on the task's issue; so does a new high-impact one.
	gs, err := e.st.Gates().List(ctx, j.AccountID, store.GateFilter{Kind: domain.GateAssumptionReview})
	if err != nil || len(gs) != 1 || gs[0].SubjectNumber != e.task.IssueNumber {
		t.Fatalf("gates = %+v, %v", gs, err)
	}
	args["statement"] = "Clocks are in sync"
	call(t, cs, "assumption_record", args)
	gs, err = e.st.Gates().List(ctx, j.AccountID, store.GateFilter{Kind: domain.GateAssumptionReview})
	if err != nil || len(gs) != 2 || gs[1].SubjectNumber != e.task.IssueNumber {
		t.Fatalf("gates = %+v, %v", gs, err)
	}

	// The schema is checked by the gateway.
	args["impact"] = "huge"
	if res, out := call(t, cs, "assumption_record", args); !res.IsError || errorCode(out) != "invalid_input" {
		t.Fatalf("bad impact: %+v %v", res, out)
	}
}

func TestAssumptionRecordWithoutARegister(t *testing.T) {
	e := newEnv(t, func(c *jobmcp.Config) { c.Assumptions = nil })
	_, tok := e.lease(t, e.w, allTools...)
	cs := e.connect(t, tok, nil)
	res, out := call(t, cs, "assumption_record", map[string]any{"statement": "x", "confidence": "low", "impact": "low", "verify_by": "test"})
	if !res.IsError || errorCode(out) != "unavailable" {
		t.Fatalf("result = %+v %v", res, out)
	}
}

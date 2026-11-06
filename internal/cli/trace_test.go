package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestTraceCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.GetTraceFunc = func(context.Context, domain.Actor, mcp.GetTraceInput) (mcp.Trace, error) {
		return mcp.Trace{Focus: "TASK-5", Goal: "GOAL-3",
			Nodes: []mcp.TraceNode{{ID: "GOAL-3", Kind: mcp.TraceGoal, Status: "in_progress", Title: "Add\x1b[31m search"}, {ID: "acme/app#12", Kind: mcp.TraceChange}},
			Edges: []mcp.TraceEdge{{From: "GOAL-3", To: "acme/app#12", Label: "built"}}}, nil
	}
	out, errOut, code := e.run(t, "trace", "acme/app#12")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"in_progress  Add [31m search", "acme/app#12  change", "GOAL-3 -> acme/app#12 (built)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if calls := e.svc.Calls(); len(calls) != 1 || calls[0].Tool != mcp.ToolGetTrace || calls[0].Input != (mcp.GetTraceInput{Ref: "acme/app#12"}) {
		t.Errorf("calls = %+v", calls)
	}
}

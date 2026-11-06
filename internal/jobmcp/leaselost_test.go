package jobmcp_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ending lets a test finish a job in the middle of a tool call: the hook the
// test plugs into a collaborator of the tool calls it once, after the call
// was authenticated.
type ending struct {
	e   *env
	job atomic.Pointer[domain.Job]
	arm atomic.Bool
	one sync.Once
}

func (n *ending) fire(t *testing.T) {
	t.Helper()
	if !n.arm.Load() {
		return
	}
	n.one.Do(func() {
		j := n.job.Load()
		_, err := n.e.st.Jobs().Complete(context.WithoutCancel(t.Context()), j.AccountID, j.ID, j.WorkerID, store.JobResult{Status: domain.JobSucceeded})
		must(t, err)
	})
}

// start leases a job with the tools and returns a session on it; calls made
// after it finish the job at the hook.
func (n *ending) start(t *testing.T, tools ...string) (domain.Job, *sdk.ClientSession) {
	t.Helper()
	j, tok := n.e.leaseGoal(t, n.e.task.GoalID, tools...)
	n.job.Store(&j)
	cs := n.e.connect(t, tok, nil)
	n.arm.Store(true)
	return j, cs
}

// gatesEnding finishes the job as a gate is opened for it.
type gatesEnding struct {
	gates.Service
	t *testing.T
	n *ending
}

func (g gatesEnding) Open(ctx context.Context, req gates.OpenRequest) (domain.Gate, error) {
	g.n.fire(g.t)
	return g.Service.Open(ctx, req)
}

// lostRecorder is an assumption register that finds the lease ended.
type lostRecorder struct{}

func (lostRecorder) RecordAssumption(context.Context, domain.Job, domain.StatedAssumption) (domain.Assumption, bool, error) {
	return domain.Assumption{}, false, store.ErrLeaseLost
}

func wantLeaseEnded(t *testing.T, tool string, res *sdk.CallToolResult, out map[string]any) {
	t.Helper()
	if !res.IsError || errorCode(out) != jobmcp.CodeUnavailable {
		t.Fatalf("%s of a finished job = %+v / %v, want %s", tool, res, out, jobmcp.CodeUnavailable)
	}
}

func TestToolsRefuseAJobWhoseLeaseEndedInTheCall(t *testing.T) {
	policy, err := knowledge.ParsePolicy([]byte(gatewayPolicy))
	must(t, err)

	t.Run("assumption_record", func(t *testing.T) {
		e := newEnv(t, func(c *jobmcp.Config) { c.Assumptions = lostRecorder{} })
		_, tok := e.lease(t, e.w, allTools...)
		res, out := call(t, e.connect(t, tok, nil), "assumption_record", map[string]any{
			"statement": "s", "confidence": "low", "impact": "low", "verify_by": "test"})
		wantLeaseEnded(t, "assumption_record", res, out)
	})

	t.Run("ask_human", func(t *testing.T) {
		n := &ending{}
		e := newEnv(t, func(c *jobmcp.Config) { c.Gates = gatesEnding{Service: c.Gates, t: t, n: n} })
		n.e = e
		_, cs := n.start(t, allTools...)
		res, out := call(t, cs, "ask_human", map[string]any{"question": "Which?", "timeout": "1m", "assumption": map[string]any{
			"statement": "s", "confidence": "low", "impact": "low", "verify_by": "test"}})
		wantLeaseEnded(t, "ask_human", res, out)
		if gs, err := e.st.Gates().List(t.Context(), e.project.AccountID, store.GateFilter{}); err != nil || len(gs) != 0 {
			t.Errorf("gates = %v, %v", gs, err)
		}
	})

	t.Run("kb_propose", func(t *testing.T) {
		n := &ending{}
		e := newEnv(t, func(c *jobmcp.Config) {
			c.Proposals = &proposals.Service{Store: c.Store, Gates: c.Gates, Policy: func(context.Context, domain.Project) (knowledge.Policy, error) {
				n.fire(t)
				return policy, nil
			}}
		})
		n.e = e
		_, cs := n.start(t, proposalTools...)
		res, out := call(t, cs, "kb_propose", map[string]any{"section": "architecture", "path": "docs/adr/2.md", "action": "add", "body": "# ADR\n", "reason": "r"})
		wantLeaseEnded(t, "kb_propose", res, out)
		if ks, err := e.st.KnowledgeProposals().List(t.Context(), e.project.AccountID, store.KnowledgeProposalFilter{}); err != nil || len(ks) != 0 {
			t.Errorf("proposals = %v, %v", ks, err)
		}
	})

	t.Run("request_access", func(t *testing.T) {
		n := &ending{}
		e := newEnv(t, func(c *jobmcp.Config) {
			c.Proposals = &proposals.Service{Store: c.Store, Gates: c.Gates, Policy: func(context.Context, domain.Project) (knowledge.Policy, error) {
				n.fire(t)
				return policy, nil
			}}
		})
		n.e = e
		_, cs := n.start(t, proposalTools...)
		res, out := call(t, cs, "request_access", map[string]any{"section": "closed", "level": "propose", "reason": "I know this code."})
		wantLeaseEnded(t, "request_access", res, out)
		if gs, err := e.st.Gates().List(t.Context(), e.project.AccountID, store.GateFilter{}); err != nil || len(gs) != 0 {
			t.Errorf("gates = %v, %v", gs, err)
		}
	})

	t.Run("evidence_attach", func(t *testing.T) {
		n := &ending{}
		e := newEnv(t, func(c *jobmcp.Config) {
			now := c.Now
			c.Now = func() time.Time { // read for the event the call appends
				n.fire(t)
				return now()
			}
		})
		n.e = e
		_, cs := n.start(t, allTools...)
		res, out := call(t, cs, "evidence_attach", map[string]any{"path": "shots/home.png"})
		wantLeaseEnded(t, "evidence_attach", res, out)
	})
}

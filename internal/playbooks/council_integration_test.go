//go:build integration

package playbooks_test

import (
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

// TestCouncilOnPostgres runs the protocol on the real store, restarting the
// engine in the debate, with a constraint a human adds meanwhile.
func TestCouncilOnPostgres(t *testing.T) {
	patience = time.Minute
	t.Cleanup(func() { patience = 5 * time.Second })
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(t.Context(), pool))
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(t.Context())
	must(t, err)
	clock := testkit.NewClock(time.Now())
	p := domain.Project{AccountID: accounts[0].ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(t.Context(), &p))
	bus := testkit.NewSignalBus(st)
	v := &env{clock: clock, st: st, bus: bus, gates: testkit.NewGates(st, bus, clock.Now), tracker: testkit.NewTracker(clock.Now),
		actions: workflow.Actions{}, account: accounts[0].ID, project: p}
	v.tracker.Install("acme/app")
	e := councilEnvOn(t, v)
	must(t, e.tracker.SetFile("acme/app", councilCommit, "lease.go", []byte("package queue")))

	c := e.start(t, three)
	if c.RunID != c.ID {
		t.Fatalf("council = %+v", c)
	}
	e.play(t, "the critiques", func() bool { return len(e.contributions(t, c.ID, domain.StageCritique)) == 3 })
	e.stop()
	cur := e.council(t, c.ID)
	cur.Constraints = append(cur.Constraints, "Stay on one server")
	must(t, e.st.Councils().Update(t.Context(), &cur))
	e.run(t)

	g := e.gate(t)
	e.decide(t, g, mcp.ToolRequestChanges, "Say what it costs")
	e.play(t, "the second brief", func() bool { return len(e.contributions(t, c.ID, domain.StageSynthesis)) == 2 })
	second := e.gate2(t, g)
	e.decide(t, second, mcp.ToolApprove, "")
	got := e.waitCouncil(t, c.ID, domain.CouncilDecided)
	if len(got.Constraints) != 2 || got.GateID != second.ID || got.Brief == nil {
		t.Fatalf("council = %+v", got)
	}
	for _, s := range []domain.CouncilStage{domain.StageProposal, domain.StageCritique} {
		if n := len(e.contributions(t, c.ID, s)); n != 3 {
			t.Errorf("%s: %d contributions", s, n)
		}
	}
	if n := len(e.contributions(t, c.ID, domain.StageRevision)); n != 6 {
		t.Errorf("revisions = %d", n)
	}
	if k := e.contributions(t, c.ID, domain.StageProposal)[0]; len(k.EvidenceCheck) != 1 || k.EvidenceCheck[0].Status != domain.SourceOK {
		t.Errorf("evidence check = %+v", k.EvidenceCheck)
	}
	if d := e.decisions(t); len(d) != 2 || d[1].CouncilID != c.ID || d[1].Choice != "pg" || e.signal(t, c.ID).DecisionID != d[1].ID {
		t.Fatalf("decisions = %+v", d)
	}
}

// gate2 plays until a gate other than first is pending.
func (e *councilEnv) gate2(t *testing.T, first domain.Gate) domain.Gate {
	t.Helper()
	var g domain.Gate
	e.play(t, "the second gate", func() bool {
		gs, err := e.gates.ListPending(t.Context(), e.account, e.project.ID)
		must(t, err)
		for _, x := range gs {
			if x.ID != first.ID {
				g = x
				return true
			}
		}
		return false
	})
	return g
}

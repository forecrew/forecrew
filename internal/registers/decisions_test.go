package registers_test

import (
	"slices"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

func (w *world) gate(kind domain.GateKind, title string, task domain.Task) domain.Gate {
	w.t.Helper()
	g := domain.Gate{AccountID: w.account, ProjectID: w.project.ID, Kind: kind, Title: title, SubjectRepo: "acme/app", GoalID: w.goal.ID, TaskID: task.ID}
	must(w.t, w.st.Gates().Create(w.t.Context(), &g))
	return g
}

func (w *world) decisions() []domain.Decision {
	w.t.Helper()
	ds, err := w.st.Decisions().List(w.t.Context(), w.account, store.DecisionFilter{})
	must(w.t, err)
	return ds
}

func TestHumanGateDecisionsAreRecorded(t *testing.T) {
	for _, tt := range []struct {
		kind     domain.GateKind
		decision domain.GateStatus
		by       domain.ActorKind
		recorded bool
	}{
		{domain.GateSpecApproval, domain.GateChangesRequested, domain.ActorUser, true},
		{domain.GateMergeApproval, domain.GateApproved, domain.ActorGitHub, true},
		{domain.GateCouncilDecision, domain.GateRejected, domain.ActorMCP, true},
		{domain.GateSpecApproval, domain.GateApproved, domain.ActorSystem, false},
		{domain.GateSpecApproval, domain.GateApproved, domain.ActorWorker, false},
		{domain.GateNeedsYou, domain.GateApproved, domain.ActorUser, false},
		{domain.GateProfileApproval, domain.GateApproved, domain.ActorUser, false},
	} {
		t.Run(string(tt.kind)+"/"+string(tt.decision)+"/"+string(tt.by), func(t *testing.T) {
			w := newWorld(t)
			g := w.gate(tt.kind, "Approve the spec (revision 2)", domain.Task{})
			by := domain.Actor{AccountID: w.account, Kind: tt.by, Name: "gian"}
			_, err := w.gates.Resolve(t.Context(), by, g.ID, tt.decision, "because")
			must(t, err)
			ds := w.decisions()
			if !tt.recorded {
				if len(ds) != 0 {
					t.Fatalf("decisions = %+v", ds)
				}
				return
			}
			if len(ds) != 1 {
				t.Fatalf("decisions = %+v", ds)
			}
			d := ds[0]
			if d.Question != g.Title || d.Choice != string(tt.decision) || d.Reasons != "because" || d.DecidedBy != by.String() ||
				d.GateID != g.ID || d.GoalID != w.goal.ID || d.Ref() != "DEC-1" {
				t.Fatalf("decision = %+v", d)
			}
			if got := w.auditActions(d.ID); !slices.Equal(got, []string{"decision.recorded"}) {
				t.Fatalf("audit = %v", got)
			}
		})
	}
}

func TestMergeDecisionRestsOnTheTasksAssumptions(t *testing.T) {
	w := newWorld(t)
	a := w.recordHigh(w.tasks[0])
	g := w.gate(domain.GateMergeApproval, "Merge TASK-1", w.tasks[0])
	_, err := w.gates.Resolve(t.Context(), w.actor(), g.ID, domain.GateApproved, "")
	must(t, err)
	ds := w.decisions()
	if len(ds) != 1 || !slices.Equal(ds[0].Assumptions, []uuid.UUID{a.ID}) {
		t.Fatalf("decisions = %+v", ds)
	}
}

func reviewGate(t *testing.T, w *world) (domain.Assumption, domain.Gate) {
	t.Helper()
	a := w.recordHigh(w.tasks[0])
	gs := w.gateList(domain.GateAssumptionReview, domain.GatePending)
	if len(gs) != 1 {
		t.Fatalf("gates = %+v", gs)
	}
	return a, gs[0]
}

func TestAssumptionReviewApproveAccepts(t *testing.T) {
	w := newWorld(t)
	a, g := reviewGate(t, w)
	_, err := w.gates.Resolve(t.Context(), w.actor(), g.ID, domain.GateApproved, "I take the risk")
	must(t, err)
	got, _ := w.st.Assumptions().Get(t.Context(), w.account, a.ID)
	if got.Status != domain.AssumptionAccepted || got.ResolvedBy != "user:gian" || got.Evidence != "I take the risk" {
		t.Fatalf("assumption = %+v", got)
	}
	ds := w.decisions()
	if len(ds) != 1 || ds[0].Choice != "approved" || !slices.Equal(ds[0].Assumptions, []uuid.UUID{a.ID}) {
		t.Fatalf("decisions = %+v", ds)
	}
}

func TestAssumptionReviewRejectInvalidates(t *testing.T) {
	w := newWorld(t)
	a, g := reviewGate(t, w)
	_, err := w.gates.Resolve(t.Context(), w.actor(), g.ID, domain.GateRejected, "")
	must(t, err)
	got, _ := w.st.Assumptions().Get(t.Context(), w.account, a.ID)
	if got.Status != domain.AssumptionInvalidated || got.Evidence != "rejected at "+g.Ref()+" without a reason" {
		t.Fatalf("assumption = %+v", got)
	}
}

func TestAssumptionReviewChangesRequestedLeavesItOpen(t *testing.T) {
	w := newWorld(t)
	a, g := reviewGate(t, w)
	_, err := w.gates.Resolve(t.Context(), w.actor(), g.ID, domain.GateChangesRequested, "check the load balancer")
	must(t, err)
	got, _ := w.st.Assumptions().Get(t.Context(), w.account, a.ID)
	if got.Status != domain.AssumptionUnverified {
		t.Fatalf("assumption = %+v", got)
	}
	if ds := w.decisions(); len(ds) != 1 || ds[0].Choice != "changes_requested" || ds[0].Reasons != "check the load balancer" {
		t.Fatalf("decisions = %+v", ds)
	}
}

// A review gate whose title names no assumption is only a decision.
func TestAssumptionReviewWithoutAnAssumption(t *testing.T) {
	w := newWorld(t)
	for _, title := range []string{"Review A-77: gone", "Review something", "Look at A-1"} {
		g := w.gate(domain.GateAssumptionReview, title, domain.Task{})
		_, err := w.gates.Resolve(t.Context(), w.actor(), g.ID, domain.GateApproved, "")
		must(t, err)
	}
	if ds := w.decisions(); len(ds) != 3 || len(ds[0].Assumptions) != 0 {
		t.Fatalf("decisions = %+v", ds)
	}
}

func TestAcceptRecommendation(t *testing.T) {
	w := newWorld(t)
	gate := w.gate(domain.GateCouncilDecision, "Pick a queue", domain.Task{})
	c := domain.Council{ID: domain.NewID(), AccountID: w.account, ProjectID: w.project.ID, Number: 2, GoalID: w.goal.ID,
		Kind: domain.CouncilTechnical, Question: "Which queue?", Status: domain.CouncilDecided, GateID: gate.ID,
		Brief: &domain.CouncilBrief{
			Question: "Which queue?", Recommendation: "postgres",
			Options: []domain.BriefOption{{Name: "postgres", Summary: "SKIP LOCKED"}, {Name: "nats", Summary: "new service"}},
			Votes: []domain.BriefVote{{Member: "architect@claude-code", Option: "postgres", Reason: "one dependency less"},
				{Member: "devils-advocate@codex", Option: "nats", Reason: "throughput"}, {Member: "ux@codex", Option: "postgres", Reason: "simple"}},
			Dissent:     []domain.BriefDissent{{Member: "devils-advocate@codex", Position: "it will not scale"}},
			Assumptions: []domain.StatedAssumption{stated("Under 100 jobs a second", domain.LevelMedium)},
		}}
	d, err := w.regs.AcceptRecommendation(t.Context(), c)
	must(t, err)
	as := w.assumptions()
	if len(as) != 1 || as[0].CouncilID != c.ID || as[0].MadeBy != "council COUNCIL-2" {
		t.Fatalf("assumptions = %+v", as)
	}
	if d.DecidedBy != domain.DecidedByPolicy || d.Choice != "postgres" || d.CouncilID != c.ID || d.GateID != gate.ID || d.GoalID != w.goal.ID ||
		d.Reasons != "architect@claude-code: one dependency less\nux@codex: simple" || d.Dissent != "devils-advocate@codex: it will not scale" ||
		len(d.Options) != 2 || !slices.Equal(d.Assumptions, []uuid.UUID{as[0].ID}) {
		t.Fatalf("decision = %+v", d)
	}
	if got := w.auditActions(d.ID); !slices.Equal(got, []string{"decision.recorded"}) {
		t.Fatalf("audit = %v", got)
	}
	// A repeated call finds the decision and the assumption.
	again, err := w.regs.AcceptRecommendation(t.Context(), c)
	if err != nil || again.ID != d.ID || len(w.assumptions()) != 1 || len(w.decisions()) != 1 {
		t.Fatalf("second accept: %+v, %v, %d assumptions, %d decisions", again, err, len(w.assumptions()), len(w.decisions()))
	}
	c.Brief = nil
	if _, err := w.regs.AcceptRecommendation(t.Context(), c); err == nil {
		t.Fatal("a council without a brief was accepted")
	}
	c.ID, c.Brief = domain.NewID(), &domain.CouncilBrief{Question: "", Recommendation: "x"}
	if _, err := w.regs.AcceptRecommendation(t.Context(), c); err == nil {
		t.Fatal("an invalid decision was recorded")
	}
}

// councilGate stores a council whose gate is pending.
func (w *world) councilGate() (domain.Council, domain.Gate) {
	w.t.Helper()
	g := w.gate(domain.GateCouncilDecision, "COUNCIL-1: Which queue?", domain.Task{})
	c := domain.Council{AccountID: w.account, ProjectID: w.project.ID, GoalID: w.goal.ID, Kind: domain.CouncilTechnical, Question: "Which queue?", GateID: g.ID,
		Brief: &domain.CouncilBrief{
			Question: "Which store?", Recommendation: "postgres", Options: []domain.BriefOption{{Name: "postgres", Summary: "SKIP LOCKED"}},
			Votes:       []domain.BriefVote{{Member: "architect@codex", Option: "postgres", Reason: "simple"}},
			Assumptions: []domain.StatedAssumption{stated("Under 100 jobs a second", domain.LevelMedium)},
		}}
	must(w.t, w.st.Councils().Create(w.t.Context(), &c))
	return c, g
}

// TestCouncilGateDecision: the decision on a council's gate names the council,
// and an approval is the decision for its recommendation.
func TestCouncilGateDecision(t *testing.T) {
	by := func(w *world) domain.Actor {
		return domain.Actor{AccountID: w.account, Kind: domain.ActorUser, Name: "gian"}
	}
	t.Run("approved", func(t *testing.T) {
		w := newWorld(t)
		c, g := w.councilGate()
		_, err := w.gates.Resolve(t.Context(), by(w), g.ID, domain.GateApproved, "go")
		must(t, err)
		ds := w.decisions()
		if len(ds) != 1 {
			t.Fatalf("decisions = %+v", ds)
		}
		d := ds[0]
		if d.CouncilID != c.ID || d.GateID != g.ID || d.Choice != "postgres" || d.Question != "Which store?" || d.DecidedBy != "user:gian" ||
			d.Reasons != "go\narchitect@codex: simple" || len(d.Options) != 1 || len(d.Assumptions) != 1 {
			t.Fatalf("decision = %+v", d)
		}
	})
	for _, status := range []domain.GateStatus{domain.GateChangesRequested, domain.GateRejected} {
		t.Run(string(status), func(t *testing.T) {
			w := newWorld(t)
			c, g := w.councilGate()
			_, err := w.gates.Resolve(t.Context(), by(w), g.ID, status, "no")
			must(t, err)
			ds := w.decisions()
			if len(ds) != 1 || ds[0].CouncilID != c.ID || ds[0].Choice != string(status) || ds[0].Reasons != "no" || len(w.assumptions()) != 0 {
				t.Fatalf("decisions = %+v", ds)
			}
		})
	}
}

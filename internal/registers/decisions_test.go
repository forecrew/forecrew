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
	// The same brief again finds the same assumption.
	if _, err := w.regs.AcceptRecommendation(t.Context(), c); err != nil || len(w.assumptions()) != 1 {
		t.Fatalf("second accept: %v, %d assumptions", err, len(w.assumptions()))
	}
	c.Brief = nil
	if _, err := w.regs.AcceptRecommendation(t.Context(), c); err == nil {
		t.Fatal("a council without a brief was accepted")
	}
	c.Brief = &domain.CouncilBrief{Question: "", Recommendation: "x"}
	if _, err := w.regs.AcceptRecommendation(t.Context(), c); err == nil {
		t.Fatal("an invalid decision was recorded")
	}
}

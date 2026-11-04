package tools_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/councils"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/tools"
)

func TestStartCouncil(t *testing.T) {
	f := newFixture(t)
	c, err := f.svc.StartCouncil(t.Context(), f.by, mcp.StartCouncilInput{Project: "app", Question: "  Which queue? ", Roles: []string{"architect", "ux"}, Constraints: []string{" Keep Postgres "}})
	must(t, err)
	if c.Ref != "COUNCIL-1" || c.Kind != domain.CouncilAdhoc || c.Question != "Which queue?" || c.Status != domain.CouncilRunning || c.RunID != c.ID || c.Constraints[0] != "Keep Postgres" {
		t.Fatalf("council = %+v", c)
	}
	starts := f.playbooks.Starts()
	if len(starts) != 1 || starts[0].Playbook != councils.Playbook || starts[0].Request.Inputs["council_id"] != c.ID.String() {
		t.Fatalf("starts = %+v", starts)
	}
	audit, err := f.st.Audit().List(t.Context(), f.account.ID, store.AuditFilter{})
	must(t, err)
	if len(audit) != 1 || audit[0].Action != "council.started" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestStartCouncilRefused(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name string
		in   mcp.StartCouncilInput
		want error
	}{
		{"unknown project", mcp.StartCouncilInput{Project: "nope", Question: "q"}, mcp.ErrNotFound},
		{"no question", mcp.StartCouncilInput{Project: "app", Question: " "}, mcp.ErrInvalidInput},
		{"unknown kind", mcp.StartCouncilInput{Project: "app", Question: "q", Kind: "debate"}, mcp.ErrInvalidInput},
		{"unknown role", mcp.StartCouncilInput{Project: "app", Question: "q", Roles: []string{"ghost"}}, mcp.ErrInvalidInput},
		{"empty constraint", mcp.StartCouncilInput{Project: "app", Question: "q", Constraints: []string{" "}}, mcp.ErrInvalidInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.StartCouncil(t.Context(), f.by, tt.in)
			wantErr(t, err, tt.want)
		})
	}
	if n := len(f.playbooks.Starts()); n != 0 {
		t.Fatalf("starts = %d", n)
	}
}

func TestStartCouncilThatCannotRun(t *testing.T) {
	f := newFixture(t)
	f.playbooks.Err = errors.New("engine down")
	if _, err := f.svc.StartCouncil(t.Context(), f.by, mcp.StartCouncilInput{Project: "app", Question: "q"}); !errors.Is(err, f.playbooks.Err) {
		t.Fatalf("err = %v", err)
	}
	list, err := f.st.Councils().List(t.Context(), f.account.ID, store.CouncilFilter{})
	must(t, err)
	if len(list) != 1 || list[0].Status != domain.CouncilFailed {
		t.Fatalf("councils = %+v", list)
	}
}

func TestGetCouncil(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.CouncilTechnical, Question: "Which queue?", Brief: &domain.CouncilBrief{Recommendation: "pg"}}
	must(t, f.st.Councils().Create(ctx, &c))
	for _, k := range []domain.CouncilContribution{
		{Role: "facilitator", Runtime: "codex", Stage: domain.StageSynthesis, Round: 1},
		{Role: "ux", Runtime: "codex", Stage: domain.StageRevision, Round: 2},
		{Role: "ux", Runtime: "codex", Stage: domain.StageRevision, Round: 1},
		{Role: "ux", Runtime: "codex", Stage: domain.StageProposal, Round: 1},
	} {
		k.AccountID, k.CouncilID, k.Content = f.account.ID, c.ID, []byte(`{}`)
		must(t, f.st.Councils().AddContribution(ctx, &k))
	}
	d, err := f.svc.GetCouncil(ctx, f.by, mcp.CouncilRefInput{Council: c.Ref()})
	must(t, err)
	var order []string
	for _, k := range d.Contributions {
		order = append(order, string(k.Stage))
	}
	if !strings.Contains(d.Untrusted, `<untrusted-data source="ux@codex, proposal round 1"`) || !strings.Contains(d.Untrusted, `<untrusted-data source="the brief of `+c.Ref()+`"`) {
		t.Fatalf("untrusted text = %s", d.Untrusted)
	}
	if strings.Join(order, " ") != "proposal revision revision synthesis" || d.Contributions[1].Round != 1 || d.Contributions[0].Member != "ux@codex" {
		t.Fatalf("contributions = %+v", d.Contributions)
	}
	if d.Gate != nil || d.Decision != nil || d.Brief == nil {
		t.Fatalf("detail = %+v", d)
	}

	g := domain.Gate{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.GateCouncilDecision, Title: "t", SubjectRepo: "acme/app"}
	must(t, f.st.Gates().Create(ctx, &g))
	c.GateID = g.ID
	must(t, f.st.Councils().Update(ctx, &c))
	decision := domain.Decision{AccountID: f.account.ID, ProjectID: f.project.ID, Question: "q", Choice: "pg", DecidedBy: "user:ana", CouncilID: c.ID, GateID: g.ID}
	must(t, f.st.Decisions().Create(ctx, &decision))
	sig, err := domain.NewSignal(f.account.ID, f.project.ID, &domain.CouncilDecidedSignal{CouncilID: c.ID, Status: domain.CouncilDecided, DecisionID: decision.ID}, t0)
	must(t, err)
	_, err = f.st.Signals().Insert(ctx, &sig)
	must(t, err)
	d, err = f.svc.GetCouncil(ctx, f.by, mcp.CouncilRefInput{Council: c.ID.String()})
	must(t, err)
	if d.Gate == nil || d.Gate.Ref != g.Ref() || d.Decision == nil || d.Decision.ID != decision.ID {
		t.Fatalf("detail = %+v", d)
	}

	// The register finds decisions by the council's ref.
	ds, err := f.svc.ListDecisions(ctx, f.by, mcp.ListDecisionsInput{Project: "app", Council: c.Ref()})
	must(t, err)
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v", ds)
	}

	_, err = f.svc.GetCouncil(ctx, f.by, mcp.CouncilRefInput{Council: "COUNCIL-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.GetCouncil(ctx, f.by, mcp.CouncilRefInput{Council: "GATE-1"})
	wantErr(t, err, mcp.ErrInvalidInput)
	other, _, _ := f.otherAccount(t)
	_, err = f.svc.GetCouncil(ctx, other, mcp.CouncilRefInput{Council: c.Ref()})
	wantErr(t, err, mcp.ErrNotFound)
}

// TestGetCouncilThatFailed: a council that ended without a decision shows none.
func TestGetCouncilThatFailed(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.CouncilTechnical, Question: "q"}
	must(t, f.st.Councils().Create(ctx, &c))
	sig, err := domain.NewSignal(f.account.ID, f.project.ID, &domain.CouncilDecidedSignal{CouncilID: c.ID, Status: domain.CouncilFailed}, t0)
	must(t, err)
	_, err = f.st.Signals().Insert(ctx, &sig)
	must(t, err)
	d, err := f.svc.GetCouncil(ctx, f.by, mcp.CouncilRefInput{Council: c.Ref()})
	must(t, err)
	if d.Decision != nil {
		t.Fatalf("decision = %+v", d.Decision)
	}
}

func TestAddCouncilConstraint(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.CouncilTechnical, Question: "q"}
	must(t, f.st.Councils().Create(ctx, &c))
	in := mcp.AddCouncilConstraintInput{Council: c.Ref(), Constraint: " Stay on Postgres "}
	for range 2 { // the same constraint counts once
		got, err := f.svc.AddCouncilConstraint(ctx, f.by, in)
		must(t, err)
		if len(got.Constraints) != 1 || got.Constraints[0] != "Stay on Postgres" {
			t.Fatalf("constraints = %v", got.Constraints)
		}
	}
	_, err := f.svc.AddCouncilConstraint(ctx, f.by, mcp.AddCouncilConstraintInput{Council: c.Ref(), Constraint: " "})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.AddCouncilConstraint(ctx, f.by, mcp.AddCouncilConstraintInput{Council: c.Ref(), Constraint: strings.Repeat("x", tools.MaxConstraint+1)})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.AddCouncilConstraint(ctx, f.by, mcp.AddCouncilConstraintInput{Council: "COUNCIL-9", Constraint: "x"})
	wantErr(t, err, mcp.ErrNotFound)
	must(t, f.st.Councils().UpdateStatus(ctx, f.account.ID, c.ID, domain.CouncilRunning, domain.CouncilFailed))
	_, err = f.svc.AddCouncilConstraint(ctx, f.by, mcp.AddCouncilConstraintInput{Council: c.Ref(), Constraint: "late"})
	wantErr(t, err, domain.ErrInvalidTransition)
}

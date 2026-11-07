package tools_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// traceFixture stores a goal with every kind of node and returns its refs.
func (f *fixture) traceFixture(t *testing.T) domain.Goal {
	t.Helper()
	ctx := t.Context()
	g := f.newGoal(t, "Login")
	g.IssueNumber = 4
	must(t, f.st.Goals().Update(ctx, &g))
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Kind: domain.CouncilTechnical, Question: "Which queue?", Brief: &domain.CouncilBrief{Recommendation: "pg"}, Status: domain.CouncilDecided}
	must(t, f.st.Councils().Create(ctx, &c))
	f.clock.Advance(time.Minute)
	s1 := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 1, Body: "v1"}
	s2 := domain.Spec{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Revision: 2, Body: strings.Repeat("é", 700)}
	must(t, f.st.Specs().Create(ctx, &s1))
	must(t, f.st.Specs().Create(ctx, &s2))
	task := domain.Task{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, SpecID: s2.ID, Seq: 1, Title: "queue", Status: domain.TaskMerged, IssueNumber: 5}
	must(t, f.st.Tasks().Create(ctx, &task))
	ch := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: task.ID, Repo: "acme/app", Number: 9, Branch: "b", HeadSHA: "abc1234", State: domain.ChangeMerged, URL: "https://github.com/acme/app/pull/9"}
	must(t, f.st.Changes().Upsert(ctx, &ch))
	f.storeReport(t, ch, "abc1234", domain.VerificationPassed)
	open := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, TaskID: task.ID, Repo: "acme/app", Number: 7, Branch: "a", HeadSHA: "000", State: domain.ChangeClosed, URL: "https://github.com/acme/app/pull/7"}
	must(t, f.st.Changes().Upsert(ctx, &open))

	specGate := domain.Gate{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.GateSpecApproval, Title: "spec", SubjectRepo: "acme/app", GoalID: g.ID, SpecID: s2.ID}
	mergeGate := domain.Gate{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.GateMergeApproval, Title: "merge", SubjectRepo: "acme/app", GoalID: g.ID, TaskID: task.ID}
	for _, gate := range []*domain.Gate{&specGate, &mergeGate} {
		must(t, f.st.Gates().Create(ctx, gate))
	}
	for _, d := range []domain.Decision{
		{Question: "Which queue?", Choice: "pg", Reasons: "simple", DecidedBy: "user:ana", CouncilID: c.ID},
		{Question: "Approve spec", Choice: "approved", DecidedBy: "user:ana", GateID: specGate.ID},
		{Question: "Merge", Choice: "approved", DecidedBy: "user:ana", GateID: mergeGate.ID},
		{Question: "Ship it", Choice: "yes", DecidedBy: "user:ana"},
		{Question: "Orphan gate", Choice: "yes", DecidedBy: "user:ana", GateID: domain.NewID()},
	} {
		d.AccountID, d.ProjectID, d.GoalID = f.account.ID, f.project.ID, g.ID
		must(t, f.st.Decisions().Create(ctx, &d))
	}
	for _, a := range []domain.Assumption{
		{MadeBy: "architect", GoalID: g.ID, Statement: "Postgres is up", Confidence: domain.LevelHigh, Impact: domain.LevelHigh, VerifyBy: domain.VerifyByTest,
			Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{{Kind: domain.DependentTask, ID: task.Ref()}, {Kind: domain.DependentChange, ID: "acme/app#9"}, {Kind: domain.DependentTest, ID: "t"}}},
		{MadeBy: "architect", GoalID: g.ID, Statement: "Loose", Confidence: domain.LevelLow, Impact: domain.LevelLow, VerifyBy: domain.VerifyByResearch,
			Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{}},
	} {
		a.AccountID, a.ProjectID = f.account.ID, f.project.ID
		must(t, f.st.Assumptions().Create(ctx, &a))
	}
	return g
}

func TestGetTrace(t *testing.T) {
	f := newFixture(t)
	g := f.traceFixture(t)
	tr, err := f.svc.GetTrace(t.Context(), f.by, mcp.GetTraceInput{Ref: g.Ref()})
	must(t, err)
	if tr.Focus != "GOAL-1" || tr.Goal != "GOAL-1" {
		t.Fatalf("focus %q goal %q", tr.Focus, tr.Goal)
	}
	kinds := map[string]int{}
	byID := map[string]mcp.TraceNode{}
	for _, n := range tr.Nodes {
		kinds[n.Kind]++
		byID[n.ID] = n
	}
	want := map[string]int{"goal": 1, "council": 1, "spec": 2, "decision": 5, "task": 1, "change": 2, "verification": 1, "merge": 1, "assumption": 2}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s nodes = %d, want %d (%v)", k, kinds[k], n, kinds)
		}
	}
	edges := map[string]bool{}
	for _, e := range tr.Edges {
		edges[e.From+">"+e.To+":"+e.Label] = true
	}
	for _, e := range []string{
		"GOAL-1>COUNCIL-1:asked", "GOAL-1>SPEC-1:written", "SPEC-1>SPEC-2:revised", "COUNCIL-1>DEC-1:decided", "DEC-1>SPEC-1:informs",
		"SPEC-2>DEC-2:decided", "TASK-1>DEC-3:decided", "GOAL-1>DEC-4:decided", "GOAL-1>DEC-5:decided", "SPEC-2>TASK-1:planned",
		"TASK-1>acme/app#7:built", "TASK-1>acme/app#9:built", "acme/app#9>verification:acme/app#9:verified",
		"verification:acme/app#9>merge:acme/app#9:merged", "A-1>TASK-1:affects", "A-1>acme/app#9:affects", "A-2>GOAL-1:affects",
	} {
		if !edges[e] {
			t.Errorf("missing edge %s in %v", e, tr.Edges)
		}
	}
	if len(tr.Edges) != 17 {
		t.Errorf("edges = %d: %v", len(tr.Edges), tr.Edges)
	}
	if n := byID["SPEC-2"]; len([]rune(n.Summary)) != 600 {
		t.Errorf("summary has %d characters", len([]rune(n.Summary)))
	}
	if n := byID["GOAL-1"]; n.URL != "https://github.com/acme/app/issues/4" || n.Status != "in_progress" && n.Status != "draft" {
		t.Errorf("goal = %+v", n)
	}
	if n := byID["A-1"]; n.Attrs["impact"] != "high" || n.Status != "unverified" {
		t.Errorf("assumption = %+v", n)
	}
	if !strings.Contains(tr.Untrusted, `<untrusted-data source="trace of GOAL-1"`) || !strings.Contains(tr.Untrusted, "Postgres is up") {
		t.Errorf("untrusted = %s", tr.Untrusted)
	}
}

func TestGetTraceFromAnyNode(t *testing.T) {
	f := newFixture(t)
	g := f.traceFixture(t)
	for ref, focus := range map[string]string{
		"SPEC-2": "SPEC-2", "TASK-1": "TASK-1", "COUNCIL-1": "COUNCIL-1", "A-1": "A-1", "DEC-2": "DEC-2",
		"acme/app#9": "acme/app#9", " goal-1 ": "GOAL-1",
	} {
		tr, err := f.svc.GetTrace(t.Context(), f.by, mcp.GetTraceInput{Ref: ref})
		if err != nil || tr.Focus != focus || tr.Goal != g.Ref() || len(tr.Nodes) != 16 {
			t.Errorf("%s: %v, focus %q goal %q, %d nodes", ref, err, tr.Focus, tr.Goal, len(tr.Nodes))
		}
	}
	// A UUID is a change.
	ch, err := f.st.Changes().GetByPR(t.Context(), f.account.ID, "acme/app", 9)
	must(t, err)
	tr, err := f.svc.GetTrace(t.Context(), f.by, mcp.GetTraceInput{Ref: ch.ID.String()})
	if err != nil || tr.Focus != "acme/app#9" {
		t.Errorf("uuid: %v, %q", err, tr.Focus)
	}
}

func TestGetTraceOfRecordsWithoutGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.CouncilAdhoc, Question: "Q?"}
	must(t, f.st.Councils().Create(ctx, &c))
	a := domain.Assumption{AccountID: f.account.ID, ProjectID: f.project.ID, MadeBy: "pm", Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{},
		Statement: "s", Confidence: domain.LevelLow, Impact: domain.LevelLow, VerifyBy: domain.VerifyByTest}
	must(t, f.st.Assumptions().Create(ctx, &a))
	d := domain.Decision{AccountID: f.account.ID, ProjectID: f.project.ID, Question: "q", Choice: "c", DecidedBy: "user:ana"}
	must(t, f.st.Decisions().Create(ctx, &d))
	for _, tt := range []struct{ ref, kind string }{{c.Ref(), "council"}, {a.Ref(), "assumption"}, {d.Ref(), "decision"}} {
		tr, err := f.svc.GetTrace(ctx, f.by, mcp.GetTraceInput{Ref: tt.ref})
		if err != nil || tr.Goal != "" || len(tr.Nodes) != 1 || tr.Nodes[0].Kind != tt.kind || tr.Focus != tt.ref || tr.Edges != nil {
			t.Errorf("%s: %v, %+v", tt.ref, err, tr)
		}
	}
}

func TestGetTraceRefused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.traceFixture(t)
	loose := domain.Change{AccountID: f.account.ID, ProjectID: f.project.ID, Repo: "acme/app", Number: 3, Branch: "b", HeadSHA: "a", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &loose))
	other, _, _ := f.otherAccount(t)
	tests := []struct {
		name string
		by   domain.Actor
		ref  string
		want error
	}{
		{"empty", f.by, " ", mcp.ErrInvalidInput},
		{"not a ref", f.by, "hello", mcp.ErrInvalidInput},
		{"job ref", f.by, "JOB-1", mcp.ErrInvalidInput},
		{"bad PR", f.by, "acme/app#x", mcp.ErrInvalidInput},
		{"unknown goal", f.by, "GOAL-99", mcp.ErrNotFound},
		{"unknown spec", f.by, "SPEC-99", mcp.ErrNotFound},
		{"unknown task", f.by, "TASK-99", mcp.ErrNotFound},
		{"unknown council", f.by, "COUNCIL-99", mcp.ErrNotFound},
		{"unknown assumption", f.by, "A-99", mcp.ErrNotFound},
		{"unknown decision", f.by, "DEC-99", mcp.ErrNotFound},
		{"unknown change", f.by, "acme/app#99", mcp.ErrNotFound},
		{"unknown change uuid", f.by, domain.NewID().String(), mcp.ErrNotFound},
		{"change without task", f.by, "acme/app#3", mcp.ErrNotFound},
		{"other account", other, "SPEC-2", mcp.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.GetTrace(ctx, tt.by, mcp.GetTraceInput{Ref: tt.ref})
			wantErr(t, err, tt.want)
		})
	}
}

// memberWith returns an actor who holds only perms on the project.
func (f *fixture) memberWith(t *testing.T, project uuid.UUID, perms ...domain.Permission) domain.Actor {
	t.Helper()
	role := domain.Role{AccountID: f.account.ID, Name: "r" + domain.NewID().String()[28:], Scope: projectScope, Permissions: perms, Custom: true}
	must(t, f.st.Roles().Create(t.Context(), &role))
	by, id := f.member(t, "", "", uuid.Nil())
	must(t, f.st.RoleBindings().Create(t.Context(), &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectUser, SubjectID: id, RoleID: role.ID, ProjectID: project, CreatedBy: "user:op"}))
	return by
}

func kindsOf(tr mcp.Trace) map[string]int {
	out := map[string]int{}
	for _, n := range tr.Nodes {
		out[n.Kind]++
	}
	return out
}

func TestGetTraceNeedsGoalReadForTheChain(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.traceFixture(t)
	reader := f.memberWith(t, f.project.ID, domain.PermProjectRead)

	// The chain behind a goal, spec, task or change is for goal.read.
	for _, ref := range []string{g.Ref(), "SPEC-2", "TASK-1", "acme/app#9"} {
		if _, err := f.svc.GetTrace(ctx, reader, mcp.GetTraceInput{Ref: ref}); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: err = %v, want denied", ref, err)
		}
	}
	// A council, decision or assumption shows what only project.read allows.
	tr, err := f.svc.GetTrace(ctx, reader, mcp.GetTraceInput{Ref: "COUNCIL-1"})
	must(t, err)
	want := map[string]int{"council": 1, "decision": 5, "assumption": 2}
	if tr.Goal != "" || len(kindsOf(tr)) != len(want) || kindsOf(tr)["council"] != 1 || kindsOf(tr)["decision"] != 5 || kindsOf(tr)["assumption"] != 2 {
		t.Fatalf("kinds = %v, goal %q", kindsOf(tr), tr.Goal)
	}
	for _, e := range tr.Edges {
		if e.From == "GOAL-1" || e.To == "GOAL-1" || strings.HasPrefix(e.To, "SPEC") || strings.HasPrefix(e.To, "TASK") {
			t.Errorf("edge %+v leaks the chain", e)
		}
	}
	if strings.Contains(tr.Untrusted, "Login") || strings.Contains(tr.Untrusted, "Spec revision") {
		t.Errorf("untrusted text leaks the chain: %s", tr.Untrusted)
	}
}

func TestGetTraceNeedsJobReadForVerification(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.traceFixture(t)
	noJobs := f.memberWith(t, f.project.ID, domain.PermProjectRead, domain.PermGoalRead)
	tr, err := f.svc.GetTrace(ctx, noJobs, mcp.GetTraceInput{Ref: g.Ref()})
	must(t, err)
	if k := kindsOf(tr); k["verification"] != 0 || k["merge"] != 1 || k["change"] != 2 || k["task"] != 1 {
		t.Errorf("kinds = %v", k)
	}
	withJobs := f.memberWith(t, f.project.ID, domain.PermProjectRead, domain.PermGoalRead, domain.PermJobRead)
	tr, err = f.svc.GetTrace(ctx, withJobs, mcp.GetTraceInput{Ref: g.Ref()})
	must(t, err)
	if kindsOf(tr)["verification"] != 1 {
		t.Errorf("kinds = %v", kindsOf(tr))
	}
}

func TestGetTraceStaysInTheProjectOfTheGoal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.traceFixture(t)
	other := f.secondProject(t)
	// Records of another project that point at the goal.
	c := domain.Council{AccountID: f.account.ID, ProjectID: other.ID, GoalID: g.ID, Kind: domain.CouncilAdhoc, Question: "Elsewhere?"}
	must(t, f.st.Councils().Create(ctx, &c))
	d := domain.Decision{AccountID: f.account.ID, ProjectID: other.ID, GoalID: g.ID, Question: "Elsewhere", Choice: "x", DecidedBy: "user:ana"}
	must(t, f.st.Decisions().Create(ctx, &d))
	a := domain.Assumption{AccountID: f.account.ID, ProjectID: other.ID, GoalID: g.ID, MadeBy: "pm", Status: domain.AssumptionUnverified, Dependents: []domain.Dependent{},
		StatedAssumption: domain.StatedAssumption{Statement: "Elsewhere", Confidence: domain.LevelLow, Impact: domain.LevelLow, VerifyBy: domain.VerifyByTest}}
	must(t, f.st.Assumptions().Create(ctx, &a))
	sp := domain.Spec{AccountID: f.account.ID, ProjectID: other.ID, GoalID: g.ID, Revision: 3, Body: "elsewhere"}
	must(t, f.st.Specs().Create(ctx, &sp))
	task := domain.Task{AccountID: f.account.ID, ProjectID: other.ID, GoalID: g.ID, SpecID: sp.ID, Seq: 2, Title: "elsewhere", Status: domain.TaskOpen}
	must(t, f.st.Tasks().Create(ctx, &task))
	ch := domain.Change{AccountID: f.account.ID, ProjectID: other.ID, TaskID: task.ID, Repo: "acme/second", Number: 5, Branch: "b", HeadSHA: "a", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &ch))
	stray := domain.Change{AccountID: f.account.ID, ProjectID: other.ID, TaskID: mustTask(t, f, g).ID, Repo: "acme/second", Number: 6, Branch: "b", HeadSHA: "a", State: domain.ChangeOpen, URL: "u"}
	must(t, f.st.Changes().Upsert(ctx, &stray))

	tr, err := f.svc.GetTrace(ctx, f.by, mcp.GetTraceInput{Ref: g.Ref()})
	must(t, err)
	if len(tr.Nodes) != 16 {
		for _, n := range tr.Nodes {
			t.Logf("%s %s", n.Kind, n.ID)
		}
		t.Fatalf("%d nodes, want the 16 of the goal's own project", len(tr.Nodes))
	}
	for _, n := range tr.Nodes {
		if strings.Contains(n.Title, "lsewhere") || n.ID == "acme/second#6" {
			t.Errorf("node %+v is from another project", n)
		}
	}

	// Who reads the record's project but not the goal's is refused the chain.
	byOther := f.memberWith(t, other.ID, domain.PermProjectRead, domain.PermGoalRead)
	if _, err := f.svc.GetTrace(ctx, byOther, mcp.GetTraceInput{Ref: c.Ref()}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("council of another project: err = %v, want denied", err)
	}
}

func mustTask(t *testing.T, f *fixture, g domain.Goal) domain.Task {
	t.Helper()
	ts, err := f.st.Tasks().List(t.Context(), f.account.ID, store.TaskFilter{GoalID: g.ID, ProjectID: f.project.ID})
	must(t, err)
	return ts[0]
}

func TestGetTraceAttachesAssumptionsToTheDecisionsThatRestOnThem(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	g := f.traceFixture(t)
	as, err := f.st.Assumptions().List(ctx, f.account.ID, store.AssumptionFilter{GoalID: g.ID})
	must(t, err)
	d := domain.Decision{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: g.ID, Question: "Built on it", Choice: "go", DecidedBy: "user:ana"}
	// A-2 is the one that rests on nothing else.
	var loose domain.Assumption
	for _, a := range as {
		if a.Statement == "Loose" {
			loose = a
		}
	}
	d.Assumptions = []uuid.UUID{loose.ID}
	must(t, f.st.Decisions().Create(ctx, &d))
	tr, err := f.svc.GetTrace(ctx, f.by, mcp.GetTraceInput{Ref: g.Ref()})
	must(t, err)
	edges := map[string]bool{}
	for _, e := range tr.Edges {
		edges[e.From+">"+e.To] = true
	}
	if !edges[loose.Ref()+">"+d.Ref()] || edges[loose.Ref()+">GOAL-1"] {
		t.Errorf("edges = %v", tr.Edges)
	}
}

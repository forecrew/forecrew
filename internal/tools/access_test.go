package tools_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const (
	orgScope     = domain.RoleScopeOrganization
	projectScope = domain.RoleScopeProject
)

// member adds a member with a built-in role on project (zero: the
// organization) and returns its actor and user ID.
func (f *fixture) member(t *testing.T, scope domain.RoleScope, role string, project uuid.UUID) (domain.Actor, uuid.UUID) {
	t.Helper()
	u := f.st.AddMember(f.account.ID, domain.User{Name: "someone"}, "")
	if role != "" {
		testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, u.ID, scope, role, project)
	}
	return domain.UserActor(f.account.ID, u.ID, domain.Credential{}), u.ID
}

func (f *fixture) secondProject(t *testing.T) domain.Project {
	t.Helper()
	p := domain.Project{AccountID: f.account.ID, Name: "second", Repo: "acme/second", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(t.Context(), &p))
	return p
}

func (f *fixture) gateOn(t *testing.T, kind domain.GateKind, project uuid.UUID) domain.Gate {
	t.Helper()
	g, err := f.gates.Open(t.Context(), gates.OpenRequest{AccountID: f.account.ID, ProjectID: project, Kind: kind, Title: string(kind), SubjectRepo: "acme/app", SubjectNumber: 1})
	must(t, err)
	return g
}

// TestEveryToolChecksTheProject calls each project tool as somebody who holds
// every project permission, but on another project.
func TestEveryToolChecksTheProject(t *testing.T) {
	w := newRegisterWorld(t)
	f := w.fixture
	ctx := t.Context()
	second := f.secondProject(t)
	by, _ := f.member(t, projectScope, domain.RoleProjectAdmin, second.ID)
	svc, notes := knowledgeService(f)
	n, err := notes.Write(ctx, w.job, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "Spool", Body: "flaky"})
	must(t, err)
	art := f.newArtifact(t, w.job, "a.log", "a")
	council, err := f.svc.StartCouncil(ctx, f.by, mcp.StartCouncilInput{Project: "app", Question: "q", Roles: []string{"architect", "ux"}})
	must(t, err)
	gate := f.gateOn(t, domain.GateSpecApproval, f.project.ID)
	job, goal := w.job.Ref(), w.goal.Ref()
	calls := map[string]func() error{
		"create_goal": func() error {
			_, err := f.svc.CreateGoal(ctx, by, mcp.CreateGoalInput{Project: "app", Title: "t"})
			return err
		},
		"dispatch_job": func() error {
			_, err := f.svc.DispatchJob(ctx, by, mcp.DispatchJobInput{Project: "app", Role: "reviewer"})
			return err
		},
		"get_project":    func() error { _, err := f.svc.GetProject(ctx, by, mcp.ProjectInput{Project: "app"}); return err },
		"get_goal":       func() error { _, err := f.svc.GetGoal(ctx, by, mcp.GoalRefInput{Goal: goal}); return err },
		"list_goals":     func() error { _, err := f.svc.ListGoals(ctx, by, mcp.ListGoalsInput{Project: "app"}); return err },
		"list_jobs":      func() error { _, err := f.svc.ListJobs(ctx, by, mcp.ListJobsInput{Project: "app"}); return err },
		"list_jobs/goal": func() error { _, err := f.svc.ListJobs(ctx, by, mcp.ListJobsInput{Goal: goal}); return err },
		"job_status":     func() error { _, err := f.svc.JobStatus(ctx, by, mcp.JobRefInput{Job: job}); return err },
		"job_logs":       func() error { _, err := f.svc.JobLogs(ctx, by, mcp.JobLogsInput{Job: job}); return err },
		"cancel_job":     func() error { _, err := f.svc.CancelJob(ctx, by, mcp.JobRefInput{Job: job}); return err },
		"list_artifacts": func() error { _, err := f.svc.ListArtifacts(ctx, by, mcp.JobRefInput{Job: job}); return err },
		"artifact": func() error {
			_, body, err := f.svc.OpenArtifact(ctx, by, art.ID.String())
			if body != nil {
				_ = body.Close()
			}
			return err
		},
		"list_test_runs/job": func() error { _, err := f.svc.ListTestRuns(ctx, by, mcp.ListTestRunsInput{Job: job}); return err },
		"list_test_runs/project": func() error {
			_, err := f.svc.ListTestRuns(ctx, by, mcp.ListTestRunsInput{Project: "app", SHA: "abc"})
			return err
		},
		"list_test_runs/change": func() error {
			_, err := f.svc.ListTestRuns(ctx, by, mcp.ListTestRunsInput{Project: "app", Change: 1})
			return err
		},
		"list_assumptions": func() error {
			_, err := f.svc.ListAssumptions(ctx, by, mcp.ListAssumptionsInput{Project: "app"})
			return err
		},
		"resolve_assumption": func() error {
			_, err := f.svc.ResolveAssumption(ctx, by, mcp.ResolveAssumptionInput{Assumption: w.as[0].Ref(), Status: domain.AssumptionAccepted})
			return err
		},
		"list_decisions": func() error {
			_, err := f.svc.ListDecisions(ctx, by, mcp.ListDecisionsInput{Project: "app"})
			return err
		},
		"search_knowledge": func() error {
			_, err := svc.SearchKnowledge(ctx, by, mcp.SearchKnowledgeInput{Project: "app", Query: "x"})
			return err
		},
		"delete_note": func() error { _, err := svc.DeleteNote(ctx, by, mcp.DeleteNoteInput{Note: n.ID.String()}); return err },
		"start_council": func() error {
			_, err := f.svc.StartCouncil(ctx, by, mcp.StartCouncilInput{Project: "app", Question: "q"})
			return err
		},
		"get_council": func() error {
			_, err := f.svc.GetCouncil(ctx, by, mcp.CouncilRefInput{Council: council.Ref})
			return err
		},
		"add_constraint": func() error {
			_, err := f.svc.AddCouncilConstraint(ctx, by, mcp.AddCouncilConstraintInput{Council: council.Ref, Constraint: "c"})
			return err
		},
		"list_pending_gates": func() error {
			_, err := f.svc.ListPendingGates(ctx, by, mcp.ProjectFilterInput{Project: "app"})
			return err
		},
		"approve": func() error { _, err := f.svc.Approve(ctx, by, mcp.GateDecisionInput{Gate: gate.Ref()}); return err },
		"request_changes": func() error {
			_, err := f.svc.RequestChanges(ctx, by, mcp.GateDecisionInput{Gate: gate.Ref(), Feedback: "x"})
			return err
		},
		"reject": func() error { _, err := f.svc.Reject(ctx, by, mcp.GateDecisionInput{Gate: gate.Ref()}); return err },
		"onboard_project": func() error {
			_, err := f.svc.OnboardProject(ctx, by, mcp.OnboardProjectInput{Repo: "acme/new"})
			return err
		},
		"list_workers": func() error { _, err := f.svc.ListWorkers(ctx, by); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: err = %v, want denied", name, err)
		}
	}
	if got, _ := f.st.Gates().Get(ctx, f.account.ID, gate.ID); got.Status != domain.GatePending {
		t.Errorf("gate is %s", got.Status)
	}
	// The same calls pass the check for a project admin of this project.
	owner, _ := f.member(t, projectScope, domain.RoleProjectAdmin, f.project.ID)
	if _, err := f.svc.GetGoal(ctx, owner, mcp.GoalRefInput{Goal: goal}); err != nil {
		t.Errorf("get_goal: %v", err)
	}
	if _, err := svc.DeleteNote(ctx, owner, mcp.DeleteNoteInput{Note: n.ID.String()}); err != nil {
		t.Errorf("delete_note: %v", err)
	}
}

func TestListsKeepTheProjectsOfTheCaller(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	second := f.secondProject(t)
	g1 := f.newGoal(t, "mine")
	g2 := domain.Goal{AccountID: f.account.ID, ProjectID: second.ID, Title: "theirs", Status: domain.GoalDraft}
	must(t, f.st.Goals().Create(ctx, &g2))
	j1 := f.newJob(t, "reviewer", g1, domain.Task{}, 0)
	j2 := domain.Job{AccountID: f.account.ID, ProjectID: second.ID, GoalID: g2.ID, Role: "reviewer", Runtime: "claude-code", Spec: domain.JobSpec{Repo: second.Repo}}
	must(t, f.st.Jobs().Create(ctx, &j2))
	gate1 := f.gateOn(t, domain.GateSpecApproval, f.project.ID)
	f.gateOn(t, domain.GateSpecApproval, second.ID)
	sha := strings.Repeat("a", 40)
	for _, j := range []domain.Job{j1, j2} {
		run := domain.TestRun{AccountID: f.account.ID, ProjectID: j.ProjectID, JobID: j.ID, SHA: sha, Suite: "unit", Status: domain.TestRunPassed}
		must(t, f.st.TestRuns().Create(ctx, &run, nil))
	}

	by, _ := f.member(t, projectScope, domain.RoleViewer, f.project.ID)
	projects, err := f.svc.ListProjects(ctx, by)
	if err != nil || len(projects) != 1 || projects[0].ID != f.project.ID {
		t.Errorf("projects = %+v, %v", projects, err)
	}
	goals, err := f.svc.ListGoals(ctx, by, mcp.ListGoalsInput{})
	if err != nil || len(goals) != 1 || goals[0].ID != g1.ID {
		t.Errorf("goals = %+v, %v", goals, err)
	}
	jobs, err := f.svc.ListJobs(ctx, by, mcp.ListJobsInput{Limit: 1})
	if err != nil || len(jobs) != 1 || jobs[0].ID != j1.ID {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}
	pending, err := f.svc.ListPendingGates(ctx, by, mcp.ProjectFilterInput{})
	if err != nil || len(pending) != 1 || pending[0].ID != gate1.ID {
		t.Errorf("gates = %+v, %v", pending, err)
	}
	runs, err := f.svc.ListTestRuns(ctx, by, mcp.ListTestRunsInput{SHA: sha})
	if err != nil || len(runs) != 1 || runs[0].JobID != j1.ID {
		t.Errorf("runs = %+v, %v", runs, err)
	}

	everyone, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	if jobs, _ = f.svc.ListJobs(ctx, everyone, mcp.ListJobsInput{}); len(jobs) != 2 {
		t.Errorf("an organization viewer sees %d jobs", len(jobs))
	}
	if projects, _ = f.svc.ListProjects(ctx, everyone); len(projects) != 2 {
		t.Errorf("an organization viewer sees %d projects", len(projects))
	}
	nobody, _ := f.member(t, "", "", uuid.Nil())
	if jobs, err = f.svc.ListJobs(ctx, nobody, mcp.ListJobsInput{}); err != nil || len(jobs) != 0 {
		t.Errorf("without a role: %+v, %v", jobs, err)
	}
}

// TestContributorApprovesSpecsNotMerges is the rule the roles are for, through
// the tools and with the gate list marking what the caller may resolve.
func TestContributorApprovesSpecsNotMerges(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, projectScope, domain.RoleContributor, f.project.ID)
	spec := f.gateOn(t, domain.GateSpecApproval, f.project.ID)
	merge := f.gateOn(t, domain.GateMergeApproval, f.project.ID)

	list, err := f.svc.ListPendingGates(ctx, by, mcp.ProjectFilterInput{Project: "app"})
	must(t, err)
	if len(list) != 2 || !list[0].CanResolve || list[1].CanResolve {
		t.Fatalf("gates = %+v", list)
	}
	_, err = f.svc.Approve(ctx, by, mcp.GateDecisionInput{Gate: merge.Ref()})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.RequestChanges(ctx, by, mcp.GateDecisionInput{Gate: merge.Ref(), Feedback: "no"})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.Reject(ctx, by, mcp.GateDecisionInput{Gate: merge.Ref()})
	wantErr(t, err, authz.ErrDenied)
	g, err := f.svc.Approve(ctx, by, mcp.GateDecisionInput{Gate: spec.Ref()})
	must(t, err)
	if g.Status != domain.GateApproved || g.ResolvedBy != by.String() {
		t.Errorf("gate = %+v", g)
	}
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{})
	must(t, err)
	denied := 0
	for _, e := range entries {
		if e.Action == "access.denied" {
			denied++
		}
	}
	if denied != 3 {
		t.Errorf("denials audited = %d", denied)
	}
	_, err = f.svc.Approve(ctx, by, mcp.GateDecisionInput{Gate: "GATE-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.Approve(ctx, by, mcp.GateDecisionInput{})
	wantErr(t, err, mcp.ErrInvalidInput)
}

func TestServiceAccountNeverResolvesAGate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	sa, err := f.svc.CreateServiceAccount(ctx, f.by, mcp.CreateServiceAccountInput{Name: "ci"})
	must(t, err)
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectServiceAccount, sa.ID, projectScope, domain.RoleMaintainer, f.project.ID)
	robot := domain.Actor{AccountID: f.account.ID, Kind: domain.ActorServiceAccount, Name: sa.ID.String()}
	g := f.gateOn(t, domain.GateSpecApproval, f.project.ID)
	list, err := f.svc.ListPendingGates(ctx, robot, mcp.ProjectFilterInput{})
	must(t, err)
	if len(list) != 1 || list[0].CanResolve {
		t.Fatalf("gates = %+v", list)
	}
	_, err = f.svc.Approve(ctx, robot, mcp.GateDecisionInput{Gate: g.Ref()})
	wantErr(t, err, authz.ErrDenied)
}

// TestResolvingAnAssumptionClosesItsReview needs the gate's permission too,
// because the review gate is resolved as the caller.
func TestResolvingAnAssumptionClosesItsReview(t *testing.T) {
	w := newRegisterWorld(t)
	f := w.fixture
	ctx := t.Context()
	a := w.as[0]
	_, err := f.gates.Open(ctx, gates.OpenRequest{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.GateAssumptionReview,
		Title: "Review " + a.Ref(), SubjectRepo: "acme/app", SubjectNumber: 1})
	must(t, err)
	role := domain.Role{AccountID: f.account.ID, Name: "resolver", Scope: projectScope, Permissions: []domain.Permission{domain.PermAssumptionResolve}, Custom: true}
	must(t, f.st.Roles().Create(ctx, &role))
	by, id := f.member(t, "", "", uuid.Nil())
	must(t, f.st.RoleBindings().Create(ctx, &domain.RoleBinding{AccountID: f.account.ID, SubjectKind: domain.SubjectUser, SubjectID: id, RoleID: role.ID, ProjectID: f.project.ID, CreatedBy: "user:op"}))
	in := mcp.ResolveAssumptionInput{Assumption: a.Ref(), Status: domain.AssumptionAccepted}
	_, err = f.svc.ResolveAssumption(ctx, by, in)
	wantErr(t, err, authz.ErrDenied)
	maintainer, _ := f.member(t, projectScope, domain.RoleMaintainer, f.project.ID)
	_, err = f.svc.ResolveAssumption(ctx, maintainer, in)
	must(t, err)
}

func TestTokensOfTheCaller(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	other, otherID := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	admin, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	create := func(name string, owner uuid.UUID) domain.MCPToken {
		tok := domain.MCPToken{AccountID: f.account.ID, Name: name, TokenHash: mcp.HashToken(name + owner.String()), OwnerKind: domain.SubjectUser, OwnerID: owner, Scopes: []domain.Scope{domain.ScopeWrite}}
		must(t, f.st.MCPTokens().Create(ctx, &tok))
		return tok
	}
	mine := create("ci", id)
	theirs := create("ci", otherID)
	viaToken := mcp.TokenActor(mine)

	list, err := f.svc.ListTokens(ctx, viaToken)
	must(t, err)
	if len(list) != 1 || list[0].ID != mine.ID || !list[0].Current {
		t.Fatalf("own tokens = %+v", list)
	}
	if list, _ = f.svc.ListTokens(ctx, admin); len(list) != 3 {
		t.Errorf("token.manage sees %d tokens", len(list))
	}
	// A name means the caller's own token; someone else's needs token.manage.
	// and without it the token looks like one that does not exist.
	_, err = f.svc.RevokeToken(ctx, by, mcp.RevokeTokenInput{Token: theirs.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	if errors.Is(err, authz.ErrDenied) {
		t.Errorf("err = %v", err)
	}
	ghost := domain.NewID().String()
	_, missing := f.svc.RevokeToken(ctx, by, mcp.RevokeTokenInput{Token: ghost})
	if strings.ReplaceAll(err.Error(), theirs.ID.String(), "") != strings.ReplaceAll(missing.Error(), ghost, "") {
		t.Errorf("the answers differ: %v, %v", err, missing)
	}
	if got, _ := f.auditOf(ctx, t); !slices.Contains(got, "access.denied") {
		t.Errorf("audit = %v", got)
	}
	out, err := f.svc.RevokeToken(ctx, other, mcp.RevokeTokenInput{Token: "ci"})
	must(t, err)
	if out.Token.ID != theirs.ID || out.Self {
		t.Errorf("revoked = %+v", out)
	}
	out, err = f.svc.RevokeToken(ctx, viaToken, mcp.RevokeTokenInput{Token: mine.ID.String()})
	must(t, err)
	if !out.Self {
		t.Errorf("revoking the token in use = %+v", out)
	}
	laptop, err := f.svc.RevokeToken(ctx, admin, mcp.RevokeTokenInput{Token: "laptop"})
	must(t, err)
	if laptop.Self {
		t.Errorf("revoked = %+v", laptop)
	}
}

// TestOwnTokensStayWithinTheScopes: own records need no role, but a read
// token cannot take its owner's other tokens down.
func TestOwnTokensStayWithinTheScopes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, id := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	create := func(name string, scope domain.Scope) domain.MCPToken {
		tok := domain.MCPToken{AccountID: f.account.ID, Name: name, TokenHash: mcp.HashToken(name), OwnerKind: domain.SubjectUser, OwnerID: id, Scopes: []domain.Scope{scope}}
		must(t, f.st.MCPTokens().Create(ctx, &tok))
		return tok
	}
	read := create("read", domain.ScopeRead)
	sibling := create("sibling", domain.ScopeAdmin)
	write := create("write", domain.ScopeWrite)

	if list, err := f.svc.ListTokens(ctx, mcp.TokenActor(read)); err != nil || len(list) != 3 {
		t.Fatalf("a read token lists %+v, %v", list, err)
	}
	for _, ref := range []string{sibling.ID.String(), "sibling"} {
		_, err := f.svc.RevokeToken(ctx, mcp.TokenActor(read), mcp.RevokeTokenInput{Token: ref})
		wantErr(t, err, authz.ErrDenied)
	}
	if tokens, _ := f.st.MCPTokens().List(ctx, f.account.ID); !tokens[2].RevokedAt.IsZero() {
		t.Fatalf("sibling = %+v", tokens[2])
	}
	if got, _ := f.auditOf(ctx, t); !slices.Contains(got, "access.denied") {
		t.Errorf("audit = %v", got)
	}

	grant := domain.UserActor(f.account.ID, id, domain.Credential{Kind: domain.CredentialOAuthGrant, ID: domain.NewID()})
	_, err := f.svc.RevokeToken(ctx, grant, mcp.RevokeTokenInput{Token: sibling.ID.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.ListTokens(ctx, grant)
	wantErr(t, err, authz.ErrDenied)

	out, err := f.svc.RevokeToken(ctx, mcp.TokenActor(write), mcp.RevokeTokenInput{Token: sibling.ID.String()})
	must(t, err)
	if out.Self || out.Token.ID != sibling.ID {
		t.Errorf("revoked = %+v", out)
	}
	// The token in use can always end itself.
	out, err = f.svc.RevokeToken(ctx, mcp.TokenActor(read), mcp.RevokeTokenInput{Token: read.ID.String()})
	must(t, err)
	if !out.Self {
		t.Errorf("revoked = %+v", out)
	}
	// A session has no scopes to meet.
	session := domain.UserActor(f.account.ID, id, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
	if _, err = f.svc.RevokeToken(ctx, session, mcp.RevokeTokenInput{Token: write.ID.String()}); err != nil {
		t.Error(err)
	}
}

func TestTeamsTools(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	_, user := f.member(t, "", "", uuid.Nil())

	team, err := f.svc.CreateTeam(ctx, by, mcp.CreateTeamInput{Name: "Core Team"})
	must(t, err)
	if team.Slug != "core-team" || team.Source != domain.SourceManual {
		t.Fatalf("team = %+v", team)
	}
	_, err = f.svc.CreateTeam(ctx, by, mcp.CreateTeamInput{Name: "Core Team"})
	wantErr(t, err, mcp.ErrConflict)
	_, err = f.svc.CreateTeam(ctx, by, mcp.CreateTeamInput{Name: " "})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.CreateTeam(ctx, by, mcp.CreateTeamInput{Name: "x", Slug: "Not A Slug"})
	wantErr(t, err, mcp.ErrInvalidInput)

	team, err = f.svc.UpdateTeam(ctx, by, mcp.UpdateTeamInput{Team: "core-team", Name: "Core", Slug: "core"})
	must(t, err)
	if team.Name != "Core" || team.Slug != "core" {
		t.Fatalf("team = %+v", team)
	}
	_, err = f.svc.UpdateTeam(ctx, by, mcp.UpdateTeamInput{Team: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.UpdateTeam(ctx, by, mcp.UpdateTeamInput{Team: ""})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.UpdateTeam(ctx, by, mcp.UpdateTeamInput{Team: team.ID.String(), Slug: "Bad Slug"})
	wantErr(t, err, mcp.ErrInvalidInput)

	ref := mcp.TeamMemberInput{Team: "core", User: user.String()}
	m, err := f.svc.AddTeamMember(ctx, by, ref)
	must(t, err)
	if m.UserID != user || m.Source != domain.SourceManual {
		t.Fatalf("member = %+v", m)
	}
	_, err = f.svc.AddTeamMember(ctx, by, mcp.TeamMemberInput{Team: "core", User: "nope"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.AddTeamMember(ctx, by, mcp.TeamMemberInput{Team: "core", User: domain.NewID().String()})
	wantErr(t, err, mcp.ErrNotFound)
	members, err := f.svc.ListTeamMembers(ctx, by, mcp.TeamRefInput{Team: "core"})
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if teams, err := f.svc.ListTeams(ctx, by); err != nil || len(teams) != 1 {
		t.Fatalf("teams = %+v, %v", teams, err)
	}
	_, err = f.svc.RemoveTeamMember(ctx, by, ref)
	must(t, err)
	_, err = f.svc.RemoveTeamMember(ctx, by, ref)
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.DeleteTeam(ctx, by, mcp.TeamRefInput{Team: "core"})
	must(t, err)
	_, err = f.svc.DeleteTeam(ctx, by, mcp.TeamRefInput{Team: "core"})
	wantErr(t, err, mcp.ErrNotFound)

	want := []string{"team.created", "team.updated", "team.member_added", "team.member_removed", "team.deleted"}
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{})
	must(t, err)
	var got []string
	for _, e := range slices.Backward(entries) {
		if e.Actor != by.String() {
			t.Errorf("actor of %s = %s", e.Action, e.Actor)
		}
		got = append(got, e.Action)
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestReadsNeedOrgRead(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, projectScope, domain.RoleProjectAdmin, f.project.ID)
	for name, call := range map[string]func() error{
		"roles":    func() error { _, err := f.svc.ListRoles(ctx, by); return err },
		"teams":    func() error { _, err := f.svc.ListTeams(ctx, by); return err },
		"members":  func() error { _, err := f.svc.ListTeamMembers(ctx, by, mcp.TeamRefInput{Team: "x"}); return err },
		"bindings": func() error { _, err := f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{}); return err },
		"accounts": func() error { _, err := f.svc.ListServiceAccounts(ctx, by); return err },
	} {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	roles, err := f.svc.ListRoles(ctx, viewer)
	if err != nil || len(roles) != len(domain.BuiltInRoles()) {
		t.Fatalf("roles = %d, %v", len(roles), err)
	}
	for name, call := range map[string]func() error{
		"create team": func() error { _, err := f.svc.CreateTeam(ctx, viewer, mcp.CreateTeamInput{Name: "x"}); return err },
		"update team": func() error { _, err := f.svc.UpdateTeam(ctx, viewer, mcp.UpdateTeamInput{Team: "x"}); return err },
		"delete team": func() error { _, err := f.svc.DeleteTeam(ctx, viewer, mcp.TeamRefInput{Team: "x"}); return err },
		"add member":  func() error { _, err := f.svc.AddTeamMember(ctx, viewer, mcp.TeamMemberInput{Team: "x"}); return err },
		"drop member": func() error {
			_, err := f.svc.RemoveTeamMember(ctx, viewer, mcp.TeamMemberInput{Team: "x"})
			return err
		},
		"create sa": func() error {
			_, err := f.svc.CreateServiceAccount(ctx, viewer, mcp.CreateServiceAccountInput{Name: "x"})
			return err
		},
		"update sa": func() error {
			_, err := f.svc.UpdateServiceAccount(ctx, viewer, mcp.UpdateServiceAccountInput{ServiceAccount: "x"})
			return err
		},
		"disable sa": func() error {
			_, err := f.svc.SetServiceAccountDisabled(ctx, viewer, mcp.SetServiceAccountInput{ServiceAccount: "x"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRoleBindingTools(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	_, user := f.member(t, "", "", uuid.Nil())
	team, err := f.svc.CreateTeam(ctx, by, mcp.CreateTeamInput{Name: "core"})
	must(t, err)
	sa, err := f.svc.CreateServiceAccount(ctx, by, mcp.CreateServiceAccountInput{Name: "ci"})
	must(t, err)

	// A project binding, an organization binding and a project role on the organization.
	onProject, err := f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectUser, Subject: user.String(), Role: "viewer", Project: "app"})
	must(t, err)
	if onProject.Role != "viewer" || onProject.RoleScope != projectScope || onProject.Project != "app" || onProject.CreatedBy != by.String() {
		t.Fatalf("binding = %+v", onProject)
	}
	onOrg, err := f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "member"})
	must(t, err)
	if onOrg.RoleScope != orgScope || onOrg.SubjectID != team.ID {
		t.Fatalf("binding = %+v", onOrg)
	}
	everywhere, err := f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectServiceAccount, Subject: "ci", Role: "maintainer"})
	must(t, err)
	if everywhere.RoleScope != projectScope || everywhere.ProjectID != uuid.Nil() || everywhere.SubjectID != sa.ID {
		t.Fatalf("binding = %+v", everywhere)
	}

	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "member"})
	wantErr(t, err, mcp.ErrConflict)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: "robot", Subject: "core", Role: "member"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectUser, Subject: "x", Role: "member"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "member", Project: "app"})
	wantErr(t, err, mcp.ErrNotFound) // an organization role is not a project role
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectTeam, Subject: "core", Role: "viewer", Project: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectUser, Subject: domain.NewID().String(), Role: "member"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectServiceAccount, Subject: "ghost", Role: "viewer"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: domain.SubjectServiceAccount, Subject: "", Role: "viewer"})
	wantErr(t, err, mcp.ErrInvalidInput)

	all, err := f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{})
	must(t, err)
	if len(all) != 4 { // with the caller's own
		t.Fatalf("bindings = %+v", all)
	}
	onApp, err := f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{Project: "app"})
	if err != nil || len(onApp) != 1 || onApp[0].Project != "app" || onApp[0].Role != "viewer" {
		t.Fatalf("on app = %+v, %v", onApp, err)
	}
	ofTeam, err := f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{SubjectKind: domain.SubjectTeam, Subject: "core"})
	if err != nil || len(ofTeam) != 1 {
		t.Fatalf("of team = %+v, %v", ofTeam, err)
	}
	_, err = f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{SubjectKind: domain.SubjectTeam, Subject: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.ListRoleBindings(ctx, by, mcp.ListRoleBindingsInput{Project: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	// A project viewer reads the bindings of that project, and no others.
	viewer, _ := f.member(t, projectScope, domain.RoleViewer, f.project.ID)
	if _, err = f.svc.ListRoleBindings(ctx, viewer, mcp.ListRoleBindingsInput{Project: "app"}); err != nil {
		t.Error(err)
	}
	second := f.secondProject(t)
	_, err = f.svc.ListRoleBindings(ctx, viewer, mcp.ListRoleBindingsInput{Project: second.Name})
	wantErr(t, err, authz.ErrDenied)

	got, err := f.svc.DeleteRoleBinding(ctx, by, mcp.RoleBindingRefInput{Binding: onProject.ID.String()})
	must(t, err)
	if got.ID != onProject.ID || got.Role != "viewer" {
		t.Errorf("deleted = %+v", got)
	}
	_, err = f.svc.DeleteRoleBinding(ctx, by, mcp.RoleBindingRefInput{Binding: onProject.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.DeleteRoleBinding(ctx, by, mcp.RoleBindingRefInput{Binding: "x"})
	wantErr(t, err, mcp.ErrInvalidInput)
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{})
	must(t, err)
	if entries[0].Action != "role_binding.deleted" || entries[0].SubjectID != onProject.ID || entries[0].ProjectID != f.project.ID ||
		!strings.Contains(string(entries[0].Data), user.String()) {
		t.Errorf("audit = %+v", entries[0])
	}
}

func TestLastOwnerStays(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, id := f.member(t, "", "", uuid.Nil())
	b := testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, id, orgScope, domain.RoleOwner, uuid.Nil())
	_, err := f.svc.DeleteRoleBinding(ctx, by, mcp.RoleBindingRefInput{Binding: b.ID.String()})
	wantErr(t, err, mcp.ErrConflict)
	wantErr(t, err, store.ErrLastOwner)
}

// TestNoEscalation walks the rules of permissions.md.
func TestNoEscalation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	owner, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	projectAdmin, _ := f.member(t, projectScope, domain.RoleProjectAdmin, f.project.ID)
	second := f.secondProject(t)
	_, plain := f.member(t, "", "", uuid.Nil())
	_, billing := f.member(t, orgScope, domain.RoleBilling, uuid.Nil())
	bind := func(by domain.Actor, kind domain.SubjectKind, subject, role, project string) error {
		_, err := f.svc.CreateRoleBinding(ctx, by, mcp.CreateRoleBindingInput{SubjectKind: kind, Subject: subject, Role: role, Project: project})
		return err
	}

	// Nobody gives a permission they lack.
	wantErr(t, bind(admin, domain.SubjectUser, plain.String(), "owner", ""), authz.ErrDenied)
	wantErr(t, bind(admin, domain.SubjectUser, plain.String(), "billing", ""), authz.ErrDenied)
	must(t, bind(owner, domain.SubjectUser, plain.String(), "owner", ""))
	// An Admin does not act on a Billing member, or on an Owner.
	wantErr(t, bind(admin, domain.SubjectUser, billing.String(), "member", ""), authz.ErrDenied)
	wantErr(t, bind(admin, domain.SubjectUser, plain.String(), "member", ""), authz.ErrDenied)
	must(t, bind(owner, domain.SubjectUser, billing.String(), "member", ""))

	// A Project admin works on their project only, and only with project.manage.
	_, member := f.member(t, orgScope, domain.RoleMember, uuid.Nil())
	must(t, bind(projectAdmin, domain.SubjectUser, member.String(), "maintainer", "app"))
	wantErr(t, bind(projectAdmin, domain.SubjectUser, member.String(), "maintainer", second.Name), authz.ErrDenied)
	wantErr(t, bind(projectAdmin, domain.SubjectUser, member.String(), "maintainer", ""), authz.ErrDenied)
	// A Maintainer cannot hand out project_admin: it holds no project.manage.
	maintainer, _ := f.member(t, projectScope, domain.RoleMaintainer, f.project.ID)
	wantErr(t, bind(maintainer, domain.SubjectUser, member.String(), "viewer", "app"), authz.ErrDenied)

	// Removing a binding needs what granting it needs.
	billingBindings, err := f.svc.ListRoleBindings(ctx, owner, mcp.ListRoleBindingsInput{SubjectKind: domain.SubjectUser, Subject: billing.String()})
	must(t, err)
	_, err = f.svc.DeleteRoleBinding(ctx, admin, mcp.RoleBindingRefInput{Binding: billingBindings[0].ID.String()})
	wantErr(t, err, authz.ErrDenied)

	// A suspended holder is still a holder.
	_, suspended := f.member(t, orgScope, domain.RoleBilling, uuid.Nil())
	f.st.SetMemberStatus(f.account.ID, suspended, domain.MembershipSuspended)
	wantErr(t, bind(admin, domain.SubjectUser, suspended.String(), "member", ""), authz.ErrDenied)

	// Roles held through a team count for the holder.
	team, err := f.svc.CreateTeam(ctx, owner, mcp.CreateTeamInput{Name: "money"})
	must(t, err)
	must(t, bind(owner, domain.SubjectTeam, "money", "billing", ""))
	_, inTeam := f.member(t, "", "", uuid.Nil())
	_, err = f.svc.AddTeamMember(ctx, owner, mcp.TeamMemberInput{Team: "money", User: inTeam.String()})
	must(t, err)
	wantErr(t, bind(admin, domain.SubjectUser, inTeam.String(), "member", ""), authz.ErrDenied)

	// Adding to a team concerns its roles; removing does not; deleting does.
	_, err = f.svc.AddTeamMember(ctx, admin, mcp.TeamMemberInput{Team: "money", User: plain.String()})
	wantErr(t, err, authz.ErrDenied)
	_, err = f.svc.RemoveTeamMember(ctx, admin, mcp.TeamMemberInput{Team: "money", User: inTeam.String()})
	must(t, err)
	_, err = f.svc.DeleteTeam(ctx, admin, mcp.TeamRefInput{Team: team.Slug})
	wantErr(t, err, authz.ErrDenied)

	// A service account that holds more than the caller is out of reach.
	sa, err := f.svc.CreateServiceAccount(ctx, owner, mcp.CreateServiceAccountInput{Name: "books"})
	must(t, err)
	must(t, bind(owner, domain.SubjectServiceAccount, "books", "billing", ""))
	_, err = f.svc.SetServiceAccountDisabled(ctx, admin, mcp.SetServiceAccountInput{ServiceAccount: "books", Disabled: true})
	wantErr(t, err, authz.ErrDenied)
	wantErr(t, bind(admin, domain.SubjectServiceAccount, sa.Name, "viewer", ""), authz.ErrDenied)
	_, err = f.svc.SetServiceAccountDisabled(ctx, owner, mcp.SetServiceAccountInput{ServiceAccount: "books", Disabled: true})
	must(t, err)

	// A token of an Owner with write scope cannot manage: it stays within its scopes.
	_, ownerID := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	tok := domain.MCPToken{AccountID: f.account.ID, Name: "w", TokenHash: mcp.HashToken("w"), OwnerKind: domain.SubjectUser, OwnerID: ownerID, Scopes: []domain.Scope{domain.ScopeWrite}}
	must(t, f.st.MCPTokens().Create(ctx, &tok))
	wantErr(t, bind(mcp.TokenActor(tok), domain.SubjectUser, member.String(), "viewer", ""), authz.ErrDenied)
}

func TestServiceAccountTools(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	by, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	sa, err := f.svc.CreateServiceAccount(ctx, by, mcp.CreateServiceAccountInput{Name: "ci", Description: "CI"})
	must(t, err)
	if sa.CreatedBy != by.String() {
		t.Fatalf("service account = %+v", sa)
	}
	_, err = f.svc.CreateServiceAccount(ctx, by, mcp.CreateServiceAccountInput{Name: "ci"})
	wantErr(t, err, mcp.ErrConflict)
	_, err = f.svc.CreateServiceAccount(ctx, by, mcp.CreateServiceAccountInput{Name: "Not Valid"})
	wantErr(t, err, mcp.ErrInvalidInput)
	empty := ""
	got, err := f.svc.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{ServiceAccount: sa.ID.String(), Name: "build", Description: &empty})
	must(t, err)
	if got.Name != "build" || got.Description != "" {
		t.Fatalf("updated = %+v", got)
	}
	got, err = f.svc.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{ServiceAccount: "build"})
	must(t, err)
	if got.Name != "build" {
		t.Fatalf("updated = %+v", got)
	}
	_, err = f.svc.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{ServiceAccount: "ghost"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{ServiceAccount: "build", Name: "Not Valid"})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = f.svc.UpdateServiceAccount(ctx, by, mcp.UpdateServiceAccountInput{})
	wantErr(t, err, mcp.ErrInvalidInput)
	got, err = f.svc.SetServiceAccountDisabled(ctx, by, mcp.SetServiceAccountInput{ServiceAccount: "build", Disabled: true})
	must(t, err)
	if got.DisabledAt.IsZero() {
		t.Fatal("not disabled")
	}
	got, err = f.svc.SetServiceAccountDisabled(ctx, by, mcp.SetServiceAccountInput{ServiceAccount: "build"})
	must(t, err)
	if !got.DisabledAt.IsZero() {
		t.Fatal("not enabled")
	}
	all, err := f.svc.ListServiceAccounts(ctx, by)
	if err != nil || len(all) != 1 {
		t.Fatalf("service accounts = %+v, %v", all, err)
	}
	if got, _ := f.auditOf(ctx, t); !slices.Contains(got, "service_account.disabled") || !slices.Contains(got, "service_account.enabled") {
		t.Errorf("audit = %v", got)
	}
}

func (f *fixture) auditOf(ctx context.Context, t *testing.T) ([]string, error) {
	t.Helper()
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{})
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Action
	}
	return out, err
}

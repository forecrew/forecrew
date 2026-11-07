package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

func (f *fixture) request(role string) workflow.JobRequest {
	return workflow.JobRequest{AccountID: f.account.ID, ProjectID: f.project.ID, Role: role}
}

func TestJobCreatorWritingRole(t *testing.T) {
	f := newFixture(t)
	goal := domain.Goal{AccountID: f.account.ID, ProjectID: f.project.ID, Title: "Ship it", Body: "Add a thing", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(t.Context(), &goal))
	req := f.request("developer")
	req.ID, req.Branch, req.Needs, req.GoalID = domain.NewID(), "agent/task-1", []string{"Go", "linux"}, goal.ID
	req.IncludeRepoFacts, req.Prompt, req.Feedback = true, "mind the tests", "fix the lint"

	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	if id != req.ID {
		t.Fatalf("id = %s, want the request's %s", id, req.ID)
	}
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	role, err := f.catalog.Resolve(t.Context(), f.account.ID, f.project.ID, "developer")
	must(t, err)
	if j.Status != domain.JobQueued || j.Role != "developer" || j.Runtime != "claude-code" || j.GoalID != goal.ID {
		t.Errorf("job = %+v", j)
	}
	if want := []string{"go", "linux"}; !slices.Equal(j.Needs, want) {
		t.Errorf("needs = %v, want %v", j.Needs, want)
	}
	s := j.Spec
	if s.JobID != id || s.AccountID != f.account.ID || s.Repo != "acme/app" || s.BaseRef != "main" || s.Branch != "agent/task-1" ||
		s.Model != "claude-strong" || s.Runtime != "claude-code" || !s.IncludeRepoFacts {
		t.Errorf("spec = %+v", s)
	}
	// The role's paths, then the project's curated knowledge excluded: it
	// changes only through proposals.
	wantPaths := append(slices.Clone(role.WritePaths), "!.forecrew/knowledge-policy.yaml",
		"!knowledge/architecture/**", "!knowledge/conventions/**", "!knowledge/lessons/**", "!knowledge/runbooks/**", "!knowledge/security/**", "!knowledge/testing/**", "!knowledge/vision/**")
	if !slices.Equal(s.WritePaths, wantPaths) || !slices.Contains(s.WritePaths, "!knowledge/**") {
		t.Errorf("write_paths = %v, want %v", s.WritePaths, wantPaths)
	}
	if !slices.Equal(s.AllowedTools, role.AllowedTools) || s.MaxTurns != role.MaxTurns || time.Duration(s.Timeout) != role.Timeout {
		t.Errorf("limits = %v %d %v", s.AllowedTools, s.MaxTurns, s.Timeout)
	}
	if len(s.OutputSchema) == 0 || strings.Contains(string(s.OutputSchema), "$ref") {
		t.Errorf("output schema must be inline: %s", s.OutputSchema)
	}
	for _, want := range []string{"Add a thing", "mind the tests", "fix the lint"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// A job records what its context pack held, and an assumption of its goal
// reaches the pack through the register.
func TestJobCreatorContextManifest(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	goal := domain.Goal{AccountID: f.account.ID, ProjectID: f.project.ID, Title: "Ship it", Body: "Add a thing", CreatedBy: "user:me"}
	must(t, f.st.Goals().Create(ctx, &goal))
	a := domain.Assumption{AccountID: f.account.ID, ProjectID: f.project.ID, GoalID: goal.ID, MadeBy: "architect",
		Statement: "The thing is cheap.", Confidence: domain.LevelLow, Impact: domain.LevelMedium, VerifyBy: domain.VerifyByTest, Status: domain.AssumptionUnverified}
	must(t, f.st.Assumptions().Create(ctx, &a))

	req := f.request("developer")
	req.GoalID = goal.ID
	id, err := f.creator.Create(ctx, req)
	must(t, err)
	j, err := f.st.Jobs().Get(ctx, f.account.ID, id)
	must(t, err)
	role, err := f.catalog.Resolve(ctx, f.account.ID, f.project.ID, "developer")
	must(t, err)
	m := j.ContextManifest
	if m == nil || m.Budget != role.ContextBudget || m.Size == 0 || len(m.Sections) != len(role.Context) {
		t.Fatalf("manifest = %+v, want the sections %v", m, role.Context)
	}
	for _, s := range m.Sections {
		if s.Name == "assumptions" && (len(s.Items) != 1 || s.Items[0].ID != a.Ref()) {
			t.Errorf("assumptions section = %+v, want %s", s, a.Ref())
		}
	}
	if !strings.Contains(j.Spec.Prompt, "The thing is cheap.") {
		t.Errorf("prompt lacks the assumption:\n%s", j.Spec.Prompt)
	}

	// A role without sections has no pack to record.
	id, err = f.creator.Create(ctx, f.request("onboarding-analyst"))
	must(t, err)
	if j, err := f.st.Jobs().Get(ctx, f.account.ID, id); err != nil || j.ContextManifest != nil {
		t.Errorf("manifest of a role without sections = %+v, %v", j.ContextManifest, err)
	}
}

func TestJobCreatorLockedPaths(t *testing.T) {
	f := newFixture(t)
	req := f.request("developer")
	req.Branch, req.LockedPaths = "agent/task-1", []string{"internal/*_test.go", "testdata/**"}
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	if !slices.Equal(j.Spec.LockedPaths, req.LockedPaths) {
		t.Errorf("locked_paths = %v", j.Spec.LockedPaths)
	}
	for _, bad := range []string{"!a_test.go", "/abs_test.go", "a\n# injected", "a`b", "a\x00b"} {
		req.LockedPaths = []string{bad}
		if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", bad, err)
		}
	}
	// Escaped glob characters and a bracketed directory are valid.
	req.LockedPaths = []string{"web/src/routes/[[]id]/a.test.ts", "[*]x_test.go"}
	if _, err := f.creator.Create(t.Context(), req); err != nil {
		t.Errorf("escaped paths: %v", err)
	}
}

// A read-only role writes the paths a request adds, in a pool that gives a
// write token; the paths are checked like the locked ones.
func TestJobCreatorWritePaths(t *testing.T) {
	f := newFixture(t)
	req := f.request("architect")
	req.Branch, req.Pool, req.WritePaths = "agent/adr-GOAL-1", domain.PoolBuild, []string{"docs/adr/**"}
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	if !slices.Contains(j.Spec.WritePaths, "docs/adr/**") {
		t.Errorf("write_paths = %v", j.Spec.WritePaths)
	}
	// Without them the role stays read-only.
	req.WritePaths = nil
	id, err = f.creator.Create(t.Context(), req)
	must(t, err)
	if j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id); err != nil || len(j.Spec.WritePaths) != 0 {
		t.Errorf("job = %+v, %v", j.Spec.WritePaths, err)
	}
	for _, bad := range []string{"!docs/**", "/etc/**", "a\n# injected", "a`b"} {
		req.WritePaths = []string{bad}
		if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestJobCreatorReadOnlyBranchAndOverrides(t *testing.T) {
	f := newFixture(t)
	req := f.request("reviewer")
	req.Repo, req.BaseRef, req.Branch = "ACME/App", "develop", "agent/task-2"
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	// A read-only role inspects the branch; the codex tier maps to the openai model.
	if j.Runtime != "codex" || j.Spec.Model != "gpt-best" || len(j.Spec.WritePaths) != 0 ||
		j.Spec.Repo != "acme/app" || j.Spec.BaseRef != "develop" || j.Spec.Branch != "agent/task-2" {
		t.Errorf("job = %+v", j)
	}
}

// TestJobCreatorPools: the job runs in the request's pool, else the role
// pack's, and a pool whose policy does not allow the role refuses it.
func TestJobCreatorPools(t *testing.T) {
	f := newFixture(t, func(d *Deps) {
		d.Files = packs(t, func(m fstest.MapFS) {
			m["packs/roles/reviewer.yaml"].Data = append(m["packs/roles/reviewer.yaml"].Data, "pool: control\n"...)
		})
	})
	pool := func(role, pool string) (string, error) {
		req := f.request(role)
		req.Pool = pool
		id, err := f.creator.Create(t.Context(), req)
		if err != nil {
			return "", err
		}
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		return j.Pool, err
	}
	for _, tt := range []struct{ role, pool, want string }{
		{"reviewer", "", domain.PoolControl},
		{"reviewer", domain.PoolBuild, domain.PoolBuild},
		{"developer", "", domain.PoolBuild},
	} {
		if got, err := pool(tt.role, tt.pool); err != nil || got != tt.want {
			t.Errorf("%s in %q: pool %q, %v; want %q", tt.role, tt.pool, got, err, tt.want)
		}
	}
	_, err := pool("developer", domain.PoolControl)
	if !errors.Is(err, jobs.ErrInvalid) || !errors.Is(err, config.ErrNotAllowed) ||
		!strings.Contains(err.Error(), "role developer writes to the repo and pool control gets no write token") {
		t.Errorf("writer in control: %v", err)
	}
	if _, err := pool("reviewer", "gpu"); !errors.Is(err, config.ErrNotAllowed) {
		t.Errorf("unknown pool: %v", err)
	}
}

// A role pack's resources reach the job.
func TestJobCreatorResources(t *testing.T) {
	f := newFixture(t, func(d *Deps) {
		d.Files = packs(t, func(m fstest.MapFS) {
			m["packs/roles/reviewer.yaml"].Data = append(m["packs/roles/reviewer.yaml"].Data, "resources: {min_cpu: 4, gpu: 1}\n"...)
		})
	})
	id, err := f.creator.Create(t.Context(), f.request("reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	if err != nil || j.Resources != (domain.Resources{MinCPU: 4, GPU: 1}) {
		t.Fatalf("resources = %+v, %v", j.Resources, err)
	}
}

// TestJobCreatorToolbox: a role with a toolbox gets it in its spec, a job that
// needs Docker and the toolbox's labels, and the tools of its MCP servers.
func TestJobCreatorToolbox(t *testing.T) {
	f := newFixture(t, func(d *Deps) {
		d.Files = packs(t, func(m fstest.MapFS) {
			m["packs/roles/reviewer.yaml"].Data = append(m["packs/roles/reviewer.yaml"].Data, "toolbox: browser\n"...)
			m["packs/toolboxes/browser.yaml"].Data = append(m["packs/toolboxes/browser.yaml"].Data, "needs: [GPU]\n"...)
		})
	})
	job := func(role string) domain.Job {
		req := f.request(role)
		req.Needs = []string{"linux"}
		id, err := f.creator.Create(t.Context(), req)
		must(t, err)
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		must(t, err)
		return j
	}
	j := job("reviewer")
	if j.Spec.Toolbox == nil || j.Spec.Toolbox.Name != "browser" || len(j.Spec.Toolbox.MCPServers) != 1 {
		t.Fatalf("toolbox = %+v", j.Spec.Toolbox)
	}
	if want := []string{"docker", "gpu", "linux"}; !slices.Equal(j.Needs, want) {
		t.Errorf("needs = %v, want %v", j.Needs, want)
	}
	if !slices.Contains(j.Spec.AllowedTools, runtime.MCPServerTool("playwright")) || !slices.Contains(j.Spec.AllowedTools, "Read") {
		t.Errorf("allowed tools = %v", j.Spec.AllowedTools)
	}
	if err := j.Spec.Validate(); err != nil {
		t.Errorf("spec: %v", err)
	}

	// Another role is unchanged, and so is the role pack's own list.
	j = job("developer")
	if j.Spec.Toolbox != nil || !slices.Equal(j.Needs, []string{"linux"}) || slices.Contains(j.Spec.AllowedTools, runtime.MCPServerTool("playwright")) {
		t.Errorf("developer job = needs %v, toolbox %+v, tools %v", j.Needs, j.Spec.Toolbox, j.Spec.AllowedTools)
	}
}

func TestJobCreatorGeneratesID(t *testing.T) {
	f := newFixture(t)
	id, err := f.creator.Create(t.Context(), f.request("reviewer"))
	must(t, err)
	if id == uuid.Nil() {
		t.Fatal("no job ID")
	}
}

func TestJobCreatorErrors(t *testing.T) {
	f := newFixture(t)
	other := f.request("developer")
	other.ProjectID = domain.NewID()
	unknown := f.request("nope")
	if _, err := f.creator.Create(t.Context(), other); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	foreign := f.request("developer")
	foreign.Repo = "acme/other"
	if _, err := f.creator.Create(t.Context(), foreign); !errors.Is(err, jobs.ErrInvalid) {
		t.Errorf("another repo: %v", err)
	}
	if _, err := f.creator.Create(t.Context(), unknown); !errors.Is(err, roles.ErrUnknownRole) {
		t.Errorf("unknown role: %v", err)
	}
	missing := f.request("developer")
	missing.TaskID = domain.NewID()
	if _, err := f.creator.Create(t.Context(), missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("context pack: %v", err)
	}
}

func TestSameVendorReviewAllowed(t *testing.T) {
	f := newFixture(t, func(d *Deps) {
		d.Config.Roles["reviewer"] = config.Role{Runtime: "claude", Tier: config.TierBest}
		d.Config.Policy.AllowSameVendorReview = true
	})
	id, err := f.creator.Create(t.Context(), f.request("reviewer"))
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	if j.Runtime != "claude-code" || j.Spec.Model != "claude-best" {
		t.Errorf("job = %+v", j)
	}
}

func (f *fixture) commandRequest() workflow.JobRequest {
	return workflow.JobRequest{
		AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Branch: "agent/task-3", Suite: "unit",
		Cell: map[string]string{"os": "linux"}, Needs: []string{"Linux"},
		Command: &domain.CommandSpec{Argv: []string{"go", "test", "./..."}, Reports: []string{"reports/unit.xml"}},
		Overlay: &domain.Overlay{Ref: "agent/task-3", Paths: []string{"a_test.go"}},
	}
}

func TestJobCreatorCommandJob(t *testing.T) {
	f := newFixture(t)
	req := f.commandRequest()
	req.ID = domain.NewID()
	req.Environment = &domain.Environment{Name: "host", Provider: domain.ProviderMachine, Needs: []string{"go"}}
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	if id != req.ID {
		t.Fatalf("id = %s, want %s", id, req.ID)
	}
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	s := j.Spec
	if j.Kind != domain.JobKindCommand || j.Pool != domain.PoolBuild || j.Status != domain.JobQueued || j.Runtime != "" ||
		!slices.Equal(j.Needs, []string{"go", "linux"}) {
		t.Errorf("job = %+v", j)
	}
	if s.Kind != domain.JobKindCommand || s.JobID != id || s.Repo != "acme/app" || s.BaseRef != "main" || s.Branch != "agent/task-3" ||
		s.Prompt != "" || s.Model != "" || len(s.WritePaths) != 0 || s.Suite != "unit" || s.Cell["os"] != "linux" ||
		s.Overlay.Ref != "agent/task-3" || time.Duration(s.Timeout) != defaultCommandTimeout || s.Command.Argv[0] != "go" {
		t.Errorf("spec = %+v", s)
	}
}

func TestJobCreatorCommandPoolAndNeeds(t *testing.T) {
	f := newFixture(t)
	req := f.commandRequest()
	req.Timeout = domain.Duration(time.Minute)
	req.Environment = &domain.Environment{Name: "ci", Provider: domain.ProviderActions, RunsOn: "ubuntu-24.04"}
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, _ := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	if j.Pool != domain.PoolActions || time.Duration(j.Spec.Timeout) != time.Minute {
		t.Errorf("pool = %q, timeout = %v", j.Pool, j.Spec.Timeout)
	}

	req = f.commandRequest()
	req.Environment = &domain.Environment{Name: "linux-go", Provider: domain.ProviderDocker, Image: "golang:1.27"}
	id, err = f.creator.Create(t.Context(), req)
	must(t, err)
	j, _ = f.st.Jobs().Get(t.Context(), f.account.ID, id)
	if j.Pool != domain.PoolBuild || !slices.Equal(j.Needs, []string{"docker", "linux"}) {
		t.Errorf("pool = %q, needs = %v", j.Pool, j.Needs)
	}
}

func TestJobCreatorCommandErrors(t *testing.T) {
	f := newFixture(t)
	tests := map[string]func(*workflow.JobRequest){
		"no command":     func(r *workflow.JobRequest) { r.Command = nil },
		"argv and shell": func(r *workflow.JobRequest) { r.Command.Shell = "make test" },
		"prompt":         func(r *workflow.JobRequest) { r.Prompt = "hi" },
		"role":           func(r *workflow.JobRequest) { r.Role = "developer" },
		"pool":           func(r *workflow.JobRequest) { r.Pool = domain.PoolControl },
		"locked paths":   func(r *workflow.JobRequest) { r.LockedPaths = []string{"a_test.go"} },
		"write paths":    func(r *workflow.JobRequest) { r.WritePaths = []string{"docs/**"} },
		"another repo":   func(r *workflow.JobRequest) { r.Repo = "acme/other" },
		"secret env":     func(r *workflow.JobRequest) { r.Command.Env = map[string]string{"API_TOKEN": "x"} },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			req := f.commandRequest()
			mut(&req)
			if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, jobs.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
	req := f.commandRequest()
	req.ProjectID = domain.NewID()
	if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	agent := f.request("developer")
	agent.Suite = "unit"
	if _, err := f.creator.Create(t.Context(), agent); !errors.Is(err, jobs.ErrInvalid) {
		t.Errorf("agent job with a suite: %v", err)
	}
}

func TestJobCreatorCancel(t *testing.T) {
	f := newFixture(t)
	id, err := f.creator.Create(t.Context(), f.commandRequest())
	must(t, err)
	must(t, f.creator.Cancel(t.Context(), f.account.ID, id))
	j, _ := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	if j.Status != domain.JobCanceled {
		t.Errorf("job is %s", j.Status)
	}
	if err := f.creator.Cancel(t.Context(), f.account.ID, id); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("cancel a finished job: %v", err)
	}
	if err := f.creator.Cancel(t.Context(), f.account.ID, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancel an unknown job: %v", err)
	}
}

// The knowledge curator is the one writing role that may write knowledge/**,
// except the protected sections and the policy file, and a read-only role
// stays read-only.
func TestJobCreatorKnowledgeExcludes(t *testing.T) {
	f := newFixture(t)
	curator := []string{"knowledge/**", "!.forecrew/knowledge-policy.yaml", "!knowledge/security/**", "!knowledge/vision/**"}
	for role, want := range map[string][]string{"knowledge-curator": curator, "reviewer": nil} {
		id, err := f.creator.Create(t.Context(), f.request(role))
		must(t, err)
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		must(t, err)
		if !slices.Equal(j.Spec.WritePaths, want) && len(j.Spec.WritePaths)+len(want) != 0 {
			t.Errorf("%s: write_paths = %v, want %v", role, j.Spec.WritePaths, want)
		}
	}
}

func (f *fixture) council(t *testing.T) domain.Council {
	t.Helper()
	c := domain.Council{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.CouncilTechnical, Question: "Which queue?", Constraints: []string{"Keep Postgres"}}
	must(t, f.st.Councils().Create(t.Context(), &c))
	return c
}

func (f *fixture) councilJob(t *testing.T, c domain.Council, role string, turn workflow.CouncilTurn) domain.Job {
	t.Helper()
	req := f.request(role)
	turn.CouncilID = c.ID
	req.ID, req.BaseRef, req.Council = domain.NewID(), "c0ffee", &turn
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	return j
}

// TestCouncilJob: a member's job is the step's, on the runtime it is pinned
// to, read-only and in its role's pool, with the council in its prompt.
func TestCouncilJob(t *testing.T) {
	f := newFixture(t)
	c := f.council(t)
	j := f.councilJob(t, c, "developer", workflow.CouncilTurn{
		Step: "proposal", Round: 1, Priority: "cost", Runtime: config.AdapterCodex, Framing: []byte(`{"question":"Which store holds the queue?"}`),
	})
	s := j.Spec
	if j.Runtime != config.AdapterCodex || s.Model != "gpt-strong" || len(s.WritePaths) != 0 || j.Pool != "build" || s.BaseRef != "c0ffee" {
		t.Fatalf("job = %+v, spec = %+v", j, s)
	}
	if !slices.Contains(s.AllowedTools, "mcp__forecrew__council_read") || slices.Contains(s.AllowedTools, "Write") {
		t.Fatalf("tools = %v", s.AllowedTools)
	}
	for _, want := range []string{"# Council step: proposal", "developer@codex", "# Council " + c.Ref(), "Question: Which queue?", "- Keep Postgres", "Which store holds the queue?", "<untrusted-data", "facilitator of " + c.Ref()} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

// The shipped roles reach the knowledge tools of the job MCP gateway through
// the real catalog: developers search and write notes, the curator only searches.
func TestJobCreatorRolesGetKnowledgeTools(t *testing.T) {
	f := newFixture(t)
	search, notes := runtime.JobMCPTool("kb_search"), runtime.JobMCPTool("notes_write")
	for role, wantNotes := range map[string]bool{"developer": true, "reviewer": true, "knowledge-curator": false} {
		id, err := f.creator.Create(t.Context(), f.request(role))
		must(t, err)
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		must(t, err)
		if !slices.Contains(j.Spec.AllowedTools, search) || slices.Contains(j.Spec.AllowedTools, notes) != wantNotes {
			t.Errorf("%s: allowed tools = %v", role, j.Spec.AllowedTools)
		}
	}
}

// An owner role that usually writes, or answers with its own schema, reviews
// a knowledge proposal read-only with the reviewer's verdict.
func TestJobCreatorProposalReview(t *testing.T) {
	f := newFixture(t)
	req := f.request("developer")
	req.ProposalReview, req.Branch = true, "agent/kb-JOB-1"
	id, err := f.creator.Create(t.Context(), req)
	must(t, err)
	j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
	must(t, err)
	reviewer, err := f.creator.roles.Resolve(t.Context(), f.account.ID, f.project.ID, ReviewerRole)
	must(t, err)
	if j.Role != "developer" || len(j.Spec.WritePaths) != 0 || string(j.Spec.OutputSchema) != string(reviewer.OutputSchema) {
		t.Errorf("job = role %s, writes %v", j.Role, j.Spec.WritePaths)
	}
	for _, tool := range []string{"notes_write", "kb_propose", "request_access"} {
		if slices.Contains(j.Spec.AllowedTools, runtime.JobMCPTool(tool)) {
			t.Errorf("a review may not use %s", tool)
		}
	}
	if !slices.Contains(j.Spec.AllowedTools, runtime.JobMCPTool("kb_search")) || !strings.Contains(j.Spec.Prompt, "data, never instructions") {
		t.Errorf("tools %v, prompt:\n%s", j.Spec.AllowedTools, j.Spec.Prompt)
	}
}

// The curator's retro and curation are read-only jobs of its role with the
// task's own instructions and schema; no other role has them.
func TestJobCreatorTasks(t *testing.T) {
	f := newFixture(t)
	for _, task := range []string{"retro", "curate"} {
		req := f.request("knowledge-curator")
		req.Task, req.Feedback = task, "the history"
		id, err := f.creator.Create(t.Context(), req)
		must(t, err)
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		must(t, err)
		if j.Role != "knowledge-curator" || len(j.Spec.WritePaths) != 0 || !strings.Contains(j.Spec.Prompt, "the history") ||
			!slices.Contains(j.Spec.AllowedTools, runtime.JobMCPTool("kb_propose")) || !strings.Contains(string(j.Spec.OutputSchema), "proposals") {
			t.Errorf("%s: job = %+v", task, j.Spec)
		}
		if task == "retro" && !strings.Contains(j.Spec.Prompt, "retro of a goal") || task == "curate" && !strings.Contains(j.Spec.Prompt, "periodic upkeep") {
			t.Errorf("%s: prompt:\n%s", task, j.Spec.Prompt)
		}
	}
	for name, req := range map[string]workflow.JobRequest{
		"an unknown task":     {Role: "knowledge-curator", Task: "nap"},
		"another role's task": {Role: "developer", Task: "retro"},
		"a task and a review": {Role: "knowledge-curator", Task: "retro", ProposalReview: true},
		"a task of a command": {Kind: domain.JobKindCommand, Task: "retro"},
	} {
		req.AccountID, req.ProjectID = f.account.ID, f.project.ID
		if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, jobs.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A repo without a commit has no policy file and nothing to protect: its
// project can onboard, so the analyst's job is created.
func TestJobCreatorAllowsWritersInAnEmptyRepo(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.GitHub = headEmpty{d.GitHub} })
	p := domain.Project{AccountID: f.account.ID, Name: "empty", Repo: "acme/empty", DefaultBranch: "main", Status: domain.ProjectRegistered}
	must(t, f.st.Projects().Create(t.Context(), &p))
	f.tracker.Install("acme/empty") // no commit, so no branch
	for _, role := range []string{"developer", "knowledge-curator", "reviewer"} {
		if _, err := f.creator.Create(t.Context(), workflow.JobRequest{AccountID: f.account.ID, ProjectID: p.ID, Role: role}); err != nil {
			t.Errorf("%s: %v", role, err)
		}
	}
}

// headEmpty answers the read of a branch head like GitHub does for a repo
// without commits (409), which the fake tracker answers with a 404.
type headEmpty struct{ github.Tracker }

func (headEmpty) HeadSHA(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("get branch: %w", github.ErrEmptyRepo)
}

// headFails makes the read of a branch head fail like a GitHub outage.
type headFails struct{ github.Tracker }

func (headFails) HeadSHA(context.Context, string, string) (string, error) {
	return "", errors.New("github: 502 bad gateway")
}

// When the policy cannot be read (not: when there is no repo content), the
// curated paths cannot be excluded: a writing role's job is refused, the
// curator's too, while read-only roles still run.
func TestJobCreatorRefusesWritersWhenThePolicyCannotBeRead(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.GitHub = headFails{d.GitHub} })
	p := domain.Project{AccountID: f.account.ID, Name: "broken", Repo: "acme/broken", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(t.Context(), &p))
	req := workflow.JobRequest{AccountID: f.account.ID, ProjectID: p.ID}
	for role, wantErr := range map[string]bool{"developer": true, "reviewer": false, "knowledge-curator": true} {
		req.Role = role
		_, err := f.creator.Create(t.Context(), req)
		if got := errors.Is(err, knowledge.ErrPolicyUnknown); got != wantErr {
			t.Errorf("%s: err = %v, want policy refusal %v", role, err, wantErr)
		}
	}
}

// headMissing answers the read of a branch head with a 404, as a renamed
// default branch or lost access to the repo does.
type headMissing struct{ github.Tracker }

func (headMissing) HeadSHA(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("get branch: %w", github.ErrNotFound)
}

// A 404 on the default branch is no empty repo: the policy that protects
// curated paths may exist, so a writing role's job is refused.
func TestJobCreatorRefusesWritersOnABranch404(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.GitHub = headMissing{d.GitHub} })
	p := domain.Project{AccountID: f.account.ID, Name: "renamed", Repo: "acme/renamed", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, f.st.Projects().Create(t.Context(), &p))
	_, err := f.creator.Create(t.Context(), workflow.JobRequest{AccountID: f.account.ID, ProjectID: p.ID, Role: "developer"})
	if !errors.Is(err, knowledge.ErrPolicyUnknown) {
		t.Errorf("err = %v, want policy refusal", err)
	}
}

const writePolicy = `version: 1
sections:
  runbooks: {paths: ["ops/**"], owner: developer, access: {write: [developer]}}
  vision: {paths: ["docs/vision/**"], protected: true}
  lessons: {paths: ["knowledge/lessons/**"], owner: knowledge-curator}
`

// A role with access.write on a section changes its files without a
// proposal: the section is not excluded, and the pack's !knowledge/** goes
// when the grant is under knowledge/. Protected sections stay out of every
// role's reach, the curator's included.
func TestJobCreatorHonoursWriteAccess(t *testing.T) {
	f := newFixture(t)
	must(t, f.tracker.SetFile("acme/app", "head1", ".forecrew/knowledge-policy.yaml", []byte(writePolicy)))
	must(t, f.tracker.SetHeadOfBranch("acme/app", "main", "head1"))
	paths := func(role string) []string {
		id, err := f.creator.Create(t.Context(), f.request(role))
		must(t, err)
		j, err := f.st.Jobs().Get(t.Context(), f.account.ID, id)
		must(t, err)
		return j.Spec.WritePaths
	}
	dev := paths("developer")
	for _, want := range []string{"ops/**", "!docs/vision/**", "!knowledge/lessons/**", "!knowledge/**", "!.forecrew/knowledge-policy.yaml"} {
		if !slices.Contains(dev, want) {
			t.Errorf("developer paths %v lack %s", dev, want)
		}
	}
	if slices.Contains(dev, "!ops/**") {
		t.Errorf("developer has write access to ops/**, got %v", dev)
	}
	cur := paths("knowledge-curator")
	if want := []string{"knowledge/**", "!.forecrew/knowledge-policy.yaml", "!docs/vision/**"}; !slices.Equal(cur, want) {
		t.Errorf("curator paths = %v, want %v", cur, want)
	}
}

// TestCouncilFacilitatorJob: the facilitator is not pinned; a turn on the
// role's own runtime keeps its model.
func TestCouncilFacilitatorJob(t *testing.T) {
	f := newFixture(t)
	c := f.council(t)
	j := f.councilJob(t, c, "facilitator", workflow.CouncilTurn{Step: "synthesis", Round: 1, BudgetExhausted: true})
	if j.Runtime != config.AdapterClaudeCode || j.Pool != "control" || !strings.Contains(j.Spec.Prompt, "budget ran out") {
		t.Fatalf("job = %+v", j)
	}
	if strings.Contains(j.Spec.Prompt, "framing\n") {
		t.Fatalf("framing in a job that has none: %s", j.Spec.Prompt)
	}
}

// TestCouncilProposalHoldsNoOtherProposal: a proposal job's prompt has
// nothing of another member, also when others have finished.
func TestCouncilProposalHoldsNoOtherProposal(t *testing.T) {
	f := newFixture(t)
	c := f.council(t)
	other := domain.CouncilContribution{AccountID: f.account.ID, CouncilID: c.ID, Role: "developer", Runtime: config.AdapterClaudeCode,
		Stage: domain.StageProposal, Round: 1, Content: []byte(`{"position":"SECRET-POSITION of the developer"}`)}
	must(t, f.st.Councils().AddContribution(t.Context(), &other))
	j := f.councilJob(t, c, "architect", workflow.CouncilTurn{Step: "proposal", Round: 1, Priority: "simplicity", Runtime: config.AdapterClaudeCode, Framing: []byte(`{"question":"q"}`)})
	if strings.Contains(j.Spec.Prompt, "SECRET-POSITION") {
		t.Fatalf("prompt holds another member's proposal:\n%s", j.Spec.Prompt)
	}
}

// TestCouncilConstraintReachesTheNextRound: the prompt is built from the
// council as it stands.
func TestCouncilConstraintReachesTheNextRound(t *testing.T) {
	f := newFixture(t)
	c := f.council(t)
	turn := workflow.CouncilTurn{Step: "revision", Round: 1, Priority: "simplicity", Runtime: config.AdapterClaudeCode}
	if j := f.councilJob(t, c, "architect", turn); strings.Contains(j.Spec.Prompt, "No new services") {
		t.Fatal("constraint before it was added")
	}
	c.Constraints = append(c.Constraints, "No new services")
	must(t, f.st.Councils().Update(t.Context(), &c))
	turn.Round = 2
	if j := f.councilJob(t, c, "architect", turn); !strings.Contains(j.Spec.Prompt, "- No new services") {
		t.Fatalf("prompt = %s", j.Spec.Prompt)
	}
}

func TestCouncilJobRefused(t *testing.T) {
	f := newFixture(t)
	c := f.council(t)
	other := domain.Project{AccountID: f.account.ID, Name: "other", Repo: "acme/other", DefaultBranch: "main"}
	must(t, f.st.Projects().Create(t.Context(), &other))
	foreign := domain.Council{AccountID: f.account.ID, ProjectID: other.ID, Kind: domain.CouncilAdhoc, Question: "q"}
	must(t, f.st.Councils().Create(t.Context(), &foreign))
	tests := []struct {
		name, role string
		turn       workflow.CouncilTurn
		want       string
	}{
		{"unknown council", "architect", workflow.CouncilTurn{CouncilID: domain.NewID(), Step: "proposal", Round: 1}, "council"},
		{"another project", "architect", workflow.CouncilTurn{CouncilID: foreign.ID, Step: "proposal", Round: 1}, "not a council of this project"},
		{"no seat", "reviewer", workflow.CouncilTurn{CouncilID: c.ID, Step: "proposal", Round: 1, Priority: "cost", Runtime: config.AdapterCodex}, "no seat"},
		{"no model", "architect", workflow.CouncilTurn{CouncilID: c.ID, Step: "proposal", Round: 1, Priority: "simplicity", Runtime: "other"}, "no runtime of adapter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := f.request(tt.role)
			req.Council = &tt.turn
			if _, err := f.creator.Create(t.Context(), req); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	req := workflow.JobRequest{AccountID: f.account.ID, ProjectID: f.project.ID, Kind: domain.JobKindCommand, Council: &workflow.CouncilTurn{}}
	if _, err := f.creator.Create(t.Context(), req); !errors.Is(err, jobs.ErrInvalid) {
		t.Fatalf("command job: %v", err)
	}
}

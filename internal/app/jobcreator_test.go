package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobs"
	"github.com/forecrew/forecrew/internal/roles"
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
	if !slices.Equal(s.WritePaths, role.WritePaths) || len(s.WritePaths) == 0 {
		t.Errorf("write_paths = %v, want the role's %v", s.WritePaths, role.WritePaths)
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

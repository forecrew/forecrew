package playbooks_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/knowledge/proposals"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/workflow"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// env is a store with one account and a registered project acme/app, a
// tracker with the App installed on it, a gate service and the registered
// actions.
type env struct {
	clock   *testkit.Clock
	st      store.Store
	bus     *testkit.SignalBus
	gates   *testkit.Gates
	tracker *testkit.Tracker
	// wrapped, when set, is the tracker the actions use instead of tracker.
	wrapped github.Tracker
	// knowledge, when set, is what knowledge.seed indexes with.
	knowledge playbooks.KnowledgeSeeder
	// proposals, when set, serves the knowledge.* actions of proposals.
	proposals *proposals.Service
	actions   workflow.Actions
	account   uuid.UUID
	project   domain.Project
}

// newEnv returns an env whose project has status (registered by default).
func newEnv(t *testing.T, status ...domain.ProjectStatus) *env {
	t.Helper()
	clock := testkit.NewClock(t0)
	st := testkit.NewStore(clock.Now)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	p := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectRegistered}
	if len(status) > 0 {
		p.Status = status[0]
	}
	must(t, st.Projects().Create(t.Context(), &p))
	bus := testkit.NewSignalBus(st)
	v := &env{
		clock: clock, st: st, bus: bus, gates: testkit.NewGates(st, bus, clock.Now), tracker: testkit.NewTracker(clock.Now),
		actions: workflow.Actions{}, account: acc.ID, project: p,
	}
	v.tracker.Install("acme/app")
	v.register()
	return v
}

// register (re)registers the actions with the env's current fakes.
func (v *env) register() {
	var tr github.Tracker = v.tracker
	if v.wrapped != nil {
		tr = v.wrapped
	}
	playbooks.Register(v.actions, playbooks.Deps{
		Store: v.st, Tracker: tr, Signals: v.bus, Sizing: config.DefaultSizing(), Roles: testRoles(), Knowledge: v.knowledge, Proposals: v.proposals, Now: v.clock.Now,
	})
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	var m map[string]any
	must(t, json.Unmarshal(b, &m))
	return m
}

// call runs the action name with a call that has a fresh key, or key when
// given, and the project of the env.
func (v *env) call(t *testing.T, name string, with map[string]any) (any, error) {
	t.Helper()
	return v.callKey(t, name, domain.NewID().String(), with)
}

func (v *env) callKey(t *testing.T, name, key string, with map[string]any) (any, error) {
	t.Helper()
	action := v.actions[name]
	if action == nil {
		t.Fatalf("action %q is not registered", name)
	}
	if head, _ := with["head"].(string); name == "github.open_or_update_pr" && head != "" {
		must(t, v.tracker.AddBranch("acme/app", head)) // the worker pushed it
	}
	run := domain.WorkflowRun{ID: domain.NewID(), AccountID: v.account, ProjectID: v.project.ID, Playbook: "onboarding"}
	return action(context.Background(), workflow.ActionCall{Key: key, Run: run, With: with})
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("error = %v, want one containing %q", err, substr)
	}
}

// testRoles resolves the roles whose write paths the actions check, from the
// shipped role pack.
func testRoles() roles.Resolver {
	defs, err := roles.Load(forecrew.Files)
	if err != nil {
		panic(err)
	}
	out := testkit.Roles{}
	for name, d := range defs {
		out[name] = roles.Role{Name: name, WritePaths: d.WritePaths}
	}
	return out
}

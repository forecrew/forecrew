package resource_test

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/jobmcp"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/runtime"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
)

// TestRegistry: every area of the task is there, with all five functions.
func TestRegistry(t *testing.T) {
	want := []string{"organization", "member", "team", "role_binding", "connection", "pool", "shape", "provider", "fleet_policy", "integration", "knowledge_policy", "schedule",
		"runtime", "auth", "domain", "repo_file", "model_connection", "model_assignment",
		"tracker_connection", "tracker_route"}
	for _, kind := range want {
		a, ok := resource.Lookup(kind)
		if !ok {
			t.Errorf("no area %s", kind)
			continue
		}
		if a.Schema == nil || a.Read == nil || a.Validate == nil || a.Diff == nil || a.Apply == nil || a.Normalize == nil || a.Needs == nil {
			t.Errorf("area %s is incomplete", kind)
		}
		if !a.ReadPermission.Valid() || a.Title == "" || a.Affects == "" {
			t.Errorf("area %s: read permission %q, title %q, affects %q", kind, a.ReadPermission, a.Title, a.Affects)
		}
		var schema map[string]any
		if err := json.Unmarshal(a.Schema(), &schema); err != nil || schema["type"] != "object" {
			t.Errorf("area %s: schema %s", kind, a.Schema())
		}
	}
	if len(resource.Areas()) != len(want) {
		t.Errorf("%d areas, want %d: a new area goes in this list", len(resource.Areas()), len(want))
	}
}

func fieldNames(v any, tag string) []string {
	var out []string
	typ := reflect.TypeOf(v)
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get(tag), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// TestEverySettingHasAResource fails when a new field of a setting has no
// place in its spec and is not named as no setting, and when forecrew.yaml
// gets a key that is neither a resource nor a bootstrap setting.
func TestEverySettingHasAResource(t *testing.T) {
	for _, a := range resource.Areas() {
		if a.Setting == nil {
			continue
		}
		tag := "json"
		if strings.Contains(reflect.TypeOf(a.Setting).PkgPath(), "internal/config") && !strings.Contains(reflect.TypeOf(a.Setting).PkgPath(), "resource") {
			tag = "yaml"
		}
		have := fieldNames(a.Spec, "json")
		for _, f := range fieldNames(a.Setting, tag) {
			if a.Kind == resource.KindOrganization && f == "max_token_ttl" {
				continue // spec key of the same name
			}
			if !slices.Contains(have, f) && !slices.Contains(a.Hidden, f) && !slices.Contains(aliases[a.Kind], f) {
				t.Errorf("%s: field %s of %T has no place in the spec and is not hidden", a.Kind, f, a.Setting)
			}
		}
	}
	bootstrap := map[string]string{
		"server": "where forecrewd listens", "github": "the GitHub App", "evidence": "artifact storage", "fleet": "the instance's currency",
		"sizing": "moves with roles", "policy": "moves with roles", "model_tiers": "the default of model_assignment", "roles": "the default of model_assignment",
		"max_test_results_size": "a limit of the server", "actions": "a limit of the server", "testing": "the projects on the testing playbook, a rollout switch",
		"goal_v3":   "the projects on the councils and learning playbook, a rollout switch",
		"knowledge": "how old knowledge may get, a tuning value of the server",
		"trackers":  "a limit of the server; connections and routes are resources from T74",
		"models":    "what the server lends organizations, a choice of the operator",
		"advisor":   "how many proposals a week, a tuning value of the server",
	}
	for _, key := range fieldNames(config.Config{}, "yaml") {
		_, isBootstrap := bootstrap[key]
		_, isResource := map[string]bool{"pools": true, "runtimes": true, "auth": true, "schedules": true}[key]
		if !isBootstrap && !isResource {
			t.Errorf("forecrew.yaml key %s is neither a resource nor a bootstrap setting", key)
		}
	}
}

// aliases are fields the spec holds under another name.
var aliases = map[string][]string{
	resource.KindConnection: {"kind"},
	resource.KindTeam:       {"name"},
	resource.KindDomain:     {"enforce_sso"},
	resource.KindProvider:   {"kind"},
	resource.KindMember:     {"status"},
}

func team(name string) map[string]any {
	return map[string]any{"display_name": name, "members": []string{}}
}

func TestPlanDiffAndApply(t *testing.T) {
	w := newWorld(t)
	p := w.plan(w.owner, wanted(t, "team", "core", team("Core")))
	if p.Status != domain.ConfigPlanPending || len(p.Changes) != 1 || p.Changes[0].Op != domain.ConfigCreate || p.Changes[0].Line != "+ Team core" {
		t.Fatalf("plan = %+v", p)
	}
	if !slices.Contains(p.Needs, domain.PermTeamManage) || p.Affects == "" || p.Cost != "Free." || !p.Reversible || p.Applies != "now" {
		t.Errorf("plan = %+v", p)
	}
	if got := w.spec("team", "core"); got != "" {
		t.Fatalf("a plan changed something: %s", got)
	}
	w.apply(w.owner, p)
	if got := w.spec("team", "core"); !strings.Contains(got, "Core") {
		t.Fatalf("team = %s", got)
	}
	p = w.plan(w.owner, wanted(t, "team", "core", team("Base")))
	c := p.Changes[0]
	if c.Op != domain.ConfigUpdate || len(c.Fields) != 1 || c.Fields[0].Path != "display_name" || c.Fields[0].Old != "Core" || c.Fields[0].New != "Base" ||
		!strings.HasPrefix(c.Line, "~ Team core") {
		t.Errorf("change = %+v", c)
	}
	// Nothing to change: no plan is stored.
	if p := w.plan(w.owner, wanted(t, "team", "core", team("Core"))); p.ID.String() != "00000000-0000-0000-0000-000000000000" || len(p.Changes) != 0 {
		t.Errorf("empty plan = %+v", p)
	}
	entries, err := w.st.Audit().List(t.Context(), w.account.ID, store.AuditFilter{})
	check(t, err)
	if !slices.ContainsFunc(entries, func(e domain.AuditEntry) bool {
		return e.Action == "config.plan.applied" && e.Actor == w.owner.String()
	}) {
		t.Errorf("no audit entry as the person: %+v", entries)
	}
}

func TestPlanValidation(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	for name, c := range map[string]mcp.ConfigWanted{
		"unknown kind":   wanted(t, "nope", "x", team("x")),
		"unknown key":    wanted(t, "team", "core", map[string]any{"display_name": "x", "invented": true}),
		"no spec":        {Kind: "team", Name: "core"},
		"bad value":      wanted(t, "team", "Bad Slug", team("x")),
		"delete nothing": {Kind: "team", Name: "ghost", Delete: true},
		"no delete":      {Kind: "organization", Name: "organization", Delete: true},
		"file value":     {Kind: "runtime", Name: "claude", Delete: true},
		"bad schedule":   wanted(t, "schedule", "curate:acme/app", map[string]any{"playbook": "curate", "project": "acme/app", "cron": "nope"}),
		"schedule name":  wanted(t, "schedule", "other", map[string]any{"playbook": "curate", "project": "acme/app", "cron": "@daily"}),
		"file schedule":  {Kind: "schedule", Name: "curate:*", Delete: true},
		"bad pool":       wanted(t, "pool", "gpu", map[string]any{"kinds": []string{}}),
		"binding name":   wanted(t, "role_binding", "x", map[string]any{"subject_kind": "team", "subject": "core", "role": "viewer"}),
		"repo file path": wanted(t, "repo_file", "app:.github/workflows/x.yml", map[string]any{"content": "x"}),
		"secret in file": wanted(t, "repo_file", "app:.forecrew/x.yaml", map[string]any{"content": "sk-ant-abcdefghijklmnop"}),
		"provider kind":  wanted(t, "provider", "home", map[string]any{"kind": "nope"}),
		"shape no size":  wanted(t, "shape", "s", map[string]any{"provider": "home", "os": "linux", "arch": "amd64"}),
		"policy name":    wanted(t, "fleet_policy", "other", map[string]any{"routing": "own_only", "plan": "free"}),
		"policy routing": wanted(t, "fleet_policy", "fleet", map[string]any{"routing": "nope", "plan": "free"}),
		"connection ref": wanted(t, "connection", "okta", map[string]any{"kind": "oidc", "issuer": "https://i", "client_id": "c", "client_secret_ref": "plain-value"}),
		"new member":     wanted(t, "member", "not-an-email", map[string]any{"status": "active"}),
		"sso no connect": wanted(t, "domain", "acme.com", map[string]any{"enforce_sso": true}),
	} {
		_, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{c}})
		if !strings.Contains(errText(err), "invalid") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Title: "ghp_abcdefghijklmnopqrstuvwxyz1234", Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "team", "core", team("x"))}})
	wantErr(t, err, mcp.ErrInvalidInput)
	if rows, _ := w.st.ConfigPlans().List(ctx, w.account.ID, store.ConfigPlanFilter{}); len(rows) != 0 {
		t.Errorf("%d plans stored", len(rows))
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestPermissionRefusal(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	viewer, member := w.person(domain.RoleViewer), w.person(domain.RoleMember)
	// A Viewer gets answers without plans.
	_, err := w.engine.Plan(ctx, viewer, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "team", "core", team("Core"))}})
	wantErr(t, err, authz.ErrDenied)
	// A Member plans, cannot apply, and asks for approval.
	p := w.plan(member, wanted(t, "team", "core", team("Core")))
	_, err = w.engine.Apply(ctx, member, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied)
	v, err := w.engine.GetPlan(ctx, member, p.ID)
	check(t, err)
	if v.CanConfirm {
		t.Error("CanConfirm for a Member")
	}
	v, err = w.engine.RequestApproval(ctx, member, p.ID)
	check(t, err)
	if v.Status != domain.ConfigPlanAwaiting {
		t.Errorf("status = %s", v.Status)
	}
	inbox, err := w.engine.ListPlans(ctx, w.owner, true, 0)
	check(t, err)
	if len(inbox) != 1 || !inbox[0].CanConfirm {
		t.Fatalf("inbox of the Owner = %+v", inbox)
	}
	if inbox, _ := w.engine.ListPlans(ctx, member, true, 0); len(inbox) != 0 {
		t.Errorf("inbox of the Member = %+v", inbox)
	}
	w.apply(w.owner, p)
	got, err := w.st.ConfigPlans().Get(ctx, w.account.ID, p.ID)
	check(t, err)
	if got.AppliedBy != w.owner.String() || got.CreatedBy != member.String() {
		t.Errorf("plan = %+v", got)
	}
	// Service accounts never confirm.
	sa := domain.ServiceAccountActor(w.account.ID, domain.NewID(), domain.NewID())
	p = w.plan(w.owner, wanted(t, "team", "next", team("Next")))
	_, err = w.engine.Apply(ctx, sa, resource.Confirmation{Plan: p.ID})
	if err == nil {
		t.Error("a service account confirmed a plan")
	}
	// An area the caller cannot read cannot be planned.
	billing := w.person(domain.RoleBilling)
	_, err = w.engine.Plan(ctx, billing, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "pool", "s", map[string]any{})}})
	wantErr(t, err, authz.ErrDenied)
	rs, err := w.engine.Get(ctx, billing, "", "")
	check(t, err)
	if slices.ContainsFunc(rs, func(r mcp.ConfigResource) bool { return r.Kind == "pool" }) {
		t.Errorf("Billing reads pools: %+v", rs)
	}
	_, err = w.engine.Get(ctx, billing, "pool", "")
	wantErr(t, err, authz.ErrDenied)
}

// TestNoEscalationThroughPlans: the tools under the areas keep their rule.
func TestNoEscalationThroughPlans(t *testing.T) {
	w := newWorld(t)
	admin := w.person(domain.RoleAdmin)
	u := w.st.AddMember(w.account.ID, domain.User{Name: "x"}, "")
	subject := u.ID.String()
	name := "user:" + subject + "/owner"
	p := w.plan(admin, wanted(t, "role_binding", name, map[string]any{"subject_kind": "user", "subject": subject, "role": "owner"}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("adding an Owner: %v", p.LowersProtection)
	}
	_, err := w.engine.Apply(t.Context(), admin, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
	wantErr(t, err, authz.ErrDenied)
	if got := w.spec("role_binding", name); got != "" {
		t.Errorf("binding = %s", got)
	}
	entries, err := w.st.Audit().List(t.Context(), w.account.ID, store.AuditFilter{})
	check(t, err)
	if !slices.ContainsFunc(entries, func(e domain.AuditEntry) bool { return e.Action == "config.plan.denied" }) {
		t.Error("the denial left no audit entry")
	}
}

func TestBudgetFields(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	admin, billing := w.person(domain.RoleAdmin), w.person(domain.RoleBilling)
	home := domain.CapacityProvider{AccountID: w.account.ID, Name: "home", Kind: domain.ProviderMicroVM, Mode: domain.ProviderModeAgent}
	check(t, w.st.CapacityProviders().Create(ctx, &home))
	provider := func() domain.CapacityProvider {
		t.Helper()
		got, err := w.st.CapacityProviders().Get(ctx, home.ID)
		check(t, err)
		return got
	}
	budget := 100.0
	withBudget := map[string]any{"kind": "microvm", "mode": "agent", "limits": map[string]any{"monthly_budget": budget}}
	withMax := map[string]any{"kind": "microvm", "mode": "agent", "limits": map[string]any{"max_machines": 3}}

	// worker.manage alone cannot change a budget field.
	p := w.plan(admin, wanted(t, "provider", "home", withBudget))
	if !slices.Contains(p.Needs, domain.PermBillingManage) || slices.Contains(p.Needs, domain.PermWorkerManage) {
		t.Fatalf("needs = %v", p.Needs)
	}
	_, err := w.engine.Apply(ctx, admin, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied)
	if provider().Limits.MonthlyBudget != nil {
		t.Fatal("the budget changed")
	}
	// billing.manage alone changes it.
	w.apply(billing, p)
	if got := provider().Limits.MonthlyBudget; got == nil || *got != budget {
		t.Fatalf("budget = %v", got)
	}
	// A plan on both needs both, and an Admin cannot do half of it.
	both := map[string]any{"kind": "microvm", "mode": "agent", "limits": map[string]any{"max_machines": 3}}
	p = w.plan(admin, wanted(t, "provider", "home", both))
	if !slices.Contains(p.Needs, domain.PermBillingManage) || !slices.Contains(p.Needs, domain.PermWorkerManage) {
		t.Fatalf("needs = %v", p.Needs)
	}
	check(t, func() error { _, err := w.engine.Cancel(ctx, admin, p.ID); return err }())
	// Quotas of the policy and the budget of a shape are the same.
	p = w.plan(w.owner, wanted(t, "fleet_policy", "fleet", map[string]any{"routing": "own_only", "plan": "free", "quotas": map[string]any{"concurrent_machines": 2}}))
	_, err = w.engine.Apply(ctx, admin, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied)
	w.apply(w.owner, p)
	// billing.manage changes nothing else of the row.
	p = w.plan(billing, wanted(t, "provider", "home", withMax))
	_, err = w.engine.Apply(ctx, billing, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied)
}

func TestExpiryAndStale(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	p := w.plan(w.owner, wanted(t, "team", "core", team("Core")))
	w.now = w.now.Add(resource.DefaultPlanTTL + time.Minute)
	v, err := w.engine.GetPlan(ctx, w.owner, p.ID)
	check(t, err)
	if v.Status != domain.ConfigPlanExpired {
		t.Errorf("status = %s", v.Status)
	}
	_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, mcp.ErrConflict)
	if inbox, _ := w.engine.ListPlans(ctx, w.owner, true, 0); len(inbox) != 0 {
		t.Errorf("an expired plan in the inbox: %+v", inbox)
	}

	a := w.plan(w.owner, wanted(t, "team", "one", team("One")))
	b := w.plan(w.owner, wanted(t, "team", "one", team("Other")))
	w.apply(w.owner, a)
	v, err = w.engine.GetPlan(ctx, w.owner, b.ID)
	check(t, err)
	if !v.Stale {
		t.Error("not stale")
	}
	_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: b.ID})
	wantErr(t, err, mcp.ErrConflict)
	_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: a.ID})
	wantErr(t, err, mcp.ErrConflict) // applied once
}

// TestApplyIsOneStep: a change that fails takes the others with it.
func TestApplyIsOneStep(t *testing.T) {
	w := newWorld(t)
	p := w.plan(w.owner, wanted(t, "team", "core", team("Core")),
		wanted(t, "team", "ghosts", map[string]any{"display_name": "G", "members": []string{domain.NewID().String()}}))
	_, err := w.engine.Apply(t.Context(), w.owner, resource.Confirmation{Plan: p.ID})
	if err == nil {
		t.Fatal("no error")
	}
	if got := w.spec("team", "core"); got != "" {
		t.Errorf("half a plan applied: %s", got)
	}
	got, _ := w.st.ConfigPlans().Get(t.Context(), w.account.ID, p.ID)
	if got.Status != domain.ConfigPlanPending {
		t.Errorf("status = %s", got.Status)
	}
}

func TestRevertRestoresValues(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.apply(w.owner, w.plan(w.owner, wanted(t, "team", "core", team("Core"))))
	before := w.spec("team", "core")
	first := w.plan(w.owner, wanted(t, "team", "core", team("Base")), wanted(t, "team", "added", team("Added")),
		wanted(t, "schedule", "curate:acme/app", map[string]any{"playbook": "curate", "project": "acme/app", "cron": "@daily"}))
	w.apply(w.owner, first)
	r, err := w.engine.Revert(ctx, w.owner, first.ID, domain.ConfigFromForm, "")
	check(t, err)
	if r.Reverts != first.ID || len(r.Changes) != 3 {
		t.Fatalf("revert = %+v", r)
	}
	if !strings.Contains(w.spec("team", "core"), "Base") {
		t.Fatal("the revert applied before it was confirmed")
	}
	w.apply(w.owner, r)
	if got := w.spec("team", "core"); got != before {
		t.Errorf("team = %s, want %s", got, before)
	}
	if w.spec("team", "added") != "" || w.spec("schedule", "curate:acme/app") != "" {
		t.Error("created resources are still there")
	}
	got, _ := w.st.ConfigPlans().Get(ctx, w.account.ID, first.ID)
	if got.Status != domain.ConfigPlanReverted {
		t.Errorf("status = %s", got.Status)
	}
	_, err = w.engine.Revert(ctx, w.owner, first.ID, domain.ConfigFromForm, "")
	wantErr(t, err, mcp.ErrConflict)
	// A deleted resource comes back.
	del := w.plan(w.owner, mcp.ConfigWanted{Kind: "team", Name: "core", Delete: true})
	if del.Reversible {
		t.Error("deleting a team loses its roles")
	}
	w.apply(w.owner, del)
	r, err = w.engine.Revert(ctx, w.owner, del.ID, domain.ConfigFromForm, "")
	check(t, err)
	w.apply(w.owner, r)
	if got := w.spec("team", "core"); got != before {
		t.Errorf("team = %s, want %s", got, before)
	}
}

func TestLoweringProtection(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	second := w.person(domain.RoleOwner)
	name := "user:" + subjectOf(second) + "/owner"
	p := w.plan(w.owner, mcp.ConfigWanted{Kind: "role_binding", Name: name, Delete: true})
	if len(p.LowersProtection) != 1 || p.ConfirmWord != "acme" {
		t.Fatalf("plan = %+v", p)
	}
	for _, word := range []string{"", "yes", "ACME"} {
		_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: word, Browser: true})
		wantErr(t, err, mcp.ErrInvalidInput)
	}
	// Whatever its source, it is not confirmed with a token.
	_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord})
	wantErr(t, err, mcp.ErrConflict)
	w.apply(w.owner, p)

	// Cloud fallback and open egress, and the revert of tightening them.
	open := map[string]any{"routing": "own_first", "plan": "free", "egress": []string{"example.com"}}
	p = w.plan(w.owner, wanted(t, "fleet_policy", "fleet", open))
	if len(p.LowersProtection) != 2 {
		t.Fatalf("reasons = %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	tight := w.plan(w.owner, wanted(t, "fleet_policy", "fleet", map[string]any{"routing": "own_only", "plan": "free"}))
	if len(tight.LowersProtection) != 0 {
		t.Errorf("tightening lowers protection: %v", tight.LowersProtection)
	}
	w.apply(w.owner, tight)
	r, err := w.engine.Revert(ctx, w.owner, tight.ID, domain.ConfigFromForm, "")
	check(t, err)
	if len(r.LowersProtection) != 2 {
		t.Errorf("the revert of tightening: %v", r.LowersProtection)
	}
	// SSO enforcement.
	conn := domain.Connection{AccountID: w.account.ID, Kind: domain.ConnectionOIDC, Name: "okta", Issuer: "https://i.example.com", ClientID: "c", ClientSecretRef: "store:conn-okta", JIT: domain.JIT{DefaultRoles: []uuid.UUID{}}, Enabled: true}
	check(t, w.st.Connections().Create(ctx, &conn))
	verified := domain.VerifiedDomain{AccountID: w.account.ID, Domain: "acme.com", VerificationToken: "t"}
	check(t, w.st.VerifiedDomains().Create(ctx, &verified))
	check(t, w.st.VerifiedDomains().SetVerified(ctx, w.account.ID, verified.ID, true))
	check(t, w.st.VerifiedDomains().SetEnforcement(ctx, w.account.ID, verified.ID, true, conn.ID))
	p = w.plan(w.owner, wanted(t, "domain", "acme.com", map[string]any{"enforce_sso": false}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	if got, _ := w.st.VerifiedDomains().Get(ctx, w.account.ID, verified.ID); got.EnforceSSO {
		t.Error("still enforced")
	}
}

func subjectOf(a domain.Actor) string { return a.Name }

func TestOwnerlessTokensAreMCP(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	check(t, w.st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: w.account.ID, Name: "laptop", TokenHash: mcp.HashToken("fcm_laptop")}))
	by := domain.Actor{AccountID: w.account.ID, Kind: domain.ActorMCP, Name: "laptop"}
	p := w.plan(by, wanted(t, "team", "core", team("Core"))) // says it comes from the CLI
	if p.Source != domain.ConfigFromMCP {
		t.Fatalf("source = %s", p.Source)
	}
	_, err := w.engine.Apply(ctx, by, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, mcp.ErrConflict)
	imported, err := w.engine.Import(ctx, by, "version: 1\nresources:\n- kind: team\n  name: other\n  spec: {display_name: O, members: []}\n", domain.ConfigFromYAML, "")
	check(t, err)
	if imported.Source != domain.ConfigFromMCP {
		t.Errorf("source = %s", imported.Source)
	}
}

func TestWhoConfirmsWhat(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	for _, source := range []domain.ConfigPlanSource{domain.ConfigFromMCP, domain.ConfigFromAssistant} {
		name := "from-" + string(source)
		p, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: source, Changes: []mcp.ConfigWanted{wanted(t, "team", name, team("x"))}})
		check(t, err)
		_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID})
		wantErr(t, err, mcp.ErrConflict)
		if w.spec("team", name) != "" {
			t.Errorf("%s: applied by a token", source)
		}
		_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID, Browser: true})
		check(t, err)
	}
	// A plan from an MCP client is in the Inbox without being asked for.
	p, err := w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromMCP, Changes: []mcp.ConfigWanted{wanted(t, "team", "inbox", team("x"))}})
	check(t, err)
	inbox, err := w.engine.ListPlans(ctx, w.owner, true, 0)
	check(t, err)
	if len(inbox) != 1 || inbox[0].ID != p.ID {
		t.Errorf("inbox = %+v", inbox)
	}
	v, err := w.engine.Cancel(ctx, w.owner, p.ID)
	check(t, err)
	if v.Status != domain.ConfigPlanCanceled {
		t.Errorf("status = %s", v.Status)
	}
	_, err = w.engine.Cancel(ctx, w.owner, p.ID)
	wantErr(t, err, mcp.ErrConflict)
	_, err = w.engine.RequestApproval(ctx, w.owner, p.ID)
	wantErr(t, err, mcp.ErrConflict)
}

func TestSecretRequired(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	conn := map[string]any{"kind": "oidc", "issuer": "https://acme.okta.com", "client_id": "abc", "client_secret_ref": "store:conn-okta", "jit": map[string]any{"enabled": false}, "enabled": true}
	p := w.plan(w.owner, wanted(t, "connection", "okta", conn))
	if len(p.Steps) != 1 || p.Steps[0].Kind != domain.ConfigStepSecret || p.Steps[0].Secret != "conn-okta" || p.Steps[0].Filled {
		t.Fatalf("steps = %+v", p.Steps)
	}
	_, err := w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, mcp.ErrConflict)
	_, err = w.engine.PutSecret(ctx, w.owner, p.ID, "other", []byte("v"))
	wantErr(t, err, mcp.ErrInvalidInput)
	member := w.person(domain.RoleMember)
	_, err = w.engine.PutSecret(ctx, member, p.ID, "conn-okta", []byte("v"))
	wantErr(t, err, mcp.ErrNotFound) // sign-in is not theirs to read
	v, err := w.engine.PutSecret(ctx, w.owner, p.ID, "conn-okta", []byte("the-value"))
	check(t, err)
	if !v.Steps[0].Filled {
		t.Fatal("not filled")
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "the-value") {
		t.Fatal("the value came back")
	}
	w.apply(w.owner, p)
	if got := w.spec("connection", "okta"); !strings.Contains(got, "store:conn-okta") {
		t.Errorf("connection = %s", got)
	}
	got, err := w.vault.Get(ctx, w.account.ID, "conn-okta")
	check(t, err)
	if string(got) != "the-value" {
		t.Errorf("value = %q", got)
	}
	if ok, _ := w.vault.Has(ctx, w.account.ID, secrets.StagedName(p.ID, "conn-okta")); ok {
		t.Error("the staged value is still there")
	}
	entries, _ := w.st.Audit().List(ctx, w.account.ID, store.AuditFilter{})
	for _, e := range entries {
		if strings.Contains(string(e.Data), "the-value") {
			t.Errorf("audited: %s", e.Data)
		}
	}
	yaml, err := w.engine.Export(ctx, w.owner)
	check(t, err)
	if strings.Contains(yaml, "the-value") || !strings.Contains(yaml, "store:conn-okta") {
		t.Errorf("export:\n%s", yaml)
	}
	// A connection has no delete: it is turned off, which lowers protection,
	// and so does turning it off by hand.
	off := map[string]any{}
	maps.Copy(off, conn)
	off["enabled"] = false
	if p := w.plan(w.owner, wanted(t, "connection", "okta", off)); len(p.LowersProtection) != 1 {
		t.Errorf("disabling: %v", p.LowersProtection)
	}
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "connection", Name: "okta", Delete: true})
	if len(p.LowersProtection) != 1 {
		t.Errorf("deleting: %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	if got := w.spec("connection", "okta"); !strings.Contains(got, `"enabled":false`) {
		t.Errorf("connection = %s", got)
	}
}

// TestSecretsAreStaged: entering a value touches nothing live, a step that
// names a saved secret says so, and what a dropped plan staged goes with it.
func TestSecretsAreStaged(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	acc := w.account.ID
	conn := func(ref, issuer string) map[string]any {
		return map[string]any{"kind": "oidc", "issuer": issuer, "client_id": "abc", "client_secret_ref": ref, "jit": map[string]any{"enabled": false}, "enabled": true}
	}
	check(t, w.vault.Put(ctx, w.owner, acc, "conn-live", []byte("live")))

	// The name is saved: the plan says so, and asks for nothing.
	reuse := w.plan(w.owner, wanted(t, "connection", "okta", conn("store:conn-live", "https://a.example.com")))
	if s := reuse.Steps[0]; !s.Reuses || !s.Filled || !strings.Contains(s.Detail, "already saved") {
		t.Fatalf("step = %+v", s)
	}
	_, err := w.engine.PutSecret(ctx, w.owner, reuse.ID, "conn-live", []byte("attack"))
	wantErr(t, err, mcp.ErrConflict)
	if got, _ := w.vault.Get(ctx, acc, "conn-live"); string(got) != "live" {
		t.Fatalf("a live secret changed before anyone confirmed: %q", got)
	}

	// Two plans race for a free name: the second finds it taken and
	// replaces nothing.
	first := w.plan(w.owner, wanted(t, "connection", "one", conn("store:conn-new", "https://a.example.com")))
	second := w.plan(w.owner, wanted(t, "connection", "two", conn("store:conn-new", "https://b.example.com")))
	_, err = w.engine.PutSecret(ctx, w.owner, first.ID, "conn-new", []byte("first"))
	check(t, err)
	_, err = w.engine.PutSecret(ctx, w.owner, second.ID, "conn-new", []byte("second"))
	check(t, err)
	if ok, _ := w.vault.Has(ctx, acc, "conn-new"); ok {
		t.Fatal("a value went live before its plan was confirmed")
	}
	w.apply(w.owner, first)
	_, err = w.engine.Apply(ctx, w.owner, resource.Confirmation{Plan: second.ID})
	wantErr(t, err, mcp.ErrConflict)
	if got, _ := w.vault.Get(ctx, acc, "conn-new"); string(got) != "first" {
		t.Errorf("value = %q", got)
	}
	if w.spec("connection", "two") != "" {
		t.Error("half a plan applied")
	}
	third := w.plan(w.owner, wanted(t, "connection", "three", conn("store:conn-third", "https://c.example.com")))
	_, err = w.engine.PutSecret(ctx, w.owner, third.ID, "conn-third", []byte("v"))
	check(t, err)

	// A canceled plan leaves nothing.
	staged := secrets.StagedName(second.ID, "conn-new")
	if ok, _ := w.vault.Has(ctx, acc, staged); !ok {
		t.Fatal("nothing staged")
	}
	_, err = w.engine.Cancel(ctx, w.owner, second.ID)
	check(t, err)
	if ok, _ := w.vault.Has(ctx, acc, staged); ok {
		t.Error("a canceled plan left its value")
	}

	// An expired one neither.
	w.now = w.now.Add(resource.DefaultPlanTTL + time.Minute)
	n, err := w.engine.Sweep(ctx)
	check(t, err)
	if n != 2 { // third, and reuse
		t.Errorf("swept %d", n)
	}
	if ok, _ := w.vault.Has(ctx, acc, secrets.StagedName(third.ID, "conn-third")); ok {
		t.Error("an expired plan left its value")
	}
	got, err := w.st.ConfigPlans().Get(ctx, acc, third.ID)
	check(t, err)
	if got.Status != domain.ConfigPlanExpired {
		t.Errorf("status = %s", got.Status)
	}
	if got, _ := w.vault.Get(ctx, acc, "conn-live"); string(got) != "live" {
		t.Errorf("a reused secret went with its plan: %q", got)
	}
	if n, err := w.engine.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("a second sweep: %d, %v", n, err)
	}
}

// TestAPlanCannotReachAnotherPlansStagedValue is the A/B case: plan A staged a
// value nobody confirmed, and plan B tries to send it to its own issuer.
func TestAPlanCannotReachAnotherPlansStagedValue(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	acc := w.account.ID
	conn := func(ref, issuer string) map[string]any {
		return map[string]any{"kind": "oidc", "issuer": issuer, "client_id": "abc", "client_secret_ref": ref, "jit": map[string]any{"enabled": false}, "enabled": true}
	}
	a := w.plan(w.owner, wanted(t, "connection", "a", conn("store:conn-a", "https://a.example.com")))
	_, err := w.engine.PutSecret(ctx, w.owner, a.ID, "conn-a", []byte("unconfirmed"))
	check(t, err)
	staged := secrets.StagedName(a.ID, "conn-a")

	// Plan B names it, as a connection and as an instance connection.
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{
		wanted(t, "connection", "b", conn("store:"+staged, "https://evil.example.com"))}})
	wantErr(t, err, mcp.ErrInvalidInput)
	rs, err := w.engine.Get(ctx, w.owner, "auth", "auth")
	check(t, err)
	var auth map[string]any
	check(t, json.Unmarshal(rs[0].Spec, &auth))
	auth["connections"] = map[string]any{"b": map[string]any{"kind": "oidc", "issuer": "https://evil.example.com", "client_id": "c", "client_secret_ref": "store:" + staged}}
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "auth", "auth", auth)}})
	wantErr(t, err, mcp.ErrInvalidInput)
	if rows, _ := w.st.ConfigPlans().List(ctx, acc, store.ConfigPlanFilter{Open: true}); len(rows) != 1 {
		t.Errorf("plans = %+v", rows)
	}

	// Nor can anything be created there, through a plan or the store.
	_, err = w.engine.PutSecret(ctx, w.owner, a.ID, staged, []byte("planted"))
	wantErr(t, err, mcp.ErrInvalidInput)
	wantErr(t, w.vault.Put(ctx, w.owner, acc, domain.StagedSecretPrefix+a.ID.String()+"/x", []byte("planted")), secrets.ErrInvalid)
	if ok, _ := w.vault.Has(ctx, acc, domain.StagedSecretPrefix+a.ID.String()+"/x"); ok {
		t.Error("a value was created in the reserved names")
	}

	// The staged value is untouched, and plan A still applies with it.
	w.apply(w.owner, a)
	if got, err := w.vault.Get(ctx, acc, "conn-a"); err != nil || string(got) != "unconfirmed" {
		t.Fatalf("value = %q, %v", got, err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.apply(w.owner, w.plan(w.owner, wanted(t, "team", "core", team("Core")),
		wanted(t, "schedule", "curate:acme/app", map[string]any{"playbook": "curate", "project": "acme/app", "cron": "@daily"}),
		wanted(t, "pool", "gpu", map[string]any{"kinds": []string{"agent"}, "roles": []string{"*"}, "credentials": map[string]any{"runtimes": []string{"*"}, "repo_token": "read"}})))
	first, err := w.engine.Export(ctx, w.owner)
	check(t, err)
	// What was exported matches: importing it changes nothing.
	p, err := w.engine.Import(ctx, w.owner, first, domain.ConfigFromYAML, "")
	check(t, err)
	if len(p.Changes) != 0 {
		t.Fatalf("changes: %+v", p.Changes)
	}
	// After a change, importing brings the export back.
	w.apply(w.owner, w.plan(w.owner, wanted(t, "team", "core", team("Changed"))))
	p, err = w.engine.Import(ctx, w.owner, first, domain.ConfigFromYAML, "")
	check(t, err)
	if len(p.Changes) != 1 || p.Source != domain.ConfigFromYAML {
		t.Fatalf("plan = %+v", p)
	}
	w.apply(w.owner, p)
	again, err := w.engine.Export(ctx, w.owner)
	check(t, err)
	if again != first {
		t.Errorf("export changed:\n%s\nwas:\n%s", again, first)
	}
	for name, doc := range map[string]string{"parse": ":\n- x", "version": "version: 9", "secret": "version: 1\nx: sk-ant-abcdefghijklmnop", "size": strings.Repeat("x", resource.MaxImport+1)} {
		_, err := w.engine.Import(ctx, w.owner, doc, domain.ConfigFromYAML, "")
		if !strings.Contains(errText(err), "invalid") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestServerSettings(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	rs, err := w.engine.Get(ctx, w.owner, "pool", "build")
	check(t, err)
	if len(rs) != 1 || rs[0].Origin != resource.OriginFile || rs[0].Scope != resource.ScopeServer {
		t.Fatalf("pool = %+v", rs)
	}
	var spec map[string]any
	check(t, json.Unmarshal(rs[0].Spec, &spec))
	spec["roles"] = []string{"developer"}
	p := w.plan(w.owner, wanted(t, "pool", "build", spec))
	if !strings.Contains(p.Affects, "restarts") || !slices.Contains(p.Needs, domain.PermPoolManage) {
		t.Errorf("plan = %+v", p)
	}
	w.apply(w.owner, p)
	rs, err = w.engine.Get(ctx, w.owner, "pool", "build")
	check(t, err)
	if rs[0].Origin != resource.OriginDatabase {
		t.Errorf("pool = %+v", rs[0])
	}
	// forecrewd reads it over the file at startup.
	stored, err := resource.Stored(ctx, w.st)
	check(t, err)
	cfg, err := config.Parse([]byte(serverYAML), env)
	check(t, err)
	merged, err := cfg.WithSettings(stored, env)
	check(t, err)
	if !slices.Equal(merged.Pools["build"].Roles, []string{"developer"}) {
		t.Errorf("pool = %+v", merged.Pools["build"])
	}
	// Deleting drops the stored value, and the file's counts again.
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "pool", Name: "build", Delete: true}))
	rs, _ = w.engine.Get(ctx, w.owner, "pool", "build")
	if rs[0].Origin != resource.OriginFile {
		t.Errorf("pool = %+v", rs[0])
	}
	// Protection, runtimes and auth.
	write := map[string]any{"kinds": []string{"agent"}, "roles": []string{"*"}, "credentials": map[string]any{"runtimes": []string{"*"}, "repo_token": "write"}}
	if p := w.plan(w.owner, wanted(t, "pool", "open", write)); len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	rs, err = w.engine.Get(ctx, w.owner, "auth", "auth")
	check(t, err)
	var auth map[string]any
	check(t, json.Unmarshal(rs[0].Spec, &auth))
	auth["token_login"], auth["instance_admins"] = true, []string{"a@example.com"}
	p = w.plan(w.owner, wanted(t, "auth", "auth", auth))
	if len(p.LowersProtection) != 2 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	testRuntimeKeys(t, w)
	testInstanceConnections(t, w)
	// A shared server keeps server settings in the file.
	other := domain.Account{Name: "Other", Slug: "other"}
	check(t, w.st.Accounts().Create(ctx, &other))
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "pool", "x", write)}})
	wantErr(t, err, mcp.ErrInvalidInput)
}

// testRuntimeKeys: one plan cannot send a variable of forecrewd anywhere, and
// a change of where keys go lowers protection.
func testRuntimeKeys(t *testing.T, w *world) {
	t.Helper()
	runtime := func(env, url string) map[string]any {
		return map[string]any{"adapter": "claude-code", "vendor": "anthropic", "credential_env": []string{env}, "base_url": url}
	}
	_, err := w.engine.Plan(t.Context(), w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{
		wanted(t, "runtime", "evil", runtime(config.EnvDatabaseURL, "https://evil.example.com"))}})
	wantErr(t, err, mcp.ErrInvalidInput)
	p := w.plan(w.owner, wanted(t, "runtime", "claude", runtime("ANTHROPIC_API_KEY", "https://proxy.example.com")))
	if len(p.LowersProtection) != 1 {
		t.Errorf("a new base_url: %v", p.LowersProtection)
	}
	p = w.plan(w.owner, wanted(t, "runtime", "other", runtime("ANTHROPIC_API_KEY", "")))
	if len(p.LowersProtection) != 1 {
		t.Errorf("a new runtime: %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
}

func testInstanceConnections(t *testing.T, w *world) {
	t.Helper()
	rs, err := w.engine.Get(t.Context(), w.owner, "auth", "auth")
	check(t, err)
	var auth map[string]any
	check(t, json.Unmarshal(rs[0].Spec, &auth))
	conn := func(ref string) map[string]any {
		return map[string]any{"okta": map[string]any{"kind": "oidc", "issuer": "https://evil.example.com", "client_id": "c", "client_secret_ref": ref}}
	}
	auth["connections"] = conn("env:" + config.EnvGitHubWebhookSecret)
	_, err = w.engine.Plan(t.Context(), w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "auth", "auth", auth)}})
	wantErr(t, err, mcp.ErrInvalidInput)
	auth["connections"] = conn("store:okta")
	p := w.plan(w.owner, wanted(t, "auth", "auth", auth))
	if !slices.ContainsFunc(p.LowersProtection, func(r string) bool { return strings.Contains(r, "sign-in connections") }) {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
}

func TestSchedules(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	rs, err := w.engine.Get(ctx, w.owner, "schedule", "curate:*")
	check(t, err)
	if len(rs) != 1 || rs[0].Origin != resource.OriginFile {
		t.Fatalf("schedules = %+v", rs)
	}
	// Over the file's default; forecrewd reads it at startup.
	p := w.plan(w.owner, wanted(t, "schedule", "curate:*", map[string]any{"playbook": "curate", "project": "*", "cron": "@daily"}))
	if !slices.Contains(p.Needs, domain.PermOrgManage) || !strings.Contains(p.Affects, "restarts") || !strings.Contains(p.Cost, "agent jobs") {
		t.Errorf("plan = %+v", p)
	}
	w.apply(w.owner, p)
	stored, err := resource.Stored(ctx, w.st)
	check(t, err)
	cfg, err := config.Parse([]byte(serverYAML), env)
	check(t, err)
	merged, err := cfg.WithSettings(stored, env)
	check(t, err)
	if len(merged.Schedules) != 2 || merged.Schedules[0].Cron != "@daily" {
		t.Errorf("schedules = %+v", merged.Schedules)
	}
	// A playbook forecrewd would not start is refused with the plan.
	strict := resource.NewEngine(resource.Config{Store: w.st, Server: cfg, Getenv: env, CheckSchedule: func(config.Schedule) error { return errors.New("no such playbook") }})
	_, err = strict.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{
		wanted(t, "schedule", "ghost:*", map[string]any{"playbook": "ghost", "project": "*", "cron": "@daily"})}})
	wantErr(t, err, mcp.ErrInvalidInput)
}

func TestRepoFilesOpenAPullRequest(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	body := "a: 1\n"
	p := w.plan(w.owner, wanted(t, "repo_file", "app:.forecrew/notes.yaml", map[string]any{"content": body}))
	if p.Applies != "pr:acme/app" || !strings.Contains(p.Affects, "merged") {
		t.Fatalf("plan = %+v", p)
	}
	out := w.apply(w.owner, p)
	if !strings.HasPrefix(out.Plan.PRURL, "https://github.com/acme/app/pull/") {
		t.Fatalf("url = %q", out.Plan.PRURL)
	}
	got, err := w.tracker.GetFile(ctx, "acme/app", "forecrew/config-"+p.ID.String(), ".forecrew/notes.yaml")
	check(t, err)
	if string(got) != body {
		t.Errorf("file = %q", got)
	}
	// A repo plan stands alone and touches one repo; it needs project.manage.
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{
		wanted(t, "repo_file", "app:.forecrew/x.yaml", map[string]any{"content": "x"}), wanted(t, "team", "core", team("Core"))}})
	wantErr(t, err, mcp.ErrInvalidInput)
	member := w.person(domain.RoleMember)
	p = w.plan(member, wanted(t, "repo_file", "app:.forecrew/y.yaml", map[string]any{"content": "y"}))
	_, err = w.engine.Apply(ctx, member, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied)

	// A knowledge policy is checked before it is proposed.
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "knowledge_policy", "app", map[string]any{"content": "nope: 1"})}})
	wantErr(t, err, mcp.ErrInvalidInput)
	policy := "version: 1\n"
	p = w.plan(w.owner, wanted(t, "knowledge_policy", "app", map[string]any{"content": policy}))
	w.apply(w.owner, p)
	check(t, w.tracker.SetFile("acme/app", "main", ".forecrew/knowledge-policy.yaml", []byte(policy)))
	check(t, w.tracker.SetFile("acme/app", "main", ".forecrew/matrix.yaml", []byte("x: 1\n")))
	if got := w.spec("knowledge_policy", "app"); !strings.Contains(got, "version") {
		t.Errorf("policy = %s", got)
	}
	if got := w.spec("repo_file", "app:.forecrew/matrix.yaml"); !strings.Contains(got, "x: 1") {
		t.Errorf("file = %s", got)
	}
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "knowledge_policy", Name: "app", Delete: true})
	w.apply(w.owner, p)
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "repo_file", Name: "app:.forecrew/matrix.yaml", Delete: true})
	w.apply(w.owner, p)
}

func TestIntegrations(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	profile := `{"integrations":[{"tool":"jira","instance":"acme","uses":["tracker"],"evidence":[{"kind":"link","location":"README.md"}],"confidence":"high","status":"suggested"}]}`
	w.project.Profile = json.RawMessage(profile)
	check(t, w.st.Projects().SetProfile(ctx, w.account.ID, w.project.ID, w.project.Profile))
	rs, err := w.engine.Get(ctx, w.owner, "integration", "app")
	check(t, err)
	if len(rs) != 1 || !strings.Contains(string(rs[0].Spec), "jira") {
		t.Fatalf("integrations = %+v", rs)
	}
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{{Kind: "integration", Name: "app", Delete: true}}})
	wantErr(t, err, mcp.ErrInvalidInput)
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "integration", "ghost", map[string]any{"integrations": []any{}})}})
	wantErr(t, err, mcp.ErrInvalidInput)
}

func TestFleetAreas(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	acc := w.account.ID
	// The home server of the task: a location with a machine type, and no
	// secure field: the machine gets its token over its own connection.
	shape := func(cpu int) map[string]any {
		return map[string]any{"provider": "home", "os": "linux", "arch": "amd64", "cpu": cpu, "memory_mb": 4096, "disk_gb": 20, "max": 3, "image": "linux", "enabled": true, "pool": "build", "price_per_minute": 0.5}
	}
	p := w.plan(w.owner, wanted(t, "shape", "small", shape(2)), wanted(t, "provider", "home", map[string]any{"kind": "microvm"}))
	if p.Changes[0].Kind != "provider" {
		t.Errorf("order = %+v", p.Changes)
	}
	if len(p.Steps) != 1 || p.Steps[0].Kind != domain.ConfigStepConnect {
		t.Fatalf("steps = %+v", p.Steps)
	}
	if !strings.Contains(p.Cost, "per job minute") || !strings.Contains(p.Cost, "your machine") {
		t.Errorf("cost = %q", p.Cost)
	}
	w.apply(w.owner, p)
	home, err := w.st.CapacityProviders().GetByName(ctx, acc, "home")
	check(t, err)
	small, err := w.st.Shapes().GetByName(ctx, acc, "small")
	check(t, err)
	if home.Status != domain.ProviderPending || small.ProviderID != home.ID {
		t.Fatalf("provider %+v shape %+v", home, small)
	}
	// What was applied reads back the same, so it does not count as a change.
	if p := w.plan(w.owner, wanted(t, "shape", "small", shape(2))); len(p.Changes) != 0 {
		t.Errorf("changes = %+v", p.Changes)
	}
	check(t, w.st.CapacityProviders().SetToken(ctx, home.ID, make([]byte, 32)))
	// A wider network and another trust zone lower protection.
	p = w.plan(w.owner, wanted(t, "provider", "home", map[string]any{"kind": "microvm", "mode": "agent", "network_allow": []string{"10.0.0.0/8"}, "paused": true}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	if got, _ := w.st.CapacityProviders().Get(ctx, home.ID); got.Status != domain.ProviderPaused {
		t.Errorf("status = %s", got.Status)
	}
	moved := shape(4)
	moved["pool"] = "control"
	p = w.plan(w.owner, wanted(t, "shape", "small", moved))
	if len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	w.apply(w.owner, p)
	if got, _ := w.st.Shapes().GetByName(ctx, acc, "small"); got.CPU != 4 {
		t.Errorf("shape = %+v", got)
	}
	// A machine type has no delete: it is turned off. A location is revoked.
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "shape", Name: "small", Delete: true}))
	if got, _ := w.st.Shapes().GetByName(ctx, acc, "small"); got.Enabled {
		t.Error("still enabled")
	}
	_, err = w.engine.Plan(ctx, w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "provider", "home", map[string]any{"kind": "aws", "mode": "agent"})}})
	wantErr(t, err, mcp.ErrInvalidInput)
	p = w.plan(w.owner, mcp.ConfigWanted{Kind: "provider", Name: "home", Delete: true})
	if p.Reversible {
		t.Error("a revoked token does not come back")
	}
	w.apply(w.owner, p)
	if w.spec("provider", "home") != "" {
		t.Error("a revoked location still reads")
	}
}

func TestMembers(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	admin := w.person(domain.RoleAdmin)
	p := w.plan(admin, wanted(t, "member", "marta@example.com", map[string]any{"status": "invited", "roles": []string{"member"}}))
	if p.Reversible {
		t.Error("an accepted invitation does not come back")
	}
	out := w.apply(admin, p)
	if len(out.ShowOnce) != 1 || !strings.Contains(out.ShowOnce[0].Value, "fci_") {
		t.Fatalf("show once = %+v", out.ShowOnce)
	}
	stored, _ := w.st.ConfigPlans().Get(ctx, w.account.ID, p.ID)
	raw, _ := json.Marshal(stored)
	if strings.Contains(string(raw), out.ShowOnce[0].Value) {
		t.Fatal("the invitation link was stored")
	}
	if got := w.spec("member", "marta@example.com"); !strings.Contains(got, "invited") {
		t.Errorf("member = %s", got)
	}
	// An Admin does not invite an Owner.
	p = w.plan(admin, wanted(t, "member", "boss@example.com", map[string]any{"status": "invited", "roles": []string{"owner"}}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("inviting an Owner: %v", p.LowersProtection)
	}
	_, err := w.engine.Apply(ctx, admin, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
	wantErr(t, err, authz.ErrDenied)
	w.apply(admin, w.plan(admin, mcp.ConfigWanted{Kind: "member", Name: "marta@example.com", Delete: true}))
	if got := w.spec("member", "marta@example.com"); got != "" {
		t.Errorf("a revoked invitation still reads: %s", got)
	}

	u, owner := w.person(domain.RoleMember), w.person(domain.RoleOwner)
	w.apply(admin, w.plan(admin, wanted(t, "member", u.Name, map[string]any{"status": "suspended"})))
	if got := w.spec("member", u.Name); !strings.Contains(got, "suspended") {
		t.Errorf("member = %s", got)
	}
	// Only an Owner acts on an Owner, and it lowers protection.
	p = w.plan(admin, wanted(t, "member", owner.Name, map[string]any{"status": "suspended"}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
	_, err = w.engine.Apply(ctx, admin, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord, Browser: true})
	wantErr(t, err, authz.ErrDenied)
	w.apply(w.owner, w.plan(w.owner, mcp.ConfigWanted{Kind: "member", Name: u.Name, Delete: true}))
	if got := w.spec("member", u.Name); got != "" {
		t.Errorf("member = %s", got)
	}
}

func TestOrganizationResource(t *testing.T) {
	w := newWorld(t)
	want := map[string]any{"name": "Acme Inc", "slug": "acme-inc", "max_token_ttl": "24h"}
	admin := w.person(domain.RoleAdmin)
	p := w.plan(admin, wanted(t, "organization", "organization", want))
	_, err := w.engine.Apply(t.Context(), admin, resource.Confirmation{Plan: p.ID})
	wantErr(t, err, authz.ErrDenied) // org.manage
	w.apply(w.owner, p)
	acc, err := w.st.Accounts().Get(t.Context(), w.account.ID)
	check(t, err)
	if acc.Name != "Acme Inc" || acc.Slug != "acme-inc" || time.Duration(acc.MaxTokenTTL) != 24*time.Hour {
		t.Errorf("account = %+v", acc)
	}
	p = w.plan(w.owner, wanted(t, "organization", "organization", map[string]any{"name": "Acme Inc", "slug": "acme-inc"}))
	if len(p.LowersProtection) != 1 {
		t.Errorf("reasons = %v", p.LowersProtection)
	}
}

func TestWithoutTheServerFile(t *testing.T) {
	w := newWorld(t)
	bare := resource.NewEngine(resource.Config{Store: w.st})
	rs, err := bare.Get(t.Context(), w.owner, "", "")
	check(t, err)
	if slices.ContainsFunc(rs, func(r mcp.ConfigResource) bool { return r.Scope == resource.ScopeServer }) {
		t.Errorf("server settings without the file: %+v", rs)
	}
	_, err = bare.Plan(t.Context(), w.owner, resource.PlanRequest{Source: domain.ConfigFromCLI, Changes: []mcp.ConfigWanted{wanted(t, "pool", "x", map[string]any{"kinds": []string{"agent"}})}})
	if err == nil {
		t.Error("a pool without the server file")
	}
}

func TestSchemaOf(t *testing.T) {
	w := newWorld(t)
	all, err := w.engine.Schema("")
	check(t, err)
	if len(all) != len(resource.Areas()) {
		t.Errorf("%d schemas", len(all))
	}
	one, err := w.engine.Schema("shape")
	check(t, err)
	if len(one) != 1 || !strings.Contains(string(one[0].Schema), "memory_mb") {
		t.Errorf("schema = %+v", one)
	}
	_, err = w.engine.Schema("nope")
	wantErr(t, err, mcp.ErrInvalidInput)
}

// TestAssistantHomeServer is the acceptance case: the job of the scripted
// runtime turns the request into a location plan, with a Connect step and no
// secure field, and a pasted token goes nowhere.
func TestAssistantHomeServer(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	creator := &testkit.JobCreator{Store: w.st}
	svc := tools.New(tools.Config{Store: w.st, Creator: creator})
	script := filepath.Join(t.TempDir(), "script.json")
	check(t, os.WriteFile(script, []byte(`{"text":"Done thinking.","output":{"reply":"Run the line on your server. Nothing changes until you confirm."}}`), 0o600))

	v, err := svc.AssistantSend(ctx, w.owner, mcp.AssistantSendInput{Message: "add my home server as a microVM host"})
	check(t, err)
	job, err := w.st.Jobs().Get(ctx, w.account.ID, v.Messages[0].JobID)
	check(t, err)

	// What the agent does in its job: one call of config_plan.
	p, err := svc.ConfigPlanFor(ctx, job, jobmcp.ConfigPlanInput{Title: "Add Home server", Changes: []mcp.ConfigWanted{
		wanted(t, "provider", "home", map[string]any{"kind": "microvm"}),
		wanted(t, "shape", "linux-small", map[string]any{"provider": "home", "os": "linux", "arch": "amd64", "cpu": 2, "memory_mb": 4096, "disk_gb": 20, "image": "linux", "max": 3, "enabled": true}),
	}})
	check(t, err)
	res, err := runtime.NewScripted(script, runtime.Config{}).Run(ctx, runtime.RunSpec{WorkDir: t.TempDir(), OutputSchema: json.RawMessage(`{"type":"object"}`)}, func(domain.JobEvent) {})
	check(t, err)
	worker := domain.Worker{AccountID: w.account.ID, Name: "control", Pool: job.Pool, TokenHash: []byte("control")}
	check(t, w.st.Workers().Create(ctx, &worker))
	_, err = w.st.Jobs().LeaseNext(ctx, w.account.ID, worker.ID, job.RequiredLabels(), time.Minute)
	check(t, err)
	_, err = w.st.Jobs().Complete(ctx, w.account.ID, job.ID, worker.ID, store.JobResult{Status: domain.JobSucceeded, Output: res.Output})
	check(t, err)

	v, err = svc.AssistantGet(ctx, w.owner, mcp.AssistantRefInput{Conversation: v.ID.String()})
	check(t, err)
	if len(v.Messages) != 2 || !strings.Contains(v.Messages[1].Body, "Run the line") || len(v.Plans) != 1 {
		t.Fatalf("view = %+v", v)
	}
	plan := v.Plans[0]
	if plan.Changes[0].Kind != "provider" || plan.Request != "add my home server as a microVM host" {
		t.Fatalf("plan = %+v", plan)
	}
	if slices.ContainsFunc(plan.Steps, func(s domain.ConfigStep) bool { return s.Kind == domain.ConfigStepSecret }) ||
		!slices.ContainsFunc(plan.Steps, func(s domain.ConfigStep) bool { return s.Kind == domain.ConfigStepConnect }) {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	// The assistant cannot apply it, and neither can a token.
	if _, err := svc.ConfigApply(ctx, w.owner, mcp.ConfigApplyInput{Plan: p.ID.String()}); !errors.Is(err, mcp.ErrConflict) {
		t.Fatalf("confirmed without the web UI: %v", err)
	}
	// A pasted token is refused and stored nowhere.
	pasted := "fcp_abcdefghijklmnopqrstuvwxyz"
	_, err = svc.AssistantSend(ctx, w.owner, mcp.AssistantSendInput{Conversation: v.ID.String(), Message: "the token is " + pasted})
	wantErr(t, err, mcp.ErrInvalidInput)
	messages, err := w.st.Conversations().Messages(ctx, w.account.ID, v.ID)
	check(t, err)
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
}

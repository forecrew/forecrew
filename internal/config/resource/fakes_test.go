package resource_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/config/resource"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tools"
)

// fullStore adds the repositories that join store.Store with the fleet task;
// only what the areas call is implemented.
type fullStore struct {
	store.Store
	x *extra
}

type extra struct {
	providers []domain.CapacityProvider
	shapes    []domain.Shape
	policy    *domain.FleetPolicy
}

func (s fullStore) WithTx(ctx context.Context, fn func(context.Context, store.Store) error) error {
	return s.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error { return fn(ctx, fullStore{tx, s.x}) })
}

func (s fullStore) CapacityProviders() store.CapacityProviderRepo {
	return providers{x: s.x}
}
func (s fullStore) Shapes() store.ShapeRepo              { return shapes{x: s.x} }
func (s fullStore) FleetPolicies() store.FleetPolicyRepo { return policies{x: s.x} }

type providers struct {
	store.CapacityProviderRepo
	x *extra
}

func (r providers) Create(_ context.Context, p *domain.CapacityProvider) error {
	p.ID = domain.NewID()
	r.x.providers = append(r.x.providers, *p)
	return nil
}

func (r providers) GetByName(_ context.Context, _ uuid.UUID, name string) (domain.CapacityProvider, error) {
	if i := slices.IndexFunc(r.x.providers, func(p domain.CapacityProvider) bool { return p.Name == name }); i >= 0 {
		return r.x.providers[i], nil
	}
	return domain.CapacityProvider{}, store.ErrNotFound
}

func (r providers) List(context.Context, uuid.UUID) ([]domain.CapacityProvider, error) {
	return r.x.providers, nil
}

func (r providers) Update(_ context.Context, p *domain.CapacityProvider) error {
	for i := range r.x.providers {
		if r.x.providers[i].ID == p.ID {
			r.x.providers[i] = *p
		}
	}
	return nil
}

func (r providers) SetStatus(_ context.Context, id uuid.UUID, status domain.ProviderStatus) error {
	for i := range r.x.providers {
		if r.x.providers[i].ID == id {
			r.x.providers[i].Status = status
		}
	}
	return nil
}

type shapes struct {
	store.ShapeRepo
	x *extra
}

func (r shapes) Create(_ context.Context, s *domain.Shape) error {
	s.ID = domain.NewID()
	r.x.shapes = append(r.x.shapes, *s)
	return nil
}

func (r shapes) GetByName(_ context.Context, _ uuid.UUID, name string) (domain.Shape, error) {
	if i := slices.IndexFunc(r.x.shapes, func(s domain.Shape) bool { return s.Name == name }); i >= 0 {
		return r.x.shapes[i], nil
	}
	return domain.Shape{}, store.ErrNotFound
}

func (r shapes) List(context.Context, uuid.UUID) ([]domain.Shape, error) { return r.x.shapes, nil }

func (r shapes) Update(_ context.Context, s *domain.Shape) error {
	for i := range r.x.shapes {
		if r.x.shapes[i].ID == s.ID {
			r.x.shapes[i] = *s
		}
	}
	return nil
}

type policies struct {
	store.FleetPolicyRepo
	x *extra
}

func (r policies) Get(_ context.Context, id uuid.UUID) (domain.FleetPolicy, error) {
	if r.x.policy != nil {
		return *r.x.policy, nil
	}
	return domain.DefaultFleetPolicy(id), nil
}

func (r policies) Put(_ context.Context, p *domain.FleetPolicy) error {
	r.x.policy = p
	return nil
}

// world is an organization with an engine over it.
type world struct {
	t       *testing.T
	st      *testkit.Store
	full    fullStore
	account domain.Account
	project domain.Project
	engine  *resource.Engine
	tracker *testkit.Tracker
	vault   secrets.Store
	now     time.Time
	owner   domain.Actor
}

func env(name string) string {
	if name == config.EnvDatabaseURL {
		return "postgres://x"
	}
	return "value"
}

const serverYAML = `
server:
  public_base_url: https://forecrew.example.com
github:
  app_id: 1
runtimes:
  claude: {adapter: claude-code, vendor: anthropic, credential_env: [ANTHROPIC_API_KEY]}
model_tiers:
  fast: {anthropic: a}
  strong: {anthropic: a}
  best: {anthropic: a}
roles:
  developer: {runtime: claude, tier: strong}
policy:
  allow_same_vendor_review: true
`

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, st: testkit.NewStore(nil), tracker: testkit.NewTracker(nil), now: time.Now().UTC()}
	w.full = fullStore{w.st, &extra{}}
	ctx := t.Context()
	w.account = domain.Account{Name: "Acme", Slug: "acme"}
	check(t, w.st.Accounts().Create(ctx, &w.account))
	w.project = domain.Project{AccountID: w.account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	check(t, w.st.Projects().Create(ctx, &w.project))
	w.tracker.Install("acme/app")
	check(t, w.tracker.SetHeadOfBranch("acme/app", "main", strings.Repeat("a", 40)))
	server, err := config.Parse([]byte(serverYAML), env)
	check(t, err)
	vault, err := secrets.New(w.st.Secrets(), make([]byte, secrets.KeySize))
	check(t, err)
	w.vault = vault
	w.engine = resource.NewEngine(resource.Config{
		Store: w.full, Access: func(st store.Store) resource.Access { return tools.New(tools.Config{Store: st}) },
		Server: server, Getenv: env, Repo: w.tracker, Secrets: vault, Now: func() time.Time { return w.now },
	})
	w.owner = w.person(domain.RoleOwner)
	return w
}

// person adds a member with a built-in organization role, with a session.
func (w *world) person(role string) domain.Actor {
	w.t.Helper()
	u := w.st.AddMember(w.account.ID, domain.User{Name: role}, "")
	if role != "" {
		testkit.Bind(w.t, w.st, w.account.ID, domain.SubjectUser, u.ID, domain.RoleScopeOrganization, role, uuid.Nil())
	}
	return domain.UserActor(w.account.ID, u.ID, domain.Credential{Kind: domain.CredentialSession, ID: domain.NewID()})
}

func spec(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	check(t, err)
	return raw
}

func wanted(t *testing.T, kind, name string, v any) mcp.ConfigWanted {
	t.Helper()
	return mcp.ConfigWanted{Kind: kind, Name: name, Spec: spec(t, v)}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// plan makes a plan from the CLI.
func (w *world) plan(by domain.Actor, changes ...mcp.ConfigWanted) domain.ConfigPlan {
	w.t.Helper()
	p, err := w.engine.Plan(w.t.Context(), by, resource.PlanRequest{Title: "t", Source: domain.ConfigFromCLI, Changes: changes})
	check(w.t, err)
	return p
}

func (w *world) apply(by domain.Actor, p domain.ConfigPlan) mcp.ConfigApplied {
	w.t.Helper()
	out, err := w.engine.Apply(w.t.Context(), by, resource.Confirmation{Plan: p.ID, Word: p.ConfirmWord})
	check(w.t, err)
	return out
}

func (w *world) spec(kind, name string) string {
	w.t.Helper()
	rs, err := w.engine.Get(w.t.Context(), w.owner, kind, name)
	check(w.t, err)
	if len(rs) == 0 {
		return ""
	}
	return string(rs[0].Spec)
}

package resource

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/store"
)

// DefaultPlanTTL is how long a plan stays valid.
const DefaultPlanTTL = 24 * time.Hour

// MaxChanges is the most changes one plan holds.
const MaxChanges = 200

// MaxImport is the largest YAML config_import reads.
const MaxImport = 1 << 20

// ExportVersion is the version of the YAML of Export.
const ExportVersion = 1

// Config holds the dependencies of an Engine.
type Config struct {
	Store store.Store
	// Access returns the existing services bound to st.
	Access func(st store.Store) Access
	// Server is forecrew.yaml as read; nil leaves server settings out.
	Server *config.Config
	Getenv func(string) string
	// CheckSchedule refuses a schedule forecrewd would not start; nil
	// accepts every playbook.
	CheckSchedule func(config.Schedule) error
	// Roles says whether a role runs in a toolbox and whether it exists; nil
	// checks nothing.
	Roles func(role string) (toolbox, known bool)
	// RoleWrites says whether a role writes code; nil counts every role as
	// writing.
	RoleWrites func(role string) bool
	// Repo reads repos and opens pull requests; nil leaves repo files out.
	Repo Repo
	// Secrets is the secret store; nil refuses secure fields.
	Secrets secrets.Store
	Logger  *slog.Logger
	Now     func() time.Time
	TTL     time.Duration
}

// Engine plans, applies and reverts changes.
type Engine struct {
	cfg Config
}

// NewEngine returns an engine.
func NewEngine(cfg Config) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultPlanTTL
	}
	if cfg.Getenv == nil {
		cfg.Getenv = func(string) string { return "" }
	}
	return &Engine{cfg: cfg}
}

func (g *Engine) env(st store.Store, by domain.Actor) *Env {
	e := &Env{Store: st, Actor: by, Authz: authz.New(st, g.cfg.Logger), Server: g.cfg.Server, Getenv: g.cfg.Getenv, Repo: g.cfg.Repo, CheckSchedule: g.cfg.CheckSchedule, Roles: g.cfg.Roles, RoleWrites: g.cfg.RoleWrites}
	if g.cfg.Access != nil {
		e.Access = g.cfg.Access(st)
	}
	return e
}

// canRead reports whether the caller may read the area.
func canRead(ctx context.Context, e *Env, a Area) (bool, error) {
	org := authz.Organization(e.Actor.AccountID)
	ok, err := e.Authz.Can(ctx, e.Actor, a.ReadPermission, org)
	if ok || err != nil || a.AlsoRead == "" {
		return ok, err
	}
	return e.Authz.Can(ctx, e.Actor, a.AlsoRead, org)
}

// mustRead is canRead for a call about the area: refusing it is audited.
func mustRead(ctx context.Context, e *Env, a Area) error {
	ok, err := canRead(ctx, e, a)
	if ok || err != nil {
		return err
	}
	return e.Authz.Authorize(ctx, e.Actor, a.ReadPermission, authz.Organization(e.Actor.AccountID))
}

// readable returns the areas by may read.
func (g *Engine) readable(ctx context.Context, e *Env) ([]Area, error) {
	var out []Area
	for _, a := range areas {
		ok, err := canRead(ctx, e, a)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// Schema returns the schema of kind, or of every kind when it is empty.
func (g *Engine) Schema(kind string) ([]SchemaInfo, error) {
	out := []SchemaInfo{}
	for _, a := range areas {
		if kind == "" || a.Kind == kind {
			out = append(out, SchemaInfo{Kind: a.Kind, Title: a.Title, Scope: a.Scope, Schema: a.Schema()})
		}
	}
	if len(out) == 0 {
		return nil, invalid("unknown kind %q", kind)
	}
	return out, nil
}

// Get returns what by may read of the configuration, for one kind and name
// when they are given.
func (g *Engine) Get(ctx context.Context, by domain.Actor, kind, name string) ([]Resource, error) {
	e := g.env(g.cfg.Store, by)
	if kind != "" {
		a, ok := Lookup(kind)
		if !ok {
			return nil, invalid("unknown kind %q", kind)
		}
		if err := mustRead(ctx, e, a); err != nil {
			return nil, err
		}
	}
	list, err := g.readable(ctx, e)
	if err != nil {
		return nil, err
	}
	out := []Resource{}
	for _, a := range list {
		if kind != "" && a.Kind != kind {
			continue
		}
		rs, err := a.Read(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", a.Kind, err)
		}
		for _, r := range rs {
			if name == "" || r.Name == name {
				r.Scope = cmp.Or(r.Scope, a.Scope)
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// PlanRequest asks for a plan.
type PlanRequest struct {
	Title   string
	Request string
	Source  domain.ConfigPlanSource
	Client  string
	Changes []Wanted
	// Untrusted names text the plan draws on that nobody verified.
	Untrusted      string
	ConversationID uuid.UUID
	JobID          uuid.UUID
	Reverts        uuid.UUID
}

func describe(p domain.ConfigPlan) string {
	if len(p.Changes) == 1 {
		return p.Changes[0].Line
	}
	return fmt.Sprintf("%d changes", len(p.Changes))
}

// current returns the resources of kind, cached for one plan.
type current map[string][]Resource

func (c current) of(ctx context.Context, e *Env, a Area) ([]Resource, error) {
	if rs, ok := c[a.Kind]; ok {
		return rs, nil
	}
	rs, err := a.Read(ctx, e)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", a.Kind, err)
	}
	c[a.Kind] = rs
	return rs, nil
}

func (c current) find(ctx context.Context, e *Env, a Area, name string) (*Resource, error) {
	rs, err := c.of(ctx, e, a)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(rs, func(r Resource) bool { return r.Name == name }); i >= 0 {
		return &rs[i], nil
	}
	return nil, nil
}

// digest fingerprints the current state of what a plan changes.
func digest(ctx context.Context, e *Env, cur current, changes []domain.ConfigChange) (string, error) {
	h := sha256.New()
	for _, c := range changes {
		a, ok := Lookup(c.Kind)
		if !ok {
			return "", invalid("unknown kind %q", c.Kind)
		}
		r, err := cur.find(ctx, e, a, c.Name)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s/%s\n", c.Kind, c.Name)
		if r != nil {
			h.Write(r.Spec)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Plan validates what is wanted and stores the plan. Everything goes into
// one error when something does not validate. A plan with no change is
// returned unstored, with no ID.
func (g *Engine) Plan(ctx context.Context, by domain.Actor, req PlanRequest) (domain.ConfigPlan, error) {
	e := g.env(g.cfg.Store, by)
	if err := e.Authz.Authorize(ctx, by, domain.PermConfigApply, authz.Organization(by.AccountID)); err != nil {
		return domain.ConfigPlan{}, err
	}
	if len(req.Changes) > MaxChanges {
		return domain.ConfigPlan{}, invalid("a plan holds at most %d changes", MaxChanges)
	}
	if err := checkNoSecret(req.Title + "\n" + req.Request); err != nil {
		return domain.ConfigPlan{}, err
	}
	if by.Kind == domain.ActorMCP {
		// An ownerless token is nobody's: no person stands behind what it
		// sends, whichever entry point it uses.
		req.Source = domain.ConfigFromMCP
	}
	now := g.cfg.Now()
	p := domain.ConfigPlan{AccountID: by.AccountID, Status: domain.ConfigPlanPending, Source: req.Source, Client: req.Client, Title: req.Title, Request: req.Request,
		Applies: "now", Changes: []domain.ConfigChange{}, Steps: []domain.ConfigStep{}, Needs: []domain.Permission{}, LowersProtection: []string{}, Reversible: true,
		Untrusted: req.Untrusted, CreatedBy: by.String(), ConversationID: req.ConversationID, JobID: req.JobID, Reverts: req.Reverts, ExpiresAt: now.Add(g.cfg.TTL)}
	wanted := slices.Clone(req.Changes)
	implied, err := impliedAuthorizations(ctx, e, wanted)
	if err != nil {
		return domain.ConfigPlan{}, err
	}
	wanted = append(wanted, implied...)
	slices.SortStableFunc(wanted, func(a, b Wanted) int { return cmp.Compare(order(a.Kind), order(b.Kind)) })
	cur := current{}
	d := draft{plan: &p}
	for _, w := range wanted {
		if err := g.add(ctx, e, cur, &d, w); err != nil {
			return domain.ConfigPlan{}, err
		}
	}
	if d.repo && d.other {
		d.problems = append(d.problems, "repo files open a pull request and apply apart from other settings: make two plans")
	}
	if err := d.reviewModels(ctx, e); err != nil {
		return domain.ConfigPlan{}, err
	}
	if len(d.problems) > 0 {
		return domain.ConfigPlan{}, fmt.Errorf("%s: %w", strings.Join(d.problems, "; "), mcp.ErrInvalidInput)
	}
	if len(p.Changes) == 0 {
		return p, nil
	}
	d.finish()
	if len(p.LowersProtection) > 0 {
		acc, err := g.cfg.Store.Accounts().Get(ctx, by.AccountID)
		if err != nil {
			return domain.ConfigPlan{}, err
		}
		p.ConfirmWord = acc.Slug
	}
	if p.BaseDigest, err = digest(ctx, e, cur, p.Changes); err != nil {
		return domain.ConfigPlan{}, err
	}
	if err := g.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ConfigPlans().Create(ctx, &p); err != nil {
			return err
		}
		return audit(ctx, tx, by, "config.plan.created", p, nil)
	}); err != nil {
		return domain.ConfigPlan{}, err
	}
	return g.fill(ctx, p)
}

// draft is a plan being built.
type draft struct {
	plan                 *domain.ConfigPlan
	problems             []string
	affects, costs, lost []string
	repo, other          bool
}

// add adds one wanted change. What is wrong with the change itself goes in
// problems, so a plan reports everything at once.
func (g *Engine) add(ctx context.Context, e *Env, cur current, d *draft, w Wanted) error {
	a, ok := Lookup(w.Kind)
	if !ok {
		d.problems = append(d.problems, fmt.Sprintf("%s: unknown kind; config_schema lists them", w.Kind))
		return nil
	}
	if err := mustRead(ctx, e, a); err != nil {
		return err
	}
	c, plan, err := g.change(ctx, e, cur, a, w)
	if errors.Is(err, mcp.ErrInvalidInput) || errors.Is(err, mcp.ErrUnavailable) {
		d.problems = append(d.problems, fmt.Sprintf("%s/%s: %v", w.Kind, w.Name, err))
		return nil
	}
	if err != nil || c == nil {
		return err
	}
	d.repo, d.other = d.repo || a.RepoFiles, d.other || !a.RepoFiles
	p := d.plan
	p.Changes = append(p.Changes, *c)
	p.Steps = append(p.Steps, plan.steps...)
	p.LowersProtection = append(p.LowersProtection, plan.protection...)
	for _, n := range plan.needs {
		p.Needs = append(p.Needs, n.Permission)
	}
	d.affects = appendText(d.affects, a.Affects)
	d.costs = appendText(d.costs, plan.cost)
	d.lost = appendText(d.lost, plan.lost)
	if !a.RepoFiles {
		return nil
	}
	pr, err := projectOf(ctx, e, strings.SplitN(w.Name, ":", 2)[0])
	if err != nil {
		return err
	}
	next := "pr:" + pr.Repo
	if p.Applies != "now" && p.Applies != next {
		d.problems = append(d.problems, "a plan changes the files of one repo")
	}
	p.Applies = next
	return nil
}

// reviewModels adds what the model changes of the plan say together: their
// problems, who they affect, and the models they put roles on that did not
// pass their test.
func (d *draft) reviewModels(ctx context.Context, e *Env) error {
	problems, affects, err := reviewModels(ctx, e, d.plan.Changes)
	if err != nil {
		return err
	}
	d.problems = append(d.problems, problems...)
	d.affects = appendText(d.affects, affects)
	if len(d.problems) > 0 {
		return nil
	}
	untested, err := guardModels(ctx, e, d.plan.Changes)
	d.plan.LowersProtection = append(d.plan.LowersProtection, untested...)
	return err
}

func appendText(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

func joined(list []string) string {
	slices.Sort(list)
	return strings.Join(slices.Compact(list), " ")
}

// finish writes what comes of all the changes together.
func (d *draft) finish() {
	p := d.plan
	if p.Title = strings.TrimSpace(p.Title); p.Title == "" {
		p.Title = describe(*p)
	}
	slices.Sort(p.Needs)
	p.Needs = slices.Compact(p.Needs)
	p.Affects = joined(d.affects)
	p.Cost = cmp.Or(joined(d.costs), "Free.")
	p.Undo = "Can be reverted from Change history."
	if len(d.lost) > 0 {
		p.Undo, p.Reversible = joined(d.lost), false
	}
}

// planned is what one change adds to a plan besides the change itself.
type planned struct {
	steps      []domain.ConfigStep
	needs      []Need
	protection []string
	cost, lost string
}

// wantedResource is the resource w asks for; nil deletes old.
func wantedResource(a Area, old *Resource, w Wanted) (*Resource, error) {
	switch {
	case w.Delete && old == nil:
		return nil, invalid("it does not exist")
	case w.Delete && a.NoDelete != "":
		return nil, invalid("%s", a.NoDelete)
	case w.Delete && old.Origin == OriginFile:
		return nil, invalid("it is set by forecrew.yaml, which is changed on the server")
	case w.Delete && a.Retire != nil:
		return &Resource{Kind: a.Kind, Name: w.Name, Spec: a.Retire(old.Spec)}, nil
	case w.Delete:
		return nil, nil
	case len(w.Spec) == 0:
		return nil, invalid("spec is required")
	}
	spec, err := a.Normalize(w.Spec)
	if err != nil {
		return nil, err
	}
	return &Resource{Kind: a.Kind, Name: w.Name, Spec: spec}, nil
}

// refuseStaged keeps a plan off the values other plans staged, whatever the
// area's own check says: they are not confirmed, and not this plan's. It keeps
// model keys to model connections the same way: anything else that names one
// would send it where its own settings say.
func refuseStaged(a Area, want Resource) error {
	if a.Secrets == nil {
		return nil
	}
	for _, s := range a.Secrets(want) {
		if strings.HasPrefix(s.Ref, domain.SecretRefStore+domain.StagedSecretPrefix) {
			return invalid("%s is a reserved name of the secret store", s.Ref)
		}
		if domain.IsModelSecretRef(s.Ref) && a.Kind != KindModelConnection {
			return invalid("%s is a model key, and only a model connection reads those", s.Ref)
		}
	}
	return nil
}

func (g *Engine) change(ctx context.Context, e *Env, cur current, a Area, w Wanted) (*domain.ConfigChange, planned, error) {
	var out planned
	old, err := cur.find(ctx, e, a, w.Name)
	if err != nil {
		return nil, out, err
	}
	want, err := wantedResource(a, old, w)
	if err != nil {
		return nil, out, err
	}
	fields := a.Diff(old, want)
	if old != nil && want != nil && len(fields) == 0 {
		return nil, out, nil
	}
	if want != nil {
		if err := refuseStaged(a, *want); err != nil {
			return nil, out, err
		}
		if err := a.Validate(ctx, e, old, *want); err != nil {
			return nil, out, err
		}
	}
	c := domain.ConfigChange{Kind: a.Kind, Name: w.Name, Fields: fields}
	switch {
	case want == nil:
		c.Op, c.Old = domain.ConfigDelete, old.Spec
	case old == nil:
		c.Op, c.New = domain.ConfigCreate, want.Spec
	default:
		c.Op, c.Old, c.New = domain.ConfigUpdate, old.Spec, want.Spec
	}
	c.Line = line(a, c)
	if out.needs, err = a.Needs(ctx, e, old, want, fields); err != nil {
		return nil, out, err
	}
	out.describe(ctx, e, a, old, want)
	if err := g.markReused(ctx, e.Actor.AccountID, out.steps); err != nil {
		return nil, out, err
	}
	return &c, out, nil
}

// markReused says on the plan which secure fields name a secret that is saved
// already: nothing is asked for and nothing is replaced.
func (g *Engine) markReused(ctx context.Context, accountID uuid.UUID, steps []domain.ConfigStep) error {
	if g.cfg.Secrets == nil {
		return nil
	}
	for i, s := range steps {
		if s.Kind != domain.ConfigStepSecret {
			continue
		}
		ok, err := g.cfg.Secrets.Has(ctx, secretScope(s, accountID), s.Secret)
		if err != nil {
			return err
		}
		if ok {
			steps[i].Reuses = true
			steps[i].Detail = "Uses the secret already saved as " + s.Secret + ". Nothing is replaced. To change it, name a new one."
		}
	}
	return nil
}

// describe fills in what the area says about a change.
func (out *planned) describe(ctx context.Context, e *Env, a Area, old, want *Resource) {
	if a.Protection != nil {
		out.protection = a.Protection(ctx, e, old, want)
	}
	if a.Lost != nil {
		out.lost = a.Lost(old, want)
	}
	if a.Steps != nil {
		defer func() { out.steps = append(out.steps, a.Steps(old, want)...) }()
	}
	if want == nil {
		return
	}
	if a.Cost != nil {
		out.cost = a.Cost(*want)
	}
	if a.Secrets == nil {
		return
	}
	for _, s := range a.Secrets(*want) {
		if name, ok := domain.StoreSecretName(s.Ref); ok {
			out.steps = append(out.steps, domain.ConfigStep{Kind: domain.ConfigStepSecret, Resource: want.Ref(), Title: s.Title, Secret: name,
				Detail: "Goes straight to secure storage. The assistant and the change history never see it."})
		}
	}
}

// secretScope is the scope a secret of the change lives in.
func secretScope(step domain.ConfigStep, accountID uuid.UUID) uuid.UUID {
	if strings.HasPrefix(step.Resource, KindAuth+"/") {
		return uuid.Nil()
	}
	return accountID
}

// valueName is the entry a step of plan id needs.
func valueName(id uuid.UUID, s domain.ConfigStep) string {
	if s.Reuses {
		return s.Secret
	}
	return secrets.StagedName(id, s.Secret)
}

// fill marks the secure fields that have their value.
func (g *Engine) fill(ctx context.Context, p domain.ConfigPlan) (domain.ConfigPlan, error) {
	p.Steps = slices.Clone(p.Steps)
	for i, s := range p.Steps {
		if s.Kind != domain.ConfigStepSecret || g.cfg.Secrets == nil {
			continue
		}
		ok, err := g.cfg.Secrets.Has(ctx, secretScope(s, p.AccountID), valueName(p.ID, s))
		if err != nil {
			return p, err
		}
		p.Steps[i].Filled = ok
	}
	return p, nil
}

// dropStaged removes what was entered for a plan that will not apply. It runs
// before the plan moves: losing a value of a plan that stays open only means
// entering it again.
func (g *Engine) dropStaged(ctx context.Context, p domain.ConfigPlan) error {
	if g.cfg.Secrets == nil {
		return nil
	}
	for _, s := range p.Steps {
		if s.Kind == domain.ConfigStepSecret && !s.Reuses {
			if err := g.cfg.Secrets.Delete(ctx, secretScope(s, p.AccountID), secrets.StagedName(p.ID, s.Secret)); err != nil {
				return err
			}
		}
	}
	return nil
}

// promoteStaged makes the values of a plan live, in its transaction. A name
// taken since the plan was made is refused: nothing live is replaced.
func (g *Engine) promoteStaged(ctx context.Context, tx store.Store, by domain.Actor, p domain.ConfigPlan) error {
	done := map[string]bool{}
	for _, s := range p.Steps {
		if s.Kind != domain.ConfigStepSecret || s.Reuses || done[s.Secret] {
			continue
		}
		done[s.Secret] = true
		err := g.cfg.Secrets.Promote(ctx, tx.Secrets(), by, secretScope(s, p.AccountID), secrets.StagedName(p.ID, s.Secret), s.Secret)
		if errors.Is(err, secrets.ErrExists) {
			return fmt.Errorf("%s was saved since this plan was made; rebuild it: %w", s.Secret, mcp.ErrConflict)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func audit(ctx context.Context, st store.Store, by domain.Actor, action string, p domain.ConfigPlan, extra map[string]any) error {
	data := map[string]any{"title": p.Title, "source": p.Source, "kinds": p.Kinds()}
	maps.Copy(data, extra)
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{AccountID: by.AccountID, Actor: by.String(), Credential: by.Credential.String(), Action: action,
		SubjectType: "config_plan", SubjectID: p.ID, Data: raw})
}

// visible reports whether by may see p: its author, or anyone who may read
// every kind it touches.
func (g *Engine) visible(ctx context.Context, e *Env, p domain.ConfigPlan) (bool, error) {
	if p.CreatedBy == e.Actor.String() {
		return true, nil
	}
	for _, kind := range p.Kinds() {
		a, ok := Lookup(kind)
		if !ok {
			return false, nil
		}
		can, err := canRead(ctx, e, a)
		if err != nil || !can {
			return false, err
		}
	}
	return true, nil
}

func (g *Engine) view(ctx context.Context, e *Env, p domain.ConfigPlan) (View, error) {
	p.Status = p.StatusAt(g.cfg.Now())
	p, err := g.fill(ctx, p)
	if err != nil {
		return View{}, err
	}
	v := View{ConfigPlan: p}
	if !p.Status.Open() {
		return v, nil
	}
	sum, err := digest(ctx, e, current{}, p.Changes)
	if err != nil {
		return View{}, err
	}
	v.Stale = sum != p.BaseDigest
	needs, err := g.needs(ctx, e, p)
	if err != nil {
		return View{}, err
	}
	v.CanConfirm = mayConfirm(e.Actor)
	for _, n := range needs {
		ok, err := e.Authz.Can(ctx, e.Actor, n.Permission, resourceOf(e, n))
		if err != nil {
			return View{}, err
		}
		v.CanConfirm = v.CanConfirm && ok
	}
	return v, nil
}

// mayConfirm reports whether the kind of actor confirms plans at all: a
// person, or an ownerless token, which is a person's until it gets its owner.
func mayConfirm(by domain.Actor) bool {
	return by.Kind == domain.ActorUser || by.Kind == domain.ActorMCP
}

func resourceOf(e *Env, n Need) authz.Resource {
	if n.ProjectID != uuid.Nil() {
		return authz.Project(e.Actor.AccountID, n.ProjectID)
	}
	return authz.Organization(e.Actor.AccountID)
}

// needs works out what confirming p takes, for the caller and as of now.
func (g *Engine) needs(ctx context.Context, e *Env, p domain.ConfigPlan) ([]Need, error) {
	out := need(domain.PermConfigApply)
	for _, c := range p.Changes {
		a, ok := Lookup(c.Kind)
		if !ok {
			return nil, invalid("unknown kind %q", c.Kind)
		}
		old, want := resources(c)
		ns, err := a.Needs(ctx, e, old, want, c.Fields)
		if err != nil {
			return nil, err
		}
		out = append(out, ns...)
	}
	return out, nil
}

func resources(c domain.ConfigChange) (old, want *Resource) {
	if len(c.Old) > 0 {
		old = &Resource{Kind: c.Kind, Name: c.Name, Spec: c.Old}
	}
	if len(c.New) > 0 {
		want = &Resource{Kind: c.Kind, Name: c.Name, Spec: c.New}
	}
	return old, want
}

func (g *Engine) plan(ctx context.Context, e *Env, id uuid.UUID) (domain.ConfigPlan, error) {
	p, err := g.cfg.Store.ConfigPlans().Get(ctx, e.Actor.AccountID, id)
	if errors.Is(err, store.ErrNotFound) {
		return p, fmt.Errorf("plan %s: %w", id, mcp.ErrNotFound)
	}
	if err != nil {
		return p, err
	}
	ok, err := g.visible(ctx, e, p)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, fmt.Errorf("plan %s: %w", id, mcp.ErrNotFound)
	}
	return p, nil
}

// GetPlan returns a plan by may see.
func (g *Engine) GetPlan(ctx context.Context, by domain.Actor, id uuid.UUID) (View, error) {
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, id)
	if err != nil {
		return View{}, err
	}
	return g.view(ctx, e, p)
}

// ListPlans returns the plans by may see, newest first. With inbox, only
// the open plans waiting for someone that by could confirm: those awaiting
// approval, and plans a model wrote, which only a person in the web UI
// confirms.
func (g *Engine) ListPlans(ctx context.Context, by domain.Actor, inbox bool, limit int) ([]View, error) {
	e := g.env(g.cfg.Store, by)
	rows, err := g.cfg.Store.ConfigPlans().List(ctx, by.AccountID, store.ConfigPlanFilter{Open: inbox, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := []View{}
	for _, p := range rows {
		ok, err := g.visible(ctx, e, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		v, err := g.view(ctx, e, p)
		if err != nil {
			return nil, err
		}
		if inbox && (v.Status == domain.ConfigPlanExpired || !v.CanConfirm || (p.Status != domain.ConfigPlanAwaiting && p.Source != domain.ConfigFromMCP)) {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// RequestApproval puts an open plan in the Inbox of people who can confirm
// it. It is the only thing a person without the permissions, or an MCP
// client, can do with a plan.
func (g *Engine) RequestApproval(ctx context.Context, by domain.Actor, id uuid.UUID) (View, error) {
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, id)
	if err != nil {
		return View{}, err
	}
	switch p.StatusAt(g.cfg.Now()) {
	case domain.ConfigPlanPending:
		p.Status = domain.ConfigPlanAwaiting
		if err := g.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := tx.ConfigPlans().Move(ctx, &p, domain.ConfigPlanPending); err != nil {
				return err
			}
			return audit(ctx, tx, by, "config.plan.approval_requested", p, nil)
		}); err != nil {
			return View{}, conflict(err)
		}
	case domain.ConfigPlanAwaiting:
	default:
		return View{}, fmt.Errorf("the plan is %s: %w", p.StatusAt(g.cfg.Now()), mcp.ErrConflict)
	}
	return g.view(ctx, e, p)
}

func conflict(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: %w", err, mcp.ErrConflict)
	}
	return err
}

// Cancel drops an open plan: its author, or someone who could confirm it.
func (g *Engine) Cancel(ctx context.Context, by domain.Actor, id uuid.UUID) (View, error) {
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, id)
	if err != nil {
		return View{}, err
	}
	if p.CreatedBy != by.String() {
		if err := e.Authz.Authorize(ctx, by, domain.PermConfigApply, authz.Organization(by.AccountID)); err != nil {
			return View{}, err
		}
	}
	from := p.Status
	if !from.Open() {
		return View{}, fmt.Errorf("the plan is %s: %w", from, mcp.ErrConflict)
	}
	if err := g.dropStaged(ctx, p); err != nil {
		return View{}, err
	}
	p.Status = domain.ConfigPlanCanceled
	if err := g.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ConfigPlans().Move(ctx, &p, from); err != nil {
			return err
		}
		return audit(ctx, tx, by, "config.plan.canceled", p, nil)
	}); err != nil {
		return View{}, conflict(err)
	}
	return g.view(ctx, e, p)
}

// PutSecret stages the value of a secure field of an open plan: it goes to
// the secret store and nowhere else, under a name of the plan, and becomes
// live when the plan is applied. Only its name is audited.
func (g *Engine) PutSecret(ctx context.Context, by domain.Actor, id uuid.UUID, name string, value []byte) (View, error) {
	if g.cfg.Secrets == nil {
		return View{}, fmt.Errorf("the secret store is not configured (%s): %w", config.EnvSecretKey, mcp.ErrUnavailable)
	}
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, id)
	if err != nil {
		return View{}, err
	}
	if !p.StatusAt(g.cfg.Now()).Open() {
		return View{}, fmt.Errorf("the plan is %s: %w", p.StatusAt(g.cfg.Now()), mcp.ErrConflict)
	}
	i := slices.IndexFunc(p.Steps, func(s domain.ConfigStep) bool { return s.Kind == domain.ConfigStepSecret && s.Secret == name })
	if i < 0 {
		return View{}, invalid("the plan has no secure field %q", name)
	}
	step := p.Steps[i]
	if step.Reuses {
		return View{}, fmt.Errorf("this plan uses the secret saved as %s; to change it, name a new one: %w", name, mcp.ErrConflict)
	}
	if err := g.authorize(ctx, e, p); err != nil {
		return View{}, err
	}
	scope := secretScope(step, by.AccountID)
	switch live, err := g.cfg.Secrets.Has(ctx, scope, name); {
	case err != nil:
		return View{}, err
	case live:
		return View{}, fmt.Errorf("%s was saved since this plan was made; rebuild it: %w", name, mcp.ErrConflict)
	}
	if err := g.cfg.Secrets.Stage(ctx, by, scope, p.ID, name, value); err != nil {
		if errors.Is(err, secrets.ErrInvalid) {
			return View{}, fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
		}
		return View{}, err
	}
	if err := audit(ctx, g.cfg.Store, by, "secret.saved", p, map[string]any{"secret": name}); err != nil {
		return View{}, err
	}
	return g.view(ctx, e, p)
}

// Confirmation is how a person confirms a plan.
type Confirmation struct {
	Plan uuid.UUID
	// Browser says the request came from the web UI's session. Only the
	// server sets it.
	Browser bool
	// Word is the word typed in the second confirmation.
	Word string
}

// checkConfirm is every rule of who may confirm p, and how.
func (g *Engine) checkConfirm(ctx context.Context, e *Env, p domain.ConfigPlan, c Confirmation) error {
	by := e.Actor
	if !mayConfirm(by) {
		return e.Authz.Authorize(ctx, by, "", authz.Organization(by.AccountID))
	}
	if st := p.StatusAt(g.cfg.Now()); !st.Open() {
		return fmt.Errorf("the plan is %s: %w", st, mcp.ErrConflict)
	}
	if p.Source.BrowserOnly() && !c.Browser {
		return fmt.Errorf("a plan from %s is confirmed by a person in the web UI: %w", p.Source, mcp.ErrConflict)
	}
	if len(p.LowersProtection) > 0 && !c.Browser {
		return fmt.Errorf("a plan that lowers protection is confirmed by a person in the web UI: %w", mcp.ErrConflict)
	}
	if len(p.LowersProtection) > 0 && c.Word != p.ConfirmWord {
		return invalid("this plan lowers protection (%s): type %q to confirm", strings.Join(p.LowersProtection, "; "), p.ConfirmWord)
	}
	if err := g.authorize(ctx, e, p); err != nil {
		return err
	}
	return g.checkSecrets(ctx, p)
}

// authorize checks everything the plan takes, as of now.
func (g *Engine) authorize(ctx context.Context, e *Env, p domain.ConfigPlan) error {
	needs, err := g.needs(ctx, e, p)
	if err != nil {
		return err
	}
	for _, n := range needs {
		if err := e.Authz.Authorize(ctx, e.Actor, n.Permission, resourceOf(e, n)); err != nil {
			return err
		}
	}
	return nil
}

// checkSecrets refuses a plan with a secure field still empty.
func (g *Engine) checkSecrets(ctx context.Context, p domain.ConfigPlan) error {
	for _, s := range p.Steps {
		if s.Kind != domain.ConfigStepSecret {
			continue
		}
		if g.cfg.Secrets == nil {
			return fmt.Errorf("the secret store is not configured (%s): %w", config.EnvSecretKey, mcp.ErrUnavailable)
		}
		ok, err := g.cfg.Secrets.Has(ctx, secretScope(s, p.AccountID), valueName(p.ID, s))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is still empty: %w", s.Title, mcp.ErrConflict)
		}
	}
	return nil
}

// Apply applies a plan in one step, as the person confirming it: everything
// happens in one transaction, so a failure changes nothing. A plan whose
// resources moved since it was made is refused, and has to be rebuilt.
func (g *Engine) Apply(ctx context.Context, by domain.Actor, c Confirmation) (Applied, error) {
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, c.Plan)
	if err != nil {
		return Applied{}, err
	}
	if err := g.checkConfirm(ctx, e, p, c); err != nil {
		return Applied{}, err
	}
	var shown []ShowOnce
	err = g.cfg.Store.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		shown, err = g.applyIn(ctx, tx, by, &p)
		return err
	})
	if err != nil {
		if errors.Is(err, authz.ErrDenied) { // the audit entry of the denial went with the transaction
			if aerr := audit(context.WithoutCancel(ctx), g.cfg.Store, by, "config.plan.denied", p, nil); aerr != nil {
				g.cfg.Logger.ErrorContext(ctx, "audit a denied plan", "plan_id", p.ID, "error", aerr)
			}
		}
		return Applied{}, conflict(err)
	}
	v, err := g.view(ctx, e, p)
	if err != nil {
		return Applied{}, err
	}
	if shown == nil {
		shown = []ShowOnce{}
	}
	return Applied{Plan: v, ShowOnce: shown}, nil
}

// applyOrder puts what others refer to first when adding and last when
// removing.
func applyOrder(a, b domain.ConfigChange) int {
	switch {
	case a.Op == domain.ConfigDelete && b.Op != domain.ConfigDelete:
		return 1
	case a.Op != domain.ConfigDelete && b.Op == domain.ConfigDelete:
		return -1
	case a.Op == domain.ConfigDelete:
		return cmp.Compare(order(b.Kind), order(a.Kind))
	}
	return cmp.Compare(order(a.Kind), order(b.Kind))
}

// applyIn does the work of Apply in the transaction tx and returns what to
// show once.
func (g *Engine) applyIn(ctx context.Context, tx store.Store, by domain.Actor, p *domain.ConfigPlan) ([]ShowOnce, error) {
	te := g.env(tx, by)
	sum, err := digest(ctx, te, current{}, p.Changes)
	if err != nil {
		return nil, err
	}
	if sum != p.BaseDigest {
		return nil, fmt.Errorf("the configuration changed since this plan was made; rebuild it: %w", mcp.ErrConflict)
	}
	ordered := slices.Clone(p.Changes)
	slices.SortStableFunc(ordered, applyOrder)
	for _, ch := range ordered {
		a, _ := Lookup(ch.Kind)
		old, want := resources(ch)
		if err := a.Apply(ctx, te, old, want); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", ch.Kind, ch.Name, err)
		}
	}
	if err := recheckModels(ctx, te, ordered); err != nil {
		return nil, err
	}
	if te.project != nil {
		if g.cfg.Repo == nil {
			return nil, fmt.Errorf("GitHub is not configured: %w", mcp.ErrUnavailable)
		}
		if p.PRURL, err = openPR(ctx, g.cfg.Repo, *te.project, "forecrew/config-"+p.ID.String(), p.Title, te.files); err != nil {
			return nil, fmt.Errorf("open the pull request: %w", err)
		}
	}
	if err := g.promoteStaged(ctx, tx, by, *p); err != nil {
		return nil, err
	}
	from := p.Status
	p.Status, p.AppliedBy, p.AppliedAt = domain.ConfigPlanApplied, by.String(), g.cfg.Now()
	if err := tx.ConfigPlans().Move(ctx, p, from); err != nil {
		return nil, err
	}
	if err := markReverted(ctx, tx, *p); err != nil {
		return nil, err
	}
	return te.shown, audit(ctx, tx, by, "config.plan.applied", *p, map[string]any{"changes": len(p.Changes), "lowers_protection": p.LowersProtection, "reverts": p.Reverts})
}

// markReverted marks the plan p undoes.
func markReverted(ctx context.Context, tx store.Store, p domain.ConfigPlan) error {
	if p.Reverts == uuid.Nil() {
		return nil
	}
	orig, err := tx.ConfigPlans().Get(ctx, p.AccountID, p.Reverts)
	if err != nil || orig.Status != domain.ConfigPlanApplied {
		return err
	}
	orig.Status = domain.ConfigPlanReverted
	return tx.ConfigPlans().Move(ctx, &orig, domain.ConfigPlanApplied)
}

// SweepBatch is how many expired plans one sweep takes.
const SweepBatch = 200

// SweepEvery is how often expired plans are swept.
const SweepEvery = time.Hour

// Sweep marks the open plans past their expiry and drops the values staged
// for them. It returns how many plans it took.
func (g *Engine) Sweep(ctx context.Context) (int, error) {
	rows, err := g.cfg.Store.ConfigPlans().ListExpired(ctx, g.cfg.Now(), SweepBatch)
	if err != nil {
		return 0, err
	}
	for i, p := range rows {
		if err := g.dropStaged(ctx, p); err != nil {
			return i, err
		}
		from := p.Status
		p.Status = domain.ConfigPlanExpired
		// An apply that won the race is what happened: nothing to do.
		if err := g.cfg.Store.ConfigPlans().Move(ctx, &p, from); err != nil && !errors.Is(err, store.ErrConflict) {
			return i, err
		}
	}
	return len(rows), nil
}

// RunSweep sweeps at once and then every interval until ctx is done.
func (g *Engine) RunSweep(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := g.Sweep(ctx); err != nil {
			g.cfg.Logger.ErrorContext(ctx, "sweep expired plans", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Revert makes a new plan with the previous values of an applied one. It is
// confirmed like any plan, so a revert that lowers protection needs the
// second confirmation too.
func (g *Engine) Revert(ctx context.Context, by domain.Actor, id uuid.UUID, source domain.ConfigPlanSource, client string) (domain.ConfigPlan, error) {
	e := g.env(g.cfg.Store, by)
	p, err := g.plan(ctx, e, id)
	if err != nil {
		return domain.ConfigPlan{}, err
	}
	if p.Status != domain.ConfigPlanApplied {
		return domain.ConfigPlan{}, fmt.Errorf("only an applied plan can be reverted; this one is %s: %w", p.Status, mcp.ErrConflict)
	}
	req := PlanRequest{Title: "Revert: " + p.Title, Source: source, Client: client, Reverts: p.ID}
	for _, c := range slices.Backward(p.Changes) {
		w := Wanted{Kind: c.Kind, Name: c.Name, Spec: c.Old}
		if c.Op == domain.ConfigCreate {
			w = Wanted{Kind: c.Kind, Name: c.Name, Delete: true}
		}
		req.Changes = append(req.Changes, w)
	}
	return g.Plan(ctx, by, req)
}

// exportDoc is the YAML of Export.
type exportDoc struct {
	Version   int           `yaml:"version"`
	Resources []exportEntry `yaml:"resources"`
}

type exportEntry struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
	Spec any    `yaml:"spec"`
}

// Export returns the YAML of everything by may read. Specs hold references
// to secrets, never their values.
func (g *Engine) Export(ctx context.Context, by domain.Actor) (string, error) {
	rs, err := g.Get(ctx, by, "", "")
	if err != nil {
		return "", err
	}
	doc := exportDoc{Version: ExportVersion, Resources: []exportEntry{}}
	for _, r := range rs {
		var spec any
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			return "", err
		}
		doc.Resources = append(doc.Resources, exportEntry{r.Kind, r.Name, spec})
	}
	out, err := yaml.Marshal(doc)
	return string(out), err
}

// Import turns the YAML of Export into a plan: what differs is changed, and
// what the file leaves out stays. A file that matches gives a plan with no
// change and no ID.
func (g *Engine) Import(ctx context.Context, by domain.Actor, data string, source domain.ConfigPlanSource, client string) (domain.ConfigPlan, error) {
	if len(data) > MaxImport {
		return domain.ConfigPlan{}, invalid("the file is over %d bytes", MaxImport)
	}
	if err := checkNoSecret(data); err != nil {
		return domain.ConfigPlan{}, err
	}
	var doc exportDoc
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
		return domain.ConfigPlan{}, invalid("the file does not parse: %v", err)
	}
	if doc.Version != ExportVersion {
		return domain.ConfigPlan{}, invalid("version must be %d", ExportVersion)
	}
	req := PlanRequest{Title: "Import", Source: source, Client: client}
	for _, r := range doc.Resources {
		spec, err := json.Marshal(r.Spec)
		if err != nil {
			return domain.ConfigPlan{}, invalid("%s/%s: %v", r.Kind, r.Name, err)
		}
		req.Changes = append(req.Changes, Wanted{Kind: r.Kind, Name: r.Name, Spec: spec})
	}
	return g.Plan(ctx, by, req)
}

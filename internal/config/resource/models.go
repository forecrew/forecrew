package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/runtime"
)

// assignmentProject starts the name of a project's model_assignment.
const assignmentProject = "project:"

// Roles that separation of duties keeps on different vendors.
const (
	developerRole = "developer"
	reviewerRole  = "reviewer"
)

type modelConnectionSpec struct {
	Vendor    string `json:"vendor"`
	Adapter   string `json:"adapter"`
	BaseURL   string `json:"base_url,omitempty"`
	KeyEnv    string `json:"key_env,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
	Enabled   bool   `json:"enabled"`
}

func modelConnectionArea() Area {
	a := base[modelConnectionSpec](KindModelConnection, "AI connection", ScopeOrganization, domain.PermModelsRead)
	a.Setting, a.Hidden = domain.ModelConnection{}, []string{"id", "account_id", "name", "updated_by", "created_at", "updated_at"}
	a.Normalize = func(raw json.RawMessage) (json.RawMessage, error) {
		// A connection is on unless the spec says otherwise.
		var on struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &on); err != nil {
			return nil, invalid("spec: %v", err)
		}
		s, err := decode[modelConnectionSpec](raw)
		if err != nil {
			return nil, err
		}
		s.Enabled = on.Enabled == nil || *on.Enabled
		return encode(s), nil
	}
	a.Needs = needsOf(domain.PermModelsManage)
	a.Read = modelConnectionRead
	a.Validate = modelConnectionValidate
	a.Apply = modelConnectionApply
	a.Protection = modelConnectionProtection
	a.Secrets = func(want Resource) []SecretField {
		s, _ := decode[modelConnectionSpec](want.Spec)
		return []SecretField{{Ref: s.SecretRef, Title: "API key of " + want.Name}}
	}
	a.Affects = "Jobs leased from now on see the change; workers need nothing."
	return a
}

func specOfConnection(c domain.ModelConnection) modelConnectionSpec {
	return modelConnectionSpec{c.Vendor, c.Adapter, c.BaseURL, c.KeyEnv, c.SecretRef, c.Enabled}
}

// modelConnectionRead lists the organization's connections and, as "Managed by
// server file", the runtimes it may use.
func modelConnectionRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.ModelConnections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, c := range rows {
		out = append(out, Resource{Kind: KindModelConnection, Name: c.Name, Spec: encode(specOfConnection(c)), Scope: ScopeOrganization, Origin: OriginDatabase})
	}
	if e.Server == nil || !e.Server.ServerKeys() {
		return out, nil
	}
	runtimes, err := runtimeArea().Read(ctx, e)
	if err != nil {
		return nil, err
	}
	for _, r := range runtimes {
		s, err := decode[runtimeSpec](r.Spec)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(rows, func(c domain.ModelConnection) bool { return c.Name == r.Name }) {
			continue
		}
		spec := modelConnectionSpec{Vendor: s.Vendor, Adapter: s.Adapter, BaseURL: s.BaseURL, Enabled: true}
		if len(s.CredentialEnv) > 0 {
			spec.KeyEnv = s.CredentialEnv[0]
		}
		out = append(out, Resource{Kind: KindModelConnection, Name: r.Name, Spec: encode(spec), Scope: ScopeServer, Origin: OriginFile})
	}
	return out, nil
}

func modelConnectionValidate(_ context.Context, _ *Env, old *Resource, want Resource) error {
	s, err := decode[modelConnectionSpec](want.Spec)
	if err != nil {
		return err
	}
	if old != nil && old.Origin == OriginFile {
		return invalid("it is managed by the server file; use another name for a connection of the organization")
	}
	if !config.ValidRuntimeName(want.Name) {
		return invalid("the name must be lowercase letters, digits and -")
	}
	if strings.TrimSpace(s.Vendor) == "" {
		return invalid("vendor is required")
	}
	if !config.ValidAdapter(s.Adapter) {
		return invalid("adapter %q is not one Forecrew runs", s.Adapter)
	}
	if s.BaseURL != "" {
		if err := config.CheckBaseURL(s.BaseURL); err != nil {
			return invalid("base_url: %v", err)
		}
	}
	own := runtime.KeyEnv(s.Adapter)
	switch {
	case own != "" && s.KeyEnv != "" && s.KeyEnv != own:
		return invalid("key_env: %s reads its key from %s", s.Adapter, own)
	case own == "" && !domain.ValidModelKeyEnv(s.KeyEnv):
		return invalid("key_env: %s needs the variable of its provider, uppercase and ending in _KEY", s.Adapter)
	}
	if !domain.ValidModelSecretRef(s.SecretRef) {
		return invalid("secret_ref must be %s%s<name>", domain.SecretRefStore, domain.ModelSecretPrefix)
	}
	return checkNoSecret(string(want.Spec))
}

func modelConnectionApply(ctx context.Context, e *Env, old, want *Resource) error {
	if want == nil {
		if err := e.Store.ModelConnections().Delete(ctx, e.Actor.AccountID, old.Name); err != nil {
			return err
		}
		return auditModel(ctx, e, "model_connection.deleted", old.Name, nil)
	}
	s, err := decode[modelConnectionSpec](want.Spec)
	if err != nil {
		return err
	}
	c := domain.ModelConnection{AccountID: e.Actor.AccountID, Name: want.Name, Vendor: s.Vendor, Adapter: s.Adapter, BaseURL: s.BaseURL, KeyEnv: s.KeyEnv,
		SecretRef: s.SecretRef, Enabled: s.Enabled, UpdatedBy: e.Actor.String()}
	if err := e.Store.ModelConnections().Put(ctx, &c); err != nil {
		return err
	}
	var before *modelConnectionSpec
	if old != nil {
		o, err := decode[modelConnectionSpec](old.Spec)
		if err != nil {
			return err
		}
		before = &o
	}
	for _, action := range connectionActions(before, s) {
		if err := auditModel(ctx, e, action, want.Name, map[string]any{"secret_ref": s.SecretRef, "base_url": s.BaseURL}); err != nil {
			return err
		}
	}
	return nil
}

// connectionActions names what happened to a connection, for the audit log.
func connectionActions(old *modelConnectionSpec, s modelConnectionSpec) []string {
	if old == nil {
		return []string{"model_connection.created"}
	}
	var out []string
	if old.SecretRef != s.SecretRef {
		out = append(out, "model_connection.rotated")
	}
	if old.Enabled != s.Enabled {
		out = append(out, map[bool]string{true: "model_connection.enabled", false: "model_connection.disabled"}[s.Enabled])
	}
	if len(out) == 0 {
		out = append(out, "model_connection.changed")
	}
	return out
}

// auditModel records a change of a model resource. Data holds references,
// never a value.
func auditModel(ctx context.Context, e *Env, action, name string, data map[string]any) error {
	all := map[string]any{"name": name}
	maps.Copy(all, data)
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return e.Store.Audit().Append(ctx, &domain.AuditEntry{AccountID: e.Actor.AccountID, Actor: e.Actor.String(), Credential: e.Actor.Credential.String(), Action: action,
		SubjectType: "model_settings", Data: raw})
}

// modelConnectionProtection flags every change of where a key goes, or of the
// key: the new endpoint or key is what the next job's code and key meet.
func modelConnectionProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want == nil {
		return nil
	}
	w, _ := decode[modelConnectionSpec](want.Spec)
	if old == nil {
		return []string{"Sends the code of jobs on " + want.Name + " to a new provider with a new key"}
	}
	o, _ := decode[modelConnectionSpec](old.Spec)
	var out []string
	if o.BaseURL != w.BaseURL || o.Adapter != w.Adapter || o.KeyEnv != w.KeyEnv {
		out = append(out, "Changes where the key of "+want.Name+" is sent")
	}
	if o.SecretRef != w.SecretRef {
		out = append(out, "Changes the key of "+want.Name)
	}
	if w.Enabled && !o.Enabled {
		out = append(out, "Sends code and the key of "+want.Name+" out again")
	}
	return out
}

type modelRoleSpec struct {
	Connection string `json:"connection"`
	Tier       string `json:"tier"`
}

type modelAssignmentSpec struct {
	Tiers            map[string]map[string]string `json:"tiers,omitempty"`
	Roles            map[string]modelRoleSpec     `json:"roles,omitempty"`
	ProjectOverrides *bool                        `json:"project_overrides,omitempty"`
}

func modelAssignmentArea() Area {
	a := base[modelAssignmentSpec](KindModelAssignment, "AI models of", ScopeOrganization, domain.PermModelsRead)
	a.Setting, a.Hidden = domain.ModelAssignment{}, []string{"id", "account_id", "project_id", "updated_by", "created_at", "updated_at"}
	a.Needs = modelAssignmentNeeds
	a.Read = modelAssignmentRead
	a.Validate = modelAssignmentValidate
	a.Apply = modelAssignmentApply
	a.Affects = "Queued jobs and jobs leased from now on follow it."
	return a
}

func specOfAssignment(a domain.ModelAssignment) modelAssignmentSpec {
	s := modelAssignmentSpec{Tiers: a.Tiers, ProjectOverrides: a.ProjectOverrides}
	for name, r := range a.Roles {
		if s.Roles == nil {
			s.Roles = map[string]modelRoleSpec{}
		}
		s.Roles[name] = modelRoleSpec(r)
	}
	return s
}

func assignmentOf(s modelAssignmentSpec) domain.ModelAssignment {
	a := domain.ModelAssignment{Tiers: s.Tiers, Roles: map[string]domain.ModelRole{}, ProjectOverrides: s.ProjectOverrides}
	for name, r := range s.Roles {
		a.Roles[name] = domain.ModelRole(r)
	}
	return a
}

// assignmentScope returns the project of a model_assignment's name, the zero
// ID for the organization's.
func assignmentScope(ctx context.Context, e *Env, name string) (uuid.UUID, error) {
	if name == organizationName {
		return uuid.Nil(), nil
	}
	project, ok := strings.CutPrefix(name, assignmentProject)
	if !ok {
		return uuid.Nil(), invalid("the name is %s, or %s<project>", organizationName, assignmentProject)
	}
	p, err := projectOf(ctx, e, project)
	return p.ID, err
}

func modelAssignmentNeeds(ctx context.Context, e *Env, old, want *Resource, _ []domain.ConfigFieldChange) ([]Need, error) {
	name := ""
	if old != nil {
		name = old.Name
	}
	if want != nil {
		name = want.Name
	}
	id, err := assignmentScope(ctx, e, name)
	if err != nil {
		return nil, err
	}
	if id == uuid.Nil() {
		return need(domain.PermModelsManage), nil
	}
	return []Need{{Permission: domain.PermModelsAssign, ProjectID: id}}, nil
}

// modelAssignmentRead lists the organization's assignment and the project
// overrides; with no row, the server file's as "Managed by server file".
func modelAssignmentRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.ModelAssignments().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	projects, err := e.Store.Projects().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	out := make([]Resource, 0, len(rows)+1)
	org := false
	for _, a := range rows {
		r := Resource{Kind: KindModelAssignment, Name: organizationName, Spec: encode(specOfAssignment(a)), Scope: ScopeOrganization, Origin: OriginDatabase}
		if a.ProjectID != uuid.Nil() {
			r.Name, r.Scope = assignmentProject+names[a.ProjectID], ScopeProject
		} else {
			org = true
		}
		out = append(out, r)
	}
	if !org && e.Server != nil && e.Server.ServerKeys() {
		s := modelAssignmentSpec{Tiers: map[string]map[string]string{}, Roles: map[string]modelRoleSpec{}}
		for tier, vendors := range e.Server.ModelTiers {
			s.Tiers[string(tier)] = vendors
		}
		for name, role := range e.Server.Roles {
			s.Roles[name] = modelRoleSpec{role.Runtime, string(role.Tier)}
		}
		out = append(out, Resource{Kind: KindModelAssignment, Name: organizationName, Spec: encode(s), Scope: ScopeOrganization, Origin: OriginFile})
	}
	return out, nil
}

func modelAssignmentValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	s, err := decode[modelAssignmentSpec](want.Spec)
	if err != nil {
		return err
	}
	id, err := assignmentScope(ctx, e, want.Name)
	if err != nil {
		return err
	}
	if id != uuid.Nil() {
		if s.ProjectOverrides != nil {
			return invalid("project_overrides is a policy of the organization")
		}
		rows, err := e.Store.ModelAssignments().List(ctx, e.Actor.AccountID)
		if err != nil {
			return err
		}
		if !models.NewState(nil, rows).Org.OverridesAllowed() {
			return invalid("the organization does not let projects override their models (models.project_overrides)")
		}
	}
	for tier, vendors := range s.Tiers {
		if !config.Tier(tier).Valid() {
			return invalid("tier %q must be fast, strong or best", tier)
		}
		for vendor, model := range vendors {
			if vendor == "" || model == "" {
				return invalid("tiers.%s: a vendor and a model are required", tier)
			}
		}
	}
	for role, r := range s.Roles {
		if !config.Tier(r.Tier).Valid() {
			return invalid("roles.%s: tier %q must be fast, strong or best", role, r.Tier)
		}
		if r.Connection == "" {
			return invalid("roles.%s: connection is required", role)
		}
	}
	return checkNoSecret(string(want.Spec))
}

func modelAssignmentApply(ctx context.Context, e *Env, old, want *Resource) error {
	name := ""
	if old != nil {
		name = old.Name
	}
	if want != nil {
		name = want.Name
	}
	id, err := assignmentScope(ctx, e, name)
	if err != nil {
		return err
	}
	if want == nil {
		if err := e.Store.ModelAssignments().Delete(ctx, e.Actor.AccountID, id); err != nil {
			return err
		}
		return auditModel(ctx, e, "model_assignment.deleted", name, nil)
	}
	s, err := decode[modelAssignmentSpec](want.Spec)
	if err != nil {
		return err
	}
	a := assignmentOf(s)
	a.AccountID, a.ProjectID, a.UpdatedBy = e.Actor.AccountID, id, e.Actor.String()
	if err := e.Store.ModelAssignments().Put(ctx, &a); err != nil {
		return err
	}
	return auditModel(ctx, e, "model_assignment.changed", name, map[string]any{"roles": s.Roles})
}

// modelWorld is the model settings of an organization as a plan would leave
// them, and where each project's rows are.
type modelWorld struct {
	state  models.State
	server *config.Config
	names  map[uuid.UUID]string
}

// modelWorldOf reads what the store holds.
func modelWorldOf(ctx context.Context, e *Env) (*modelWorld, error) {
	conns, err := e.Store.ModelConnections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	rows, err := e.Store.ModelAssignments().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	projects, err := e.Store.Projects().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	w := &modelWorld{state: models.NewState(conns, rows), server: &config.Config{}, names: map[uuid.UUID]string{}}
	for _, p := range projects {
		w.names[p.ID] = p.Name
	}
	if e.Server != nil {
		w.server = e.Server
		all, err := stored(ctx, e.Store)
		if err != nil {
			return nil, err
		}
		if merged, err := e.Server.WithSettings(all, e.Getenv); err == nil {
			w.server = merged
		}
	}
	return w, nil
}

// overlay puts the changes of a plan on w.
func (w *modelWorld) overlay(ctx context.Context, e *Env, changes []domain.ConfigChange) error {
	for _, c := range changes {
		_, want := resources(c)
		switch c.Kind {
		case KindModelConnection:
			if want == nil {
				delete(w.state.Connections, c.Name)
				continue
			}
			s, err := decode[modelConnectionSpec](want.Spec)
			if err != nil {
				return err
			}
			w.state.Connections[c.Name] = domain.ModelConnection{Name: c.Name, Vendor: s.Vendor, Adapter: s.Adapter, BaseURL: s.BaseURL, KeyEnv: s.KeyEnv, SecretRef: s.SecretRef, Enabled: s.Enabled}
		case KindModelAssignment:
			id, err := assignmentScope(ctx, e, c.Name)
			if err != nil {
				return err
			}
			var a domain.ModelAssignment
			if want != nil {
				s, err := decode[modelAssignmentSpec](want.Spec)
				if err != nil {
					return err
				}
				a = assignmentOf(s)
			}
			if id == uuid.Nil() {
				w.state.Org = a
			} else if want == nil {
				delete(w.state.Projects, id)
			} else {
				w.state.Projects[id] = a
			}
		}
	}
	return nil
}

// scopes lists the assignments that count, with who they are for.
func (w *modelWorld) scopes() []modelScope {
	out := []modelScope{{"the organization", uuid.Nil(), w.state.Org}}
	if !w.state.Org.OverridesAllowed() {
		return out
	}
	for _, id := range slices.SortedFunc(maps.Keys(w.state.Projects), func(a, b uuid.UUID) int { return strings.Compare(w.names[a], w.names[b]) }) {
		out = append(out, modelScope{"project " + w.names[id], id, w.state.Projects[id]})
	}
	return out
}

type modelScope struct {
	title      string
	project    uuid.UUID
	assignment domain.ModelAssignment
}

// problems lists what would leave a role without a working connection.
func (w *modelWorld) problems(e *Env) []string {
	var out []string
	for _, s := range w.scopes() {
		for _, role := range slices.Sorted(maps.Keys(s.assignment.Roles)) {
			d := w.state.Decide(w.server, models.Request{ProjectID: s.project, Role: role})
			if d.Waiting != "" {
				out = append(out, s.title+": "+d.Waiting)
				continue
			}
			if e.Roles == nil {
				continue
			}
			switch toolbox, known := e.Roles(role); {
			case !known:
				out = append(out, fmt.Sprintf("%s: there is no role %s", s.title, role))
			case toolbox && d.Adapter == config.AdapterOpencode:
				out = append(out, fmt.Sprintf("%s: role %s runs in a toolbox, and opencode does not run in a toolbox container yet: give it a connection of claude-code or codex", s.title, role))
			}
		}
		if !w.server.Policy.AllowSameVendorReview {
			out = append(out, w.separation(s)...)
		}
	}
	return out
}

// separation keeps the reviewer off the developer's vendor, as config.md's
// policy.allow_same_vendor_review says.
func (w *modelWorld) separation(s modelScope) []string {
	dev := w.state.Decide(w.server, models.Request{ProjectID: s.project, Role: developerRole})
	rev := w.state.Decide(w.server, models.Request{ProjectID: s.project, Role: reviewerRole})
	if dev.Waiting != "" || rev.Waiting != "" || dev.Vendor != rev.Vendor || (dev.Source == models.SourceServer && rev.Source == models.SourceServer) {
		return nil
	}
	return []string{fmt.Sprintf("%s: reviewer and developer both use vendor %q; set policy.allow_same_vendor_review to allow it", s.title, dev.Vendor)}
}

// users lists the roles on a connection, with who they are for.
func (w *modelWorld) users(connection string) []string {
	var out []string
	for _, s := range w.scopes() {
		for _, role := range slices.Sorted(maps.Keys(s.assignment.Roles)) {
			if s.assignment.Roles[role].Connection == connection {
				out = append(out, role+" ("+s.title+")")
			}
		}
	}
	return out
}

// retired reports whether c turns a connection off or deletes it.
func retired(c domain.ConfigChange) bool {
	if c.Kind != KindModelConnection {
		return false
	}
	old, want := resources(c)
	if want == nil {
		return true
	}
	s, _ := decode[modelConnectionSpec](want.Spec)
	if old == nil {
		return false
	}
	o, _ := decode[modelConnectionSpec](old.Spec)
	return o.Enabled && !s.Enabled
}

func touchesModels(changes []domain.ConfigChange) bool {
	return slices.ContainsFunc(changes, func(c domain.ConfigChange) bool {
		return c.Kind == KindModelConnection || c.Kind == KindModelAssignment
	})
}

// reviewModels looks at a plan's model changes together, which no area can do
// alone: a plan may create a connection and assign it, or turn one off and
// reassign its roles. It returns what is wrong and who the plan affects.
func reviewModels(ctx context.Context, e *Env, changes []domain.ConfigChange) (problems []string, affects string, err error) {
	if !touchesModels(changes) {
		return nil, "", nil
	}
	w, err := modelWorldOf(ctx, e)
	if err != nil {
		return nil, "", err
	}
	var notes []string
	for _, c := range changes {
		if !retired(c) {
			continue
		}
		if users := w.users(c.Name); len(users) > 0 {
			notes = append(notes, "Connection "+c.Name+" serves "+strings.Join(users, ", ")+".")
		}
	}
	if err := w.overlay(ctx, e, changes); err != nil {
		return nil, "", err
	}
	return w.problems(e), strings.Join(notes, " "), nil
}

// recheckModels is the same check inside the transaction of a plan, on what
// its changes left: a role assigned since the plan was made is not in its
// digest.
func recheckModels(ctx context.Context, e *Env, changes []domain.ConfigChange) error {
	if !touchesModels(changes) {
		return nil
	}
	w, err := modelWorldOf(ctx, e)
	if err != nil {
		return err
	}
	if problems := w.problems(e); len(problems) > 0 {
		return invalid("%s", strings.Join(problems, "; "))
	}
	return nil
}

package resource

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
)

// KindModelAuthorization lets a model connection receive the private code of
// a project, or of every project.
const KindModelAuthorization = "model_authorization"

// modelAuthorizationSpec is empty: who and when come from the plan.
type modelAuthorizationSpec struct{}

func modelAuthorizationArea() Area {
	a := base[modelAuthorizationSpec](KindModelAuthorization, "Private code for", ScopeProject, domain.PermModelsRead)
	a.Setting = domain.ModelAuthorization{}
	a.Hidden = []string{"id", "account_id", "project_id", "connection_id", "connection", "adapter", "base_url", "vendor", "key_env", "authorized_by", "created_at"}
	a.Needs = modelAuthorizationNeeds
	a.Read = modelAuthorizationRead
	a.Validate = modelAuthorizationValidate
	a.Apply = modelAuthorizationApply
	a.Protection = func(_ context.Context, _ *Env, _, want *Resource) []string {
		if want == nil {
			return nil
		}
		conn, project, _ := splitAuthorization(want.Name)
		return []string{"Sends the private code of " + projectWords(project) + " through connection " + conn}
	}
	a.Affects = "Jobs on private repos run on the connection only where it is authorized; revoking stops new jobs."
	return a
}

// authorizationName is the name of a model_authorization.
func authorizationName(connection, project string) string { return connection + "@" + project }

// splitAuthorization reads <connection>@<project>, the project being
// domain.AllProjects for every project.
func splitAuthorization(name string) (connection, project string, ok bool) {
	connection, project, ok = strings.Cut(name, "@")
	return connection, project, ok && connection != "" && project != ""
}

func projectWords(project string) string {
	if project == domain.AllProjects {
		return "every project"
	}
	return "project " + project
}

func modelAuthorizationNeeds(ctx context.Context, e *Env, old, want *Resource, _ []domain.ConfigFieldChange) ([]Need, error) {
	_, project, ok := splitAuthorization(firstOf(old, want).Name)
	if !ok {
		return nil, invalid("the name is <connection>@<project>, or <connection>@%s", domain.AllProjects)
	}
	if project == domain.AllProjects {
		return need(domain.PermModelsAuthorize), nil
	}
	p, err := projectOf(ctx, e, project)
	if err != nil {
		return nil, err
	}
	return []Need{{Permission: domain.PermModelsAssign, ProjectID: p.ID}}, nil
}

func modelAuthorizationRead(ctx context.Context, e *Env) ([]Resource, error) {
	rows, err := e.Store.ModelAuthorizations().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	names, err := projectNames(ctx, e)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, a := range rows {
		r := Resource{Kind: KindModelAuthorization, Name: authorizationName(a.Connection, domain.AllProjects), Spec: encode(modelAuthorizationSpec{}), Scope: ScopeOrganization, Origin: OriginDatabase}
		if a.ProjectID != uuid.Nil() {
			r.Name, r.Scope = authorizationName(a.Connection, names[a.ProjectID]), ScopeProject
		}
		out = append(out, r)
	}
	return out, nil
}

func projectNames(ctx context.Context, e *Env) (map[uuid.UUID]string, error) {
	projects, err := e.Store.Projects().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	return names, nil
}

// modelAuthorizationValidate checks the name; that the connection is one of
// the organization's is checked with the whole plan (reviewModels), which may
// create it.
func modelAuthorizationValidate(ctx context.Context, e *Env, _ *Resource, want Resource) error {
	conn, project, ok := splitAuthorization(want.Name)
	if !ok || !config.ValidRuntimeName(conn) {
		return invalid("the name is <connection>@<project>, or <connection>@%s", domain.AllProjects)
	}
	if project != domain.AllProjects {
		if _, err := projectOf(ctx, e, project); err != nil {
			return err
		}
	}
	return nil
}

func modelAuthorizationApply(ctx context.Context, e *Env, old, want *Resource) error {
	conn, project, _ := splitAuthorization(firstOf(old, want).Name)
	id := uuid.Nil()
	if project != domain.AllProjects {
		p, err := projectOf(ctx, e, project)
		if err != nil {
			return err
		}
		id = p.ID
	}
	c, err := connectionNamed(ctx, e, conn)
	if err != nil {
		return err
	}
	if want == nil {
		if err := e.Store.ModelAuthorizations().Delete(ctx, e.Actor.AccountID, c.ID, id); err != nil {
			return err
		}
		return auditModel(ctx, e, "model_authorization.deleted", old.Name, nil)
	}
	a := domain.AuthorizationOf(c, id, e.Actor.String())
	if err := e.Store.ModelAuthorizations().Put(ctx, &a); err != nil {
		return err
	}
	return auditModel(ctx, e, "model_authorization.created", want.Name, map[string]any{"base_url": c.BaseURL, "adapter": c.Adapter})
}

// connectionNamed returns the organization's connection called name.
func connectionNamed(ctx context.Context, e *Env, name string) (domain.ModelConnection, error) {
	rows, err := e.Store.ModelConnections().List(ctx, e.Actor.AccountID)
	if err != nil {
		return domain.ModelConnection{}, err
	}
	i := slices.IndexFunc(rows, func(c domain.ModelConnection) bool { return c.Name == name })
	if i < 0 {
		return domain.ModelConnection{}, invalid("connection %s is not one of the organization's", name)
	}
	return rows[i], nil
}

// impliedAuthorizations adds to a plan the authorizations its project
// assignments need: a plan that puts a role of a project on a connection
// sends that project's code there, so confirming it authorizes the
// connection for the project, and the plan says so like any other change.
// A connection to a local address, one authorized already and one the plan
// lists itself need none.
func impliedAuthorizations(ctx context.Context, e *Env, wanted []Wanted) ([]Wanted, error) {
	if !slices.ContainsFunc(wanted, func(w Wanted) bool {
		return w.Kind == KindModelAssignment && strings.HasPrefix(w.Name, assignmentProject)
	}) {
		return nil, nil
	}
	w, err := modelWorldOf(ctx, e)
	if err != nil {
		return nil, err
	}
	auths, err := e.Store.ModelAuthorizations().List(ctx, e.Actor.AccountID)
	if err != nil {
		return nil, err
	}
	listed := listedAuthorizations(wanted)
	conns := plannedConnections(e, w, wanted)
	var out []Wanted
	for _, x := range wanted {
		project, ok := strings.CutPrefix(x.Name, assignmentProject)
		if x.Kind != KindModelAssignment || !ok || x.Delete {
			continue
		}
		s, err := decode[modelAssignmentSpec](x.Spec)
		if err != nil {
			continue // Validate reports it
		}
		p, err := projectOf(ctx, e, project)
		if err != nil {
			continue
		}
		before := w.state.Projects[p.ID].Roles
		for _, role := range slices.Sorted(maps.Keys(s.Roles)) {
			name := s.Roles[role].Connection
			key := authorizationName(name, project)
			if before[role].Connection == name || listed[key] || listed[authorizationName(name, domain.AllProjects)] || !needsAuthorization(conns, auths, name, p.ID) {
				continue
			}
			listed[key] = true
			out = append(out, Wanted{Kind: KindModelAuthorization, Name: key, Spec: encode(modelAuthorizationSpec{})})
		}
	}
	return out, nil
}

// needsAuthorization reports whether connection name, one of the
// organization's, would receive the code of project without an
// authorization: it is not local and none covers it.
func needsAuthorization(conns map[string]domain.ModelConnection, auths []domain.ModelAuthorization, name string, project uuid.UUID) bool {
	c, known := conns[name]
	return known && !models.LocalEndpoint(c.BaseURL) && !slices.ContainsFunc(auths, func(a domain.ModelAuthorization) bool { return a.Covers(project, c) })
}

// listedAuthorizations are the authorizations a plan names itself.
func listedAuthorizations(wanted []Wanted) map[string]bool {
	listed := map[string]bool{}
	for _, x := range wanted {
		if x.Kind == KindModelAuthorization {
			listed[x.Name] = true
		}
	}
	return listed
}

// plannedConnections are the organization's connections as the wanted
// changes leave their endpoints.
func plannedConnections(e *Env, w *modelWorld, wanted []Wanted) map[string]domain.ModelConnection {
	conns := maps.Clone(w.state.Connections)
	for _, x := range wanted {
		if x.Kind != KindModelConnection || x.Delete {
			continue
		}
		if s, err := decode[modelConnectionSpec](x.Spec); err == nil {
			conns[x.Name] = domain.ModelConnection{Name: x.Name, Vendor: s.Vendor, Adapter: s.Adapter, BaseURL: s.BaseURL, KeyEnv: s.KeyEnv,
				ID: conns[x.Name].ID, AccountID: e.Actor.AccountID}
		}
	}
	return conns
}

// describeAuthorizations says, for each authorization a plan makes, whose
// code goes to which provider and where, and refuses one for a connection the
// organization does not have.
func describeAuthorizations(ctx context.Context, e *Env, w *modelWorld, changes []domain.ConfigChange) (problems, notes []string, err error) {
	cat, _ := models.DefaultCatalog() // without it, the vendor is named
	for _, c := range changes {
		if c.Kind != KindModelAuthorization || c.Op == domain.ConfigDelete {
			continue
		}
		name, project, _ := splitAuthorization(c.Name)
		conn, ok := w.state.Connections[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s/%s: connection %s is not one of the organization's", c.Kind, c.Name, name))
			continue
		}
		repos, err := reposOf(ctx, e, project)
		if err != nil {
			return nil, nil, err
		}
		notes = append(notes, fmt.Sprintf("The code of %s goes to %s through connection %s.", repos, providerWords(cat, conn), name))
	}
	return problems, notes, nil
}

// reposOf lists the repos of project, or of every project.
func reposOf(ctx context.Context, e *Env, project string) (string, error) {
	if project != domain.AllProjects {
		p, err := projectOf(ctx, e, project)
		return p.Repo, err
	}
	projects, err := e.Store.Projects().List(ctx, e.Actor.AccountID)
	if err != nil {
		return "", err
	}
	repos := make([]string, 0, len(projects))
	for _, p := range projects {
		repos = append(repos, p.Repo)
	}
	slices.Sort(repos)
	return "every project (" + cmp.Or(strings.Join(repos, ", "), "none yet") + ")", nil
}

// providerWords names the provider of a connection and where it runs, from
// the catalog.
func providerWords(cat *models.Catalog, c domain.ModelConnection) string {
	if models.LocalEndpoint(c.BaseURL) {
		return c.BaseURL + ", on your own network"
	}
	p, ok := cat.Provider(c.Vendor)
	if !ok {
		return fmt.Sprintf("%s (vendor %s, not in the model catalog)", cmp.Or(c.BaseURL, "its vendor's endpoint"), c.Vendor)
	}
	where := p.Where()
	if c.BaseURL != "" && c.BaseURL != p.BaseURL {
		return fmt.Sprintf("%s at %s (%s)", p.Title, c.BaseURL, where)
	}
	return fmt.Sprintf("%s (%s)", p.Title, where)
}

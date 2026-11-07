// Package resource makes every setting of Forecrew a declarative resource
// (docs/contracts/config-resources.md): a kind, a name and a spec. Each area
// registers Schema, Read, Validate, Diff and Apply on top of the existing
// services. Engine turns a list of wanted resources into a stored change
// plan, and applies, reverts, exports and imports them. Forms, YAML, the CLI,
// MCP clients and the assistant all go through it.
package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Resource kinds.
const (
	KindOrganization    = "organization"
	KindMember          = "member"
	KindDomain          = "domain"
	KindTeam            = "team"
	KindRoleBinding     = "role_binding"
	KindConnection      = "connection"
	KindPool            = "pool"
	KindRuntime         = "runtime"
	KindAuth            = "auth"
	KindProvider        = "provider"
	KindShape           = "shape"
	KindFleetPolicy     = "fleet_policy"
	KindSchedule        = "schedule"
	KindIntegration     = "integration"
	KindKnowledgePolicy = "knowledge_policy"
	KindRepoFile        = "repo_file"
	KindModelConnection = "model_connection"
	KindModelAssignment = "model_assignment"
	KindTrackerConn     = "tracker_connection"
	KindTrackerRoute    = "tracker_route"
)

// Scope and origin of a resource.
const (
	ScopeOrganization = "organization"
	ScopeServer       = "server"
	ScopeProject      = "project"
	OriginFile        = "file"     // set only by forecrew.yaml
	OriginDatabase    = "database" // stored over it
)

// The types of the tools live in internal/mcp, which this package builds on.
type (
	// Resource is one setting.
	Resource = mcp.ConfigResource
	// SchemaInfo describes one kind of resource.
	SchemaInfo = mcp.ConfigSchema
	// Wanted is one change someone asks for.
	Wanted = mcp.ConfigWanted
	// View is a plan as the server shows it.
	View = mcp.ConfigPlanView
	// ShowOnce is a value Forecrew generated, shown once.
	ShowOnce = mcp.ConfigShowOnce
	// Applied is the answer of Apply.
	Applied = mcp.ConfigApplied
)

// Need is a permission a change takes, on a project or on the organization.
type Need struct {
	Permission domain.Permission
	ProjectID  uuid.UUID
}

// Access is what areas use of the existing services, bound to the
// transaction of the plan: each call checks the actor's permission and the
// rule of No escalation. *tools.Service implements it.
type Access interface {
	CreateTeam(ctx context.Context, by domain.Actor, in mcp.CreateTeamInput) (domain.Team, error)
	UpdateTeam(ctx context.Context, by domain.Actor, in mcp.UpdateTeamInput) (domain.Team, error)
	DeleteTeam(ctx context.Context, by domain.Actor, in mcp.TeamRefInput) (domain.Team, error)
	AddTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error)
	RemoveTeamMember(ctx context.Context, by domain.Actor, in mcp.TeamMemberInput) (domain.TeamMember, error)
	CreateRoleBinding(ctx context.Context, by domain.Actor, in mcp.CreateRoleBindingInput) (mcp.RoleBindingInfo, error)
	DeleteRoleBinding(ctx context.Context, by domain.Actor, in mcp.RoleBindingRefInput) (mcp.RoleBindingInfo, error)
	CreateInvitation(ctx context.Context, by domain.Actor, in mcp.CreateInvitationInput) (mcp.CreateInvitationOutput, error)
	RevokeInvitation(ctx context.Context, by domain.Actor, in mcp.InvitationRefInput) (domain.Invitation, error)
	// CheckHolder is the rule of No escalation for acting on a subject that
	// holds roles.
	CheckHolder(ctx context.Context, by domain.Actor, kind domain.SubjectKind, id uuid.UUID) error
	// TrackerConnectionRow checks a wanted tracker connection short of asking
	// the tracker, and lists the config keys its adapter fills in.
	TrackerConnectionRow(ctx context.Context, by domain.Actor, in mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, []string, error)
	// TrackerDerived lists the config keys the adapter of a connection fills
	// in.
	TrackerDerived(c domain.TrackerConnection) []string
	// TrackerRouteRow checks a wanted route; ask checks its states against
	// the tracker.
	TrackerRouteRow(ctx context.Context, by domain.Actor, in mcp.CreateTrackerRouteInput, ask bool) (domain.TrackerRoute, error)
}

// FileChange is one file of a repo plan; nil Content deletes it.
type FileChange struct {
	Path    string
	Content []byte
}

// Env is what an area works with.
type Env struct {
	Store  store.Store // in the plan's transaction while applying
	Actor  domain.Actor
	Authz  *authz.Engine
	Access Access
	// Server is forecrew.yaml as read, without stored settings.
	Server *config.Config
	Getenv func(string) string
	Repo   Repo
	// CheckSchedule refuses a schedule forecrewd would not start.
	CheckSchedule func(config.Schedule) error
	// Roles says whether a role runs in a toolbox and whether it exists; nil
	// checks nothing.
	Roles func(role string) (toolbox, known bool)
	// repo collects the files of a repo plan while applying.
	files   []FileChange
	project *domain.Project
	// shown collects what Apply generated and shows once.
	shown []ShowOnce
}

// SecretField is a secret a spec refers to.
type SecretField struct {
	Ref   string // the reference in the spec
	Title string
}

// Area is one registered kind of resource. Schema, Read, Validate, Diff and
// Apply are required, the others optional.
type Area struct {
	Kind  string
	Title string // what people call it
	// Scope is where its resources apply.
	Scope string
	// ReadPermission is what reading it takes; a caller without it sees
	// nothing of the area, and neither does the assistant.
	ReadPermission domain.Permission
	// AlsoRead is another permission that reads the area: billing.manage on
	// the areas with budget fields (permissions.md, Budget fields).
	AlsoRead domain.Permission
	// Schema is the JSON Schema of the spec.
	Schema func() json.RawMessage
	// Read returns the resources as they are.
	Read func(ctx context.Context, e *Env) ([]Resource, error)
	// Normalize gives a wanted spec the form Read gives it, refusing keys
	// the schema does not have.
	Normalize func(raw json.RawMessage) (json.RawMessage, error)
	// Validate checks a wanted resource; old is nil for a new one. Its
	// error wraps mcp.ErrInvalidInput and says what is wrong.
	Validate func(ctx context.Context, e *Env, old *Resource, want Resource) error
	// Diff lists what differs, for people; empty means nothing changes.
	Diff func(old, want *Resource) []domain.ConfigFieldChange
	// Apply makes old into want; a nil want deletes old.
	Apply func(ctx context.Context, e *Env, old, want *Resource) error

	// Needs lists what a change takes, beyond config.apply.
	Needs func(ctx context.Context, e *Env, old, want *Resource, fields []domain.ConfigFieldChange) ([]Need, error)
	// Protection gives a reason for each drop in protection of a change.
	Protection func(ctx context.Context, e *Env, old, want *Resource) []string
	// Secrets lists the secrets a spec refers to.
	Secrets func(want Resource) []SecretField
	// Steps lists what has to happen after the plan, such as connecting a
	// machine.
	Steps func(old, want *Resource) []domain.ConfigStep
	// Affects says who notices, Cost what it costs.
	Affects string
	Cost    func(want Resource) string
	// Lost says what cannot come back after the change; empty means it can
	// be reverted.
	Lost func(old, want *Resource) string
	// NoDelete, when set, says why a resource of the area is not deleted.
	NoDelete string
	// Retire returns the spec that turns off a resource that has no delete:
	// deleting it, and reverting its creation, means this.
	Retire func(spec json.RawMessage) json.RawMessage
	// RepoFiles marks a kind whose Apply opens a pull request.
	RepoFiles bool
	// Setting is the type that holds the area's settings and Hidden the
	// JSON fields of it that are no setting: TestEverySettingHasAResource
	// fails on any other field the spec lacks.
	Setting any
	Hidden  []string
	// Spec is a zero value of the spec.
	Spec any
}

// base starts the area of a spec type S: its schema, its normal form and the
// generic diff.
func base[S any](kind, title, scope string, read domain.Permission) Area {
	var zero S
	return Area{
		Kind: kind, Title: title, Scope: scope, ReadPermission: read, Spec: zero,
		Schema: schemaOf[S](),
		Normalize: func(raw json.RawMessage) (json.RawMessage, error) {
			s, err := decode[S](raw)
			if err != nil {
				return nil, err
			}
			return encode(s), nil
		},
		Diff: diffSpecs,
	}
}

// areas is the registry; the order is the order of Apply, so what others
// refer to comes first.
var areas = []Area{
	organizationArea(), runtimeArea(), poolArea(), authArea(), scheduleArea(),
	modelConnectionArea(), modelAssignmentArea(), modelAuthorizationArea(),
	connectionArea(), domainArea(), memberArea(), teamArea(), roleBindingArea(),
	providerArea(), shapeArea(), fleetPolicyArea(), trackerConnectionArea(), trackerRouteArea(),
	integrationArea(), knowledgePolicyArea(), repoFileArea(),
}

// Areas returns the registry.
func Areas() []Area { return slices.Clone(areas) }

// Lookup returns the area of kind.
func Lookup(kind string) (Area, bool) {
	i := slices.IndexFunc(areas, func(a Area) bool { return a.Kind == kind })
	if i < 0 {
		return Area{}, false
	}
	return areas[i], true
}

func order(kind string) int {
	return slices.IndexFunc(areas, func(a Area) bool { return a.Kind == kind })
}

// ---- specs

// invalid wraps a problem as bad input.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), mcp.ErrInvalidInput)
}

// canon returns raw with sorted keys, without insignificant space.
func canon(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// encode returns v as a canonical spec.
func encode(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err == nil {
		raw, err = canon(raw)
	}
	if err != nil {
		panic("resource: spec does not encode: " + err.Error()) // spec types are plain data
	}
	return raw
}

// decode reads a spec strictly: an unknown key is an error, because the
// assistant may only propose what the schema has.
func decode[S any](raw json.RawMessage) (S, error) {
	var s S
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, invalid("spec: %v", err)
	}
	return s, nil
}

// schemaOf returns the JSON Schema of S.
func schemaOf[S any]() func() json.RawMessage {
	return func() json.RawMessage {
		s, err := jsonschema.For[S](nil)
		if err != nil {
			panic("resource: no schema: " + err.Error()) // spec types are plain data
		}
		raw, err := json.Marshal(s)
		if err != nil {
			panic("resource: no schema: " + err.Error())
		}
		return raw
	}
}

// diffSpecs lists the fields that differ between two specs, with dotted
// paths into nested objects. Values are shown as JSON.
func diffSpecs(old, want *Resource) []domain.ConfigFieldChange {
	var a, b map[string]any
	if old != nil {
		_ = json.Unmarshal(old.Spec, &a) // specs are valid JSON objects
	}
	if want != nil {
		_ = json.Unmarshal(want.Spec, &b)
	}
	out := []domain.ConfigFieldChange{}
	diffMaps("", a, b, &out)
	return out
}

func diffMaps(prefix string, a, b map[string]any, out *[]domain.ConfigFieldChange) {
	keys := slices.Collect(maps.Keys(a))
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		x, y := a[k], b[k]
		mx, okx := x.(map[string]any)
		my, oky := y.(map[string]any)
		switch {
		case okx && oky, okx && y == nil, oky && x == nil:
			diffMaps(prefix+k+".", mx, my, out)
		case !sameValue(x, y):
			*out = append(*out, domain.ConfigFieldChange{Path: prefix + k, Old: show(x), New: show(y)})
		}
	}
}

func sameValue(x, y any) bool {
	a, _ := json.Marshal(x)
	b, _ := json.Marshal(y)
	return bytes.Equal(a, b)
}

func show(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// line writes a change in one line with its sign, for the plan card.
func line(a Area, c domain.ConfigChange) string {
	switch c.Op {
	case domain.ConfigCreate:
		return "+ " + a.Title + " " + c.Name
	case domain.ConfigDelete:
		return "- " + a.Title + " " + c.Name
	}
	parts := make([]string, 0, len(c.Fields))
	for _, f := range c.Fields {
		parts = append(parts, fmt.Sprintf("%s: %s -> %s", f.Path, orNone(f.Old), orNone(f.New)))
	}
	return "~ " + a.Title + " " + c.Name + " (" + strings.Join(parts, ", ") + ")"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// touches reports whether a field at or under one of the paths changed.
func touches(fields []domain.ConfigFieldChange, paths ...string) bool {
	return slices.ContainsFunc(fields, func(f domain.ConfigFieldChange) bool {
		return slices.ContainsFunc(paths, func(p string) bool { return f.Path == p || strings.HasPrefix(f.Path, p+".") })
	})
}

// onlyTouches reports whether every changed field is under one of the paths.
func onlyTouches(fields []domain.ConfigFieldChange, paths ...string) bool {
	return !slices.ContainsFunc(fields, func(f domain.ConfigFieldChange) bool {
		return !touches([]domain.ConfigFieldChange{f}, paths...)
	})
}

// need returns organization needs for the permissions.
func need(perms ...domain.Permission) []Need {
	out := make([]Need, 0, len(perms))
	for _, p := range perms {
		out = append(out, Need{Permission: p})
	}
	return out
}

// needsOf is the Needs of an area that takes one permission.
func needsOf(p domain.Permission) func(context.Context, *Env, *Resource, *Resource, []domain.ConfigFieldChange) ([]Need, error) {
	return func(context.Context, *Env, *Resource, *Resource, []domain.ConfigFieldChange) ([]Need, error) {
		return need(p), nil
	}
}

// missing maps a store.ErrNotFound to bad input naming what.
func missing(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return invalid("%s does not exist", what)
	}
	return err
}

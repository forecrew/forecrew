package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// serverNote says when a server setting counts: forecrewd reads them at
// startup.
const serverNote = "Takes effect when forecrewd restarts. Until then the old value stays in use."

// specOfYAML turns a value of forecrew.yaml into a spec, keeping the keys of
// the file.
func specOfYAML(v any) (json.RawMessage, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// stored returns the settings of the server's scope.
func stored(ctx context.Context, st store.Store) ([]config.Setting, error) {
	rows, err := st.ConfigSettings().ListAll(ctx, uuid.Nil())
	if err != nil {
		return nil, err
	}
	out := make([]config.Setting, 0, len(rows))
	for _, r := range rows {
		out = append(out, config.Setting{Kind: r.Kind, Name: r.Name, Spec: r.Spec})
	}
	return out, nil
}

// Stored returns the settings forecrewd reads over forecrew.yaml at startup.
func Stored(ctx context.Context, st store.Store) ([]config.Setting, error) { return stored(ctx, st) }

// checkServer refuses a change the merged configuration would not pass: the
// same check as forecrew.yaml. A server of several organizations keeps its
// settings in the file until they move to organization scope.
func checkServer(ctx context.Context, e *Env, want config.Setting) error {
	if e.Server == nil {
		return fmt.Errorf("server settings: %w", mcp.ErrUnavailable)
	}
	all, err := stored(ctx, e.Store)
	if err != nil {
		return err
	}
	all = slices.DeleteFunc(all, func(s config.Setting) bool { return s.Kind == want.Kind && s.Name == want.Name })
	if _, err := e.Server.WithSettings(append(all, want), e.Getenv); err != nil {
		if errors.Is(err, config.ErrInvalid) {
			return fmt.Errorf("%w: %w", err, mcp.ErrInvalidInput)
		}
		return err
	}
	return nil
}

// singleOrganization refuses a server setting on a server shared by several
// organizations: one organization's admin does not decide for the others.
func singleOrganization(ctx context.Context, e *Env) error {
	accounts, err := e.Store.Accounts().List(ctx)
	if err != nil {
		return err
	}
	if len(accounts) > 1 {
		return invalid("this is a server setting and the server has several organizations: change it in forecrew.yaml")
	}
	return nil
}

// readServer reads a server setting: the file's values, with the stored ones
// over them.
func readServer(kind string, file func(c *config.Config) (map[string]json.RawMessage, error)) func(context.Context, *Env) ([]Resource, error) {
	return func(ctx context.Context, e *Env) ([]Resource, error) {
		if e.Server == nil {
			return nil, nil
		}
		values, err := file(e.Server)
		if err != nil {
			return nil, err
		}
		over, err := e.Store.ConfigSettings().List(ctx, uuid.Nil(), kind)
		if err != nil {
			return nil, err
		}
		origin := map[string]string{}
		for name := range values {
			origin[name] = OriginFile
		}
		for _, s := range over {
			values[s.Name], origin[s.Name] = s.Spec, OriginDatabase
		}
		out := make([]Resource, 0, len(values))
		for _, name := range slices.Sorted(maps.Keys(values)) {
			spec, err := canon(values[name])
			if err != nil {
				return nil, err
			}
			out = append(out, Resource{Kind: kind, Name: name, Spec: spec, Scope: ScopeServer, Origin: origin[name]})
		}
		return out, nil
	}
}

// serverArea fills in what pools, runtimes and auth have in common: the file
// is the default, and a stored value goes over it.
func serverArea(a Area, kind string, file func(c *config.Config) (map[string]json.RawMessage, error)) Area {
	a.Affects = serverNote
	a.Read = readServer(kind, file)
	a.Validate = func(ctx context.Context, e *Env, _ *Resource, want Resource) error {
		if err := singleOrganization(ctx, e); err != nil {
			return err
		}
		return checkServer(ctx, e, config.Setting{Kind: kind, Name: want.Name, Spec: want.Spec})
	}
	a.Apply = func(ctx context.Context, e *Env, old, want *Resource) error {
		if want == nil {
			// Deleting drops the stored value: the file's counts again.
			err := e.Store.ConfigSettings().Delete(ctx, uuid.Nil(), kind, old.Name)
			if errors.Is(err, store.ErrNotFound) {
				return invalid("%s is set by forecrew.yaml, which is changed on the server", old.Ref())
			}
			return err
		}
		return e.Store.ConfigSettings().Put(ctx, &domain.ConfigSetting{Kind: kind, Name: want.Name, Spec: want.Spec, UpdatedBy: e.Actor.String()})
	}
	return a
}

// fileValues encodes the entries of a map of forecrew.yaml.
func fileValues[V any](m map[string]V) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(m))
	for name, v := range m {
		raw, err := specOfYAML(v)
		if err != nil {
			return nil, err
		}
		out[name] = raw
	}
	return out, nil
}

type poolCredentialsSpec struct {
	Runtimes  []string `json:"runtimes"`
	RepoToken string   `json:"repo_token"`
}

type poolSpec struct {
	Roles       []string            `json:"roles"`
	Kinds       []domain.JobKind    `json:"kinds"`
	Credentials poolCredentialsSpec `json:"credentials"`
}

func poolArea() Area {
	a := base[poolSpec](KindPool, "Trust zone", ScopeServer, domain.PermWorkerRead)
	a.Setting, a.Spec = config.Pool{}, poolSpec{}
	a.Needs = needsOf(domain.PermPoolManage)
	a = serverArea(a, config.SettingPool, func(c *config.Config) (map[string]json.RawMessage, error) { return fileValues(c.Pools) })
	a.Protection = poolProtection
	return a
}

func poolProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want == nil {
		return nil
	}
	var before poolSpec
	if old != nil {
		before, _ = decode[poolSpec](old.Spec)
	}
	w, _ := decode[poolSpec](want.Spec)
	if w.Credentials.RepoToken == config.RepoTokenWrite && before.Credentials.RepoToken != config.RepoTokenWrite {
		return []string{"Gives jobs of " + want.Name + " a token that writes to the repo"}
	}
	return nil
}

type runtimeSpec struct {
	Adapter       string   `json:"adapter"`
	Vendor        string   `json:"vendor"`
	CredentialEnv []string `json:"credential_env"`
	BaseURL       string   `json:"base_url"`
}

func runtimeArea() Area {
	a := base[runtimeSpec](KindRuntime, "AI runtime", ScopeServer, domain.PermOrgRead)
	a.Setting, a.Spec = config.Runtime{}, runtimeSpec{}
	a.Needs = needsOf(domain.PermOrgManage)
	return serverArea(a, config.SettingRuntime, func(c *config.Config) (map[string]json.RawMessage, error) { return fileValues(c.Runtimes) })
}

type authConnectionSpec struct {
	Kind            domain.ConnectionKind `json:"kind"`
	Issuer          string                `json:"issuer"`
	ClientID        string                `json:"client_id"`
	ClientSecretRef string                `json:"client_secret_ref"`
	GroupClaim      string                `json:"group_claim"`
}

type authSpec struct {
	SessionIdle     string                        `json:"session_idle"`
	SessionAbsolute string                        `json:"session_absolute"`
	TokenDefaultTTL string                        `json:"token_default_ttl"`
	TokenLogin      bool                          `json:"token_login"`
	Connections     map[string]authConnectionSpec `json:"connections"`
	InstanceAdmins  []string                      `json:"instance_admins"`
}

func authArea() Area {
	a := base[authSpec](KindAuth, "Sign-in settings", ScopeServer, domain.PermConnectionManage)
	a.Setting, a.Spec = config.Auth{}, authSpec{}
	a.Needs = needsOf(domain.PermOrgManage)
	a = serverArea(a, config.SettingAuth, func(c *config.Config) (map[string]json.RawMessage, error) {
		raw, err := specOfYAML(c.Auth)
		return map[string]json.RawMessage{config.AuthSettingName: raw}, err
	})
	a.Protection = authProtection
	a.Secrets = authSecrets
	return a
}

func authProtection(_ context.Context, _ *Env, old, want *Resource) []string {
	if want == nil || old == nil {
		return nil
	}
	o, _ := decode[authSpec](old.Spec)
	w, _ := decode[authSpec](want.Spec)
	out := wider("Adds someone who can open every organization's settings", o.InstanceAdmins, w.InstanceAdmins)
	if w.TokenLogin && !o.TokenLogin {
		out = append(out, "Lets a token sign in to the web UI")
	}
	return out
}

func authSecrets(want Resource) []SecretField {
	s, _ := decode[authSpec](want.Spec)
	var out []SecretField
	for _, name := range slices.Sorted(maps.Keys(s.Connections)) {
		out = append(out, SecretField{Ref: s.Connections[name].ClientSecretRef, Title: "Client secret of " + name})
	}
	return out
}

package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// Kinds of the settings stored over forecrew.yaml.
const (
	SettingPool     = "pool"
	SettingRuntime  = "runtime"
	SettingAuth     = "auth"
	SettingSchedule = "schedule"
	// AuthSettingName is the name of the one auth setting.
	AuthSettingName = "auth"
)

// ScheduleName is the name of a schedule as a setting.
func ScheduleName(s Schedule) string { return s.Playbook + ":" + s.Project }

// Setting is a value stored over the file's default: the YAML of one pool,
// one runtime, one schedule or the whole of auth, as JSON.
type Setting struct {
	Kind string
	Name string
	Spec []byte
}

// checkCredentialEnv refuses a stored runtime that names a variable the file
// does not hand to a runtime: forecrewd sends what it names to the runtime's
// endpoint, and its environment also holds the database URL and the GitHub key.
func (c *Config) checkCredentialEnv(name string, r Runtime) error {
	for _, env := range r.CredentialEnv {
		if !slices.ContainsFunc(slices.Collect(maps.Values(c.Runtimes)), func(f Runtime) bool { return slices.Contains(f.CredentialEnv, env) }) {
			return fmt.Errorf("runtime %s: credential_env %s is not used by a runtime of forecrew.yaml: %w", name, env, ErrInvalid)
		}
	}
	return nil
}

// checkConnectionRefs refuses a stored connection whose secret is an env: or
// secret: reference the file does not use: forecrewd sends it to the
// connection's issuer. A store: reference holds only what a person saved.
func (c *Config) checkConnectionRefs(a Auth) error {
	for name, conn := range a.Connections {
		if _, ok := domain.StoreSecretName(conn.ClientSecretRef); ok {
			continue
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(c.Auth.Connections)), func(f Connection) bool { return f.ClientSecretRef == conn.ClientSecretRef }) {
			return fmt.Errorf("auth.connections.%s: client_secret_ref is not used by forecrew.yaml; use store:name: %w", name, ErrInvalid)
		}
	}
	return nil
}

// WithSettings returns a copy of c with the stored settings over the file's
// values, read and checked like the file: secrets of a runtime or a
// connection come from getenv, and only those the file already names, so a
// stored setting never reaches another variable of forecrewd. The error wraps ErrInvalid, and c is not
// changed, so a bad setting never costs the server its file.
func (c *Config) WithSettings(settings []Setting, getenv func(string) string) (*Config, error) {
	out := *c
	out.Pools = maps.Clone(c.Pools)
	out.Runtimes = maps.Clone(c.Runtimes)
	out.Schedules = slices.Clone(c.Schedules)
	if out.Runtimes == nil {
		out.Runtimes = map[string]Runtime{}
	}
	for _, s := range settings {
		if err := c.apply(&out, s, getenv); err != nil {
			return nil, err
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// apply puts one setting on out; c is the file.
func (c *Config) apply(out *Config, s Setting, getenv func(string) string) error {
	switch s.Kind {
	case SettingPool:
		var p Pool
		if err := decodeStrict(s.Spec, &p); err != nil {
			return fmt.Errorf("pool %s: %w", s.Name, err)
		}
		out.Pools[s.Name] = p
	case SettingRuntime:
		var r Runtime
		if err := decodeStrict(s.Spec, &r); err != nil {
			return fmt.Errorf("runtime %s: %w", s.Name, err)
		}
		if err := c.checkCredentialEnv(s.Name, r); err != nil {
			return err
		}
		r.Credentials = map[string]string{}
		for _, env := range r.CredentialEnv {
			r.Credentials[env] = getenv(env)
		}
		out.Runtimes[s.Name] = r
	case SettingAuth:
		var a Auth
		if err := decodeStrict(s.Spec, &a); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		if err := c.checkConnectionRefs(a); err != nil {
			return err
		}
		out.Auth = a
		out.readAuth(getenv)
		out.applyAuthDefaults()
	case SettingSchedule:
		var sc Schedule
		if err := decodeStrict(s.Spec, &sc); err != nil {
			return fmt.Errorf("schedule %s: %w", s.Name, err)
		}
		if ScheduleName(sc) != s.Name {
			return fmt.Errorf("schedule %s: its name must be %s: %w", s.Name, ScheduleName(sc), ErrInvalid)
		}
		i := slices.IndexFunc(out.Schedules, func(x Schedule) bool { return ScheduleName(x) == s.Name })
		if i < 0 {
			out.Schedules = append(out.Schedules, sc)
		} else {
			out.Schedules[i] = sc
		}
	}
	return nil
}

package config

import (
	"fmt"
	"maps"
)

// Kinds of the settings stored over forecrew.yaml.
const (
	SettingPool    = "pool"
	SettingRuntime = "runtime"
	SettingAuth    = "auth"
	// AuthSettingName is the name of the one auth setting.
	AuthSettingName = "auth"
)

// Setting is a value stored over the file's default: the YAML of one pool,
// one runtime or the whole of auth, as JSON.
type Setting struct {
	Kind string
	Name string
	Spec []byte
}

// WithSettings returns a copy of c with the stored settings over the file's
// values, read and checked like the file: secrets of a runtime or a
// connection come from getenv. The error wraps ErrInvalid, and c is not
// changed, so a bad setting never costs the server its file.
func (c *Config) WithSettings(settings []Setting, getenv func(string) string) (*Config, error) {
	out := *c
	out.Pools = maps.Clone(c.Pools)
	out.Runtimes = maps.Clone(c.Runtimes)
	for _, s := range settings {
		switch s.Kind {
		case SettingPool:
			var p Pool
			if err := decodeStrict(s.Spec, &p); err != nil {
				return nil, fmt.Errorf("pool %s: %w", s.Name, err)
			}
			out.Pools[s.Name] = p
		case SettingRuntime:
			var r Runtime
			if err := decodeStrict(s.Spec, &r); err != nil {
				return nil, fmt.Errorf("runtime %s: %w", s.Name, err)
			}
			r.Credentials = map[string]string{}
			for _, env := range r.CredentialEnv {
				r.Credentials[env] = getenv(env)
			}
			if out.Runtimes == nil {
				out.Runtimes = map[string]Runtime{}
			}
			out.Runtimes[s.Name] = r
		case SettingAuth:
			var a Auth
			if err := decodeStrict(s.Spec, &a); err != nil {
				return nil, fmt.Errorf("auth: %w", err)
			}
			out.Auth = a
			out.readAuth(getenv)
			out.applyAuthDefaults()
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

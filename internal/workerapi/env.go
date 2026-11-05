package workerapi

import (
	"maps"
	"slices"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// RuntimeEnv returns the Config.Env of forecrewd: the name (a key of
// runtimes) and credentials of the runtime configured for the job's role,
// or else of the first runtime (by name) with the job's adapter. The map is
// a copy; nil when the runtime has no credential_env. The name is empty
// when no runtime has the adapter.
func RuntimeEnv(cfg *config.Config) func(j domain.Job) (string, map[string]string) {
	return func(j domain.Job) (string, map[string]string) {
		name := runtimeOf(cfg, j)
		if name == "" {
			return "", nil
		}
		return name, credentials(cfg.Runtimes[name])
	}
}

// RuntimeEndpoint returns the Config.Endpoint of forecrewd: the custom model
// endpoint of the runtime RuntimeEnv picks for the job, nil when it has none.
func RuntimeEndpoint(cfg *config.Config) func(j domain.Job) *domain.Endpoint {
	return func(j domain.Job) *domain.Endpoint {
		return cfg.Runtimes[runtimeOf(cfg, j)].Endpoint()
	}
}

// runtimeOf returns the key of the runtime that serves the job.
func runtimeOf(cfg *config.Config, j domain.Job) string {
	if r, ok := cfg.Roles[j.Role]; ok {
		if rt, ok := cfg.Runtimes[r.Runtime]; ok && rt.Adapter == j.Runtime {
			return r.Runtime
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Runtimes)) {
		if cfg.Runtimes[name].Adapter == j.Runtime {
			return name
		}
	}
	return ""
}

func credentials(rt config.Runtime) map[string]string {
	if len(rt.Credentials) == 0 {
		return nil
	}
	return maps.Clone(rt.Credentials)
}

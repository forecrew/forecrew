package workerapi

import (
	"maps"
	"slices"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

// RuntimeEnv returns the Config.Env of forecrewd: the credentials of the
// runtime configured for the job's role, or else of the first runtime (by
// name) with the job's adapter. The map is a copy; nil when the runtime has
// no credential_env.
func RuntimeEnv(cfg *config.Config) func(j domain.Job) map[string]string {
	return func(j domain.Job) map[string]string {
		if r, ok := cfg.Roles[j.Role]; ok {
			if rt, ok := cfg.Runtimes[r.Runtime]; ok && rt.Adapter == j.Runtime {
				return credentials(rt)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(cfg.Runtimes)) {
			if rt := cfg.Runtimes[name]; rt.Adapter == j.Runtime {
				return credentials(rt)
			}
		}
		return nil
	}
}

func credentials(rt config.Runtime) map[string]string {
	if len(rt.Credentials) == 0 {
		return nil
	}
	return maps.Clone(rt.Credentials)
}

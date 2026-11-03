package domain

// Default worker pools (docs/contracts/config.md, pools). A worker belongs
// to one pool, set by an admin; it only leases jobs of that pool.
const (
	PoolBuild   = "build"   // today's workers: every role and kind
	PoolControl = "control" // read-only agent work, no write tokens
	PoolActions = "actions" // command jobs on GitHub-hosted runners, served by forecrewd
)

// OrDefaultPool returns pool, or PoolBuild when it is empty.
func OrDefaultPool(pool string) string {
	if pool == "" {
		return PoolBuild
	}
	return pool
}

// CommandPool returns the pool of a command job running in env: PoolActions
// for provider actions, otherwise PoolBuild (also without an environment).
func CommandPool(env *Environment) string {
	if env != nil && env.Provider == ProviderActions {
		return PoolActions
	}
	return PoolBuild
}

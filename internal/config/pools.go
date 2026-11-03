package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// Environment variables holding the evidence storage credentials.
const (
	EnvEvidenceAccessKeyID     = "FORECREW_EVIDENCE_ACCESS_KEY_ID"
	EnvEvidenceSecretAccessKey = "FORECREW_EVIDENCE_SECRET_ACCESS_KEY" // #nosec G101 -- variable name, not a secret
)

// Evidence defaults.
const (
	DefaultEvidenceRegion  = "us-east-1"
	DefaultMaxArtifactSize = 100 << 20 // 100 MiB
)

// Repo token scopes a pool may receive.
const (
	RepoTokenWrite = "write" // contents write for writing roles, read otherwise
	RepoTokenRead  = "read"  // contents read, whatever the role
	RepoTokenNone  = "none"  // no repo token
)

// AnyRole matches every role in Pool.Roles and every runtime in
// PoolCredentials.Runtimes.
const AnyRole = "*"

// Evidence configures the S3-compatible object storage of artifacts.
type Evidence struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	PathStyle bool   `yaml:"path_style"`
	// MaxArtifactSize is the largest artifact a worker may upload, in bytes.
	MaxArtifactSize int64 `yaml:"max_artifact_size"`
	// RetentionDays is how long artifacts are kept; 0 keeps them forever.
	RetentionDays int `yaml:"retention_days"`
	// AccessKeyID comes from FORECREW_EVIDENCE_ACCESS_KEY_ID.
	AccessKeyID string `yaml:"-"`
	// SecretAccessKey comes from FORECREW_EVIDENCE_SECRET_ACCESS_KEY.
	SecretAccessKey string `yaml:"-"`
}

// Pool is the policy of a worker pool: what its workers may run and which
// credentials their jobs may receive.
type Pool struct {
	// Roles are the roles whose agent jobs the pool runs, or "*".
	Roles []string `yaml:"roles"`
	// Kinds are the job kinds the pool runs.
	Kinds       []domain.JobKind `yaml:"kinds"`
	Credentials PoolCredentials  `yaml:"credentials"`
}

// PoolCredentials says which secrets jobs of a pool may receive.
type PoolCredentials struct {
	// Runtimes are the runtimes (keys of runtimes) whose credential_env the
	// pool's jobs may receive, or "*".
	Runtimes []string `yaml:"runtimes"`
	// RepoToken is the widest repo token scope: write, read or none.
	RepoToken string `yaml:"repo_token"`
}

// DefaultPools returns the pools every installation has unless pools in
// forecrew.yaml redefines them.
func DefaultPools() map[string]Pool {
	return map[string]Pool{
		domain.PoolBuild: {
			Roles: []string{AnyRole}, Kinds: []domain.JobKind{domain.JobKindAgent, domain.JobKindCommand},
			Credentials: PoolCredentials{Runtimes: []string{AnyRole}, RepoToken: RepoTokenWrite},
		},
		domain.PoolControl: {
			Roles: []string{AnyRole}, Kinds: []domain.JobKind{domain.JobKindAgent},
			Credentials: PoolCredentials{Runtimes: []string{AnyRole}, RepoToken: RepoTokenRead},
		},
		domain.PoolActions: {
			Kinds:       []domain.JobKind{domain.JobKindCommand},
			Credentials: PoolCredentials{RepoToken: RepoTokenRead},
		},
	}
}

// ErrNotAllowed means a pool's policy does not allow a job.
var ErrNotAllowed = errors.New("not allowed in pool")

// Allows reports whether the pool runs a job of kind for role: the kind is
// listed and, for agent jobs, so is the role (or "*"). A writing role also
// needs a pool whose repo token scope is write.
func (p Pool) Allows(role string, kind domain.JobKind, writes bool) bool {
	return p.Check("", role, kind, writes) == nil
}

// Check is Allows with the reason: it returns an error wrapping
// ErrNotAllowed that says why pool name refuses the job, or nil.
func (p Pool) Check(name, role string, kind domain.JobKind, writes bool) error {
	kind = kind.OrDefault()
	if !slices.Contains(p.Kinds, kind) {
		return fmt.Errorf("pool %s does not run %s jobs: %w", name, kind, ErrNotAllowed)
	}
	if kind == domain.JobKindCommand {
		return nil
	}
	if !slices.Contains(p.Roles, AnyRole) && !slices.Contains(p.Roles, role) {
		return fmt.Errorf("pool %s does not run role %s: %w", name, role, ErrNotAllowed)
	}
	if writes && p.Credentials.RepoToken != RepoTokenWrite {
		return fmt.Errorf("role %s writes to the repo and pool %s gets no write token: %w", role, name, ErrNotAllowed)
	}
	return nil
}

// AllowsRuntime reports whether the pool's jobs may receive the credentials
// of runtime (a key of runtimes).
func (c PoolCredentials) AllowsRuntime(runtime string) bool {
	return slices.Contains(c.Runtimes, AnyRole) || slices.Contains(c.Runtimes, runtime)
}

func (c *Config) readEvidence(getenv func(string) string) {
	if c.Evidence == nil {
		return
	}
	e := c.Evidence
	e.AccessKeyID = getenv(EnvEvidenceAccessKeyID)
	e.SecretAccessKey = getenv(EnvEvidenceSecretAccessKey)
	if e.Region == "" {
		e.Region = DefaultEvidenceRegion
	}
	if e.MaxArtifactSize == 0 {
		e.MaxArtifactSize = DefaultMaxArtifactSize
	}
}

// applyPoolDefaults adds the default pools forecrew.yaml does not define.
func (c *Config) applyPoolDefaults() {
	if c.Pools == nil {
		c.Pools = map[string]Pool{}
	}
	for name, p := range DefaultPools() {
		if _, ok := c.Pools[name]; !ok {
			c.Pools[name] = p
		}
	}
}

func (c *Config) validateEvidence(p *problems) {
	e := c.Evidence
	if e == nil {
		return
	}
	if u, err := url.Parse(e.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		p.addf("evidence.endpoint must be an absolute http(s) URL, got %q", e.Endpoint)
	}
	if e.Bucket == "" {
		p.addf("evidence.bucket must be set")
	}
	if e.MaxArtifactSize < 0 {
		p.addf("evidence.max_artifact_size must be positive")
	}
	if e.RetentionDays < 0 {
		p.addf("evidence.retention_days must not be negative")
	}
	if e.AccessKeyID == "" {
		p.addf("%s is not set", EnvEvidenceAccessKeyID)
	}
	if e.SecretAccessKey == "" {
		p.addf("%s is not set", EnvEvidenceSecretAccessKey)
	}
}

func (c *Config) validatePools(p *problems) {
	for _, name := range sortedKeys(c.Pools) {
		c.validatePool(name, c.Pools[name], p)
	}
}

func (c *Config) validatePool(name string, pool Pool, p *problems) {
	if !domain.ValidName(name) {
		p.addf("pools.%s: name must be a lowercase name such as build", name)
	}
	if len(pool.Kinds) == 0 {
		p.addf("pools.%s.kinds must list agent, command or both", name)
	}
	for _, k := range pool.Kinds {
		if !slices.Contains(domain.JobKinds(), k) {
			p.addf("pools.%s.kinds: unknown kind %q", name, k)
		}
	}
	if slices.Contains(pool.Roles, "") {
		p.addf("pools.%s.roles: empty role name", name)
	}
	for _, rt := range pool.Credentials.Runtimes {
		if _, ok := c.Runtimes[rt]; !ok && rt != AnyRole {
			p.addf("pools.%s.credentials.runtimes: %q is not a configured runtime", name, rt)
		}
	}
	switch pool.Credentials.RepoToken {
	case RepoTokenWrite, RepoTokenRead, RepoTokenNone:
	default:
		p.addf("pools.%s.credentials.repo_token must be write, read or none, got %q", name, pool.Credentials.RepoToken)
	}
}

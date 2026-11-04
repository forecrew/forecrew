// Package config loads and validates forecrew.yaml (forecrewd) and
// worker.yaml (forecrew-worker). Secrets never come from the files: they are
// read from environment variables, see docs/contracts/config.md.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Sentinel errors.
var (
	// ErrInvalid is wrapped by every validation error.
	ErrInvalid = errors.New("invalid config")
	// ErrUnknownRole is returned by Model for a role missing from roles.
	ErrUnknownRole = errors.New("unknown role")
)

// Environment variables holding forecrewd secrets.
const (
	EnvDatabaseURL         = "FORECREW_DATABASE_URL"
	EnvGitHubPrivateKey    = "FORECREW_GITHUB_PRIVATE_KEY"
	EnvGitHubWebhookSecret = "FORECREW_GITHUB_WEBHOOK_SECRET" // #nosec G101 -- variable name, not a secret
)

// Defaults applied by Load.
const (
	DefaultListen       = ":8080"
	DefaultTLSListen    = ":443"
	DefaultCertCacheDir = "/var/lib/forecrew/certs"
)

// Runtime adapters known to forecrew.
const (
	AdapterClaudeCode = "claude-code"
	AdapterCodex      = "codex"
)

// Tier is a model tier.
type Tier string

// Model tiers.
const (
	TierFast   Tier = "fast"
	TierStrong Tier = "strong"
	TierBest   Tier = "best"
)

func (t Tier) valid() bool { return t == TierFast || t == TierStrong || t == TierBest }

// Config is forecrewd's configuration.
type Config struct {
	Server Server `yaml:"server"`
	// DatabaseURL comes from FORECREW_DATABASE_URL.
	DatabaseURL string             `yaml:"-"`
	GitHub      GitHub             `yaml:"github"`
	Runtimes    map[string]Runtime `yaml:"runtimes"`
	// ModelTiers maps tier -> vendor -> model name.
	ModelTiers map[Tier]map[string]string `yaml:"model_tiers"`
	Roles      map[string]Role            `yaml:"roles"`
	Policy     Policy                     `yaml:"policy"`
	// Pools holds the worker pools by name; Parse adds DefaultPools that
	// are not redefined.
	Pools map[string]Pool `yaml:"pools"`
	// Evidence is the artifact storage; nil when not configured.
	Evidence *Evidence `yaml:"evidence"`
	// Sizing is the rule that gives goals their councils from their triage.
	Sizing Sizing `yaml:"sizing"`
	// MaxTestResultsSize is the largest request body (bytes) a worker may
	// send to complete a command job with test runs; zero means
	// DefaultMaxTestResultsSize.
	MaxTestResultsSize int64 `yaml:"max_test_results_size"`
	Auth               Auth  `yaml:"auth"`
}

// DefaultMaxTestResultsSize is the default of Config.MaxTestResultsSize.
const DefaultMaxTestResultsSize = 32 << 20 // 32 MiB

// Server configures the HTTP listener.
type Server struct {
	Listen        string `yaml:"listen"`
	PublicBaseURL string `yaml:"public_base_url"`
	// TLSDomain, when set, makes forecrewd serve HTTPS itself with automatic
	// Let's Encrypt certificates for that domain.
	TLSDomain    string `yaml:"tls_domain"`
	CertCacheDir string `yaml:"cert_cache_dir"`
}

// GitHub configures the GitHub App.
type GitHub struct {
	AppID int64 `yaml:"app_id"`
	// PrivateKey (PEM) comes from FORECREW_GITHUB_PRIVATE_KEY.
	PrivateKey string `yaml:"-"`
	// WebhookSecret comes from FORECREW_GITHUB_WEBHOOK_SECRET.
	WebhookSecret string `yaml:"-"`
	// Allowlist holds the GitHub logins who may command @forecrew.
	Allowlist []string `yaml:"allowlist"`
}

// Allowed reports whether login is in the allowlist, ignoring case.
func (g GitHub) Allowed(login string) bool {
	return slices.ContainsFunc(g.Allowlist, func(a string) bool { return strings.EqualFold(a, login) })
}

// Runtime is an agent runtime workers can run.
type Runtime struct {
	Adapter string `yaml:"adapter"` // claude-code or codex
	Vendor  string `yaml:"vendor"`  // model vendor, keys model_tiers, e.g. anthropic
	// CredentialEnv names the environment variables forecrewd reads and
	// passes to the worker in the job spec env.
	CredentialEnv []string `yaml:"credential_env"`
	// Credentials holds the values of CredentialEnv, read from the environment.
	Credentials map[string]string `yaml:"-"`
}

// Role assigns a runtime and model tier to a role.
type Role struct {
	Runtime string `yaml:"runtime"`
	Tier    Tier   `yaml:"tier"`
}

// Policy holds cross-cutting rules.
type Policy struct {
	// AllowSameVendorReview allows the reviewer to use the developer's vendor.
	AllowSameVendorReview bool `yaml:"allow_same_vendor_review"`
}

// Load reads forecrew.yaml from path. getenv supplies secrets (os.Getenv in
// production).
func Load(path string, getenv func(string) string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's config file
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, getenv)
}

// Parse decodes and validates forecrew.yaml, applies defaults and reads
// secrets with getenv.
func Parse(data []byte, getenv func(string) string) (*Config, error) {
	var c Config
	if err := decodeStrict(data, &c); err != nil {
		return nil, err
	}
	c.DatabaseURL = getenv(EnvDatabaseURL)
	c.GitHub.PrivateKey = getenv(EnvGitHubPrivateKey)
	c.GitHub.WebhookSecret = getenv(EnvGitHubWebhookSecret)
	for name, rt := range c.Runtimes {
		rt.Credentials = map[string]string{}
		for _, env := range rt.CredentialEnv {
			rt.Credentials[env] = getenv(env)
		}
		c.Runtimes[name] = rt
	}
	c.readEvidence(getenv)
	c.readAuth(getenv)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	c.Server.PublicBaseURL = strings.TrimRight(c.Server.PublicBaseURL, "/")
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
		if c.Server.TLSDomain != "" {
			c.Server.Listen = DefaultTLSListen
		}
	}
	if c.Server.TLSDomain != "" && c.Server.CertCacheDir == "" {
		c.Server.CertCacheDir = DefaultCertCacheDir
	}
	if c.MaxTestResultsSize == 0 {
		c.MaxTestResultsSize = DefaultMaxTestResultsSize
	}
	c.applyPoolDefaults()
	c.applySizing()
	c.applyAuthDefaults()
}

// problems collects validation messages.
type problems []string

func (p *problems) addf(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(p, "; "))
}

// Validate checks the whole configuration and reports every problem at once.
func (c *Config) Validate() error {
	var p problems
	c.validateServer(&p)
	if c.DatabaseURL == "" {
		p.addf("%s is not set", EnvDatabaseURL)
	}
	c.validateGitHub(&p)
	c.validateRuntimes(&p)
	c.validateRoles(&p)
	c.validatePools(&p)
	c.validateEvidence(&p)
	c.validateSizing(&p)
	if c.MaxTestResultsSize < 0 {
		p.addf("max_test_results_size must be positive")
	}
	c.validateAuth(&p)
	return p.err()
}

func (c *Config) validateServer(p *problems) {
	s := c.Server
	u, err := url.Parse(s.PublicBaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		p.addf("server.public_base_url must be an absolute http(s) URL, got %q", s.PublicBaseURL)
		return
	}
	if s.TLSDomain != "" && (u.Scheme != "https" || !strings.EqualFold(u.Hostname(), s.TLSDomain)) {
		p.addf("server.public_base_url must be https://%s when server.tls_domain is set", s.TLSDomain)
	}
}

func (c *Config) validateGitHub(p *problems) {
	if c.GitHub.AppID <= 0 {
		p.addf("github.app_id must be set")
	}
	if c.GitHub.PrivateKey == "" {
		p.addf("%s is not set", EnvGitHubPrivateKey)
	}
	if c.GitHub.WebhookSecret == "" {
		p.addf("%s is not set", EnvGitHubWebhookSecret)
	}
}

var runtimeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (c *Config) validateRuntimes(p *problems) {
	if len(c.Runtimes) == 0 {
		p.addf("runtimes: at least one runtime is required")
	}
	for _, name := range sortedKeys(c.Runtimes) {
		rt := c.Runtimes[name]
		if !runtimeName.MatchString(name) {
			p.addf("runtimes.%s: name must match %s", name, runtimeName)
		}
		if rt.Adapter != AdapterClaudeCode && rt.Adapter != AdapterCodex {
			p.addf("runtimes.%s.adapter must be %s or %s, got %q", name, AdapterClaudeCode, AdapterCodex, rt.Adapter)
		}
		if rt.Vendor == "" {
			p.addf("runtimes.%s.vendor must be set", name)
		}
		for _, env := range rt.CredentialEnv {
			if rt.Credentials[env] == "" {
				p.addf("runtimes.%s: %s is not set", name, env)
			}
		}
	}
	for _, tier := range sortedKeys(c.ModelTiers) {
		if !tier.valid() {
			p.addf("model_tiers.%s: tier must be fast, strong or best", tier)
		}
	}
}

func (c *Config) validateRoles(p *problems) {
	for _, name := range sortedKeys(c.Roles) {
		r := c.Roles[name]
		rt, ok := c.Runtimes[r.Runtime]
		if !ok {
			p.addf("roles.%s.runtime %q is not a configured runtime", name, r.Runtime)
			continue
		}
		if !r.Tier.valid() {
			p.addf("roles.%s.tier must be fast, strong or best, got %q", name, r.Tier)
			continue
		}
		if c.ModelTiers[r.Tier][rt.Vendor] == "" {
			p.addf("roles.%s: model_tiers.%s.%s is not set", name, r.Tier, rt.Vendor)
		}
	}
	if v, ok := c.SameVendorReview(); ok {
		p.addf("roles: reviewer and developer both use vendor %q; set policy.allow_same_vendor_review to allow it", v)
	}
}

// SameVendorReview reports whether the reviewer and developer roles use
// runtimes of the same vendor without policy.allow_same_vendor_review, which
// separation of duties forbids, and returns that vendor.
func (c *Config) SameVendorReview() (string, bool) {
	dev, okDev := c.vendor("developer")
	rev, okRev := c.vendor("reviewer")
	if okDev && okRev && dev == rev && !c.Policy.AllowSameVendorReview {
		return dev, true
	}
	return "", false
}

func (c *Config) vendor(role string) (string, bool) {
	r, ok := c.Roles[role]
	if !ok {
		return "", false
	}
	rt, ok := c.Runtimes[r.Runtime]
	return rt.Vendor, ok
}

// Model returns the runtime adapter and the model a role runs with.
func (c *Config) Model(role string) (adapter, model string, err error) {
	r, ok := c.Roles[role]
	if !ok {
		return "", "", fmt.Errorf("role %q: %w", role, ErrUnknownRole)
	}
	rt := c.Runtimes[r.Runtime]
	return rt.Adapter, c.ModelTiers[r.Tier][rt.Vendor], nil
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}

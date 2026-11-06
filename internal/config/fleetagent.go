package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// Where a fleet agent's credentials to forecrewd come from: a provider token
// from `forecrew provider add`, or a join code that gets it one.
const (
	EnvProviderToken = "FORECREW_PROVIDER_TOKEN" // #nosec G101 -- the name of a variable
	EnvJoinCode      = "FORECREW_JOIN_CODE"
)

// FleetAgent is forecrew-fleet-agent's configuration (agent.yaml).
type FleetAgent struct {
	ServerURL string              `yaml:"server_url"` // forecrewd public_base_url
	Kind      domain.ProviderKind `yaml:"kind"`       // what this place runs on
	// TokenFile is where a token got with a join code is kept, readable by
	// the agent's user alone, and read from at the next start.
	TokenFile string `yaml:"token_file"`
	// Token comes from FORECREW_PROVIDER_TOKEN.
	Token string `yaml:"-"`
	// JoinCode comes from FORECREW_JOIN_CODE.
	JoinCode string `yaml:"-"`
}

// LoadFleetAgent reads agent.yaml from path.
func LoadFleetAgent(path string, getenv func(string) string) (*FleetAgent, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's config file
	if err != nil {
		return nil, fmt.Errorf("read fleet agent config: %w", err)
	}
	return ParseFleetAgent(data, getenv)
}

// ParseFleetAgent decodes and validates agent.yaml and reads the credentials
// with getenv.
func ParseFleetAgent(data []byte, getenv func(string) string) (*FleetAgent, error) {
	var a FleetAgent
	if err := decodeStrict(data, &a); err != nil {
		return nil, err
	}
	a.Token = getenv(EnvProviderToken)
	a.JoinCode = getenv(EnvJoinCode)
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// Validate checks the configuration and reports every problem at once. A
// missing token is not one: it may be in the token file already.
func (a *FleetAgent) Validate() error {
	var p problems
	u, err := url.Parse(a.ServerURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		p.addf("server_url must be an absolute http(s) URL, got %q", a.ServerURL)
	}
	if !slices.Contains(domain.ProviderKinds(), a.Kind) {
		p.addf("kind must be one of %v, got %q", domain.ProviderKinds(), a.Kind)
	}
	if a.TokenFile == "" {
		p.addf("token_file is required")
	}
	return p.err()
}

package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	// ImageBuild lets a person start the build of the Windows image of this
	// host from forecrewd (docs/fleet.md, Windows images on a host). Without it
	// the agent refuses such a request; build-image still runs by hand.
	ImageBuild *FleetAgentImageBuild `yaml:"image_build"`
	// Token comes from FORECREW_PROVIDER_TOKEN.
	Token string `yaml:"-"`
	// JoinCode comes from FORECREW_JOIN_CODE.
	JoinCode string `yaml:"-"`
}

// FleetAgentImageBuild is what stays on the host when a person asks forecrewd for
// a Windows image build: everything but the ISO's name, its checksum, the
// licence statement and the version, which the request carries. Nothing here is
// a secret.
type FleetAgentImageBuild struct {
	// TemplatesDir is deploy/images of the release this agent is from, Packer
	// the command (default packer), and Worker forecrew-worker.exe for
	// windows/amd64.
	TemplatesDir string `yaml:"templates_dir"`
	Packer       string `yaml:"packer"`
	Worker       string `yaml:"worker"`
	// OutDir holds one directory per build, with its log, progress and manifest.
	OutDir string `yaml:"out_dir"`
	// URIBase is where other hosts fetch the image from.
	URIBase string `yaml:"uri_base"`
	// ISODir is the only directory an ISO may be in: a request names a path,
	// and this keeps it from naming another file of the host.
	ISODir string `yaml:"iso_dir"`
	// GuestTools and GuestToolsSHA256 are the virtio-win installer, for Proxmox.
	GuestTools       string `yaml:"guest_tools"`
	GuestToolsSHA256 string `yaml:"guest_tools_sha256"`
	// CacheDir (a Hyper-V location's image_dir) and Switch are for Hyper-V.
	CacheDir string `yaml:"cache_dir"`
	Switch   string `yaml:"switch"`
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
	if a.ImageBuild != nil {
		a.ImageBuild.validate(&p, a.Kind)
	}
	return p.err()
}

func (b FleetAgentImageBuild) validate(p *problems, kind domain.ProviderKind) {
	if kind != domain.ProviderHyperV && kind != domain.ProviderProxmox {
		p.addf("image_build is for a hyperv or proxmox location, not %s", kind)
	}
	for name, v := range map[string]string{"templates_dir": b.TemplatesDir, "worker": b.Worker, "out_dir": b.OutDir, "iso_dir": b.ISODir} {
		if !filepath.IsAbs(v) {
			p.addf("image_build.%s must be an absolute path, got %q", name, v)
		}
	}
	for name, v := range map[string]string{"guest_tools": b.GuestTools, "cache_dir": b.CacheDir} {
		if v != "" && !filepath.IsAbs(v) {
			p.addf("image_build.%s must be an absolute path, got %q", name, v)
		}
	}
	if !strings.HasPrefix(b.URIBase, "https://") && !strings.HasPrefix(b.URIBase, "http://") {
		p.addf("image_build.uri_base must be an http or https address other hosts fetch the image from, got %q", b.URIBase)
	}
	if (b.GuestTools == "") != (b.GuestToolsSHA256 == "") {
		p.addf("image_build.guest_tools and image_build.guest_tools_sha256 go together")
	}
}

package tart

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Where the configuration is, and the variable that moves it.
const (
	envConfig     = "FORECREW_TART_CONFIG"
	defaultConfig = "/etc/forecrew-fleet-agent/tart.yaml"
)

// maxVMs is Apple's limit: the macOS licence (section 2B(iii)) allows two
// additional copies of macOS in virtual machines on each Apple-branded
// computer. No setting raises it.
const maxVMs = 2

// Config is tart.yaml. It holds no secrets: registry credentials are named by
// reference, and the agent reads them from its own environment.
type Config struct {
	// StateDir holds the records and, as TART_HOME, Tart's own storage.
	StateDir string `yaml:"state_dir"`
	// TrustedKeysFile lists the public keys images must be signed with.
	TrustedKeysFile string `yaml:"trusted_keys_file"`
	// Tart is the command.
	Tart string `yaml:"tart"`
	// MaxMachines is how many run at once, at most two; 0 means two.
	MaxMachines int `yaml:"max_machines"`
	// MinTartVersion is the oldest Tart the agent works with.
	MinTartVersion string `yaml:"min_tart_version"`
	// MinFreeGB is the free disk the host must keep for images and clones.
	MinFreeGB int `yaml:"min_free_gb"`
	// BootWaitSeconds is how long Create waits for a machine's guest agent.
	BootWaitSeconds int `yaml:"boot_wait_seconds"`
	// Softnet runs machines behind Softnet, which keeps them off the LAN and
	// the host; false leaves Tart's NAT, where they can reach both.
	Softnet bool `yaml:"softnet"`
	// RegistryUsername and RegistryPassword are references (env:NAME) to the
	// credentials of the registry images come from; RegistryHost limits them
	// to that registry.
	RegistryUsername string `yaml:"registry_username"`
	RegistryPassword string `yaml:"registry_password"`
	RegistryHost     string `yaml:"registry_host"`
	// AllowLocalBase lets a shape name a VM already on this Mac
	// (provider_config base_vm) instead of a signed image. Off, because the
	// agent cannot check such a VM.
	AllowLocalBase bool `yaml:"allow_local_base"`
	// ImageCacheDays is how long a pulled image unused stays cached.
	ImageCacheDays int `yaml:"image_cache_days"`
}

func defaults() Config {
	return Config{
		StateDir: "/var/lib/forecrew-fleet-agent/tart", TrustedKeysFile: "/etc/forecrew-fleet-agent/image-keys.pub",
		Tart: "tart", MinTartVersion: "2.27.3", MinFreeGB: 100, BootWaitSeconds: 300, Softnet: true, ImageCacheDays: 14,
	}
}

var (
	envRef       = regexp.MustCompile(`^env:([A-Z_][A-Z0-9_]*)$`)
	registryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	versionRe    = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// LoadConfig reads the file at path; a missing file gives the defaults.
func LoadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's file
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read tart config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("read tart config: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var bad []error
	if !filepath.IsAbs(c.StateDir) {
		bad = append(bad, fmt.Errorf("state_dir must be absolute, got %q", c.StateDir))
	}
	if c.MaxMachines < 0 || c.MaxMachines > maxVMs {
		bad = append(bad, fmt.Errorf("max_machines must be between 0 and %d: Apple's licence allows two macOS VMs per Mac, got %d", maxVMs, c.MaxMachines))
	}
	if !versionRe.MatchString(c.MinTartVersion) {
		bad = append(bad, fmt.Errorf("min_tart_version must look like 2.27.3, got %q", c.MinTartVersion))
	}
	if c.MinFreeGB < 0 || c.BootWaitSeconds < 10 || c.ImageCacheDays < 1 {
		bad = append(bad, errors.New("min_free_gb must not be negative, boot_wait_seconds must be at least 10 and image_cache_days at least 1"))
	}
	for key, v := range map[string]string{"registry_username": c.RegistryUsername, "registry_password": c.RegistryPassword} {
		if v != "" && !envRef.MatchString(v) {
			bad = append(bad, fmt.Errorf("%s must be a reference, env:NAME, never the value itself", key))
		}
	}
	if (c.RegistryUsername == "") != (c.RegistryPassword == "") {
		bad = append(bad, errors.New("registry_username and registry_password go together"))
	}
	if c.RegistryHost != "" && !registryHost.MatchString(c.RegistryHost) {
		bad = append(bad, fmt.Errorf("registry_host must be a registry host name, got %q", c.RegistryHost))
	}
	return errors.Join(bad...)
}

// refName returns the variable an env:NAME reference names.
func refName(ref string) string {
	if m := envRef.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ""
}

func (c Config) limit() int {
	if c.MaxMachines > 0 {
		return c.MaxMachines
	}
	return maxVMs
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

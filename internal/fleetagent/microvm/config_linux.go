package microvm

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Where the configuration is, and the variable that moves it.
const (
	envConfig     = "FORECREW_MICROVM_CONFIG"
	defaultConfig = "/etc/forecrew-fleet-agent/microvm.yaml"
)

// Config is microvm.yaml. It holds no secrets; every key has a default.
type Config struct {
	// StateDir holds the records, the images and the jails, on one file
	// system, which should support reflinks (btrfs, XFS) for fast clones.
	StateDir string `yaml:"state_dir"`
	// TrustedKeysFile lists the public keys images must be signed with, one
	// per line.
	TrustedKeysFile string `yaml:"trusted_keys_file"`
	// Firecracker, Jailer and CloudHypervisor are the commands.
	Firecracker     string `yaml:"firecracker"`
	Jailer          string `yaml:"jailer"`
	CloudHypervisor string `yaml:"cloud_hypervisor"`
	// UIDBase is the first of the users machines run as, one per machine.
	// Nothing else on the host may use them.
	UIDBase int `yaml:"uid_base"`
	// Subnet is where machines' /30 networks come from.
	Subnet string `yaml:"subnet"`
	// DNS is the resolver machines use.
	DNS string `yaml:"dns"`
	// MaxMachines is how many run at once; 0 sizes it from the host's memory.
	MaxMachines int `yaml:"max_machines"`
	// GPUs are the PCI addresses of the devices bound to vfio-pci, one per
	// machine that asks for one.
	GPUs []string `yaml:"gpus"`
	// CAFile is what the server's certificate chains to; 0 finds the host's
	// bundle.
	CAFile string `yaml:"ca_file"`
	// ImageCacheDays is how long an image unused stays cached.
	ImageCacheDays int `yaml:"image_cache_days"`
	// Egress is open (the internet and network_allow) or allowlist
	// (network_allow only, never a private network), for an instance's
	// hosted location.
	Egress string `yaml:"egress"`
}

func defaults() Config {
	return Config{
		StateDir: "/var/lib/forecrew-microvm", TrustedKeysFile: "/etc/forecrew-fleet-agent/image-keys.pub",
		Firecracker: "firecracker", Jailer: "jailer", CloudHypervisor: "cloud-hypervisor",
		UIDBase: 900000, Subnet: "172.30.0.0/16", DNS: "1.1.1.1", ImageCacheDays: 14, Egress: egressOpen,
	}
}

// LoadConfig reads the file at path; a missing file gives the defaults.
func LoadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's file
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read microvm config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("read microvm config: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var bad []error
	if !filepath.IsAbs(c.StateDir) {
		bad = append(bad, fmt.Errorf("state_dir must be absolute, got %q", c.StateDir))
	}
	if p, err := netip.ParsePrefix(c.Subnet); err != nil || !p.Addr().Is4() || p.Bits() > 28 {
		bad = append(bad, fmt.Errorf("subnet must be an IPv4 network of at least /28, got %q", c.Subnet))
	}
	if a, err := netip.ParseAddr(c.DNS); err != nil || !a.Is4() {
		bad = append(bad, fmt.Errorf("dns must be an IPv4 address, got %q", c.DNS))
	}
	if c.UIDBase < 1000 {
		bad = append(bad, fmt.Errorf("uid_base must be at least 1000, got %d", c.UIDBase))
	}
	if c.Egress != egressOpen && c.Egress != egressAllowlist {
		bad = append(bad, fmt.Errorf("egress must be %s or %s, got %q", egressOpen, egressAllowlist, c.Egress))
	}
	if c.ImageCacheDays < 1 {
		bad = append(bad, fmt.Errorf("image_cache_days must be at least 1, got %d", c.ImageCacheDays))
	}
	return errors.Join(bad...)
}

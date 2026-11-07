package proxmox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Where the configuration is, and the variable that moves it.
const (
	envConfig     = "FORECREW_PROXMOX_CONFIG"
	defaultConfig = "/etc/forecrew-fleet-agent/proxmox.yaml"
)

// Config is proxmox.yaml. It holds no secrets: the API token is in a file it
// names. There is no key to turn off the checking of the API's certificate; a
// self-signed one is trusted by giving its CA in ca_file.
type Config struct {
	// APIURL is the API's address, https only.
	APIURL string `yaml:"api_url"`
	// TokenFile holds the API token, user@realm!name=secret, readable by the
	// agent's user alone (the installer writes it).
	TokenFile string `yaml:"token_file"`
	// CAFile is what the API's certificate chains to, for a self-signed one
	// (/etc/pve/pve-root-ca.pem on a node is the cluster's CA); empty uses the
	// system's roots.
	CAFile string `yaml:"ca_file"`
	// TLSServerName is the name the certificate must carry, when the API is
	// reached by an address; the certificate is checked all the same.
	TLSServerName string `yaml:"tls_server_name"`
	// TrustedKeysFile lists the public keys images must be signed with.
	TrustedKeysFile string `yaml:"trusted_keys_file"`
	// Nodes are the nodes machines may be made on. A shape picks one of them.
	Nodes []string `yaml:"nodes"`
	// Bridge is the bridge of a machine's network device, and VLAN its tag
	// (0 for none); a shape may name its own.
	Bridge string `yaml:"bridge"`
	VLAN   int    `yaml:"vlan"`
	// DNS are the resolvers machines may use; with none, port 53 is open to any
	// destination.
	DNS []string `yaml:"dns"`
	// MaxMachines is how many Forecrew VMs the cluster holds at once.
	MaxMachines int `yaml:"max_machines"`
	// PollInterval spaces the polls of a task or the guest agent.
	PollInterval time.Duration `yaml:"poll_interval"`
}

func defaults() Config {
	return Config{ // #nosec G101 -- file names, not secrets
		APIURL: "https://localhost:8006", TokenFile: "/etc/forecrew-fleet-agent/proxmox-token",
		TrustedKeysFile: "/etc/forecrew-fleet-agent/image-keys.pub", Bridge: "vmbr0", MaxMachines: 8, PollInterval: 2 * time.Second,
	}
}

// LoadConfig reads the file at path; a missing file gives the defaults, which
// fail validation for want of nodes.
func LoadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's file
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("read proxmox config: %w", err)
	}
	if err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("read proxmox config: %w", err)
		}
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var bad []error
	if len(c.Nodes) == 0 {
		bad = append(bad, errors.New("nodes must list the nodes machines may be made on"))
	}
	for _, n := range c.Nodes {
		if !validNode(n) {
			bad = append(bad, fmt.Errorf("node %q is not a node name", n))
		}
	}
	if !filepath.IsAbs(c.TokenFile) {
		bad = append(bad, fmt.Errorf("token_file must be absolute, got %q", c.TokenFile))
	}
	if !filepath.IsAbs(c.TrustedKeysFile) {
		bad = append(bad, fmt.Errorf("trusted_keys_file must be absolute, got %q", c.TrustedKeysFile))
	}
	if c.CAFile != "" && !filepath.IsAbs(c.CAFile) {
		bad = append(bad, fmt.Errorf("ca_file must be absolute, got %q", c.CAFile))
	}
	if !validBridge(c.Bridge) {
		bad = append(bad, fmt.Errorf("bridge %q is not a bridge name", c.Bridge))
	}
	if c.VLAN < 0 || c.VLAN > 4094 {
		bad = append(bad, fmt.Errorf("vlan must be 0 to 4094, got %d", c.VLAN))
	}
	for _, d := range c.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			bad = append(bad, fmt.Errorf("dns must list IPv4 addresses, got %q", d))
		}
	}
	if c.MaxMachines < 1 {
		bad = append(bad, fmt.Errorf("max_machines must be at least 1, got %d", c.MaxMachines))
	}
	if c.PollInterval < time.Millisecond {
		bad = append(bad, fmt.Errorf("poll_interval must be at least 1ms, got %s", c.PollInterval))
	}
	return errors.Join(bad...)
}

// validNode accepts what a node's name can be: a host name. The value goes into
// URL paths.
func validNode(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !alnum(r) && r != '-' {
			return false
		}
	}
	return s[0] != '-'
}

// validBridge accepts a Linux interface name or an SDN VNet name.
func validBridge(s string) bool {
	if s == "" || len(s) > 15 {
		return false
	}
	for _, r := range s {
		if !alnum(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func alnum(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' }

// shapeConfig is what a shape's provider_config says. Node and storage are the
// shape's choice, within what proxmox.yaml allows.
type shapeConfig struct {
	// Node makes the machine; Template is the VMID of the template cloned.
	Node     string `json:"node"`
	Template int    `json:"template"`
	// Storage, when set, is where a full clone goes; without it the clone is
	// linked to the template's disk.
	Storage string `json:"storage"`
	// Pool is a resource pool the VM joins.
	Pool string `json:"pool"`
	// Disk is the key of the disk to size, scsi0 by default.
	Disk string `json:"disk"`
	// Bridge and VLAN replace the location's.
	Bridge string `json:"bridge"`
	VLAN   *int   `json:"vlan"`
}

// parseShape reads a shape's provider_config strictly: a misspelled key is an
// error, not a setting that silently does nothing.
func parseShape(raw map[string]any, c Config) (shapeConfig, error) {
	var s shapeConfig
	b, err := json.Marshal(raw)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("provider_config: %w", err)
	}
	if s.Template < 100 || s.Template > 999999999 {
		return s, errors.New("provider_config.template must be the VMID of a template")
	}
	if s.Node == "" && len(c.Nodes) == 1 {
		s.Node = c.Nodes[0]
	}
	if !slices.Contains(c.Nodes, s.Node) {
		return s, fmt.Errorf("provider_config.node %q is not one of the nodes in proxmox.yaml (%s)", s.Node, strings.Join(c.Nodes, ", "))
	}
	for k, v := range map[string]string{"storage": s.Storage, "pool": s.Pool} {
		if v != "" && !validName(v) {
			return s, fmt.Errorf("provider_config.%s %q is not a name", k, v)
		}
	}
	if s.Disk == "" {
		s.Disk = "scsi0"
	}
	if !diskKeyRE.MatchString(s.Disk) {
		return s, fmt.Errorf("provider_config.disk %q is not a disk key such as scsi0", s.Disk)
	}
	if s.Bridge == "" {
		s.Bridge = c.Bridge
	}
	if !validBridge(s.Bridge) {
		return s, fmt.Errorf("provider_config.bridge %q is not a bridge name", s.Bridge)
	}
	vlan := c.VLAN
	if s.VLAN != nil {
		vlan = *s.VLAN
	}
	if vlan < 0 || vlan > 4094 {
		return s, fmt.Errorf("provider_config.vlan must be 0 to 4094, got %d", vlan)
	}
	s.VLAN = &vlan
	return s, nil
}

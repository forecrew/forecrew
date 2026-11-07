package hyperv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Where the configuration is, and the variable that moves it.
const (
	envConfig     = "FORECREW_HYPERV_CONFIG"
	defaultConfig = `C:\ProgramData\forecrew-fleet-agent\hyperv.yaml`
	// defaultState is under the folder install.ps1 closes to the agent, SYSTEM and
	// Administrators.
	defaultState = `C:\ProgramData\forecrew-fleet-agent\state\hyperv`
)

// unsafePath holds what a path given to a script must not: quotes, shell and
// wildcard characters.
const unsafePath = "\"'<>|?*%$`;&\x00\r\n"

var driveRoot = regexp.MustCompile(`^[A-Za-z]:\\`)

// validPath accepts an absolute path, on a drive or native to this host (so
// that a test can keep its files in a directory of its own), that is a name and
// nothing a script could take for more: no UNC share, no special characters.
func validPath(s string) bool {
	return (driveRoot.MatchString(s) || filepath.IsAbs(s)) && !strings.HasPrefix(s, `\\`) && !strings.ContainsAny(s, unsafePath)
}

// switchName is a Hyper-V virtual switch's name.
var switchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,63}$`)

// Config is hyperv.yaml. It holds no secrets: the host's own account is the
// agent's, a member of Hyper-V Administrators.
type Config struct {
	// StateDir holds the scripts and, by default, the images and the VMs.
	StateDir string `yaml:"state_dir"`
	// VMDir holds one directory per machine; ImageDir the base images.
	VMDir    string `yaml:"vm_dir"`
	ImageDir string `yaml:"image_dir"`
	// TrustedKeysFile lists the public keys images must be signed with.
	TrustedKeysFile string `yaml:"trusted_keys_file"`
	// Switch is the virtual switch machines connect to, and VLAN the tag of
	// their device (0 for none); a shape may name its own.
	Switch string `yaml:"switch"`
	VLAN   int    `yaml:"vlan"`
	// Generation of the VMs: 1 boots the BIOS images the Windows template builds.
	Generation int `yaml:"generation"`
	// DNS are the resolvers machines may use; with none, port 53 is open to any
	// destination.
	DNS []string `yaml:"dns"`
	// MaxMachines is how many run at once on this host.
	MaxMachines int `yaml:"max_machines"`
	// PowerShell is the command that runs the scripts.
	PowerShell string `yaml:"powershell"`
	// ImageCacheDays is how long an image unused stays cached.
	ImageCacheDays int `yaml:"image_cache_days"`
	// ScriptTimeout bounds one script.
	ScriptTimeout time.Duration `yaml:"script_timeout"`
}

func defaults() Config {
	return Config{
		StateDir: defaultState, TrustedKeysFile: `C:\ProgramData\forecrew-fleet-agent\image-keys.pub`, Generation: 1, MaxMachines: 2,
		PowerShell: "powershell.exe", ImageCacheDays: 14, ScriptTimeout: 10 * time.Minute,
	}
}

// LoadConfig reads the file at path; a missing file gives the defaults, which
// fail validation for want of a switch.
func LoadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's file
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("read hyperv config: %w", err)
	}
	if err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("read hyperv config: %w", err)
		}
	}
	c.fill()
	return c, c.validate()
}

// fill sets the directories that follow StateDir.
func (c *Config) fill() {
	if c.VMDir == "" {
		c.VMDir = strings.TrimRight(c.StateDir, `\`) + `\vms`
	}
	if c.ImageDir == "" {
		c.ImageDir = strings.TrimRight(c.StateDir, `\`) + `\images`
	}
}

func (c Config) validate() error {
	var bad []error
	for name, v := range map[string]string{"state_dir": c.StateDir, "vm_dir": c.VMDir, "image_dir": c.ImageDir, "trusted_keys_file": c.TrustedKeysFile} {
		if !validPath(v) {
			bad = append(bad, fmt.Errorf("%s must be an absolute path such as C:\\ProgramData\\x, got %q", name, v))
		}
	}
	if !switchName.MatchString(c.Switch) {
		bad = append(bad, fmt.Errorf("switch must name a virtual switch, got %q", c.Switch))
	}
	if c.VLAN < 0 || c.VLAN > 4094 {
		bad = append(bad, fmt.Errorf("vlan must be 0 to 4094, got %d", c.VLAN))
	}
	if c.Generation != 1 && c.Generation != 2 {
		bad = append(bad, fmt.Errorf("generation must be 1 or 2, got %d", c.Generation))
	}
	for _, d := range c.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			bad = append(bad, fmt.Errorf("dns must list IPv4 addresses, got %q", d))
		}
	}
	if c.MaxMachines < 1 {
		bad = append(bad, fmt.Errorf("max_machines must be at least 1, got %d", c.MaxMachines))
	}
	if c.PowerShell == "" {
		bad = append(bad, errors.New("powershell must name the command"))
	}
	if c.ImageCacheDays < 1 {
		bad = append(bad, fmt.Errorf("image_cache_days must be at least 1, got %d", c.ImageCacheDays))
	}
	if c.ScriptTimeout < time.Second {
		bad = append(bad, fmt.Errorf("script_timeout must be at least 1s, got %s", c.ScriptTimeout))
	}
	return errors.Join(bad...)
}

// shapeConfig is what a shape's provider_config says: nothing is needed, and
// the switch and VLAN may be the shape's own.
type shapeConfig struct {
	Switch string `json:"switch"`
	VLAN   *int   `json:"vlan"`
}

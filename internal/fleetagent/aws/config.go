package aws

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

// Where the configuration is, and the variable that moves it.
const (
	envConfig     = "FORECREW_AWS_CONFIG"
	defaultConfig = "/etc/forecrew-fleet-agent/aws.yaml"
)

var (
	regionRE  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	subnetRE  = regexp.MustCompile(`^subnet-[0-9a-f]{8,17}$`)
	groupRE   = regexp.MustCompile(`^sg-[0-9a-f]{8,17}$`)
	fleetRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	typeRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.*-]{0,40}$`)
	accountRE = regexp.MustCompile(`^[0-9]{12}$`)
)

// Config is aws.yaml. It holds no credentials, ever: the AWS SDK finds them the
// way it always does, and on EC2 that is the instance role. Unknown keys, such as
// an access key someone pasted in, are refused.
type Config struct {
	// Region is where machines start.
	Region string `yaml:"region"`
	// Fleet is the value of the forecrew:fleet tag on everything this agent
	// creates. The kit's role allows exactly this value, and the agent ends every
	// instance in the region that carries it, so it is required and must be unique
	// per account and region.
	Fleet string `yaml:"fleet"`
	// Subnets are the subnets machines may start in; a shape names one, or gets
	// the first.
	Subnets []string `yaml:"subnets"`
	// SecurityGroup is the group of every machine.
	SecurityGroup string `yaml:"security_group"`
	// InstanceTypes lists the instance types a shape may use, with * as in the
	// role's policy.
	InstanceTypes []string `yaml:"instance_types"`
	// TrustedKeysFile lists the public keys images must be signed with.
	TrustedKeysFile string `yaml:"trusted_keys_file"`
	// TrustedAMIOwners are accounts, besides this one, whose AMIs may be used
	// without the digest tag (an organization's own image account). Empty means
	// only this account's AMIs, which the kit's AMI copy tags.
	TrustedAMIOwners []string `yaml:"trusted_ami_owners"`
	// AssignPublicIP gives machines a public address, for a public subnet; a
	// private subnet with a NAT gateway needs none.
	AssignPublicIP bool `yaml:"assign_public_ip"`
	// MaxMachines is the most machines at once, whatever forecrewd asks: the
	// customer's own limit on the bill.
	MaxMachines int `yaml:"max_machines"`
	// MonthlyBudget stops new machines when the month's estimated spend reaches
	// it, in the currency of the shapes' prices; 0 is no limit.
	MonthlyBudget float64 `yaml:"monthly_budget"`
	// BudgetWarnPercent is when the location reports a warning.
	BudgetWarnPercent int `yaml:"budget_warn_percent"`
	// MaxLifetime is how long any machine may live: its own timer shuts it
	// down, and the agent terminates what outlives it.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
	// StateDir holds the spend ledger.
	StateDir string `yaml:"state_dir"`
}

func defaults() Config {
	return Config{
		TrustedKeysFile: "/etc/forecrew-fleet-agent/image-keys.pub", MaxMachines: 10,
		BudgetWarnPercent: 80, MaxLifetime: 24 * time.Hour, StateDir: "/var/lib/forecrew-fleet-agent/aws",
	}
}

// LoadConfig reads the file at path. Unlike a microVM host, an account has no
// useful defaults for its network, so a missing file is an error.
func LoadConfig(path string) (Config, error) {
	c := defaults()
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's file
	if errors.Is(err, fs.ErrNotExist) {
		return c, fmt.Errorf("no AWS configuration: write %s (deploy/byoc/aws makes it)", path)
	}
	if err != nil {
		return c, fmt.Errorf("read aws config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("read aws config: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	return errors.Join(c.validateNetwork(), c.validateLimits())
}

// validateNetwork checks where machines start and what they may be.
func (c Config) validateNetwork() error {
	var bad []error
	add := func(format string, args ...any) { bad = append(bad, fmt.Errorf(format, args...)) }
	if !regionRE.MatchString(c.Region) {
		add("region must be an AWS region such as eu-west-1, got %q", c.Region)
	}
	if !fleetRE.MatchString(c.Fleet) {
		add("fleet must be 1 to 64 letters, digits and . _ -, got %q", c.Fleet)
	}
	if len(c.Subnets) == 0 {
		add("subnets must list at least one subnet")
	}
	for _, s := range c.Subnets {
		if !subnetRE.MatchString(s) {
			add("subnets: %q is not a subnet ID", s)
		}
	}
	if !groupRE.MatchString(c.SecurityGroup) {
		add("security_group must be a security group ID, got %q", c.SecurityGroup)
	}
	if len(c.InstanceTypes) == 0 {
		add("instance_types must list at least one instance type or pattern, such as c7i.*")
	}
	for _, t := range c.InstanceTypes {
		if !typeRE.MatchString(t) {
			add("instance_types: %q is not an instance type or pattern", t)
		}
	}
	for _, o := range c.TrustedAMIOwners {
		if !accountRE.MatchString(o) {
			add("trusted_ami_owners: %q is not an account ID", o)
		}
	}
	return errors.Join(bad...)
}

// validateLimits checks the brakes and where the agent keeps its state.
func (c Config) validateLimits() error {
	var bad []error
	add := func(format string, args ...any) { bad = append(bad, fmt.Errorf(format, args...)) }
	if c.MaxMachines < 1 || c.MaxMachines > 1000 {
		add("max_machines must be between 1 and 1000, got %d", c.MaxMachines)
	}
	if c.MonthlyBudget < 0 {
		add("monthly_budget must not be negative")
	}
	if c.BudgetWarnPercent < 1 || c.BudgetWarnPercent > 100 {
		add("budget_warn_percent must be between 1 and 100, got %d", c.BudgetWarnPercent)
	}
	if c.MaxLifetime < time.Hour || c.MaxLifetime > 14*24*time.Hour {
		add("max_lifetime must be between 1h and 336h, got %s", c.MaxLifetime)
	}
	if !filepath.IsAbs(c.StateDir) {
		add("state_dir must be absolute, got %q", c.StateDir)
	}
	return errors.Join(bad...)
}

// allowsType reports whether an instance type is one the configuration lists.
func (c Config) allowsType(t string) bool {
	return slices.ContainsFunc(c.InstanceTypes, func(p string) bool {
		ok, err := path.Match(p, t)
		return err == nil && ok
	})
}

// ShapeConfig is a shape's provider_config for this kind (fleet.md).
type ShapeConfig struct {
	InstanceType  string
	Subnet        string
	SecurityGroup string
	Spot          bool
}

// shapeConfig reads and checks what a shape says against what this agent allows.
// It refuses a key it does not know, as doctor does, and a value outside the
// configuration, which the role would refuse anyway.
func (c Config) shapeConfig(raw map[string]any) (ShapeConfig, error) {
	var s ShapeConfig
	for k, v := range raw {
		var ok bool
		switch k {
		case "instance_type":
			s.InstanceType, ok = v.(string)
		case "region":
			var r string
			if r, ok = v.(string); ok && r != c.Region {
				return s, fmt.Errorf("the shape says region %q, this agent works in %s", r, c.Region)
			}
		case "subnet":
			s.Subnet, ok = v.(string)
		case "security_group":
			s.SecurityGroup, ok = v.(string)
		case "spot":
			s.Spot, ok = v.(bool)
		default:
			return s, fmt.Errorf("provider_config: unknown key %q (aws knows instance_type, region, subnet, security_group and spot)", k)
		}
		if !ok {
			return s, fmt.Errorf("provider_config: %s has the wrong type", k)
		}
	}
	if s.InstanceType == "" {
		return s, errors.New("provider_config: instance_type is required")
	}
	if !c.allowsType(s.InstanceType) {
		return s, fmt.Errorf("instance type %q is not in this agent's instance_types", s.InstanceType)
	}
	if s.Subnet == "" {
		s.Subnet = c.Subnets[0]
	}
	if !slices.Contains(c.Subnets, s.Subnet) {
		return s, fmt.Errorf("subnet %q is not one of this agent's subnets", s.Subnet)
	}
	if s.SecurityGroup == "" {
		s.SecurityGroup = c.SecurityGroup
	}
	if s.SecurityGroup != c.SecurityGroup {
		return s, fmt.Errorf("security group %q is not this agent's", s.SecurityGroup)
	}
	return s, nil
}

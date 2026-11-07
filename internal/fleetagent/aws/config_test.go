package aws

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodConfig = `
region: eu-west-1
fleet: forecrew
subnets: [subnet-0a1b2c3d4e5f60718]
security_group: sg-0a1b2c3d4e5f60718
instance_types: ["c7i.*", "m7g.large"]
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aws.yaml")
	must(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, goodConfig+"max_machines: 4\nmax_lifetime: 6h\nmonthly_budget: 250.5\nassign_public_ip: true\n"))
	must(t, err)
	if c.Fleet != "forecrew" || c.MaxMachines != 4 || c.MaxLifetime != 6*time.Hour || c.MonthlyBudget != 250.5 || !c.AssignPublicIP || c.BudgetWarnPercent != 80 {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"a pasted access key": {goodConfig + "access_key_id: AKIAIOSFODNN7EXAMPLE\n", "access_key_id"},
		"a secret key":        {goodConfig + "secret_access_key: x\n", "secret_access_key"},
		"bad region":          {strings.Replace(goodConfig, "eu-west-1", "mars", 1), "region"},
		"no subnets":          {strings.Replace(goodConfig, "[subnet-0a1b2c3d4e5f60718]", "[]", 1), "subnets must list"},
		"bad subnet":          {strings.Replace(goodConfig, "subnet-0a1b2c3d4e5f60718", "vpc-1", 1), "not a subnet ID"},
		"bad group":           {strings.Replace(goodConfig, "sg-0a1b2c3d4e5f60718", "default", 1), "security group ID"},
		"no types":            {strings.Replace(goodConfig, `["c7i.*", "m7g.large"]`, "[]", 1), "instance_types"},
		"any type":            {strings.Replace(goodConfig, `"c7i.*"`, `"C7I; DROP"`, 1), "not an instance type"},
		"bad owner":           {goodConfig + "trusted_ami_owners: [me]\n", "account ID"},
		"too many machines":   {goodConfig + "max_machines: 5000\n", "max_machines"},
		"negative budget":     {goodConfig + "monthly_budget: -1\n", "monthly_budget"},
		"warn percent":        {goodConfig + "budget_warn_percent: 0\n", "budget_warn_percent"},
		"lifetime too short":  {goodConfig + "max_lifetime: 1m\n", "max_lifetime"},
		"relative state dir":  {goodConfig + "state_dir: state\n", "state_dir"},
		"no fleet":            {strings.Replace(goodConfig, "fleet: forecrew\n", "", 1), "fleet must"},
		"bad fleet":           {goodConfig + "fleet: 'a b'\n", "fleet"},
		"not yaml":            {"region: [", "read aws config"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "no AWS configuration") {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Fatal("a directory was read")
	}
}

func TestShapeConfigTypes(t *testing.T) {
	c := testConfig(t.TempDir())
	for _, raw := range []map[string]any{
		{"instance_type": 5}, {"instance_type": "c7i.large", "spot": "yes"}, {"instance_type": "c7i.large", "subnet": 1},
		{"instance_type": "c7i.large", "security_group": 1}, {"instance_type": "c7i.large", "region": 1}, {},
	} {
		if _, err := c.shapeConfig(raw); err == nil {
			t.Errorf("%v was accepted", raw)
		}
	}
	got, err := c.shapeConfig(map[string]any{"instance_type": "c7i.large", "spot": true, "region": testRegion})
	must(t, err)
	if got != (ShapeConfig{InstanceType: "c7i.large", Subnet: testSubnet, SecurityGroup: testGroup, Spot: true}) {
		t.Fatalf("shape config = %+v", got)
	}
}

func TestNewRegistered(t *testing.T) {
	t.Setenv(envConfig, filepath.Join(t.TempDir(), "none.yaml"))
	if _, err := newRegistered(); err == nil {
		t.Fatal("no configuration, no provider")
	}
	t.Setenv(envConfig, writeConfig(t, goodConfig))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	p, err := newRegistered()
	must(t, err) // no call is made: creating the provider needs neither network nor credentials
	if p == nil {
		t.Fatal("no provider")
	}
	t.Setenv(envConfig, "")
	if _, err := newRegistered(); err == nil || !strings.Contains(err.Error(), defaultConfig) {
		t.Fatalf("the default path is %s: %v", defaultConfig, err)
	}
}

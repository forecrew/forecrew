package aws

import (
	"errors"
	"slices"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/forecrew/forecrew/internal/domain"
)

func instanceType(name string, cpus int32, mib int64, arch types.ArchitectureType, gpus int32) types.InstanceTypeInfo {
	t := types.InstanceTypeInfo{
		InstanceType: types.InstanceType(name), VCpuInfo: &types.VCpuInfo{DefaultVCpus: awssdk.Int32(cpus)},
		MemoryInfo:    &types.MemoryInfo{SizeInMiB: awssdk.Int64(mib)},
		ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: []types.ArchitectureType{arch}},
	}
	if gpus > 0 {
		t.GpuInfo = &types.GpuInfo{Gpus: []types.GpuDeviceInfo{{Count: awssdk.Int32(gpus)}}}
	}
	return t
}

func checkOf(t *testing.T, c domain.ProviderCapabilities, name string) domain.ProviderCheck {
	t.Helper()
	for _, k := range c.Checks {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("no check %s in %+v", name, c.Checks)
	return domain.ProviderCheck{}
}

func TestCapabilities(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.instanceTypes = []types.InstanceTypeInfo{
		instanceType("c7i.large", 2, 4096, types.ArchitectureTypeX8664, 0),
		instanceType("c7i.8xlarge", 32, 65536, types.ArchitectureTypeX8664, 0),
		instanceType("m7g.large", 2, 8192, types.ArchitectureTypeArm64, 0),
		instanceType("g6.xlarge", 4, 16384, types.ArchitectureTypeX8664, 4), // not allowed: not asked for
	}
	c, err := e.p.Capabilities(t.Context())
	must(t, err)
	if c.MaxCPU != 32 || c.MaxMemoryMB != 65536 || c.MaxGPU != 0 || c.MaxMachines != 3 || c.MaxDiskGB != maxDiskGB ||
		c.CloudAccount != testAccount || c.Region != testRegion {
		t.Fatalf("capabilities = %+v", c)
	}
	if !slices.Equal(c.OS, []domain.MachineOS{domain.OSLinux}) || !slices.Equal(c.ImageFormats, []domain.ImageFormat{domain.ImageAMI}) ||
		!slices.Equal(c.Arch, []domain.MachineArch{domain.ArchAMD64, domain.ArchARM64}) {
		t.Fatalf("capabilities = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"credentials", "instance_types", "subnets", "security_group", "budget"} {
		if k := checkOf(t, c, name); k.Status != statusOK {
			t.Errorf("%s: %+v", name, k)
		}
	}
	if !strings.Contains(checkOf(t, c, "credentials").Detail, testAccount) {
		t.Error("the account is not in the credentials check")
	}
}

func TestCapabilitiesCountGPUs(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.InstanceTypes = []string{"g6.*"} })
	e.ec2.instanceTypes = []types.InstanceTypeInfo{instanceType("g6.12xlarge", 48, 196608, types.ArchitectureTypeX8664, 4)}
	c, err := e.p.Capabilities(t.Context())
	must(t, err)
	if c.MaxGPU != 4 {
		t.Fatalf("gpus = %d", c.MaxGPU)
	}
}

func TestCapabilitiesSayWhatIsWrong(t *testing.T) {
	e := newEnv(t, nil)
	e.sts.err = errors.New("no credentials in the chain")
	delete(e.ec2.subnets, testSubnet2)
	e.ec2.groups[testGroup] = types.SecurityGroup{GroupId: awssdk.String(testGroup), IpPermissions: []types.IpPermission{{IpProtocol: awssdk.String("tcp")}}}
	c, err := e.p.Capabilities(t.Context())
	must(t, err)
	for name, want := range map[string]struct{ status, in string }{
		"credentials":    {statusFail, "no credentials"},
		"instance_types": {statusFail, "knows none"},
		"subnets":        {statusFail, "1 of the 2"},
		"security_group": {statusWarn, "inbound"},
	} {
		k := checkOf(t, c, name)
		if k.Status != want.status || !strings.Contains(k.Detail, want.in) {
			t.Errorf("%s: %+v", name, k)
		}
	}
	if c.CloudAccount != "" {
		t.Error("an account without credentials")
	}

	e.ec2.groups[testGroup] = types.SecurityGroup{GroupId: awssdk.String(testGroup), IpPermissionsEgress: []types.IpPermission{
		{IpProtocol: awssdk.String("-1"), IpRanges: []types.IpRange{{CidrIp: awssdk.String("0.0.0.0/0")}}}}}
	c, _ = e.p.Capabilities(t.Context())
	if k := checkOf(t, c, "security_group"); k.Status != statusWarn || !strings.Contains(k.Detail, "every address") {
		t.Errorf("a group open to the world: %+v", k)
	}
	delete(e.ec2.groups, testGroup)
	c, _ = e.p.Capabilities(t.Context())
	if k := checkOf(t, c, "security_group"); k.Status != statusFail {
		t.Errorf("a missing group: %+v", k)
	}
}

func TestCapabilitiesFailuresOfEC2(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.policy = &policyDocument{} // the role allows nothing
	c, err := e.p.Capabilities(t.Context())
	must(t, err)
	for _, name := range []string{"instance_types", "subnets", "security_group"} {
		if k := checkOf(t, c, name); k.Status != statusFail || !strings.Contains(k.Detail, "UnauthorizedOperation") {
			t.Errorf("%s: %+v", name, k)
		}
	}
}

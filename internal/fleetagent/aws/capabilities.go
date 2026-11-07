package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/forecrew/forecrew/internal/domain"
)

// Check statuses (domain.ProviderCheck).
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// Capabilities implements fleet.Provider: what the account can start now, found
// by asking EC2 about the instance types, subnets and group the configuration
// names, with a check for each thing that can be wrong. It holds facts, never
// credentials.
func (p *Provider) Capabilities(ctx context.Context) (domain.ProviderCapabilities, error) {
	c := domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSLinux}, ImageFormats: []domain.ImageFormat{domain.ImageAMI},
		MaxMachines: p.cfg.MaxMachines, MaxDiskGB: maxDiskGB, Region: p.cfg.Region,
	}
	account, err := p.accountID(ctx)
	if err != nil {
		c.Checks = append(c.Checks, domain.ProviderCheck{Name: "credentials", Status: statusFail, Detail: err.Error()})
	} else {
		c.CloudAccount = account
		c.Checks = append(c.Checks, domain.ProviderCheck{Name: "credentials", Status: statusOK, Detail: "account " + account})
	}
	c.Checks = append(c.Checks, p.instanceTypes(ctx, &c), p.subnets(ctx), p.securityGroup(ctx), p.budget())
	return c, nil
}

// instanceTypes sizes the location from the types the configuration allows.
func (p *Provider) instanceTypes(ctx context.Context, c *domain.ProviderCapabilities) domain.ProviderCheck {
	in := &ec2.DescribeInstanceTypesInput{Filters: []types.Filter{{Name: awssdk.String("instance-type"), Values: p.cfg.InstanceTypes}}}
	found := 0
	for pages := ec2.NewDescribeInstanceTypesPaginator(p.ec2, in); pages.HasMorePages(); {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return domain.ProviderCheck{Name: "instance_types", Status: statusFail, Detail: err.Error()}
		}
		for _, t := range page.InstanceTypes {
			found++
			sizeBy(c, t)
		}
	}
	if found == 0 {
		return domain.ProviderCheck{Name: "instance_types", Status: statusFail, Detail: "EC2 knows none of " + strings.Join(p.cfg.InstanceTypes, ", ") + " in " + p.cfg.Region}
	}
	return domain.ProviderCheck{Name: "instance_types", Status: statusOK, Detail: fmt.Sprintf("%d types, up to %d CPUs and %d MB", found, c.MaxCPU, c.MaxMemoryMB)}
}

// sizeBy widens the location's per-machine limits to hold instance type t.
func sizeBy(c *domain.ProviderCapabilities, t types.InstanceTypeInfo) {
	if t.VCpuInfo != nil {
		c.MaxCPU = max(c.MaxCPU, int(awssdk.ToInt32(t.VCpuInfo.DefaultVCpus)))
	}
	if t.MemoryInfo != nil {
		c.MaxMemoryMB = max(c.MaxMemoryMB, int(awssdk.ToInt64(t.MemoryInfo.SizeInMiB)))
	}
	if t.GpuInfo != nil {
		n := 0
		for _, g := range t.GpuInfo.Gpus {
			n += int(awssdk.ToInt32(g.Count))
		}
		c.MaxGPU = max(c.MaxGPU, n)
	}
	if t.ProcessorInfo == nil {
		return
	}
	for _, a := range t.ProcessorInfo.SupportedArchitectures {
		switch a {
		case types.ArchitectureTypeX8664:
			c.Arch = appendOnce(c.Arch, domain.ArchAMD64)
		case types.ArchitectureTypeArm64:
			c.Arch = appendOnce(c.Arch, domain.ArchARM64)
		}
	}
}

func appendOnce[T comparable](s []T, v T) []T {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func (p *Provider) subnets(ctx context.Context) domain.ProviderCheck {
	out, err := p.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: p.cfg.Subnets})
	if err != nil {
		return domain.ProviderCheck{Name: "subnets", Status: statusFail, Detail: err.Error()}
	}
	if len(out.Subnets) != len(p.cfg.Subnets) {
		return domain.ProviderCheck{Name: "subnets", Status: statusFail, Detail: fmt.Sprintf("found %d of the %d subnets", len(out.Subnets), len(p.cfg.Subnets))}
	}
	return domain.ProviderCheck{Name: "subnets", Status: statusOK, Detail: fmt.Sprintf("%d subnets", len(out.Subnets))}
}

// securityGroup checks the machines' group exists and is closed: nothing in, and
// nothing out that the kit's group does not allow. A group that lets machines
// reach the VPC's own addresses is the customer's choice, but the location says
// so.
func (p *Provider) securityGroup(ctx context.Context) domain.ProviderCheck {
	out, err := p.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{p.cfg.SecurityGroup}})
	if err != nil {
		return domain.ProviderCheck{Name: "security_group", Status: statusFail, Detail: err.Error()}
	}
	if len(out.SecurityGroups) != 1 {
		return domain.ProviderCheck{Name: "security_group", Status: statusFail, Detail: "no such group " + p.cfg.SecurityGroup}
	}
	g := out.SecurityGroups[0]
	switch {
	case len(g.IpPermissions) > 0:
		return domain.ProviderCheck{Name: "security_group", Status: statusWarn, Detail: "the group accepts inbound traffic: machines should accept none"}
	case wideOpen(g.IpPermissionsEgress):
		return domain.ProviderCheck{Name: "security_group", Status: statusWarn, Detail: "the group lets machines reach every address, the VPC and its private networks too"}
	}
	return domain.ProviderCheck{Name: "security_group", Status: statusOK, Detail: "closed to inbound traffic"}
}

// wideOpen reports an egress rule for all protocols to 0.0.0.0/0, which is what
// a new group has by default.
func wideOpen(rules []types.IpPermission) bool {
	return slices.ContainsFunc(rules, func(r types.IpPermission) bool {
		return awssdk.ToString(r.IpProtocol) == "-1" && slices.ContainsFunc(r.IpRanges, func(x types.IpRange) bool { return awssdk.ToString(x.CidrIp) == "0.0.0.0/0" })
	})
}

// budget is the alert: a warning when the month's estimate reaches the percent
// the configuration names, and a failure when it reaches the budget, which is when
// new machines stop.
func (p *Provider) budget() domain.ProviderCheck {
	if p.ledger == nil {
		return domain.ProviderCheck{Name: "budget", Status: statusOK, Detail: "no monthly budget set on this agent"}
	}
	spent, limit := p.ledger.spent(), p.cfg.MonthlyBudget
	pct := int(spent / limit * 100)
	detail := fmt.Sprintf("estimated %d%% of the monthly budget of %g", pct, limit)
	switch {
	case spent >= limit:
		return domain.ProviderCheck{Name: "budget", Status: statusFail, Detail: detail + ": no new machines until next month"}
	case pct >= p.cfg.BudgetWarnPercent:
		return domain.ProviderCheck{Name: "budget", Status: statusWarn, Detail: detail}
	}
	return domain.ProviderCheck{Name: "budget", Status: statusOK, Detail: detail}
}

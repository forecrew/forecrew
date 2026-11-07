package aws

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/forecrew/forecrew/internal/fleet"
)

// ec2API is the part of the EC2 API the provider uses. *ec2.Client is one; the
// tests' fake is another, and it enforces the role's policy on every call.
type ec2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
}

// identityAPI says whose credentials these are.
type identityAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// capacityCodes are the EC2 errors that mean there is no room now, which is not
// the machine's fault: the controller tries again, or another shape.
var capacityCodes = []string{
	"InsufficientInstanceCapacity", "InsufficientHostCapacity", "InstanceLimitExceeded", "VcpuLimitExceeded",
	"MaxSpotInstanceCountExceeded", "SpotMaxPriceTooLow", "InsufficientFreeAddressesInSubnet",
}

// notFoundCodes say an instance is gone.
var notFoundCodes = []string{"InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed"}

func hasCode(err error, codes []string) bool {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return false
	}
	for _, c := range codes {
		if api.ErrorCode() == c {
			return true
		}
	}
	return false
}

// capacity turns a "no room" error into fleet.ErrCapacity.
func capacity(err error) error {
	if hasCode(err, capacityCodes) {
		return errors.Join(fleet.ErrCapacity, err)
	}
	return err
}

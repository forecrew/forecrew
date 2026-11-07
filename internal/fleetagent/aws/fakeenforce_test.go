package aws

import (
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// What the provider sends must pass the role: the fake EC2 authorizes with the
// same policy, so every Create in these tests already proves it. This one checks
// that the fake would refuse what the role refuses, so those proofs mean something.
func TestFakeEC2EnforcesThePolicy(t *testing.T) {
	e := newEnv(t, nil)
	inst := e.create(e.request())
	good := e.ec2.runs[0]

	for name, mutate := range map[string]func(in *ec2.RunInstancesInput){
		"a volume with no tags": func(in *ec2.RunInstancesInput) {
			in.TagSpecifications = in.TagSpecifications[:1]
		},
		"an instance profile": func(in *ec2.RunInstancesInput) {
			in.IamInstanceProfile = &types.IamInstanceProfileSpecification{Name: awssdk.String("admin")}
		},
		"IMDSv1": func(in *ec2.RunInstancesInput) { in.MetadataOptions.HttpTokens = types.HttpTokensStateOptional },
		"an unencrypted disk": func(in *ec2.RunInstancesInput) {
			in.BlockDeviceMappings = []types.BlockDeviceMapping{{DeviceName: awssdk.String("/dev/xvda"), Ebs: &types.EbsBlockDevice{VolumeSize: awssdk.Int32(10)}}}
		},
		"a type that is not listed": func(in *ec2.RunInstancesInput) { in.InstanceType = "p5.48xlarge" },
	} {
		in := *good
		meta := *good.MetadataOptions
		in.MetadataOptions = &meta
		in.ClientToken = awssdk.String("fc-other-" + name)
		mutate(&in)
		_, err := e.ec2.RunInstances(t.Context(), &in)
		if apiCode(err) != "UnauthorizedOperation" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// And the direct calls the agent never makes.
	stranger := e.ec2.seed(map[string]string{"Name": "database"}, time.Now())
	if _, err := e.ec2.TerminateInstances(t.Context(), &ec2.TerminateInstancesInput{InstanceIds: []string{stranger}}); apiCode(err) != "UnauthorizedOperation" {
		t.Errorf("terminate an untagged instance: %v", err)
	}
	if _, err := e.ec2.CreateTags(t.Context(), &ec2.CreateTagsInput{Resources: []string{stranger}, Tags: []types.Tag{{Key: awssdk.String(TagFleet), Value: awssdk.String(testFleet)}}}); apiCode(err) != "UnauthorizedOperation" {
		t.Errorf("adopt an untagged instance: %v", err)
	}
	if _, err := e.ec2.DeleteTags(t.Context(), &ec2.DeleteTagsInput{Resources: []string{inst.ExternalID}, Tags: []types.Tag{{Key: awssdk.String(TagFleet)}}}); apiCode(err) != "UnauthorizedOperation" {
		t.Errorf("hide the fleet tag: %v", err)
	}
	// DeleteTags with no tags deletes every user tag, as EC2 does; the role refuses it.
	if _, err := e.ec2.DeleteTags(t.Context(), &ec2.DeleteTagsInput{Resources: []string{inst.ExternalID}}); apiCode(err) != "UnauthorizedOperation" {
		t.Errorf("delete every tag: %v", err)
	}
	e.ec2.policy = nil
	if _, err := e.ec2.DeleteTags(t.Context(), &ec2.DeleteTagsInput{Resources: []string{inst.ExternalID}}); err != nil {
		t.Fatal(err)
	}
	if tags := e.ec2.get(inst.ExternalID).Tags; len(tags) != 0 {
		t.Errorf("EC2 deletes every user tag when none is named, and %v is left", tags)
	}
	policy := kitPolicy(t)
	e.ec2.policy = &policy
	if err := e.p.Destroy(t.Context(), stranger); err == nil || !strings.Contains(err.Error(), "not a machine") {
		t.Errorf("Destroy must refuse before asking EC2: %v", err)
	}
	if e.ec2.get(stranger).State.Name != types.InstanceStateNameRunning {
		t.Error("the stranger was terminated")
	}
	if len(e.ec2.denied) == 0 {
		t.Error("nothing was denied")
	}
}

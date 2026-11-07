package aws

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// fakeEC2 is an in-memory EC2, built from the documented API: the calls the
// provider makes, their errors, DescribeInstances filters and paging, and the
// eventual consistency of a new instance. When policy is set it authorizes every
// call the way IAM does, from the same context keys, so a request the role would
// refuse fails here too.
type fakeEC2 struct {
	mu     sync.Mutex
	now    func() time.Time
	policy *policyDocument

	account, region string
	pageSize        int

	instances map[string]*types.Instance
	userData  map[string]string
	order     []string
	seq       int
	tokens    map[string]string // client token -> instance

	images        map[string]types.Image
	subnets       map[string]bool
	groups        map[string]types.SecurityGroup
	instanceTypes []types.InstanceTypeInfo

	// failures to inject
	runErr    error
	describeE error
	// capacity is how many instances may still start; negative is no limit.
	capacity int
	// unseen is how many DescribeInstances calls do not yet show a new instance.
	unseen int
	hidden map[string]int

	calls  []string
	runs   []*ec2.RunInstancesInput
	denied []string
}

func newFakeEC2(now func() time.Time) *fakeEC2 {
	return &fakeEC2{
		now: now, account: testAccount, region: testRegion, capacity: -1, pageSize: 2,
		instances: map[string]*types.Instance{}, userData: map[string]string{}, tokens: map[string]string{}, hidden: map[string]int{},
		images: map[string]types.Image{}, subnets: map[string]bool{testSubnet: true, testSubnet2: true},
		groups: map[string]types.SecurityGroup{testGroup: {GroupId: awssdk.String(testGroup)}},
	}
}

func apiError(code, msg string) error { return &smithy.GenericAPIError{Code: code, Message: msg} }

func (f *fakeEC2) arn(kind, id string) string {
	return "arn:aws:ec2:" + f.region + ":" + f.account + ":" + kind + "/" + id
}

// authorize checks the requests of one API call against the policy.
func (f *fakeEC2) authorize(call string, reqs ...request) error {
	f.calls = append(f.calls, call)
	if f.policy == nil {
		return nil
	}
	for _, r := range reqs {
		if r.context == nil {
			r.context = map[string][]string{}
		}
		r.context["aws:RequestedRegion"] = []string{f.region}
		if !f.policy.allows(r) {
			f.denied = append(f.denied, f.policy.why(r))
			return apiError("UnauthorizedOperation", "You are not authorized to perform this operation: "+r.String())
		}
	}
	return nil
}

func tagContext(prefix string, tags []types.Tag, into map[string][]string) {
	keys := []string{}
	for _, t := range tags {
		into[prefix+awssdk.ToString(t.Key)] = []string{awssdk.ToString(t.Value)}
		keys = append(keys, awssdk.ToString(t.Key))
	}
	if prefix == "aws:RequestTag/" && len(keys) > 0 {
		into["aws:TagKeys"] = keys
	}
}

func (f *fakeEC2) tagsOf(in *ec2.RunInstancesInput, kind types.ResourceType) []types.Tag {
	for _, s := range in.TagSpecifications {
		if s.ResourceType == kind {
			return s.Tags
		}
	}
	return nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, in)
	if f.runErr != nil {
		return nil, f.runErr
	}
	if token := awssdk.ToString(in.ClientToken); token != "" {
		if id, ok := f.tokens[token]; ok {
			return &ec2.RunInstancesOutput{Instances: []types.Instance{*f.instances[id]}}, nil
		}
	}
	img, ok := f.images[awssdk.ToString(in.ImageId)]
	if !ok {
		return nil, apiError("InvalidAMIID.NotFound", "The image id does not exist")
	}
	if len(in.NetworkInterfaces) != 1 || in.SubnetId != nil || len(in.SecurityGroupIds) > 0 {
		return nil, apiError("InvalidParameterCombination", "network interfaces and a subnet or groups cannot be mixed")
	}
	nic := in.NetworkInterfaces[0]
	if !f.subnets[awssdk.ToString(nic.SubnetId)] {
		return nil, apiError("InvalidSubnetID.NotFound", "no such subnet")
	}
	if len(nic.Groups) != 1 || !f.hasGroup(nic.Groups[0]) {
		return nil, apiError("InvalidGroup.NotFound", "no such group")
	}
	id := fmt.Sprintf("i-%017x", f.seq+1)
	reqs, err := f.launchRequests(in, id, img, nic)
	if err != nil {
		return nil, err
	}
	if err := f.authorize("RunInstances", reqs...); err != nil {
		return nil, err
	}
	if f.capacity == 0 {
		return nil, apiError("InsufficientInstanceCapacity", "We currently do not have sufficient capacity")
	}
	if f.capacity > 0 {
		f.capacity--
	}
	f.seq++
	inst := &types.Instance{
		InstanceId: awssdk.String(id), InstanceType: in.InstanceType, ImageId: in.ImageId, LaunchTime: awssdk.Time(f.now()),
		State: &types.InstanceState{Name: types.InstanceStateNameRunning}, Tags: slices.Clone(f.tagsOf(in, types.ResourceTypeInstance)),
		SubnetId: nic.SubnetId,
	}
	if in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot {
		inst.InstanceLifecycle = types.InstanceLifecycleTypeSpot
	}
	f.instances[id] = inst
	f.userData[id] = awssdk.ToString(in.UserData)
	f.order = append(f.order, id)
	f.hidden[id] = f.unseen
	f.tokens[awssdk.ToString(in.ClientToken)] = id
	return &ec2.RunInstancesOutput{Instances: []types.Instance{*inst}}, nil
}

func (f *fakeEC2) hasGroup(id string) bool { _, ok := f.groups[id]; return ok }

func (f *fakeEC2) instanceContext(i *types.Instance) map[string][]string {
	c := map[string][]string{}
	tagContext("ec2:ResourceTag/", i.Tags, c)
	return c
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authorize("DescribeInstances", request{action: "ec2:DescribeInstances", resource: "*"}); err != nil {
		return nil, err
	}
	if f.describeE != nil {
		return nil, f.describeE
	}
	match, err := f.matching(in)
	if err != nil {
		return nil, err
	}
	start := 0
	if in.NextToken != nil {
		start, _ = strconv.Atoi(*in.NextToken)
	}
	end := min(start+f.pageSize, len(match))
	out := &ec2.DescribeInstancesOutput{}
	for _, i := range match[start:end] {
		out.Reservations = append(out.Reservations, types.Reservation{Instances: []types.Instance{*i}})
	}
	if end < len(match) {
		out.NextToken = awssdk.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeEC2) filtered(i *types.Instance, filters []types.Filter) bool {
	for _, fl := range filters {
		name := awssdk.ToString(fl.Name)
		switch {
		case name == "instance-state-name":
			if !slices.Contains(fl.Values, string(i.State.Name)) {
				return false
			}
		case strings.HasPrefix(name, "tag:"):
			key := strings.TrimPrefix(name, "tag:")
			hit := false
			for _, t := range i.Tags {
				if awssdk.ToString(t.Key) == key && slices.Contains(fl.Values, awssdk.ToString(t.Value)) {
					hit = true
				}
			}
			if !hit {
				return false
			}
		default:
			panic("fake EC2: unsupported filter " + name)
		}
	}
	return true
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var reqs []request
	for _, id := range in.InstanceIds {
		i, ok := f.instances[id]
		if !ok {
			return nil, apiError("InvalidInstanceID.NotFound", "The instance ID '"+id+"' does not exist")
		}
		reqs = append(reqs, request{action: "ec2:TerminateInstances", resource: f.arn("instance", id), context: f.instanceContext(i)})
	}
	if err := f.authorize("TerminateInstances", reqs...); err != nil {
		return nil, err
	}
	out := &ec2.TerminateInstancesOutput{}
	for _, id := range in.InstanceIds {
		i := f.instances[id]
		i.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
		i.StateReason = &types.StateReason{Code: awssdk.String("Client.UserInitiatedShutdown")}
		out.TerminatingInstances = append(out.TerminatingInstances, types.InstanceStateChange{InstanceId: awssdk.String(id)})
	}
	return out, nil
}

// interrupt takes a spot instance back, as EC2 does.
func (f *fakeEC2) interrupt(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.instances[id]
	i.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
	i.StateReason = &types.StateReason{Code: awssdk.String("Server.SpotInstanceTermination")}
}

func (f *fakeEC2) setState(id string, s types.InstanceStateName) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id].State = &types.InstanceState{Name: s}
}

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ec2.CreateTagsOutput{}, f.changeTags("CreateTags", in.Resources, in.Tags, func(i *types.Instance) {
		for _, t := range in.Tags {
			i.Tags = slices.DeleteFunc(i.Tags, func(x types.Tag) bool { return awssdk.ToString(x.Key) == awssdk.ToString(t.Key) })
			i.Tags = append(i.Tags, t)
		}
	})
}

func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ec2.DeleteTagsOutput{}, f.changeTags("DeleteTags", in.Resources, in.Tags, func(i *types.Instance) {
		for _, t := range in.Tags {
			i.Tags = slices.DeleteFunc(i.Tags, func(x types.Tag) bool { return awssdk.ToString(x.Key) == awssdk.ToString(t.Key) })
		}
	})
}

func (f *fakeEC2) changeTags(call string, ids []string, tags []types.Tag, apply func(*types.Instance)) error {
	var reqs []request
	for _, id := range ids {
		i, ok := f.instances[id]
		if !ok {
			return apiError("InvalidInstanceID.NotFound", "no such instance")
		}
		c := f.instanceContext(i)
		var keys []string
		for _, t := range tags {
			keys = append(keys, awssdk.ToString(t.Key))
		}
		c["aws:TagKeys"] = keys
		reqs = append(reqs, request{action: "ec2:" + call, resource: f.arn("instance", id), context: c})
	}
	if err := f.authorize(call, reqs...); err != nil {
		return err
	}
	for _, id := range ids {
		apply(f.instances[id])
	}
	return nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authorize("DescribeImages", request{action: "ec2:DescribeImages", resource: "*"}); err != nil {
		return nil, err
	}
	out := &ec2.DescribeImagesOutput{}
	for _, id := range in.ImageIds {
		if img, ok := f.images[id]; ok {
			out.Images = append(out.Images, img)
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authorize("DescribeInstanceTypes", request{action: "ec2:DescribeInstanceTypes", resource: "*"}); err != nil {
		return nil, err
	}
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, t := range f.instanceTypes {
		if slices.Contains(in.InstanceTypes, t.InstanceType) {
			out.InstanceTypes = append(out.InstanceTypes, t)
			continue
		}
		for _, fl := range in.Filters {
			if awssdk.ToString(fl.Name) == "instance-type" && slices.ContainsFunc(fl.Values, func(p string) bool { return glob(p).MatchString(string(t.InstanceType)) }) {
				out.InstanceTypes = append(out.InstanceTypes, t)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authorize("DescribeSubnets", request{action: "ec2:DescribeSubnets", resource: "*"}); err != nil {
		return nil, err
	}
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range in.SubnetIds {
		if f.subnets[id] {
			out.Subnets = append(out.Subnets, types.Subnet{SubnetId: awssdk.String(id)})
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.authorize("DescribeSecurityGroups", request{action: "ec2:DescribeSecurityGroups", resource: "*"}); err != nil {
		return nil, err
	}
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range in.GroupIds {
		if g, ok := f.groups[id]; ok {
			out.SecurityGroups = append(out.SecurityGroups, g)
		}
	}
	return out, nil
}

// live returns the instances that are not terminated.
func (f *fakeEC2) live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range f.order {
		if f.instances[id].State.Name != types.InstanceStateNameTerminated {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakeEC2) get(id string) types.Instance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.instances[id]
}

func (f *fakeEC2) tag(id, key string) string {
	i := f.get(id)
	return tagOf(i, key)
}

// seed adds an instance that someone else made, tagged as given.
func (f *fakeEC2) seed(tags map[string]string, launched time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("i-%017x", f.seq)
	i := &types.Instance{InstanceId: awssdk.String(id), LaunchTime: awssdk.Time(launched), State: &types.InstanceState{Name: types.InstanceStateNameRunning}}
	for k, v := range tags {
		i.Tags = append(i.Tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	f.instances[id] = i
	f.order = append(f.order, id)
	f.hidden[id] = 0
	return id
}

// fakeSTS answers who the credentials belong to.
type fakeSTS struct {
	account string
	err     error
	calls   int
	mu      sync.Mutex
}

func (s *fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &sts.GetCallerIdentityOutput{Account: awssdk.String(s.account), Arn: awssdk.String("arn:aws:sts::" + s.account + ":assumed-role/forecrew-agent/i-0")}, nil
}

// launchRequests are the authorizations a RunInstances needs: one per resource it
// creates or reads, with the context keys IAM builds for each.
func (f *fakeEC2) launchRequests(in *ec2.RunInstancesInput, id string, img types.Image, nic types.InstanceNetworkInterfaceSpecification) ([]request, error) {
	vol := fmt.Sprintf("vol-%017x", f.seq+1)
	eni := fmt.Sprintf("eni-%017x", f.seq+1)
	subnetARN := f.arn("subnet", awssdk.ToString(nic.SubnetId))

	instCtx := map[string][]string{"ec2:InstanceType": {string(in.InstanceType)}}
	tagContext("aws:RequestTag/", f.tagsOf(in, types.ResourceTypeInstance), instCtx)
	if m := in.MetadataOptions; m != nil {
		instCtx["ec2:MetadataHttpTokens"] = []string{string(m.HttpTokens)}
		if m.HttpPutResponseHopLimit != nil {
			instCtx["ec2:MetadataHttpPutResponseHopLimit"] = []string{strconv.Itoa(int(*m.HttpPutResponseHopLimit))}
		}
	}
	if in.IamInstanceProfile != nil {
		instCtx["ec2:IamInstanceProfile"] = []string{"arn:aws:iam::" + f.account + ":instance-profile/x"}
	}
	volCtx := map[string][]string{}
	tagContext("aws:RequestTag/", f.tagsOf(in, types.ResourceTypeVolume), volCtx)
	for _, b := range in.BlockDeviceMappings {
		if b.Ebs != nil && awssdk.ToBool(b.Ebs.Encrypted) {
			volCtx["ec2:Encrypted"] = []string{"true"}
		}
	}
	eniCtx := map[string][]string{"ec2:Subnet": {subnetARN}}
	tagContext("aws:RequestTag/", f.tagsOf(in, types.ResourceTypeNetworkInterface), eniCtx)
	owner := map[string][]string{"ec2:Owner": {awssdk.ToString(img.OwnerId)}}
	reqs := []request{
		{action: "ec2:RunInstances", resource: f.arn("instance", id), context: instCtx},
		{action: "ec2:RunInstances", resource: f.arn("volume", vol), context: volCtx},
		{action: "ec2:RunInstances", resource: f.arn("network-interface", eni), context: eniCtx},
		{action: "ec2:RunInstances", resource: subnetARN},
		{action: "ec2:RunInstances", resource: f.arn("security-group", nic.Groups[0])},
		{action: "ec2:RunInstances", resource: "arn:aws:ec2:" + f.region + "::image/" + awssdk.ToString(in.ImageId), context: owner},
		{action: "ec2:RunInstances", resource: "arn:aws:ec2:" + f.region + "::snapshot/snap-0123456789abcdef0", context: owner},
	}
	kinds := map[types.ResourceType]string{
		types.ResourceTypeInstance: id, types.ResourceTypeVolume: vol, types.ResourceTypeNetworkInterface: eni,
	}
	spot := in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot
	if spot {
		sir := "sir-" + id
		kinds[types.ResourceTypeSpotInstancesRequest] = sir
		spotCtx := map[string][]string{}
		tagContext("aws:RequestTag/", f.tagsOf(in, types.ResourceTypeSpotInstancesRequest), spotCtx)
		reqs = append(reqs, request{action: "ec2:RunInstances", resource: f.arn("spot-instances-request", sir), context: spotCtx})
	}
	for _, s := range in.TagSpecifications {
		rid, ok := kinds[s.ResourceType]
		if !ok {
			return nil, apiError("InvalidParameterValue", "tags for a resource that is not created: "+string(s.ResourceType))
		}
		c := map[string][]string{"ec2:CreateAction": {"RunInstances"}}
		tagContext("aws:RequestTag/", s.Tags, c)
		reqs = append(reqs, request{action: "ec2:CreateTags", resource: f.arn(map[types.ResourceType]string{
			types.ResourceTypeInstance: "instance", types.ResourceTypeVolume: "volume",
			types.ResourceTypeNetworkInterface: "network-interface", types.ResourceTypeSpotInstancesRequest: "spot-instances-request",
		}[s.ResourceType], rid), context: c})
	}
	return reqs, nil
}

// matching returns the instances a DescribeInstances asks for.
func (f *fakeEC2) matching(in *ec2.DescribeInstancesInput) ([]*types.Instance, error) {
	var match []*types.Instance
	for _, id := range in.InstanceIds {
		if _, ok := f.instances[id]; !ok {
			return nil, apiError("InvalidInstanceID.NotFound", "The instance ID '"+id+"' does not exist")
		}
	}
	listing := len(in.InstanceIds) == 0
	for _, id := range f.order {
		i := f.instances[id]
		if (!listing && !slices.Contains(in.InstanceIds, id)) || (listing && f.hidden[id] > 0) {
			continue
		}
		if f.filtered(i, in.Filters) {
			match = append(match, i)
		}
	}
	if listing {
		for id := range f.hidden {
			f.hidden[id] = max(f.hidden[id]-1, 0)
		}
	}
	return match, nil
}

package aws

import (
	"maps"
	"testing"
)

// These tests are about the kit's role (deploy/byoc/aws): what it lets the agent
// do, and above all what it does not. The policy is read from the kit's own file,
// and the same evaluator authorizes every call of the provider tests' fake EC2.

func arn(kind, id string) string {
	return "arn:aws:ec2:" + testRegion + ":" + testAccount + ":" + kind + "/" + id
}

func ctx(kv ...string) map[string][]string {
	m := map[string][]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = append(m[kv[i]], kv[i+1])
	}
	return m
}

func TestRoleCannotTouchUntaggedResources(t *testing.T) {
	d := kitPolicy(t)
	ours := ctx("ec2:ResourceTag/forecrew:fleet", testFleet)
	cases := []struct {
		name  string
		r     request
		allow bool
	}{
		// Terminating: only what carries the fleet tag.
		{"terminate a machine of the fleet", request{"ec2:TerminateInstances", arn("instance", "i-1"), ours}, true},
		{"terminate an untagged instance", request{"ec2:TerminateInstances", arn("instance", "i-2"), nil}, false},
		{"terminate another fleet's machine", request{"ec2:TerminateInstances", arn("instance", "i-3"), ctx("ec2:ResourceTag/forecrew:fleet", "other")}, false},
		{"terminate with a look-alike tag", request{"ec2:TerminateInstances", arn("instance", "i-4"), ctx("ec2:ResourceTag/forecrew:fleet", "forecrew2")}, false},
		{"terminate in another account", request{"ec2:TerminateInstances", "arn:aws:ec2:" + testRegion + ":999999999999:instance/i-5", ours}, false},
		{"terminate in another region", request{"ec2:TerminateInstances", "arn:aws:ec2:us-east-1:" + testAccount + ":instance/i-6", ours}, false},
		// Things the agent never does.
		{"stop an instance", request{"ec2:StopInstances", arn("instance", "i-1"), ours}, false},
		{"modify an instance", request{"ec2:ModifyInstanceAttribute", arn("instance", "i-1"), ours}, false},
		{"delete a volume", request{"ec2:DeleteVolume", arn("volume", "vol-1"), ours}, false},
		{"delete a snapshot", request{"ec2:DeleteSnapshot", "arn:aws:ec2:" + testRegion + "::snapshot/snap-1", nil}, false},
		{"deregister an image", request{"ec2:DeregisterImage", "arn:aws:ec2:" + testRegion + "::image/ami-1", nil}, false},
		{"pass a role to a machine", request{"iam:PassRole", "arn:aws:iam::" + testAccount + ":role/admin", nil}, false},
		{"read a secret", request{"secretsmanager:GetSecretValue", "*", nil}, false},
		{"call IAM", request{"iam:CreateUser", "*", nil}, false},
		// Tags: of its own machines, never the fleet or machine tags.
		{"tag a machine of the fleet", request{"ec2:CreateTags", arn("instance", "i-1"), withKeys(ours, "forecrew:org", "forecrew:job")}, true},
		{"tag an untagged instance", request{"ec2:CreateTags", arn("instance", "i-2"), withKeys(nil, "forecrew:fleet")}, false},
		{"tag an untagged instance with another key", request{"ec2:CreateTags", arn("instance", "i-2"), withKeys(nil, "team")}, false},
		{"adopt a machine of another fleet", request{"ec2:CreateTags", arn("instance", "i-3"), withKeys(ctx("ec2:ResourceTag/forecrew:fleet", "other"), "forecrew:fleet")}, false},
		{"move the fleet tag of its own machine", request{"ec2:CreateTags", arn("instance", "i-1"), withKeys(ours, "forecrew:fleet")}, false},
		{"hide the fleet tag", request{"ec2:DeleteTags", arn("instance", "i-1"), withKeys(ours, "forecrew:fleet")}, false},
		{"change the machine tag", request{"ec2:CreateTags", arn("instance", "i-1"), withKeys(ours, "forecrew:machine", "team")}, false},
		{"change the price tag", request{"ec2:CreateTags", arn("instance", "i-1"), withKeys(ours, "forecrew:price")}, false},
		{"delete every tag of its own machine", request{"ec2:DeleteTags", arn("instance", "i-1"), ours}, false},
		{"remove a tag of its own machine", request{"ec2:DeleteTags", arn("instance", "i-1"), withKeys(ours, "forecrew:job")}, true},
		{"tag a volume it did not just create", request{"ec2:CreateTags", arn("volume", "vol-1"), withKeys(nil, "forecrew:fleet")}, false},
		{"tag at launch without the fleet tag", request{"ec2:CreateTags", arn("instance", "i-7"), ctx("ec2:CreateAction", "RunInstances", "aws:RequestTag/team", "x")}, false},
		{"tag at launch", request{"ec2:CreateTags", arn("instance", "i-7"), ctx("ec2:CreateAction", "RunInstances", "aws:RequestTag/forecrew:fleet", testFleet)}, true},
		{"tag at launch for another fleet", request{"ec2:CreateTags", arn("instance", "i-7"), ctx("ec2:CreateAction", "RunInstances", "aws:RequestTag/forecrew:fleet", "other")}, false},
		// Looking.
		{"describe instances", request{"ec2:DescribeInstances", "*", ctx("aws:RequestedRegion", testRegion)}, true},
		{"describe in another region", request{"ec2:DescribeInstances", "*", ctx("aws:RequestedRegion", "us-east-1")}, false},
		{"describe the account's volumes", request{"ec2:DescribeVolumes", "*", ctx("aws:RequestedRegion", testRegion)}, false},
		{"read user data", request{"ec2:DescribeInstanceAttribute", arn("instance", "i-1"), ctx("aws:RequestedRegion", testRegion)}, false},
		{"read the console", request{"ec2:GetConsoleOutput", arn("instance", "i-1"), ctx("aws:RequestedRegion", testRegion)}, false},
	}
	for _, c := range cases {
		if c.r.context != nil {
			if _, ok := c.r.context["aws:RequestedRegion"]; !ok {
				c.r.context["aws:RequestedRegion"] = []string{testRegion}
			}
		}
		if got := d.allows(c.r); got != c.allow {
			t.Errorf("%s: allowed = %v, want %v (%s)", c.name, got, c.allow, d.why(c.r))
		}
	}
}

func withKeys(base map[string][]string, keys ...string) map[string][]string {
	m := map[string][]string{}
	maps.Copy(m, base)
	m["aws:TagKeys"] = keys
	return m
}

// launch is a well-formed RunInstances, as the provider makes it, as IAM
// requests: one per resource the call touches.
func launch() []request {
	subnet := arn("subnet", testSubnet)
	tags := func(kv ...string) map[string][]string {
		return ctx(append([]string{"aws:RequestTag/forecrew:fleet", testFleet}, kv...)...)
	}
	owner := ctx("ec2:Owner", testAccount)
	return []request{
		{"ec2:RunInstances", arn("instance", "i-1"), tags("ec2:InstanceType", "c7i.large", "ec2:Tenancy", "default", "ec2:MetadataHttpTokens", "required", "ec2:MetadataHttpPutResponseHopLimit", "1")},
		{"ec2:RunInstances", arn("volume", "vol-1"), tags("ec2:Encrypted", "true", "ec2:VolumeType", "gp3", "ec2:VolumeSize", "30")},
		{"ec2:RunInstances", arn("network-interface", "eni-1"), tags("ec2:Subnet", subnet)},
		{"ec2:RunInstances", subnet, nil},
		{"ec2:RunInstances", arn("security-group", testGroup), nil},
		{"ec2:RunInstances", "arn:aws:ec2:" + testRegion + "::image/ami-1", owner},
		{"ec2:RunInstances", "arn:aws:ec2:" + testRegion + "::snapshot/snap-1", owner},
	}
}

func TestRoleLetsTheProviderLaunch(t *testing.T) {
	d := kitPolicy(t)
	for _, r := range launch() {
		if r.context == nil {
			r.context = map[string][]string{}
		}
		r.context["aws:RequestedRegion"] = []string{testRegion}
		if !d.allows(r) {
			t.Errorf("a launch is refused: %s", d.why(r))
		}
	}
}

func TestRoleRefusesABadLaunch(t *testing.T) {
	d := kitPolicy(t)
	reasons := map[string]func(rs []request){
		"no fleet tag on the instance":  func(rs []request) { delete(rs[0].context, "aws:RequestTag/forecrew:fleet") },
		"no fleet tag on the volume":    func(rs []request) { delete(rs[1].context, "aws:RequestTag/forecrew:fleet") },
		"no fleet tag on the interface": func(rs []request) { delete(rs[2].context, "aws:RequestTag/forecrew:fleet") },
		"another fleet's tag":           func(rs []request) { rs[0].context["aws:RequestTag/forecrew:fleet"] = []string{"other"} },
		"an instance type not listed":   func(rs []request) { rs[0].context["ec2:InstanceType"] = []string{"p5.48xlarge"} },
		"a type that only looks listed": func(rs []request) { rs[0].context["ec2:InstanceType"] = []string{"c7ix.large"} },
		"IMDSv1":                        func(rs []request) { rs[0].context["ec2:MetadataHttpTokens"] = []string{"optional"} },
		"two metadata hops":             func(rs []request) { rs[0].context["ec2:MetadataHttpPutResponseHopLimit"] = []string{"2"} },
		"an instance profile": func(rs []request) {
			rs[0].context["ec2:IamInstanceProfile"] = []string{"arn:aws:iam::1:instance-profile/admin"}
		},
		"a dedicated host":      func(rs []request) { rs[0].context["ec2:Tenancy"] = []string{"host"} },
		"an io2 volume":         func(rs []request) { rs[1].context["ec2:VolumeType"] = []string{"io2"} },
		"a huge volume":         func(rs []request) { rs[1].context["ec2:VolumeSize"] = []string{"5000"} },
		"an unencrypted volume": func(rs []request) { delete(rs[1].context, "ec2:Encrypted") },
		"a subnet that is not listed": func(rs []request) {
			rs[2].context["ec2:Subnet"] = []string{arn("subnet", "subnet-0000000000000000f")}
			rs[3].resource = arn("subnet", "subnet-0000000000000000f")
		},
		"a security group that is not it": func(rs []request) { rs[4].resource = arn("security-group", "sg-0000000000000000f") },
		"an image of another account":     func(rs []request) { rs[5].context["ec2:Owner"] = []string{"999999999999"} },
		"a public image":                  func(rs []request) { rs[5].context["ec2:Owner"] = []string{"amazon"} },
		"a snapshot of another account":   func(rs []request) { rs[6].context["ec2:Owner"] = []string{"999999999999"} },
	}
	for why, mutate := range reasons {
		rs := launch()
		for i := range rs {
			if rs[i].context == nil {
				rs[i].context = map[string][]string{}
			}
			rs[i].context["aws:RequestedRegion"] = []string{testRegion}
			// each request gets its own copy of the maps
			cp := map[string][]string{}
			for k, v := range rs[i].context {
				cp[k] = append([]string{}, v...)
			}
			rs[i].context = cp
		}
		mutate(rs)
		denied := false
		for _, r := range rs {
			denied = denied || !d.allows(r)
		}
		if !denied {
			t.Errorf("the role allows a launch with %s", why)
		}
	}
}

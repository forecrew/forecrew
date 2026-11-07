package aws

import (
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

func tagMap(tags []types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	return m
}

func TestCreateTagsEverythingAndClosesTheMachine(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	inst := e.create(req)

	if inst.MachineID != req.MachineID || !inst.Running || !strings.HasPrefix(inst.ExternalID, "i-") {
		t.Fatalf("instance = %+v", inst)
	}
	if len(e.ec2.runs) != 1 {
		t.Fatalf("%d RunInstances calls", len(e.ec2.runs))
	}
	in := e.ec2.runs[0]
	for _, kind := range []types.ResourceType{types.ResourceTypeInstance, types.ResourceTypeVolume, types.ResourceTypeNetworkInterface} {
		tags := tagMap(e.ec2.tagsOf(in, kind))
		for k, v := range map[string]string{TagFleet: testFleet, fleet.TagMachine: req.MachineID.String(), fleet.TagOrg: testOrg, "team": "platform"} {
			if tags[k] != v {
				t.Errorf("%s tags: %s = %q, want %q", kind, k, tags[k], v)
			}
		}
	}
	if m := in.MetadataOptions; m == nil || m.HttpTokens != types.HttpTokensStateRequired || awssdk.ToInt32(m.HttpPutResponseHopLimit) != 1 {
		t.Errorf("metadata options = %+v", m)
	}
	if in.IamInstanceProfile != nil {
		t.Error("a machine must not hold an instance profile")
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
		t.Errorf("shutdown behavior = %s", in.InstanceInitiatedShutdownBehavior)
	}
	if b := in.BlockDeviceMappings[0]; awssdk.ToInt32(b.Ebs.VolumeSize) != 30 || !awssdk.ToBool(b.Ebs.Encrypted) || !awssdk.ToBool(b.Ebs.DeleteOnTermination) || awssdk.ToString(b.DeviceName) != "/dev/xvda" {
		t.Errorf("block device = %+v", b)
	}
	if n := in.NetworkInterfaces[0]; awssdk.ToString(n.SubnetId) != testSubnet || !awssdk.ToBool(n.DeleteOnTermination) || awssdk.ToBool(n.AssociatePublicIpAddress) {
		t.Errorf("network interface = %+v", n)
	}
	if in.InstanceMarketOptions != nil || in.CreditSpecification != nil {
		t.Error("an on-demand c7i machine has neither market options nor credits")
	}
	raw, err := base64.StdEncoding.DecodeString(awssdk.ToString(in.UserData))
	must(t, err)
	if !strings.Contains(string(raw), "FORECREW_BOOTSTRAP_TOKEN="+testToken) || !strings.Contains(string(raw), "shutdown, -P, '+120'") {
		t.Errorf("user data = %s", raw)
	}
	if got := e.ec2.tag(inst.ExternalID, TagPrice); got != "0.01" {
		t.Errorf("price tag = %q", got)
	}
	if strings.Contains(e.logs.String(), testToken) {
		t.Error("the bootstrap token is in the logs")
	}
}

func TestCreateRetryReplacesTheMachine(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	first := e.create(req)
	req.Bootstrap = &fleet.Bootstrap{ServerURL: "https://forecrew.example.com", Token: "fcb_second"}
	second := e.create(req)
	if first.ExternalID == second.ExternalID {
		t.Fatal("a retry must boot a new machine with its new token")
	}
	if live := e.ec2.live(); !slices.Equal(live, []string{second.ExternalID}) {
		t.Fatalf("live = %v", live)
	}
	if t1, t2 := awssdk.ToString(e.ec2.runs[0].ClientToken), awssdk.ToString(e.ec2.runs[1].ClientToken); t1 == t2 || t1 == "" {
		t.Errorf("client tokens %q and %q must differ, so EC2 does not hand back the first machine", t1, t2)
	}
}

func TestCreateSpot(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Shape.ProviderConfig = map[string]any{"instance_type": "m7g.large", "spot": true}
	e.addAMI(testAMI, testAccount, e.img.Digest)
	e.create(req)
	in := e.ec2.runs[0]
	if o := in.InstanceMarketOptions; o == nil || o.MarketType != types.MarketTypeSpot || o.SpotOptions.InstanceInterruptionBehavior != types.InstanceInterruptionBehaviorTerminate || o.SpotOptions.SpotInstanceType != types.SpotInstanceTypeOneTime {
		t.Fatalf("market options = %+v", o)
	}
	if tags := tagMap(e.ec2.tagsOf(in, types.ResourceTypeSpotInstancesRequest)); tags[TagFleet] != testFleet {
		t.Errorf("spot request tags = %v", tags)
	}
}

func TestBurstableTypesGetStandardCredits(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.InstanceTypes = []string{"t3.*"} })
	req := e.request()
	req.Shape.ProviderConfig = map[string]any{"instance_type": "t3.large"}
	// The policy of the test account lists c7i and m7g only: this checks the request, not IAM.
	e.ec2.policy = nil
	e.create(req)
	if c := e.ec2.runs[0].CreditSpecification; c == nil || awssdk.ToString(c.CpuCredits) != "standard" {
		t.Fatalf("credit specification = %+v", c)
	}
}

func TestSpotInterruptionIsGoneFromTheList(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Shape.ProviderConfig = map[string]any{"instance_type": "c7i.large", "spot": true}
	inst := e.create(req)
	e.clock.advance(5 * time.Minute) // past what a new instance may be missing for
	if list, err := e.p.List(t.Context()); err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	e.ec2.interrupt(inst.ExternalID)
	list, err := e.p.List(t.Context())
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("an interrupted machine is still listed: %+v", list)
	}
	// forecrewd fails what is missing from the list, and its job is requeued
	// through its lease; the agent says why in its log.
	if logs := e.logs.String(); !strings.Contains(logs, "Server.SpotInstanceTermination") {
		t.Errorf("the log does not say the machine was taken back:\n%s", logs)
	}
}

func apiCode(err error) string {
	if api, ok := errors.AsType[smithy.APIError](err); ok {
		return api.ErrorCode()
	}
	return ""
}

func TestNoRoomIsErrCapacity(t *testing.T) {
	for _, code := range []string{"InsufficientInstanceCapacity", "MaxSpotInstanceCountExceeded", "VcpuLimitExceeded"} {
		e := newEnv(t, nil)
		e.ec2.runErr = apiError(code, "no room")
		_, err := e.p.Create(t.Context(), e.request())
		if !errors.Is(err, fleet.ErrCapacity) {
			t.Errorf("%s: err = %v", code, err)
		}
	}
	e := newEnv(t, nil)
	e.ec2.runErr = apiError("InvalidParameterValue", "bad")
	_, err := e.p.Create(t.Context(), e.request())
	if err == nil || errors.Is(err, fleet.ErrCapacity) || apiCode(err) != "InvalidParameterValue" {
		t.Errorf("a refused request is not a lack of room: %v", err)
	}
}

func TestMaxMachines(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 2 })
	a := e.create(e.request())
	e.create(e.request())
	_, err := e.p.Create(t.Context(), e.request())
	if !errors.Is(err, fleet.ErrCapacity) || !strings.Contains(err.Error(), "limit of 2 machines") {
		t.Fatalf("err = %v", err)
	}
	must(t, e.p.Destroy(t.Context(), a.ExternalID))
	e.create(e.request())
}

func TestBudgetStopsNewMachinesAndAlerts(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MonthlyBudget = 10 })
	req := e.request()
	req.Shape.PricePerMinute = 1
	inst := e.create(req)

	check := func() domain.ProviderCheck {
		c, err := e.p.Capabilities(t.Context())
		must(t, err)
		for _, k := range c.Checks {
			if k.Name == "budget" {
				return k
			}
		}
		t.Fatal("no budget check")
		return domain.ProviderCheck{}
	}
	if k := check(); k.Status != statusOK {
		t.Fatalf("at the start: %+v", k)
	}
	e.clock.advance(8*time.Minute + 30*time.Second)
	if k := check(); k.Status != statusWarn || !strings.Contains(k.Detail, "85%") {
		t.Fatalf("at 85%%: %+v", k)
	}
	e.clock.advance(2 * time.Minute)
	if k := check(); k.Status != statusFail || !strings.Contains(k.Detail, "no new machines") {
		t.Fatalf("over budget: %+v", k)
	}
	_, err := e.p.Create(t.Context(), e.request())
	if !errors.Is(err, fleet.ErrCapacity) || !strings.Contains(err.Error(), "monthly budget") {
		t.Fatalf("a new machine over budget: %v", err)
	}
	// What the machine cost stays counted after it is gone.
	must(t, e.p.Destroy(t.Context(), inst.ExternalID))
	if got := e.p.ledger.spent(); got < 10 || got > 11 {
		t.Fatalf("spent after destroy = %v", got)
	}
	if k := check(); k.Status != statusFail {
		t.Fatalf("still over budget: %+v", k)
	}
}

func TestDestroy(t *testing.T) {
	e := newEnv(t, nil)
	inst := e.create(e.request())
	must(t, e.p.Destroy(t.Context(), inst.ExternalID))
	if len(e.ec2.live()) != 0 {
		t.Fatal("the instance is still there")
	}
	// Destroying what is gone succeeds, whether EC2 still lists it as terminated
	// or has forgotten it.
	must(t, e.p.Destroy(t.Context(), inst.ExternalID))
	must(t, e.p.Destroy(t.Context(), "i-00000000000000000"))
	if err := e.p.Destroy(t.Context(), "../etc"); err == nil {
		t.Error("a path was taken for an instance ID")
	}
}

func TestDestroyRefusesWhatIsNotOurs(t *testing.T) {
	e := newEnv(t, nil)
	stranger := e.ec2.seed(map[string]string{"Name": "database"}, time.Now())
	other := e.ec2.seed(map[string]string{TagFleet: "someone-else", fleet.TagMachine: uuid.New().String()}, time.Now())
	for _, id := range []string{stranger, other} {
		if err := e.p.Destroy(t.Context(), id); err == nil || !strings.Contains(err.Error(), "not a machine of this agent") {
			t.Errorf("Destroy(%s) = %v", id, err)
		}
		if e.ec2.get(id).State.Name != types.InstanceStateNameRunning {
			t.Errorf("%s was touched", id)
		}
	}
	for _, call := range e.ec2.calls {
		if call == "TerminateInstances" {
			t.Error("the agent asked EC2 to terminate an instance that is not its own")
		}
	}
}

func TestUpdate(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	inst := e.create(req)
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	must(t, e.p.Update(t.Context(), inst.ExternalID, fleet.UpdateRequest{
		Tags:     map[string]string{fleet.TagMachine: "ignored", TagFleet: "ignored", fleet.TagOrg: "other-org", fleet.TagJob: "T-1", "team": "platform"},
		Deadline: deadline,
	}))
	tags := tagMap(e.ec2.get(inst.ExternalID).Tags)
	for k, v := range map[string]string{
		fleet.TagMachine: req.MachineID.String(), TagFleet: testFleet, fleet.TagOrg: "other-org", fleet.TagJob: "T-1", "team": "platform", TagDeadline: deadline.Format(time.RFC3339),
	} {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
	// Each field is the whole new value: what is not asked for any more goes,
	// and applying the same values again succeeds.
	again := fleet.UpdateRequest{Tags: map[string]string{fleet.TagOrg: "other-org"}}
	must(t, e.p.Update(t.Context(), inst.ExternalID, again))
	must(t, e.p.Update(t.Context(), inst.ExternalID, again))
	tags = tagMap(e.ec2.get(inst.ExternalID).Tags)
	for _, k := range []string{fleet.TagJob, "team", TagDeadline} {
		if _, ok := tags[k]; ok {
			t.Errorf("tag %s should be gone", k)
		}
	}
	for _, k := range []string{fleet.TagMachine, TagFleet, TagPrice, "Name"} {
		if _, ok := tags[k]; !ok {
			t.Errorf("tag %s was removed", k)
		}
	}
	if err := e.p.Update(t.Context(), inst.ExternalID, fleet.UpdateRequest{NetworkAllow: []string{"10.0.0.0/8"}}); err == nil {
		t.Error("network_allow was accepted")
	}
	must(t, e.p.Destroy(t.Context(), inst.ExternalID))
	if err := e.p.Update(t.Context(), inst.ExternalID, again); err == nil || !strings.Contains(err.Error(), "no machine") {
		t.Errorf("update of a terminated machine = %v", err)
	}
	if err := e.p.Update(t.Context(), "nope", again); err == nil {
		t.Error("a bad ID was accepted")
	}
}

func TestDeadlineTerminatesTheMachine(t *testing.T) {
	e := newEnv(t, nil)
	req := e.request()
	req.Deadline = time.Now().Add(60 * time.Millisecond)
	inst := e.create(req)
	eventually(t, "the deadline to end the machine", func() bool { return len(e.ec2.live()) == 0 })
	if e.ec2.get(inst.ExternalID).State.Name != types.InstanceStateNameTerminated {
		t.Fatal("not terminated")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestListFindsOnlyOurs(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.pageSize = 1
	a, b := e.create(e.request()), e.create(e.request())
	e.ec2.seed(map[string]string{"Name": "database"}, time.Now())
	e.ec2.seed(map[string]string{TagFleet: "someone-else", fleet.TagMachine: uuid.New().String()}, time.Now())
	e.ec2.seed(map[string]string{TagFleet: testFleet}, time.Now()) // ours, from before machine IDs: listed with none

	list, err := e.p.List(t.Context())
	must(t, err)
	if len(list) != 3 {
		t.Fatalf("list = %+v", list)
	}
	var ids []string
	for _, i := range list {
		ids = append(ids, i.ExternalID)
		if _, ok := i.Tags["Name"]; ok {
			t.Error("Name is not a tag of Forecrew's")
		}
	}
	if !slices.Contains(ids, a.ExternalID) || !slices.Contains(ids, b.ExternalID) {
		t.Fatalf("ids = %v", ids)
	}
	for _, i := range list {
		if i.ExternalID == a.ExternalID && (i.MachineID == uuid.Nil() || i.CreatedAt.IsZero() || !i.Running || i.Tags[fleet.TagOrg] != testOrg) {
			t.Errorf("instance = %+v", i)
		}
	}
}

func TestListCleansWhatLeaked(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.unseen = 0
	now := e.clock.now()
	tag := func(extra map[string]string) map[string]string {
		m := map[string]string{TagFleet: testFleet, fleet.TagMachine: uuid.New().String()}
		maps.Copy(m, extra)
		return m
	}
	healthy := e.ec2.seed(tag(nil), now.Add(-time.Hour))
	old := e.ec2.seed(tag(nil), now.Add(-3*time.Hour))
	late := e.ec2.seed(tag(map[string]string{TagDeadline: now.Add(-10 * time.Minute).Format(time.RFC3339)}), now.Add(-time.Hour))
	grace := e.ec2.seed(tag(map[string]string{TagDeadline: now.Add(-30 * time.Second).Format(time.RFC3339)}), now.Add(-time.Hour))
	stopped := e.ec2.seed(tag(nil), now.Add(-time.Hour))
	e.ec2.setState(stopped, types.InstanceStateNameStopped)
	stranger := e.ec2.seed(map[string]string{"Name": "someone's old server"}, now.Add(-100*time.Hour))

	list, err := e.p.List(t.Context())
	must(t, err)
	var got []string
	for _, i := range list {
		got = append(got, i.ExternalID)
	}
	slices.Sort(got)
	want := []string{healthy, grace}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for id, state := range map[string]types.InstanceStateName{
		old: types.InstanceStateNameTerminated, late: types.InstanceStateNameTerminated, stopped: types.InstanceStateNameTerminated,
		stranger: types.InstanceStateNameRunning, healthy: types.InstanceStateNameRunning, grace: types.InstanceStateNameRunning,
	} {
		if s := e.ec2.get(id).State.Name; s != state {
			t.Errorf("%s is %s, want %s", id, s, state)
		}
	}
}

func TestListStartsDeadlineTimersAfterARestart(t *testing.T) {
	e := newEnv(t, nil)
	soon := time.Now().Add(80 * time.Millisecond).UTC()
	id := e.ec2.seed(map[string]string{TagFleet: testFleet, fleet.TagMachine: uuid.New().String(), TagDeadline: soon.Format(time.RFC3339Nano)}, time.Now())
	if _, err := e.p.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	// RFC 3339 without fractions is what Create writes; with them it parses too.
	eventually(t, "the deadline to end a machine found by List", func() bool { return e.ec2.get(id).State.Name == types.InstanceStateNameTerminated })
}

func TestANewInstanceMissingFromEC2IsStillListed(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.unseen = 3 // DescribeInstances is eventually consistent
	inst := e.create(e.request())
	list, err := e.p.List(t.Context())
	must(t, err)
	if len(list) != 1 || list[0].ExternalID != inst.ExternalID {
		t.Fatalf("list = %+v: forecrewd would fail the machine as gone", list)
	}
	e.clock.advance(consistencyWindow + time.Second)
	e.ec2.mu.Lock()
	e.ec2.hidden[inst.ExternalID] = 100
	e.ec2.mu.Unlock()
	if list, err = e.p.List(t.Context()); err != nil || len(list) != 0 {
		t.Fatalf("after the window: %+v, %v", list, err)
	}
}

func TestListFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.ec2.describeE = apiError("RequestLimitExceeded", "slow down")
	if _, err := e.p.List(t.Context()); err == nil {
		t.Fatal("a failed list was taken for an empty one")
	}
	if _, err := e.p.Create(t.Context(), e.request()); err == nil {
		t.Fatal("Create went on without knowing what exists")
	}
}

func TestCreateRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(e *env, r *fleet.CreateRequest)
		want string
	}{
		"windows":       {func(_ *env, r *fleet.CreateRequest) { r.Shape.OS = domain.OSWindows }, "Linux machines"},
		"macos":         {func(_ *env, r *fleet.CreateRequest) { r.Shape.OS = domain.OSMacOS }, "tart"},
		"network allow": {func(_ *env, r *fleet.CreateRequest) { r.NetworkAllow = []string{"10.1.0.0/16"} }, "network_allow"},
		"no bootstrap":  {func(_ *env, r *fleet.CreateRequest) { r.Bootstrap = nil }, "bootstrap"},
		"huge disk":     {func(_ *env, r *fleet.CreateRequest) { r.Shape.DiskGB = 20000 }, "disk_gb"},
		"unknown key":   {func(_ *env, r *fleet.CreateRequest) { r.Shape.ProviderConfig["ebs_optimised"] = true }, "unknown key"},
		"other type":    {func(_ *env, r *fleet.CreateRequest) { r.Shape.ProviderConfig["instance_type"] = "p5.48xlarge" }, "instance_types"},
		"other subnet":  {func(_ *env, r *fleet.CreateRequest) { r.Shape.ProviderConfig["subnet"] = "subnet-0123456789abcdef0" }, "subnets"},
		"other group": {func(_ *env, r *fleet.CreateRequest) {
			r.Shape.ProviderConfig["security_group"] = "sg-0123456789abcdef0"
		}, "security group"},
		"other region":     {func(_ *env, r *fleet.CreateRequest) { r.Shape.ProviderConfig["region"] = "us-east-1" }, "region"},
		"bad token":        {func(_ *env, r *fleet.CreateRequest) { r.Bootstrap.Token = "fcb_x\nFORECREW_X=y" }, "token"},
		"bad url":          {func(_ *env, r *fleet.CreateRequest) { r.Bootstrap.ServerURL = "https://x.example/a b" }, "server URL"},
		"too many tags":    {func(_ *env, r *fleet.CreateRequest) { r.Tags = manyTags(60) }, "EC2 allows"},
		"reserved tag key": {func(_ *env, r *fleet.CreateRequest) { r.Tags["aws:cloudformation:stack-id"] = "x" }, "aws:"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			req := e.request()
			req.Shape.ProviderConfig = map[string]any{"instance_type": "c7i.large", "subnet": testSubnet}
			tc.edit(e, &req)
			_, err := e.p.Create(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(e.ec2.runs) != 0 {
				t.Fatal("an instance was requested")
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("the token is in the error")
			}
		})
	}
}

func manyTags(n int) map[string]string {
	m := map[string]string{}
	for i := range n {
		m["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	return m
}

func TestImageChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(e *env)
		want string
	}{
		"not an AMI": {func(e *env) { e.img.Format = domain.ImageRootfs }, "boots an AMI"},
		"wrong arch": {func(e *env) { e.img.Arch = domain.ArchARM64; e.img.Signature = signed(e) }, "amd64"},
		"unsigned":   {func(e *env) { e.img.Signature = "" }, "signature"},
		"signed by a stranger": {func(e *env) {
			_, other, _ := imagesKey()
			e.img.Signature = signWith(other, e.img)
		}, "signature"},
		"tampered":     {func(e *env) { e.img.Digest = "sha256:" + strings.Repeat("cd", 32) }, "signature"},
		"bad uri":      {func(e *env) { e.img.URI = "https://example.com/x.img"; e.img.Signature = signed(e) }, "ami:<region>/<ami id>"},
		"other region": {func(e *env) { e.img.URI = "ami:us-east-1/" + testAMI; e.img.Signature = signed(e) }, "us-east-1"},
		"unknown ami":  {func(e *env) { e.img.URI = "ami:" + testRegion + "/ami-00000000000000000"; e.img.Signature = signed(e) }, "cannot see"},
		"not ours":     {func(e *env) { e.addAMI(testAMI, "999999999999", e.img.Digest) }, "does not trust"},
		"no tag":       {func(e *env) { e.addAMI(testAMI, testAccount, "") }, "forecrew:image-digest"},
		"wrong tag":    {func(e *env) { e.addAMI(testAMI, testAccount, "sha256:"+strings.Repeat("00", 32)) }, "forecrew:image-digest"},
		"not available": {func(e *env) {
			a := e.ec2.images[testAMI]
			a.State = types.ImageStatePending
			e.ec2.images[testAMI] = a
		}, "available"},
		"ami arch differs": {func(e *env) {
			a := e.ec2.images[testAMI]
			a.Architecture = types.ArchitectureValuesArm64
			e.ec2.images[testAMI] = a
		}, "arm64"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			tc.edit(e)
			req := e.request()
			req.Image = e.img
			_, err := e.p.Create(t.Context(), req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(e.ec2.runs) != 0 {
				t.Fatal("an instance was requested")
			}
		})
	}
}

func TestATrustedOwnersAMIDoesNotNeedTheTag(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.TrustedAMIOwners = []string{"999999999999"} })
	e.addAMI(testAMI, "999999999999", "")
	// The policy of the kit allows only this account's images: widening
	// trusted_ami_owners means widening the role, which the fake enforces too.
	if _, err := e.p.Create(t.Context(), e.request()); apiCode(err) != "UnauthorizedOperation" {
		t.Fatalf("the role must refuse an image of another account: %v", err)
	}
	e.ec2.policy = nil
	e.create(e.request())
}

func TestIdentityFailureIsNotCached(t *testing.T) {
	e := newEnv(t, nil)
	e.sts.err = errors.New("no credentials")
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || !strings.Contains(err.Error(), "who am I") {
		t.Fatalf("err = %v", err)
	}
	e.sts.err = nil
	e.create(e.request())
	e.create(e.request())
	if e.sts.calls != 2 {
		t.Errorf("%d identity calls: a good answer is kept, a bad one is not", e.sts.calls)
	}
}

func TestConcurrentCreatesRespectTheLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxMachines = 2 })
	errs := make(chan error, 6)
	for range 6 {
		go func() {
			_, err := e.p.Create(t.Context(), e.request())
			errs <- err
		}()
	}
	made := 0
	for range 6 {
		if err := <-errs; err == nil {
			made++
		} else if !errors.Is(err, fleet.ErrCapacity) {
			t.Error(err)
		}
	}
	if made != 2 || len(e.ec2.live()) != 2 {
		t.Fatalf("made %d, live %d", made, len(e.ec2.live()))
	}
}

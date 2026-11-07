package aws

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/forecrew/forecrew/internal/fleet"
)

// The provider talks to the real AWS SDK client here, over HTTP, to a server that
// answers in EC2's query protocol (form-encoded requests, XML answers), so what the
// SDK puts on the wire and how it reads EC2's answers and errors is exercised, not
// only the provider's own logic. Nothing leaves the machine.
type wireEC2 struct {
	mu       sync.Mutex
	requests []url.Values
	image    string
	failRun  string // an error code RunInstances answers
}

const ec2NS = "http://ec2.amazonaws.com/doc/2016-11-15/"

func (w *wireEC2) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests = append(w.requests, r.PostForm)
	rw.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	switch action := r.PostForm.Get("Action"); action {
	case "DescribeImages":
		fmt.Fprintf(rw, `<DescribeImagesResponse xmlns=%q><requestId>r</requestId><imagesSet><item>
<imageId>%s</imageId><imageOwnerId>%s</imageOwnerId><imageState>available</imageState><architecture>x86_64</architecture>
<rootDeviceType>ebs</rootDeviceType><rootDeviceName>/dev/xvda</rootDeviceName><virtualizationType>hvm</virtualizationType>
<tagSet><item><key>%s</key><value>sha256:%s</value></item></tagSet></item></imagesSet></DescribeImagesResponse>`,
			ec2NS, w.image, testAccount, TagImageDigest, strings.Repeat("ab", 32))
	case "DescribeInstances":
		w.describe(rw, r.PostForm)
	case "RunInstances":
		if w.failRun != "" {
			rw.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(rw, `<Response><Errors><Error><Code>%s</Code><Message>no room</Message></Error></Errors><RequestID>r</RequestID></Response>`, w.failRun)
			return
		}
		fmt.Fprintf(rw, `<RunInstancesResponse xmlns=%q><requestId>r</requestId><reservationId>r-1</reservationId><ownerId>%s</ownerId>
<instancesSet><item><instanceId>i-0123456789abcdef0</instanceId><imageId>%s</imageId><instanceState><code>0</code><name>pending</name></instanceState>
<instanceType>c7i.large</instanceType><launchTime>2026-11-07T12:00:00.000Z</launchTime>%s</item></instancesSet></RunInstancesResponse>`,
			ec2NS, testAccount, w.image, instanceTags(r.PostForm))
	case "TerminateInstances":
		fmt.Fprintf(rw, `<TerminateInstancesResponse xmlns=%q><requestId>r</requestId><instancesSet><item><instanceId>%s</instanceId>
<currentState><code>32</code><name>shutting-down</name></currentState><previousState><code>16</code><name>running</name></previousState></item></instancesSet></TerminateInstancesResponse>`,
			ec2NS, r.PostForm.Get("InstanceId.1"))
	default:
		http.Error(rw, "unsupported action "+action, http.StatusBadRequest)
	}
}

// instanceTags is the tagSet of the instance a RunInstances made.
func instanceTags(f url.Values) string {
	var b strings.Builder
	b.WriteString("<tagSet>")
	for i := 1; ; i++ {
		prefix := fmt.Sprintf("TagSpecification.%d.", i)
		if f.Get(prefix+"ResourceType") != "instance" {
			if f.Get(prefix+"ResourceType") == "" {
				break
			}
			continue
		}
		for j := 1; f.Get(fmt.Sprintf("%sTag.%d.Key", prefix, j)) != ""; j++ {
			fmt.Fprintf(&b, "<item><key>%s</key><value>%s</value></item>", f.Get(fmt.Sprintf("%sTag.%d.Key", prefix, j)), f.Get(fmt.Sprintf("%sTag.%d.Value", prefix, j)))
		}
	}
	b.WriteString("</tagSet>")
	return b.String()
}

// describe answers DescribeInstances: an unknown ID, or a list that pages.
func (w *wireEC2) describe(rw http.ResponseWriter, f url.Values) {
	if id := f.Get("InstanceId.1"); id != "" {
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>The instance ID '%s' does not exist</Message></Error></Errors><RequestID>r</RequestID></Response>`, id)
		return
	}
	item := func(id string) string {
		return fmt.Sprintf(`<item><reservationId>r-%s</reservationId><instancesSet><item><instanceId>%s</instanceId><instanceState><code>16</code><name>running</name></instanceState>
<launchTime>%s</launchTime><tagSet><item><key>forecrew:fleet</key><value>forecrew</value></item><item><key>forecrew:machine</key><value>%s</value></item></tagSet></item></instancesSet></item>`,
			id, id, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001")
	}
	if f.Get("NextToken") == "" {
		fmt.Fprintf(rw, `<DescribeInstancesResponse xmlns=%q><requestId>r</requestId><reservationSet>%s</reservationSet><nextToken>page2</nextToken></DescribeInstancesResponse>`, ec2NS, item("i-0000000000000001"))
		return
	}
	fmt.Fprintf(rw, `<DescribeInstancesResponse xmlns=%q><requestId>r</requestId><reservationSet>%s</reservationSet></DescribeInstancesResponse>`, ec2NS, item("i-0000000000000002"))
}

func (w *wireEC2) calls(action string) []url.Values {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []url.Values
	for _, r := range w.requests {
		if r.Get("Action") == action {
			out = append(out, r)
		}
	}
	return out
}

func wireEnv(t *testing.T) (*env, *wireEC2) {
	t.Helper()
	e := newEnv(t, nil)
	w := &wireEC2{image: testAMI}
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	client := ec2.New(ec2.Options{
		Region: testRegion, BaseEndpoint: awssdk.String(srv.URL), Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		RetryMaxAttempts: 1,
	})
	var err error
	e.p, err = newWith(e.p.cfg, e.p.log, client, &fakeSTS{account: testAccount}, e.clock.now)
	must(t, err)
	return e, w
}

func TestWireCreateAndDestroy(t *testing.T) {
	e, w := wireEnv(t)
	req := e.request()
	req.Shape.ProviderConfig = map[string]any{"instance_type": "c7i.large", "spot": true}
	req.Deadline = time.Now().Add(time.Hour)
	inst, err := e.p.Create(t.Context(), req)
	must(t, err)
	if inst.ExternalID != "i-0123456789abcdef0" || inst.MachineID != req.MachineID || inst.Tags[TagFleet] != testFleet || !inst.Running {
		t.Fatalf("instance = %+v", inst)
	}

	runs := w.calls("RunInstances")
	if len(runs) != 1 {
		t.Fatalf("%d RunInstances", len(runs))
	}
	f := runs[0]
	for k, want := range map[string]string{
		"ImageId": testAMI, "InstanceType": "c7i.large", "MinCount": "1", "MaxCount": "1",
		"NetworkInterface.1.SubnetId": testSubnet, "NetworkInterface.1.SecurityGroupId.1": testGroup,
		"NetworkInterface.1.DeleteOnTermination": "true", "NetworkInterface.1.AssociatePublicIpAddress": "false",
		"BlockDeviceMapping.1.DeviceName": "/dev/xvda", "BlockDeviceMapping.1.Ebs.VolumeSize": "30", "BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
		"BlockDeviceMapping.1.Ebs.Encrypted": "true", "BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
		"MetadataOptions.HttpTokens": "required", "MetadataOptions.HttpPutResponseHopLimit": "1", "MetadataOptions.HttpEndpoint": "enabled",
		"InstanceInitiatedShutdownBehavior": "terminate",
		"InstanceMarketOptions.MarketType":  "spot", "InstanceMarketOptions.SpotOptions.SpotInstanceType": "one-time",
		"InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior": "terminate",
	} {
		if got := f.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(f.Get("ClientToken"), "fc-") || f.Get("UserData") == "" {
		t.Errorf("client token %q, user data %q", f.Get("ClientToken"), f.Get("UserData"))
	}
	for k := range f {
		if strings.HasPrefix(k, "IamInstanceProfile") {
			t.Errorf("a machine asks for an instance profile: %s", k)
		}
	}
	checkTagSpecifications(t, f)
	if f.Get("Authorization") != "" {
		t.Error("credentials in the form")
	}

	// Destroy of an instance EC2 does not know succeeds, with the SDK's real error.
	must(t, e.p.Destroy(t.Context(), "i-0aaaaaaaaaaaaaaaa"))
	if len(w.calls("TerminateInstances")) != 0 {
		t.Error("terminated something that does not exist")
	}
}

func TestWireErrorsAndPaging(t *testing.T) {
	e, w := wireEnv(t)
	w.failRun = "InsufficientInstanceCapacity"
	if _, err := e.p.Create(t.Context(), e.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("no capacity: %v", err)
	}
	w.failRun = "UnauthorizedOperation"
	if _, err := e.p.Create(t.Context(), e.request()); err == nil || errors.Is(err, fleet.ErrCapacity) || apiCode(err) != "UnauthorizedOperation" {
		t.Fatalf("refused: %v", err)
	}
	// Two pages of DescribeInstances are both read.
	list, err := e.p.List(t.Context())
	must(t, err)
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	var filters url.Values
	for _, c := range w.calls("DescribeInstances") {
		if c.Get("Filter.2.Name") == "instance-state-name" && c.Get("NextToken") == "" {
			filters = c // the first page of List
		}
	}
	if filters.Get("Filter.1.Name") != "tag:"+TagFleet || filters.Get("Filter.1.Value.1") != testFleet || filters.Get("Filter.2.Name") != "instance-state-name" {
		t.Errorf("filters = %v", filters)
	}
	if list[0].ExternalID != "i-0000000000000001" || list[1].MachineID.String() != "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001" {
		t.Errorf("list = %+v", list)
	}
	// Terminating asks for the instance by its ID.
	e.p.known = map[string]seen{}
	if err := e.p.terminate(t.Context(), "i-0000000000000001"); err != nil {
		t.Fatal(err)
	}
	if got := w.calls("TerminateInstances"); len(got) != 1 || got[0].Get("InstanceId.1") != "i-0000000000000001" {
		t.Fatalf("terminate = %v", got)
	}
}

// checkTagSpecifications checks that every resource RunInstances creates is tagged
// with the fleet, and that all four kinds are.
func checkTagSpecifications(t *testing.T, f url.Values) {
	t.Helper()
	kinds := map[string]bool{}
	for i := 1; f.Get(fmt.Sprintf("TagSpecification.%d.ResourceType", i)) != ""; i++ {
		p := fmt.Sprintf("TagSpecification.%d.", i)
		kinds[f.Get(p+"ResourceType")] = true
		found := false
		for j := 1; f.Get(fmt.Sprintf("%sTag.%d.Key", p, j)) != ""; j++ {
			found = found || (f.Get(fmt.Sprintf("%sTag.%d.Key", p, j)) == TagFleet && f.Get(fmt.Sprintf("%sTag.%d.Value", p, j)) == testFleet)
		}
		if !found {
			t.Errorf("%s has no fleet tag", f.Get(p+"ResourceType"))
		}
	}
	for _, want := range []string{"instance", "volume", "network-interface", "spot-instances-request"} {
		if !kinds[want] {
			t.Errorf("no tags for %s: %v", want, kinds)
		}
	}
}

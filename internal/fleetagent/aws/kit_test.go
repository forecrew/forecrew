package aws

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/domain"
)

// Lint of the setup kit in deploy/byoc/aws. cfn-lint and tofu validate run in
// deploy/byoc/aws/validate.sh where they are installed; these tests need neither,
// and check what matters most: that the two templates make the same role, and that
// the role and the machines' network are as narrow as the documentation says.

// noValue marks AWS::NoValue, which drops a list item.
type noValue struct{}

// cfn resolves the intrinsic functions of a CloudFormation template the kit uses,
// for given parameter values.
type cfn struct {
	t          testing.TB
	params     map[string]any
	conditions map[string]*yaml.Node
	attrs      map[string]string
	doc        *yaml.Node
}

func loadCFN(t testing.TB, params map[string]any) *cfn {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(kitFile(t, "cloudformation.yaml"), &doc); err != nil {
		t.Fatalf("cloudformation.yaml: %v", err)
	}
	c := &cfn{t: t, params: params, doc: doc.Content[0], conditions: map[string]*yaml.Node{}, attrs: map[string]string{
		"MachineSecurityGroup.GroupId": testGroup,
	}}
	c.params["AWS::Region"] = testRegion
	c.params["AWS::AccountId"] = testAccount
	c.params["AWS::NoValue"] = noValue{}
	c.params["MachineSecurityGroup"] = testGroup
	cond := c.section("Conditions")
	for i := 0; i < len(cond.Content); i += 2 {
		c.conditions[cond.Content[i].Value] = cond.Content[i+1]
	}
	return c
}

func (c *cfn) section(name string) *yaml.Node {
	for i := 0; i < len(c.doc.Content); i += 2 {
		if c.doc.Content[i].Value == name {
			return c.doc.Content[i+1]
		}
	}
	c.t.Fatalf("no section %s", name)
	return nil
}

func child(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// evalCondition evaluates a condition by name.
func (c *cfn) cond(name string) bool {
	n, ok := c.conditions[name]
	if !ok {
		c.t.Fatalf("no condition %s", name)
	}
	return c.truth(n)
}

func (c *cfn) truth(n *yaml.Node) bool {
	switch n.Tag {
	case "!Equals":
		return reflect.DeepEqual(c.value(n.Content[0]), c.value(n.Content[1]))
	case "!Not":
		return !c.truth(n.Content[0])
	case "!And":
		for _, x := range n.Content {
			if !c.truth(x) {
				return false
			}
		}
		return true
	case "!Condition":
		return c.cond(n.Value)
	}
	c.t.Fatalf("unsupported condition %s", n.Tag)
	return false
}

var subRE = regexp.MustCompile(`\$\{([^}]+)\}`)

func (c *cfn) sub(s string, extra map[string]any) string {
	return subRE.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := extra[name]; ok {
			return fmt.Sprint(v)
		}
		if v, ok := c.attrs[name]; ok {
			return v
		}
		v, ok := c.params[name]
		if !ok {
			c.t.Fatalf("no value for ${%s}", name)
		}
		return fmt.Sprint(v)
	})
}

// value resolves a node to strings, lists and maps.
func (c *cfn) value(n *yaml.Node) any {
	switch n.Kind {
	case yaml.ScalarNode:
		return c.scalar(n)
	case yaml.SequenceNode:
		return c.sequence(n)
	case yaml.MappingNode:
		if len(n.Content) == 2 && n.Content[0].Value == "Fn::Base64" {
			return c.value(n.Content[1])
		}
		out := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			out[n.Content[i].Value] = c.value(n.Content[i+1])
		}
		return out
	}
	c.t.Fatalf("unsupported node %v", n.Kind)
	return nil
}

func (c *cfn) scalar(n *yaml.Node) any {
	switch n.Tag {
	case "!Ref":
		v, ok := c.params[n.Value]
		if !ok {
			c.t.Fatalf("no value for !Ref %s", n.Value)
		}
		return v
	case "!Sub":
		return c.sub(n.Value, nil)
	case "!GetAtt":
		return c.attrs[n.Value]
	case "!Condition":
		return c.cond(n.Value)
	}
	var v any
	if err := n.Decode(&v); err != nil {
		c.t.Fatal(err)
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (c *cfn) sequence(n *yaml.Node) any {
	switch n.Tag {
	case "!If":
		if c.cond(n.Content[0].Value) {
			return c.value(n.Content[1])
		}
		return c.value(n.Content[2])
	case "!Join":
		var parts []string
		for _, p := range asList(c.value(n.Content[1])) {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, c.value(n.Content[0]).(string))
	case "!Sub":
		extra := map[string]any{}
		maps.Copy(extra, c.value(n.Content[1]).(map[string]any))
		return c.sub(c.value(n.Content[0]).(string), extra)
	case "!Equals", "!Not", "!And":
		return c.truth(n)
	}
	var out []any
	for _, x := range n.Content {
		v := c.value(x)
		if _, drop := v.(noValue); drop {
			continue
		}
		out = append(out, asList(v)...) // a list parameter inside a list
	}
	return out
}

func asList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

func (c *cfn) resource(name string) *yaml.Node {
	r := child(c.section("Resources"), name)
	if r == nil {
		c.t.Fatalf("no resource %s", name)
	}
	return r
}

func cfnParams(subnets ...string) map[string]any {
	p := map[string]any{
		"Mode": "ec2", "FleetName": testFleet, "InstanceTypes": []string{"c7i.*", "m7g.large"},
		"MachineSubnet1": testSubnet, "MachineSubnet2": "", "MachineSubnet3": "", "CopyAmiId": "",
		"ImageSigningKey": "ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "AssignPublicIp": "false", "MaxMachines": 10, "MonthlyBudget": 0,
		"MaxLifetimeHours": 24, "JoinCode": "K7PQ-2M4X", "InstallRef": "v0.2.0", "ServerUrl": "https://forecrew.example.com", "AgentVersion": "latest",
		"VpcId": "vpc-0a1b2c3d", "AgentSubnetId": testSubnet, "AgentInstanceType": "t3.small", "HostAvailabilityZone": "",
	}
	for i, s := range subnets {
		p[fmt.Sprintf("MachineSubnet%d", i+1)] = s
	}
	return p
}

// normalized turns a policy into data that compares: JSON round-tripped.
func normalized(t testing.TB, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	must(t, json.Unmarshal(b, &out))
	return out
}

// The role is written twice, in each template. This is what keeps them the same.
func TestTemplatesMakeTheSameRole(t *testing.T) {
	for name, subnets := range map[string][]string{"two subnets": {testSubnet, testSubnet2}, "one subnet": {testSubnet}} {
		t.Run(name, func(t *testing.T) {
			c := loadCFN(t, cfnParams(subnets...))
			role := c.resource("AgentRole")
			policies := child(role, "Properties").Content
			var got any
			for i := 0; i < len(policies); i += 2 {
				if policies[i].Value == "Policies" {
					p := c.value(policies[i+1]).([]any)[0].(map[string]any)
					got = p["PolicyDocument"]
				}
			}
			if got == nil {
				t.Fatal("no policy in the role")
			}
			vars := kitVars()
			var arns []string
			for _, s := range subnets {
				arns = append(arns, "arn:aws:ec2:"+testRegion+":"+testAccount+":subnet/"+s)
			}
			vars["subnet_arns"] = mustJSON(arns)
			vars["network_arns"] = mustJSON(append(slices.Clone(arns), "arn:aws:ec2:"+testRegion+":"+testAccount+":security-group/"+testGroup))
			text := string(kitFile(t, "policy.json.tftpl"))
			for k, v := range vars {
				text = strings.ReplaceAll(text, "${"+k+"}", v)
			}
			var want any
			must(t, json.Unmarshal([]byte(text), &want))
			if !reflect.DeepEqual(normalized(t, got), want) {
				a, _ := json.MarshalIndent(normalized(t, got), "", " ")
				b, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("CloudFormation makes\n%s\nOpenTofu makes\n%s", a, b)
			}
		})
	}
}

// A policy of the kit never allows more than it should, whatever the statement.
func TestPolicyShape(t *testing.T) {
	d := kitPolicy(t)
	seen := map[string]bool{}
	for _, s := range d.Statement {
		if seen[s.Sid] {
			t.Errorf("statement id %s twice", s.Sid)
		}
		seen[s.Sid] = true
		checkStatement(t, s)
	}
	for _, sid := range []string{"RunInstance", "RunInstanceVolume", "RunInstanceNetworkInterface", "TerminateOwnMachines", "TagOnLaunch", "ChangeTagsOfOwnMachines"} {
		if !seen[sid] {
			t.Errorf("no statement %s", sid)
		}
	}
}

func describes(a string) bool { return strings.HasPrefix(a, "ec2:Describe") }

func checkStatement(t *testing.T, s statement) {
	t.Helper()
	for _, a := range s.Action {
		if strings.HasSuffix(a, "*") && s.Effect == "Allow" {
			t.Errorf("%s: wildcard action %s", s.Sid, a)
		}
		if !strings.HasPrefix(a, "ec2:") {
			t.Errorf("%s: %s is not an EC2 action", s.Sid, a)
		}
	}
	if s.Effect != "Allow" {
		return
	}
	onlyDescribes := !slices.ContainsFunc(s.Action, func(a string) bool { return !describes(a) })
	for _, r := range s.Resource {
		// Describe calls have no resource-level permissions.
		if (r == "*" && !onlyDescribes) || strings.Contains(r, ":*:") {
			t.Errorf("%s: resource %s is not pinned to this account and region", s.Sid, r)
		}
	}
	// Anything that changes something is conditioned, or pinned by its ARNs: the
	// subnets and group RunInstances reads.
	if len(s.Condition) == 0 && !onlyDescribes && s.Sid != "RunInstanceNetwork" {
		t.Errorf("%s: a change with no condition", s.Sid)
	}
}

// Everything the kit makes that can leak or be reached is closed.
func TestCloudFormationIsClosed(t *testing.T) {
	c := loadCFN(t, cfnParams(testSubnet))
	params := c.section("Parameters")
	if v := child(child(params, "JoinCode"), "NoEcho"); v == nil || v.Value != "true" {
		t.Error("the join code is shown in the console")
	}
	agent := child(c.resource("AgentInstance"), "Properties")
	meta := child(agent, "MetadataOptions")
	if child(meta, "HttpTokens").Value != "required" {
		t.Error("the agent's instance allows IMDSv1")
	}
	text := string(kitFile(t, "cloudformation.yaml"))
	for _, bad := range []string{"Principal: '*'", `"Principal": "*"`, "AdministratorAccess", "PowerUserAccess", "AccessKey", "SecretAccessKey", "'ec2:*'", "iam:PassRole"} {
		if strings.Contains(text, bad) {
			t.Errorf("cloudformation.yaml has %s", bad)
		}
	}
	if !strings.Contains(text, "Encrypted: true") {
		t.Error("the agent's disk is not encrypted")
	}
	if domain.SecretPatterns.MatchString(text) {
		t.Errorf("cloudformation.yaml holds something shaped like a secret: %s", domain.SecretPatterns.FindString(text))
	}

	sg := child(c.resource("MachineSecurityGroup"), "Properties")
	if in := child(sg, "SecurityGroupIngress"); in == nil || len(in.Content) != 0 {
		t.Error("machines accept inbound traffic")
	}
	checkEgress(t, egressOf(t, c.value(child(sg, "SecurityGroupEgress")).([]any)))

	// The agent's group opens HTTPS to the same public ranges and no private one.
	agentSG := child(c.resource("AgentSecurityGroup"), "Properties")
	var https []netip.Prefix
	for _, r := range egressOf(t, c.value(child(agentSG, "SecurityGroupEgress")).([]any)) {
		if r.proto == "tcp" && !strings.HasPrefix(r.cidr, "169.254.169.") {
			https = append(https, netip.MustParsePrefix(r.cidr))
		}
	}
	checkPartition(t, https)
	if coveredBy(https, netip.MustParseAddr("10.1.2.3")) || strings.Contains(text, "CidrIp: 0.0.0.0/0") {
		t.Error("the agent's group reaches private ranges")
	}
}

type rule struct{ proto, cidr string }

func egressOf(t *testing.T, raw []any) []rule {
	t.Helper()
	var out []rule
	for _, r := range raw {
		m := r.(map[string]any)
		out = append(out, rule{fmt.Sprint(m["IpProtocol"]), fmt.Sprint(m["CidrIp"])})
	}
	return out
}

// checkEgress proves what the security group of machines allows: DNS and time on
// their link-local addresses, and every public IPv4 address, none of the private,
// loopback, link-local (the metadata endpoint, 169.254.169.254, included), shared
// or multicast ones, whatever the protocol.
func checkEgress(t *testing.T, rules []rule) {
	t.Helper()
	var open []netip.Prefix
	for _, r := range rules {
		p, err := netip.ParsePrefix(r.cidr)
		if err != nil {
			t.Fatalf("rule %+v: %v", r, err)
		}
		if r.proto == "-1" {
			open = append(open, p)
		} else if !p.Addr().IsLinkLocalUnicast() || p.Bits() != 32 {
			t.Errorf("a rule for %s %s: only DNS and time may name a protocol", r.proto, r.cidr)
		}
	}
	if len(rules) > 60 {
		t.Errorf("%d rules: a security group holds 60", len(rules))
	}
	checkPartition(t, open)
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "52.95.110.1", "172.15.255.255", "172.32.0.1", "192.167.1.1", "100.63.255.255", "100.128.0.1", "223.255.255.255"} {
		if !coveredBy(open, netip.MustParseAddr(ip)) {
			t.Errorf("%s is public and not open", ip)
		}
	}
	for _, ip := range []string{"10.0.0.5", "172.16.5.5", "172.31.255.255", "192.168.1.1", "169.254.169.254", "169.254.169.253", "127.0.0.1", "100.64.0.1", "224.0.0.1", "255.255.255.255", "0.0.0.0"} {
		if coveredBy(open, netip.MustParseAddr(ip)) {
			t.Errorf("%s is private and open", ip)
		}
	}
}

// checkPartition checks that the open ranges and the forbidden ones are disjoint
// and together are all of IPv4.
func checkPartition(t *testing.T, open []netip.Prefix) {
	t.Helper()
	forbidden := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/3"}
	var forbiddenSize, openSize uint64
	for _, f := range forbidden {
		fp := netip.MustParsePrefix(f)
		forbiddenSize += 1 << (32 - fp.Bits())
		for _, o := range open {
			if o.Overlaps(fp) {
				t.Errorf("%s is open and overlaps %s", o, f)
			}
		}
	}
	for i, o := range open {
		openSize += 1 << (32 - o.Bits())
		for _, p := range open[i+1:] {
			if o.Overlaps(p) {
				t.Errorf("%s and %s overlap", o, p)
			}
		}
	}
	if openSize+forbiddenSize != 1<<32 {
		t.Errorf("the open rules cover %d addresses and the forbidden ones %d: together they must be all of IPv4", openSize, forbiddenSize)
	}
}

func coveredBy(ps []netip.Prefix, a netip.Addr) bool {
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}

// The OpenTofu module spells out the same ranges.
func TestOpenTofuNetworkMatchesCloudFormation(t *testing.T) {
	c := loadCFN(t, cfnParams(testSubnet))
	sg := child(c.resource("MachineSecurityGroup"), "Properties")
	var want []string
	for _, r := range egressOf(t, c.value(child(sg, "SecurityGroupEgress")).([]any)) {
		if r.proto == "-1" {
			want = append(want, r.cidr)
		}
	}
	main := string(kitFile(t, "main.tf"))
	block := regexp.MustCompile(`(?s)public_cidrs = \[(.*?)\n  \]`).FindStringSubmatch(main)
	if block == nil {
		t.Fatal("no public_cidrs in main.tf")
	}
	got := regexp.MustCompile(`"([0-9./]+)"`).FindAllStringSubmatch(block[1], -1)
	var cidrs []string
	for _, g := range got {
		cidrs = append(cidrs, g[1])
	}
	slices.Sort(want)
	slices.Sort(cidrs)
	if !slices.Equal(want, cidrs) {
		t.Fatalf("CloudFormation opens %v, OpenTofu %v", want, cidrs)
	}
	var rules []rule
	for _, x := range cidrs {
		rules = append(rules, rule{"-1", x})
	}
	rules = append(rules, rule{"udp", "169.254.169.253/32"}, rule{"tcp", "169.254.169.253/32"}, rule{"udp", "169.254.169.123/32"})
	checkEgress(t, rules)
}

// The agent's start-up script is in both templates.
func TestUserDataIsTheSameInBothTemplates(t *testing.T) {
	tf := string(kitFile(t, "user-data.sh.tftpl"))
	rename := map[string]string{
		"image_signing_key": "ImageSigningKey", "agent_kind": "AgentKind", "region": "AWS::Region", "fleet_name": "FleetName",
		"machine_subnets": "MachineSubnets", "machine_security_group": "MachineSecurityGroup", "instance_types": "InstanceTypesList",
		"assign_public_ip": "AssignPublicIp", "max_machines": "MaxMachines", "monthly_budget": "MonthlyBudget",
		"max_lifetime_hours": "MaxLifetimeHours", "join_code": "JoinCode", "install_ref": "InstallRef", "server_url": "ServerUrl",
		"agent_version": "AgentVersion",
	}
	names := subRE.FindAllStringSubmatch(tf, -1)
	for _, n := range names {
		if _, ok := rename[n[1]]; !ok {
			t.Fatalf("user-data.sh.tftpl uses ${%s}, which this test does not know", n[1])
		}
	}
	for from, to := range rename {
		tf = strings.ReplaceAll(tf, "${"+from+"}", "${"+to+"}")
	}
	c := loadCFN(t, cfnParams(testSubnet))
	ud := child(child(c.resource("AgentInstance"), "Properties"), "UserData")
	sub := ud.Content[1] // the !Sub sequence: text, then the map
	if sub.Tag != "!Sub" || len(sub.Content) != 2 {
		t.Fatal("the user data is not a Sub with a map")
	}
	if got := sub.Content[0].Value; got != tf {
		t.Fatalf("the scripts differ:\nCloudFormation:\n%s\nOpenTofu:\n%s", got, tf)
	}
}

// Rendered for each mode, the script is valid shell, and the secret is only in it
// where it must be.
func TestUserDataRuns(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for mode, kind := range map[string]string{"ec2": "aws", "microvm": "microvm", "mac": "tart"} {
		t.Run(mode, func(t *testing.T) {
			c := loadCFN(t, cfnParams(testSubnet, testSubnet2))
			c.params["Mode"] = mode
			ud := child(child(c.resource("AgentInstance"), "Properties"), "UserData")
			script := c.value(ud).(string)
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n: %v\n%s\n%s", err, out, script)
			}
			if !strings.Contains(script, "--kind '"+kind+"'") {
				t.Errorf("mode %s starts the wrong kind:\n%s", mode, script)
			}
			if mode == "ec2" && !strings.Contains(script, "subnets: ["+testSubnet+", "+testSubnet2+"]") {
				t.Errorf("the subnets are not in aws.yaml:\n%s", script)
			}
			if strings.Count(script, "K7PQ-2M4X") != 1 {
				t.Error("the join code must appear once, in the environment of the installer")
			}
			if mode == "ec2" {
				checkAWSConfig(t, script)
			}
		})
	}
}

// checkAWSConfig reads the aws.yaml the script writes the way the agent does.
func checkAWSConfig(t *testing.T, script string) {
	t.Helper()
	body := regexp.MustCompile(`(?s)<<'YAML'\n(.*?)\nYAML`).FindStringSubmatch(script)
	if body == nil {
		t.Fatal("no aws.yaml in the script")
	}
	cfg := defaults()
	dec := yaml.NewDecoder(strings.NewReader(body[1]))
	dec.KnownFields(true)
	must(t, dec.Decode(&cfg))
	must(t, cfg.validate())
	if cfg.SecurityGroup != testGroup || cfg.Fleet != testFleet || !cfg.allowsType("c7i.xlarge") || cfg.allowsType("p5.48xlarge") {
		t.Errorf("config = %+v", cfg)
	}
}

// What a person must read to trust the kit does not contain a secret, and the
// OpenTofu module is consistent with itself.
func TestOpenTofuModule(t *testing.T) {
	files := map[string]string{}
	for _, f := range []string{"main.tf", "variables.tf", "outputs.tf", "versions.tf", "user-data.sh.tftpl", "policy.json.tftpl"} {
		files[f] = string(kitFile(t, f))
		if domain.SecretPatterns.MatchString(files[f]) {
			t.Errorf("%s holds something shaped like a secret", f)
		}
	}
	checkVariables(t, files)
	if !regexp.MustCompile(`(?s)variable "join_code" \{.*?sensitive\s*=\s*true`).MatchString(files["variables.tf"]) {
		t.Error("the join code is not sensitive")
	}
	if !strings.Contains(files["main.tf"], `http_tokens                 = "required"`) || !strings.Contains(files["main.tf"], "encrypted             = true") {
		t.Error("the agent's instance allows IMDSv1 or has an unencrypted disk")
	}
	// Every variable of a template is given by the module's templatefile call.
	for _, tpl := range []string{"user-data.sh.tftpl", "policy.json.tftpl"} {
		call := tpl + `", {`
		i := strings.Index(files["main.tf"], call)
		if i < 0 {
			t.Fatalf("main.tf does not render %s", tpl)
		}
		args := files["main.tf"][i : i+strings.Index(files["main.tf"][i:], "})")]
		for _, m := range subRE.FindAllStringSubmatch(files[tpl], -1) {
			if !regexp.MustCompile(`(?m)^\s*` + m[1] + `\s*=`).MatchString(args) {
				t.Errorf("%s needs %s, which main.tf does not pass", tpl, m[1])
			}
		}
	}
}

// checkVariables checks that every var.x is declared and every variable is used.
func checkVariables(t *testing.T, files map[string]string) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^variable "([a-z_]+)"`).FindAllStringSubmatch(files["variables.tf"], -1) {
		declared[m[1]] = true
	}
	used := map[string]bool{}
	for _, f := range []string{"main.tf", "outputs.tf", "variables.tf"} {
		for _, m := range regexp.MustCompile(`var\.([a-z_]+)`).FindAllStringSubmatch(files[f], -1) {
			used[m[1]] = true
			if !declared[m[1]] {
				t.Errorf("%s uses var.%s, which is not declared", f, m[1])
			}
		}
	}
	for v := range declared {
		if !used[v] {
			t.Errorf("variable %s is never used", v)
		}
	}
}

// The AMI copy is the one way an image gets into the account, so it stays as
// small as it looks.
func TestCopyRoleIsNarrow(t *testing.T) {
	c := loadCFN(t, cfnParams(testSubnet))
	c.params["CopyAmiId"] = "ami-0123456789abcdef0"
	role := c.value(child(c.resource("CopyRole"), "Properties")).(map[string]any)
	pol := role["Policies"].([]any)[0].(map[string]any)["PolicyDocument"]
	d := parsePolicy(t, mustMarshal(t, pol))
	for _, r := range []request{
		{"ec2:CopyImage", "arn:aws:ec2:" + testRegion + "::image/ami-1", nil},
		{"ec2:DescribeImages", "*", nil},
		{"ec2:CreateTags", "arn:aws:ec2:" + testRegion + "::image/ami-2", ctx("ec2:CreateAction", "CopyImage")},
		{"ec2:DeregisterImage", "arn:aws:ec2:" + testRegion + "::image/ami-1", ctx("ec2:ResourceTag/forecrew:kit", "aws-byoc")},
	} {
		if !d.allows(r) {
			t.Errorf("refused: %s", d.why(r))
		}
	}
	for _, r := range []request{
		{"ec2:DeregisterImage", "arn:aws:ec2:" + testRegion + "::image/ami-1", nil},
		{"ec2:CreateTags", "arn:aws:ec2:" + testRegion + "::image/ami-1", nil}, // an image that already exists
		{"ec2:CreateTags", "arn:aws:ec2:" + testRegion + "::image/ami-1", ctx("ec2:CreateAction", "RegisterImage")},
		{"ec2:DeleteSnapshot", "arn:aws:ec2:" + testRegion + "::snapshot/snap-1", ctx("ec2:ResourceTag/forecrew:kit", "other")},
		{"ec2:RunInstances", "*", nil},
		{"ec2:TerminateInstances", "*", nil},
	} {
		if d.allows(r) {
			t.Errorf("allowed: %s", r)
		}
	}
	if code := c.value(child(child(c.resource("CopyFunction"), "Properties"), "Code")).(map[string]any)["ZipFile"].(string); !strings.Contains(code, "cfnresponse.send") || !strings.Contains(code, "copy_image") {
		t.Error("the copy function does not answer CloudFormation")
	}
}

func mustMarshal(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

// OpenTofu checks its inputs with the patterns CloudFormation does.
func TestOpenTofuChecksTheSameInputs(t *testing.T) {
	c := loadCFN(t, cfnParams(testSubnet))
	vars := string(kitFile(t, "variables.tf"))
	for cfnName, tfName := range map[string]string{
		"ServerUrl": "server_url", "JoinCode": "join_code", "ImageSigningKey": "image_signing_key", "AgentVersion": "agent_version",
		"InstallRef": "install_ref", "FleetName": "fleet_name", "AgentInstanceType": "agent_instance_type", "HostAvailabilityZone": "host_availability_zone",
	} {
		want := child(child(c.section("Parameters"), cfnName), "AllowedPattern")
		if want == nil {
			t.Fatalf("%s has no AllowedPattern", cfnName)
		}
		block := regexp.MustCompile(`(?s)variable "` + tfName + `" \{.*?\n\}`).FindString(vars)
		got := regexp.MustCompile(`regex\("(.*?)", var\.` + tfName + `\)`).FindStringSubmatch(block)
		if got == nil {
			t.Errorf("%s has no regex check", tfName)
			continue
		}
		if tf := strings.ReplaceAll(got[1], `\\`, `\`); tf != want.Value {
			t.Errorf("%s checks %q, %s checks %q", tfName, tf, cfnName, want.Value)
		}
	}
	// A release tag, never a branch; and no fleet name that every account shares.
	for _, ref := range []string{"main", "master", "v", "v0.2.0/../x"} {
		if regexp.MustCompile(child(child(c.section("Parameters"), "InstallRef"), "AllowedPattern").Value).MatchString(ref) {
			t.Errorf("install ref %q is accepted", ref)
		}
	}
	if child(child(c.section("Parameters"), "FleetName"), "Default") != nil || child(child(c.section("Parameters"), "InstallRef"), "Default") != nil {
		t.Error("FleetName and InstallRef have no default")
	}
	// Two kits in one VPC must not share a name: neither template has a default.
	if regexp.MustCompile(`(?s)variable "fleet_name" \{.*?\n\}`).FindString(vars) != "" && regexp.MustCompile(`(?m)^\s+default\s*=`).MatchString(regexp.MustCompile(`(?s)variable "fleet_name" \{.*?\n\}`).FindString(vars)) {
		t.Error("fleet_name has a default in OpenTofu")
	}
	if strings.Contains(string(kitFile(t, "main.tf")), "sha1(") {
		t.Error("OpenTofu makes up a fleet name")
	}
}

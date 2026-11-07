package aws

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A small evaluator of IAM identity policies, for the subset the kit's policy
// uses: Allow and Deny, wildcards in actions and resources, and the condition
// operators below with ForAllValues and ForAnyValue. It follows IAM's rules: an
// explicit Deny wins, nothing allowed is denied, a condition on a key the
// request does not have fails, except for the negated operators and ForAllValues.

// listOf reads a JSON string or list of strings.
type listOf []string

func (l *listOf) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = listOf{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		var n json.Number
		if err2 := json.Unmarshal(b, &n); err2 == nil {
			*l = listOf{n.String()}
			return nil
		}
		return err
	}
	*l = many
	return nil
}

type statement struct {
	Sid       string
	Effect    string
	Action    listOf
	Resource  listOf
	Condition map[string]map[string]listOf
}

type policyDocument struct {
	Version   string
	Statement []statement
}

func parsePolicy(t testing.TB, raw []byte) policyDocument {
	t.Helper()
	var d policyDocument
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("policy: %v", err)
	}
	return d
}

// request is one authorization: an action on a resource with context keys.
type request struct {
	action   string
	resource string
	context  map[string][]string
}

func (r request) String() string { return r.action + " on " + r.resource }

func glob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, c := range pattern {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (s statement) matches(r request) bool {
	act := false
	for _, a := range s.Action {
		if glob(strings.ToLower(a)).MatchString(strings.ToLower(r.action)) {
			act = true
		}
	}
	res := false
	for _, x := range s.Resource {
		if glob(x).MatchString(r.resource) {
			res = true
		}
	}
	return act && res && s.conditions(r)
}

func (s statement) conditions(r request) bool {
	for op, keys := range s.Condition {
		for key, want := range keys {
			if !evalCondition(op, r.context[key], want) {
				return false
			}
		}
	}
	return true
}

func evalCondition(op string, have, want []string) bool {
	set := ""
	if before, after, ok := strings.Cut(op, ":"); ok {
		set, op = before, after
	}
	if op == "Null" {
		isNull := len(have) == 0
		return isNull == (want[0] == "true")
	}
	negated := strings.Contains(op, "Not")
	single := func(h string) bool { return anyMatch(op, h, want) }
	switch set {
	case "ForAllValues":
		for _, h := range have {
			if !single(h) {
				return false
			}
		}
		return true
	case "ForAnyValue":
		return slices.ContainsFunc(have, single)
	}
	if len(have) == 0 {
		return negated
	}
	if negated {
		for _, h := range have {
			if !single(h) {
				return false
			}
		}
		return true
	}
	return slices.ContainsFunc(have, single)
}

// anyMatch evaluates one value against the policy's values; for a negated
// operator that is "matches none of them".
func anyMatch(op, have string, want []string) bool {
	positive := strings.Replace(op, "Not", "", 1)
	hit := false
	for _, w := range want {
		switch positive {
		case "StringEquals", "ArnEquals":
			hit = hit || have == w
		case "StringLike", "ArnLike":
			hit = hit || glob(w).MatchString(have)
		case "Bool":
			hit = hit || strings.EqualFold(have, w)
		case "NumericLessThanEquals":
			a, err1 := strconv.ParseFloat(have, 64)
			b, err2 := strconv.ParseFloat(w, 64)
			hit = hit || (err1 == nil && err2 == nil && a <= b)
		default:
			panic("iamsim: unsupported operator " + op)
		}
	}
	if strings.Contains(op, "Not") {
		return !hit
	}
	return hit
}

// allows evaluates r: an explicit Deny wins, then some Allow must match.
func (d policyDocument) allows(r request) bool {
	allowed := false
	for _, s := range d.Statement {
		if !s.matches(r) {
			continue
		}
		if s.Effect == "Deny" {
			return false
		}
		allowed = allowed || s.Effect == "Allow"
	}
	return allowed
}

// why says which statements matched, for a failing test.
func (d policyDocument) why(r request) string {
	var out []string
	for _, s := range d.Statement {
		if s.matches(r) {
			out = append(out, s.Effect+" "+s.Sid)
		}
	}
	return fmt.Sprintf("%s: matched %v", r, out)
}

// The values the policy is rendered with in tests.
const (
	testRegion  = "eu-west-1"
	testAccount = "123456789012"
	testFleet   = "forecrew"
	testSubnet  = "subnet-0a1b2c3d4e5f60718"
	testSubnet2 = "subnet-0f1e2d3c4b5a69788"
	testGroup   = "sg-0a1b2c3d4e5f60718"
)

// kitVars are the template variables for the test account.
func kitVars() map[string]string {
	subnets := []string{
		"arn:aws:ec2:" + testRegion + ":" + testAccount + ":subnet/" + testSubnet,
		"arn:aws:ec2:" + testRegion + ":" + testAccount + ":subnet/" + testSubnet2,
	}
	group := "arn:aws:ec2:" + testRegion + ":" + testAccount + ":security-group/" + testGroup
	return map[string]string{
		"region": testRegion, "account": testAccount, "fleet": testFleet,
		"instance_types": mustJSON([]string{"c7i.*", "m7g.large"}),
		"subnet_arns":    mustJSON(subnets),
		"network_arns":   mustJSON(append(subnets, group)),
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// kitFile reads a file of the kit.
func kitFile(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "byoc", "aws", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// kitPolicy renders the policy of the kit for the test account.
func kitPolicy(t testing.TB) policyDocument {
	t.Helper()
	text := string(kitFile(t, "policy.json.tftpl"))
	for k, v := range kitVars() {
		text = strings.ReplaceAll(text, "${"+k+"}", v)
	}
	if strings.Contains(text, "${") {
		t.Fatalf("the policy template has a variable the test does not set: %s", text[strings.Index(text, "${"):][:30])
	}
	return parsePolicy(t, []byte(text))
}

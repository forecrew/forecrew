package config

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

const evidenceYAML = `
evidence:
  endpoint: http://garage:3900
  bucket: forecrew-evidence
  path_style: true
  retention_days: 90
pools:
  control:
    roles: [reviewer, architect]
    kinds: [agent]
    credentials: {runtimes: [codex], repo_token: read}
  gpu:
    roles: ["*"]
    kinds: [command]
    credentials: {repo_token: none}
`

var evidenceEnv = map[string]string{EnvEvidenceAccessKeyID: "GK1", EnvEvidenceSecretAccessKey: "s3cret"}

func TestParsePoolsAndEvidence(t *testing.T) {
	c, err := Parse([]byte(validYAML+evidenceYAML), env(evidenceEnv))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Evidence
	if e == nil || e.Region != DefaultEvidenceRegion || e.MaxArtifactSize != DefaultMaxArtifactSize || !e.PathStyle ||
		e.RetentionDays != 90 || e.AccessKeyID != "GK1" || e.SecretAccessKey != "s3cret" {
		t.Fatalf("evidence = %+v", e)
	}
	if c.MaxTestResultsSize != DefaultMaxTestResultsSize {
		t.Errorf("max_test_results_size = %d, want the default", c.MaxTestResultsSize)
	}
	if len(c.Pools) != 4 {
		t.Fatalf("pools = %v", c.Pools)
	}
	ctl := c.Pools[domain.PoolControl]
	if ctl.Allows("developer", domain.JobKindAgent, false) || !ctl.Allows("reviewer", "", false) ||
		!ctl.Credentials.AllowsRuntime("codex") || ctl.Credentials.AllowsRuntime("claude") {
		t.Errorf("control = %+v", ctl)
	}
	if !c.Pools["gpu"].Allows("", domain.JobKindCommand, false) || c.Pools["gpu"].Allows("developer", domain.JobKindAgent, true) {
		t.Errorf("gpu = %+v", c.Pools["gpu"])
	}
}

func TestParseFleet(t *testing.T) {
	c, err := Parse([]byte(validYAML+"fleet:\n  currency: CHF\n"), env(nil))
	if err != nil || c.Fleet.Currency != "CHF" {
		t.Fatalf("fleet = %+v, %v", c.Fleet, err)
	}
	c, err = Parse([]byte(validYAML), env(nil))
	if err != nil || c.Fleet.Currency != DefaultFleetCurrency || c.Fleet.Hosted || c.Fleet.FreePlan != (FreePlan{}) {
		t.Fatalf("default = %+v, %v", c.Fleet, err)
	}
	c, err = Parse([]byte(validYAML+"fleet:\n  hosted: true\n"), env(nil))
	if err != nil || !c.Fleet.Hosted || c.Fleet.FreePlan != (FreePlan{}) {
		t.Fatalf("hosted = %+v, %v", c.Fleet, err)
	}
	c, err = Parse([]byte(validYAML+"fleet:\n  free_plan:\n    concurrent_machines: 2\n    monthly_minutes: 600\n"), env(nil))
	if err != nil || c.Fleet.FreePlan != (FreePlan{ConcurrentMachines: 2, MonthlyMinutes: 600}) {
		t.Fatalf("free plan = %+v, %v", c.Fleet, err)
	}
	if _, err := Parse([]byte(validYAML+"fleet:\n  free_plan:\n    monthly_minutes: -1\n"), env(nil)); err == nil {
		t.Fatal("a negative limit was accepted")
	}
}

func TestParseMaxTestResultsSize(t *testing.T) {
	c, err := Parse([]byte(validYAML+"max_test_results_size: 1048576\n"), env(nil))
	if err != nil || c.MaxTestResultsSize != 1<<20 {
		t.Fatalf("size = %d, %v", c.MaxTestResultsSize, err)
	}
}

func TestDefaultPools(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Evidence != nil {
		t.Errorf("evidence = %+v, want nil", c.Evidence)
	}
	build, control, actions := c.Pools[domain.PoolBuild], c.Pools[domain.PoolControl], c.Pools[domain.PoolActions]
	tests := []struct {
		name   string
		pool   Pool
		role   string
		kind   domain.JobKind
		writes bool
		want   bool
	}{
		{"build writer", build, "developer", domain.JobKindAgent, true, true},
		{"build command", build, "", domain.JobKindCommand, false, true},
		{"control reviewer", control, "reviewer", domain.JobKindAgent, false, true},
		{"control writer", control, "developer", domain.JobKindAgent, true, false},
		{"control command", control, "", domain.JobKindCommand, false, false},
		{"actions command", actions, "", domain.JobKindCommand, false, true},
		{"actions agent", actions, "reviewer", domain.JobKindAgent, false, false},
	}
	for _, tt := range tests {
		if got := tt.pool.Allows(tt.role, tt.kind, tt.writes); got != tt.want {
			t.Errorf("%s: Allows = %v, want %v", tt.name, got, tt.want)
		}
	}
	if !build.Credentials.AllowsRuntime("claude") || actions.Credentials.AllowsRuntime("claude") || control.Credentials.RepoToken != RepoTokenRead {
		t.Error("default credentials")
	}
}

// TestPoolCheck: each refusal says which rule refused the job.
func TestPoolCheck(t *testing.T) {
	pools := DefaultPools()
	reviewers := Pool{Roles: []string{"reviewer"}, Kinds: []domain.JobKind{domain.JobKindAgent},
		Credentials: PoolCredentials{RepoToken: RepoTokenWrite}}
	tests := []struct {
		name, pool, role string
		p                Pool
		kind             domain.JobKind
		writes           bool
		want             string
	}{
		{"kind", domain.PoolControl, "", pools[domain.PoolControl], domain.JobKindCommand, false, "pool control does not run command jobs"},
		{"default kind", domain.PoolActions, "reviewer", pools[domain.PoolActions], "", false, "pool actions does not run agent jobs"},
		{"role", "rev", "developer", reviewers, domain.JobKindAgent, false, "pool rev does not run role developer"},
		{"write token", domain.PoolControl, "developer", pools[domain.PoolControl], domain.JobKindAgent, true,
			"role developer writes to the repo and pool control gets no write token"},
		{"listed role", "rev", "reviewer", reviewers, domain.JobKindAgent, true, ""},
		{"command ignores roles", domain.PoolBuild, "", pools[domain.PoolBuild], domain.JobKindCommand, true, ""},
	}
	for _, tt := range tests {
		err := tt.p.Check(tt.pool, tt.role, tt.kind, tt.writes)
		if tt.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestParsePoolsAndEvidenceInvalid(t *testing.T) {
	tests := []struct {
		name    string
		replace [2]string
		env     map[string]string
		want    string
	}{
		{name: "bad currency", replace: [2]string{"evidence:", "fleet:\n  currency: chf\nevidence:"}, want: "fleet.currency"},
		{name: "bad endpoint", replace: [2]string{"http://garage:3900", "garage:3900"}, want: "evidence.endpoint must be"},
		{name: "no bucket", replace: [2]string{"bucket: forecrew-evidence", "bucket: ''"}, want: "evidence.bucket"},
		{name: "negative size", replace: [2]string{"path_style: true", "path_style: true\n  max_artifact_size: -1"}, want: "max_artifact_size"},
		{name: "negative test results size", replace: [2]string{"evidence:", "max_test_results_size: -5\nevidence:"}, want: "max_test_results_size"},
		{name: "negative retention", replace: [2]string{"retention_days: 90", "retention_days: -1"}, want: "retention_days"},
		{name: "no access key", env: map[string]string{EnvEvidenceAccessKeyID: ""}, want: EnvEvidenceAccessKeyID + " is not set"},
		{name: "no secret key", env: map[string]string{EnvEvidenceSecretAccessKey: ""}, want: EnvEvidenceSecretAccessKey + " is not set"},
		{name: "key in file", replace: [2]string{"bucket: forecrew-evidence", "bucket: b\n  secret_access_key: x"}, want: "field secret_access_key not found"},
		{name: "bad pool name", replace: [2]string{"  gpu:", "  GPU Box:"}, want: "pools.GPU Box: name"},
		{name: "no kinds", replace: [2]string{"kinds: [command]", "kinds: []"}, want: "pools.gpu.kinds must list"},
		{name: "unknown kind", replace: [2]string{"kinds: [command]", "kinds: [script]"}, want: `unknown kind "script"`},
		{name: "empty role", replace: [2]string{`roles: ["*"]`, `roles: [""]`}, want: "empty role name"},
		{name: "unknown runtime", replace: [2]string{"runtimes: [codex]", "runtimes: [gemini]"}, want: `"gemini" is not a configured runtime`},
		{name: "bad repo token", replace: [2]string{"repo_token: none", "repo_token: admin"}, want: "repo_token must be write, read or none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := validYAML + evidenceYAML
			if tt.replace[0] != "" {
				if !strings.Contains(y, tt.replace[0]) {
					t.Fatalf("fixture lacks %q", tt.replace[0])
				}
				y = strings.Replace(y, tt.replace[0], tt.replace[1], 1)
			}
			vars := map[string]string{}
			maps.Copy(vars, evidenceEnv)
			maps.Copy(vars, tt.env)
			_, err := Parse([]byte(y), env(vars))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

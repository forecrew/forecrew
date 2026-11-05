package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

const validYAML = `
server:
  public_base_url: https://forecrew.example.com/
github:
  app_id: 12345
  allowlist: [Octocat]
runtimes:
  claude:
    adapter: claude-code
    vendor: anthropic
    credential_env: [ANTHROPIC_API_KEY]
  codex:
    adapter: codex
    vendor: openai
    credential_env: [OPENAI_API_KEY]
model_tiers:
  fast: {anthropic: claude-haiku, openai: gpt-mini}
  strong: {anthropic: claude-sonnet, openai: gpt}
  best: {anthropic: claude-opus, openai: gpt-pro}
roles:
  developer: {runtime: claude, tier: strong}
  reviewer: {runtime: codex, tier: best}
policy:
  allow_same_vendor_review: false
`

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		EnvDatabaseURL:         "postgres://localhost/forecrew",
		EnvGitHubPrivateKey:    "-----BEGIN KEY-----",
		EnvGitHubWebhookSecret: "whsec",
		"ANTHROPIC_API_KEY":    "sk-ant",
		"OPENAI_API_KEY":       "sk-oai",
	}
	maps.Copy(base, overrides)
	return func(k string) string { return base[k] }
}

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.PublicBaseURL != "https://forecrew.example.com" || c.Server.Listen != DefaultListen || c.Server.CertCacheDir != "" {
		t.Errorf("server = %+v", c.Server)
	}
	if c.DatabaseURL == "" || c.GitHub.PrivateKey == "" || c.GitHub.WebhookSecret != "whsec" {
		t.Errorf("secrets not read: %+v", c)
	}
	if c.Runtimes["claude"].Credentials["ANTHROPIC_API_KEY"] != "sk-ant" {
		t.Errorf("credentials = %v", c.Runtimes["claude"].Credentials)
	}
	adapter, model, err := c.Model("developer")
	if err != nil || adapter != AdapterClaudeCode || model != "claude-sonnet" {
		t.Errorf("Model(developer) = %s, %s, %v", adapter, model, err)
	}
	if _, _, err := c.Model("nobody"); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("Model(nobody) err = %v", err)
	}
	if !c.GitHub.Allowed("octocat") || c.GitHub.Allowed("mallory") {
		t.Error("Allowed is wrong")
	}
}

func TestParseTLSDefaults(t *testing.T) {
	y := strings.Replace(validYAML, "public_base_url: https://forecrew.example.com/", "public_base_url: https://forecrew.example.com\n  tls_domain: forecrew.example.com", 1)
	c, err := Parse([]byte(y), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != DefaultTLSListen || c.Server.CertCacheDir != DefaultCertCacheDir {
		t.Errorf("server = %+v", c.Server)
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name    string
		replace [2]string
		env     map[string]string
		want    string
	}{
		{name: "unknown field", replace: [2]string{"policy:", "bogus: 1\npolicy:"}, want: "field bogus not found"},
		{name: "secret in file", replace: [2]string{"policy:", "database_url: x\npolicy:"}, want: "field database_url not found"},
		{name: "bad yaml", replace: [2]string{"server:", "server: ["}, want: "yaml"},
		{name: "no database url", env: map[string]string{EnvDatabaseURL: ""}, want: EnvDatabaseURL},
		{name: "no private key", env: map[string]string{EnvGitHubPrivateKey: ""}, want: EnvGitHubPrivateKey},
		{name: "no webhook secret", env: map[string]string{EnvGitHubWebhookSecret: ""}, want: EnvGitHubWebhookSecret},
		{name: "no credential", env: map[string]string{"OPENAI_API_KEY": ""}, want: "runtimes.codex: OPENAI_API_KEY is not set"},
		{name: "relative url", replace: [2]string{"https://forecrew.example.com/", "forecrew.example.com"}, want: "public_base_url must be an absolute"},
		{name: "tls host mismatch", replace: [2]string{"https://forecrew.example.com/", "https://other.example.com\n  tls_domain: forecrew.example.com"}, want: "must be https://forecrew.example.com"},
		{name: "tls over http", replace: [2]string{"https://forecrew.example.com/", "http://forecrew.example.com\n  tls_domain: forecrew.example.com"}, want: "must be https://"},
		{name: "no app id", replace: [2]string{"app_id: 12345", "app_id: 0"}, want: "github.app_id"},
		{name: "bad adapter", replace: [2]string{"adapter: codex", "adapter: gemini"}, want: "runtimes.codex.adapter"},
		{name: "no vendor", replace: [2]string{"vendor: openai", "vendor: ''"}, want: "runtimes.codex.vendor"},
		{name: "bad runtime name", replace: [2]string{"  codex:\n", "  Codex_X:\n"}, want: "runtimes.Codex_X: name must match"},
		{name: "bad tier key", replace: [2]string{"best: {", "ultra: {"}, want: "model_tiers.ultra"},
		{name: "unknown role runtime", replace: [2]string{"{runtime: codex, tier: best}", "{runtime: gemini, tier: best}"}, want: `roles.reviewer.runtime "gemini"`},
		{name: "bad role tier", replace: [2]string{"tier: best}", "tier: huge}"}, want: "roles.reviewer.tier"},
		{name: "missing model", replace: [2]string{"openai: gpt-pro", "other: gpt-pro"}, want: "model_tiers.best.openai is not set"},
		{name: "same vendor review", replace: [2]string{"{runtime: codex, tier: best}", "{runtime: claude, tier: best}"}, want: "reviewer and developer both use vendor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := validYAML
			if tt.replace[0] != "" {
				if !strings.Contains(y, tt.replace[0]) {
					t.Fatalf("test fixture does not contain %q", tt.replace[0])
				}
				y = strings.Replace(y, tt.replace[0], tt.replace[1], 1)
			}
			_, err := Parse([]byte(y), env(tt.env))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
		})
	}
}

func TestParseSameVendorAllowed(t *testing.T) {
	y := strings.Replace(validYAML, "{runtime: codex, tier: best}", "{runtime: claude, tier: best}", 1)
	y = strings.Replace(y, "allow_same_vendor_review: false", "allow_same_vendor_review: true", 1)
	if _, err := Parse([]byte(y), env(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := Parse(nil, env(nil))
	for _, want := range []string{"public_base_url", "github.app_id", "at least one runtime"} {
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forecrew.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, env(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(nil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestSameVendorReview(t *testing.T) {
	c := &Config{
		Runtimes: map[string]Runtime{"a": {Vendor: "anthropic"}, "a2": {Vendor: "anthropic"}, "o": {Vendor: "openai"}},
		Roles:    map[string]Role{"developer": {Runtime: "a"}, "reviewer": {Runtime: "o"}},
	}
	if v, ok := c.SameVendorReview(); ok {
		t.Errorf("different vendors: SameVendorReview = %q, true", v)
	}
	c.Roles["reviewer"] = Role{Runtime: "a2"}
	if v, ok := c.SameVendorReview(); !ok || v != "anthropic" {
		t.Errorf("same vendor: SameVendorReview = %q, %v; want anthropic, true", v, ok)
	}
	c.Policy.AllowSameVendorReview = true
	if _, ok := c.SameVendorReview(); ok {
		t.Error("allowed same vendor reported")
	}
	c.Policy.AllowSameVendorReview = false
	delete(c.Roles, "reviewer")
	if _, ok := c.SameVendorReview(); ok {
		t.Error("missing reviewer reported")
	}
}

func TestParseActions(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Actions.MaxInFlight != DefaultActionsMaxInFlight {
		t.Errorf("default max_in_flight = %d", c.Actions.MaxInFlight)
	}
	c, err = Parse([]byte(validYAML+"actions:\n  max_in_flight: 3\n"), env(nil))
	if err != nil || c.Actions.MaxInFlight != 3 {
		t.Errorf("max_in_flight = %d, %v", c.Actions.MaxInFlight, err)
	}
	if _, err = Parse([]byte(validYAML+"actions:\n  max_in_flight: -1\n"), env(nil)); !errors.Is(err, ErrInvalid) {
		t.Errorf("negative max_in_flight: %v", err)
	}
}

func TestAcceptRecommendations(t *testing.T) {
	src := strings.Replace(validYAML, "policy:", "policy:\n  accept_recommendations: [technical, adhoc]", 1)
	c, err := Parse([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Policy.AcceptRecommendations, []domain.CouncilKind{domain.CouncilTechnical, domain.CouncilAdhoc}) {
		t.Fatalf("accept_recommendations = %v", c.Policy.AcceptRecommendations)
	}
	src = strings.Replace(validYAML, "policy:", "policy:\n  accept_recommendations: [debate]", 1)
	if _, err := Parse([]byte(src), env(nil)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `unknown council kind "debate"`) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestModelOn(t *testing.T) {
	c := &Config{
		Roles:      map[string]Role{"architect": {Runtime: "claude", Tier: TierBest}},
		Runtimes:   map[string]Runtime{"claude": {Adapter: AdapterClaudeCode, Vendor: "anthropic"}, "gpt": {Adapter: AdapterCodex, Vendor: "openai"}},
		ModelTiers: map[Tier]map[string]string{TierBest: {"anthropic": "a-best", "openai": "o-best"}},
	}
	if m, err := c.ModelOn("architect", AdapterCodex); err != nil || m != "o-best" {
		t.Fatalf("codex = %q, %v", m, err)
	}
	if _, err := c.ModelOn("ghost", AdapterCodex); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("unknown role: %v", err)
	}
	if _, err := c.ModelOn("architect", "other"); err == nil {
		t.Fatal("an adapter without a runtime gave a model")
	}
}

// opencodeYAML puts the developer on a DeepSeek runtime of opencode.
var opencodeYAML = strings.NewReplacer(
	"  codex:\n", "  deepseek:\n    adapter: opencode\n    vendor: deepseek\n    credential_env: [DEEPSEEK_API_KEY]\n  codex:\n",
	"strong: {anthropic: claude-sonnet,", "strong: {deepseek: deepseek/deepseek-chat, anthropic: claude-sonnet,",
	"developer: {runtime: claude,", "developer: {runtime: deepseek,",
).Replace(validYAML)

func opencodeEnv() func(string) string {
	return env(map[string]string{"DEEPSEEK_API_KEY": "sk-ds"})
}

func TestParseOpencode(t *testing.T) {
	c, err := Parse([]byte(opencodeYAML), opencodeEnv())
	if err != nil {
		t.Fatal(err)
	}
	adapter, model, err := c.Model("developer")
	if err != nil || adapter != AdapterOpencode || model != "deepseek/deepseek-chat" {
		t.Errorf("Model(developer) = %s, %s, %v", adapter, model, err)
	}
	if e := c.Runtimes["deepseek"].Endpoint(); e != nil {
		t.Errorf("endpoint without base_url: %+v", e)
	}
}

func TestParseOpencodeInvalid(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{"no provider", "deepseek/deepseek-chat", "deepseek-chat", "model_tiers.strong.deepseek"},
		{"no model", "deepseek/deepseek-chat", "deepseek/", "provider/model"},
		{"expansion", "deepseek/deepseek-chat", "'deepseek/{env:HOME}'", "provider/model"},
		{"unknown adapter", "adapter: opencode", "adapter: aider", "runtimes.deepseek.adapter must be one of claude-code, codex, opencode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(opencodeYAML, tt.old, tt.new, 1)), opencodeEnv())
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestOpencodeSameVendorReview(t *testing.T) {
	both := strings.Replace(opencodeYAML, "reviewer: {runtime: codex, tier: best}", "reviewer: {runtime: deepseek, tier: strong}", 1)
	if _, err := Parse([]byte(both), opencodeEnv()); err == nil || !strings.Contains(err.Error(), `vendor "deepseek"`) {
		t.Fatalf("err = %v", err)
	}
	// Two runtimes of one adapter are different vendors: the vendor counts.
	two := strings.NewReplacer(
		"  codex:\n", "  local:\n    adapter: opencode\n    vendor: ollama\n  codex:\n",
		"strong: {deepseek:", "strong: {ollama: ollama/qwen3, deepseek:",
		"reviewer: {runtime: codex, tier: best}", "reviewer: {runtime: local, tier: strong}",
	).Replace(opencodeYAML)
	if _, err := Parse([]byte(two), opencodeEnv()); err != nil {
		t.Fatal(err)
	}
}

func TestBaseURL(t *testing.T) {
	tests := []struct {
		url  string
		want string // part of the error; empty means valid
	}{
		{"https://api.example.com/v1", ""},
		{"https://8.8.8.8/v1", ""},
		{"http://localhost:11434/v1", ""},
		{"http://LOCALHOST:11434/v1", ""},
		{"http://127.0.0.1:1234/v1", ""},
		{"http://[::1]:1234/v1", ""},
		{"http://10.1.2.3/v1", ""},
		{"http://172.16.0.1/v1", ""},
		{"http://192.168.1.10:11434/v1", ""},
		{"http://[fd00::1]/v1", ""},
		{"http://api.example.com/v1", "http is only allowed"},
		{"http://ollama.internal/v1", "http is only allowed"},
		{"http://8.8.8.8/v1", "http is only allowed"},
		{"http://169.254.169.254/latest", "http is only allowed"},
		{"http://0.0.0.0/v1", "http is only allowed"},
		{"ftp://example.com", "absolute http(s) URL"},
		{"api.example.com/v1", "absolute http(s) URL"},
		{"https://", "absolute http(s) URL"},
		{"https://user:key@api.example.com/v1", "credentials"},
		{"https://api.example.com/v1?key=abc", "credentials"},
		{"https://api.example.com/v1#x", "credentials"},
		{"https://api.example.com/{env:HOME}", "braces"},
		{"https://bad host/", "not a valid URL"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			yaml := strings.Replace(opencodeYAML, "vendor: deepseek\n", "vendor: deepseek\n    base_url: \""+tt.url+"\"\n", 1)
			c, err := Parse([]byte(yaml), opencodeEnv())
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if e := c.Runtimes["deepseek"].Endpoint(); e == nil || e.BaseURL != tt.url || e.KeyEnv != "DEEPSEEK_API_KEY" {
					t.Fatalf("endpoint = %+v", e)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "runtimes.deepseek.base_url") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBaseURLOnEveryAdapter(t *testing.T) {
	yaml := strings.NewReplacer(
		"vendor: anthropic\n", "vendor: anthropic\n    base_url: https://proxy.example.com\n",
		"vendor: openai\n", "vendor: openai\n    base_url: https://proxy.example.com/v1\n",
	).Replace(validYAML)
	c, err := Parse([]byte(yaml), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Runtimes["codex"].Endpoint(); e == nil || e.KeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("endpoint = %+v", e)
	}
}

func TestEndpointWithoutKey(t *testing.T) {
	if e := (Runtime{BaseURL: "http://localhost:11434/v1"}).Endpoint(); e == nil || e.KeyEnv != "" {
		t.Fatalf("endpoint = %+v", e)
	}
}

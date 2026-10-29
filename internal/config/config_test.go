package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

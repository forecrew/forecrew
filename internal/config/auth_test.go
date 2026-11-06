package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

const authYAML = `
auth:
  session_idle: 12h
  session_absolute: 168h
  token_default_ttl: 720h
  token_login: true
  instance_admins: [ana@example.com, 'github:583231']
  connections:
    github:
      kind: github
      client_id: Iv1.abc
      client_secret_ref: env:FORECREW_GITHUB_CLIENT_SECRET
    okta:
      kind: oidc
      issuer: https://acme.okta.com
      client_id: forecrew
      client_secret_ref: secret:okta-client
      group_claim: groups
`

func TestParseAuth(t *testing.T) {
	cfg, err := Parse([]byte(validYAML+authYAML), env(map[string]string{"FORECREW_GITHUB_CLIENT_SECRET": "s3cr3t"}))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.SessionIdle != 12*time.Hour || a.SessionAbsolute != 168*time.Hour || a.TokenDefaultTTL != 720*time.Hour || !a.TokenLogin {
		t.Fatalf("auth = %+v", a)
	}
	gh := a.Connections["github"].Domain("github")
	if gh.Kind != domain.ConnectionGitHub || gh.ClientSecretRef != "env:FORECREW_GITHUB_CLIENT_SECRET" || !gh.Enabled || gh.AccountID != [16]byte{} {
		t.Fatalf("github connection = %+v", gh)
	}
	if okta := a.Connections["okta"].Domain("okta"); okta.GroupClaim != "groups" || okta.Issuer != "https://acme.okta.com" {
		t.Fatalf("okta connection = %+v", okta)
	}
	if !a.IsInstanceAdmin("", "583231") || !a.IsInstanceAdmin("ANA@example.com", "") || !a.IsInstanceAdmin("bob@example.com", "583231") {
		t.Error("IsInstanceAdmin refuses an admin")
	}
	if a.IsInstanceAdmin("bob@example.com", "1") || a.IsInstanceAdmin("", "") || a.IsInstanceAdmin("583231", "ana@example.com") || a.IsInstanceAdmin("github:583231", "") {
		t.Error("IsInstanceAdmin accepts someone else")
	}
}

func TestParseAuthDefaults(t *testing.T) {
	cfg, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.SessionIdle != DefaultSessionIdle || a.SessionAbsolute != DefaultSessionAbsolute || a.TokenDefaultTTL != DefaultTokenTTL || a.TokenLogin || len(a.Connections) != 0 {
		t.Fatalf("defaults = %+v", a)
	}
}

func TestParseAuthInvalid(t *testing.T) {
	secret := map[string]string{"FORECREW_GITHUB_CLIENT_SECRET": "s3cr3t"}
	tests := []struct {
		name    string
		replace [2]string
		env     map[string]string
		want    string
	}{
		{name: "missing env secret", want: "auth.connections.github: FORECREW_GITHUB_CLIENT_SECRET is not set"},
		{name: "secret value in the file", replace: [2]string{"env:FORECREW_GITHUB_CLIENT_SECRET", "ghs_0123456789abcdef"}, env: secret, want: "client_secret_ref must be env:NAME, secret:name or store:name"},
		{name: "client_secret field", replace: [2]string{"client_secret_ref: secret:okta-client", "client_secret: x"}, env: secret, want: "field client_secret not found"},
		{name: "jit on an instance connection", replace: [2]string{"group_claim: groups", "jit: {enabled: true}"}, env: secret, want: "field jit not found"},
		{name: "bad kind", replace: [2]string{"kind: oidc", "kind: saml"}, env: secret, want: "auth.connections.okta"},
		{name: "http issuer", replace: [2]string{"https://acme.okta.com", "http://acme.okta.com"}, env: secret, want: "issuer must be an https URL"},
		{name: "bad name", replace: [2]string{"    okta:", "    Okta SSO:"}, env: secret, want: "auth.connections.Okta SSO"},
		{name: "idle longer than absolute", replace: [2]string{"session_idle: 12h", "session_idle: 200h"}, env: secret, want: "session_idle must not be longer"},
		{name: "negative ttl", replace: [2]string{"token_default_ttl: 720h", "token_default_ttl: -1h"}, env: secret, want: "durations must be positive"},
		{name: "bad duration", replace: [2]string{"token_default_ttl: 720h", "token_default_ttl: 90 days"}, env: secret, want: "yaml"},
		{name: "empty admin", replace: [2]string{"[ana@example.com,", "['',"}, env: secret, want: "must be an email or github:<user ID>"},
		{name: "github login admin", replace: [2]string{"[ana@example.com,", "[octocat,"}, env: secret, want: "not a GitHub login"},
		{name: "github admin without an ID", replace: [2]string{"'github:583231'", "'github:'"}, env: secret, want: "must be an email or github:<user ID>"},
		{name: "github admin by login", replace: [2]string{"'github:583231'", "'github:octocat'"}, env: secret, want: "must be an email or github:<user ID>"},
		{name: "two at signs", replace: [2]string{"[ana@example.com,", "['a@b@c',"}, env: secret, want: "must be an email or github:<user ID>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := validYAML + authYAML
			if tt.replace[0] != "" {
				if !strings.Contains(y, tt.replace[0]) {
					t.Fatalf("fixture does not contain %q", tt.replace[0])
				}
				y = strings.Replace(y, tt.replace[0], tt.replace[1], 1)
			}
			_, err := Parse([]byte(y), env(tt.env))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "ghs_0123") {
				t.Fatalf("the error repeats a secret: %v", err)
			}
		})
	}
}

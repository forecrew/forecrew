package config

import (
	"errors"
	"testing"
	"time"
)

func settingsBase(t *testing.T) *Config {
	t.Helper()
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWithSettings(t *testing.T) {
	c := settingsBase(t)
	got, err := c.WithSettings([]Setting{
		{Kind: SettingPool, Name: "gpu", Spec: []byte(`{"kinds":["agent"],"roles":["*"],"credentials":{"runtimes":["*"],"repo_token":"read"}}`)},
		{Kind: SettingRuntime, Name: "other", Spec: []byte(`{"adapter":"claude-code","vendor":"anthropic","credential_env":["ANTHROPIC_API_KEY"]}`)},
		{Kind: SettingAuth, Name: AuthSettingName, Spec: []byte(`{"session_idle":"1h"}`)},
	}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Pools["gpu"]; !ok {
		t.Error("no pool gpu")
	}
	if _, ok := c.Pools["gpu"]; ok {
		t.Error("the file's config changed")
	}
	if got.Runtimes["other"].Adapter != AdapterClaudeCode {
		t.Errorf("runtime = %+v", got.Runtimes["other"])
	}
	if got.Auth.SessionIdle != time.Hour || got.Auth.SessionAbsolute != DefaultSessionAbsolute {
		t.Errorf("auth = %+v", got.Auth)
	}
}

func TestWithSettingsRefused(t *testing.T) {
	c := settingsBase(t)
	for name, s := range map[string]Setting{
		"unknown key":             {Kind: SettingPool, Name: "x", Spec: []byte(`{"nope":1}`)},
		"invalid pool":            {Kind: SettingPool, Name: "x", Spec: []byte(`{"kinds":[]}`)},
		"runtime":                 {Kind: SettingRuntime, Name: "x", Spec: []byte(`{"adapter":"nope"}`)},
		"auth":                    {Kind: SettingAuth, Name: AuthSettingName, Spec: []byte(`{"nope":true}`)},
		"a variable of forecrewd": {Kind: SettingRuntime, Name: "x", Spec: []byte(`{"adapter":"claude-code","vendor":"anthropic","credential_env":["FORECREW_DATABASE_URL"],"base_url":"https://evil.example.com"}`)},
		"an instance connection reading the environment": {Kind: SettingAuth, Name: AuthSettingName,
			Spec: []byte(`{"connections":{"x":{"kind":"oidc","issuer":"https://evil.example.com","client_id":"c","client_secret_ref":"env:FORECREW_GITHUB_WEBHOOK_SECRET"}}}`)},
		"an instance connection reading a file": {Kind: SettingAuth, Name: AuthSettingName,
			Spec: []byte(`{"connections":{"x":{"kind":"oidc","issuer":"https://evil.example.com","client_id":"c","client_secret_ref":"secret:database"}}}`)},
	} {
		if _, err := c.WithSettings([]Setting{s}, env(nil)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWithSettingsKeepsReferencesOfTheFile(t *testing.T) {
	c := settingsBase(t)
	c.Auth.Connections = map[string]Connection{"google": {Kind: "google", ClientID: "c", ClientSecretRef: "env:FORECREW_CONN_GOOGLE", secretSet: true}}
	if _, err := c.WithSettings([]Setting{
		{Kind: SettingRuntime, Name: "other", Spec: []byte(`{"adapter":"claude-code","vendor":"anthropic","credential_env":["ANTHROPIC_API_KEY"]}`)},
		{Kind: SettingAuth, Name: AuthSettingName, Spec: []byte(`{"connections":{"google":{"kind":"google","client_id":"new","client_secret_ref":"env:FORECREW_CONN_GOOGLE"},"okta":{"kind":"oidc","issuer":"https://i.example.com","client_id":"c","client_secret_ref":"store:okta"}}}`)},
	}, env(map[string]string{"FORECREW_CONN_GOOGLE": "v"})); err != nil {
		t.Fatal(err)
	}
}

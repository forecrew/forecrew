package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFleetAgent(t *testing.T) {
	env := map[string]string{EnvProviderToken: "fcp_x", EnvJoinCode: "K7PQ-2M4X"}
	y := "server_url: https://forecrew.example.com\nkind: microvm\ntoken_file: /var/lib/agent/token\n"
	a, err := ParseFleetAgent([]byte(y), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if a.ServerURL != "https://forecrew.example.com" || a.Kind != "microvm" || a.TokenFile != "/var/lib/agent/token" || a.Token != "fcp_x" || a.JoinCode != "K7PQ-2M4X" {
		t.Fatalf("a = %+v", a)
	}
}

func TestParseFleetAgentProblems(t *testing.T) {
	none := func(string) string { return "" }
	_, err := ParseFleetAgent([]byte("server_url: nope\nkind: vmware\n"), none)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"server_url", "kind", "token_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %s", err, want)
		}
	}
	if _, err := ParseFleetAgent([]byte("unknown: 1\n"), none); err == nil {
		t.Error("unknown field passed")
	}
}

func TestLoadFleetAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if _, err := LoadFleetAgent(path, os.Getenv); err == nil {
		t.Error("missing file passed")
	}
	if err := os.WriteFile(path, []byte("server_url: http://localhost\nkind: tart\ntoken_file: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFleetAgent(path, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}

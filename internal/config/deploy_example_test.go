package config

import (
	"os"
	"strings"
	"testing"
)

// The example config of the Compose stack must stay valid in both modes.
func TestComposeExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../deploy/compose/forecrew.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	getenv := env(map[string]string{"CODEX_API_KEY": "sk-codex"})

	plain, err := Parse(data, getenv)
	if err != nil {
		t.Fatalf("plain HTTP mode: %v", err)
	}
	if plain.Server.TLSDomain != "" || plain.Server.Listen != ":80" {
		t.Errorf("plain HTTP mode server = %+v", plain.Server)
	}

	tls := strings.NewReplacer(
		"  # tls_domain:", "  tls_domain:",
		"  # cert_cache_dir:", "  cert_cache_dir:",
		"  listen: \":80\"\n", "",
	).Replace(string(data))
	c, err := Parse([]byte(tls), getenv)
	if err != nil {
		t.Fatalf("own certificates mode: %v", err)
	}
	if c.Server.TLSDomain != "forecrew.example.com" || c.Server.CertCacheDir != "/var/lib/forecrew/certs" || c.Server.Listen != ":443" {
		t.Errorf("own certificates mode server = %+v", c.Server)
	}
}

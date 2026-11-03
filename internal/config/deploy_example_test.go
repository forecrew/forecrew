package config

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The example config of the Compose stack must stay valid in both modes.
func TestComposeExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../deploy/compose/forecrew.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	getenv := env(map[string]string{
		"CODEX_API_KEY": "sk-codex", EnvEvidenceAccessKeyID: "GKaccess", EnvEvidenceSecretAccessKey: "secret",
	})

	plain, err := Parse(data, getenv)
	if err != nil {
		t.Fatalf("plain HTTP mode: %v", err)
	}
	if plain.Server.TLSDomain != "" || plain.Server.Listen != ":80" {
		t.Errorf("plain HTTP mode server = %+v", plain.Server)
	}
	// The bucket and endpoint are those garage-init and the garage service create.
	if e := plain.Evidence; e == nil || e.Endpoint != "http://garage:3900" || e.Bucket != "forecrew-evidence" || !e.PathStyle || e.AccessKeyID != "GKaccess" {
		t.Errorf("evidence = %+v", e)
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

// forecrewd must not receive Garage's admin token or RPC secret: it reads
// .env only, and the Garage secrets live in garage.env.
func TestComposeKeepsGarageSecretsFromForecrewd(t *testing.T) {
	data, err := os.ReadFile("../../deploy/compose/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Services map[string]struct {
			EnvFile     yaml.Node         `yaml:"env_file"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	envFiles := func(n yaml.Node) []string {
		var many []string
		if n.Decode(&many) == nil {
			return many
		}
		var one string
		_ = n.Decode(&one)
		return []string{one}
	}
	for name, svc := range file.Services {
		files := strings.Join(envFiles(svc.EnvFile), " ")
		wantGarage := name == "garage" || name == "garage-init"
		if got := strings.Contains(files, "garage.env"); got != wantGarage {
			t.Errorf("service %s env_file %q: garage.env = %v, want %v", name, files, got, wantGarage)
		}
		for k := range svc.Environment {
			if strings.HasPrefix(k, "GARAGE_") {
				t.Errorf("service %s sets %s directly", name, k)
			}
		}
	}
	if !strings.Contains(strings.Join(envFiles(file.Services["forecrewd"].EnvFile), " "), ".env") {
		t.Error("forecrewd does not read .env")
	}
}

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

func TestParseFleetAgentImageBuild(t *testing.T) {
	none := func(string) string { return "" }
	head := "server_url: https://f.example.com\nkind: proxmox\ntoken_file: /t\n"
	ok := head + `image_build:
  templates_dir: /opt/forecrew/deploy/images
  worker: /opt/forecrew/forecrew-worker.exe
  out_dir: /var/lib/forecrew-images
  uri_base: https://images.example.com
  iso_dir: /srv/isos
  guest_tools: /srv/virtio.exe
  guest_tools_sha256: abc
`
	a, err := ParseFleetAgent([]byte(ok), none)
	if err != nil {
		t.Fatal(err)
	}
	if b := a.ImageBuild; b == nil || b.ISODir != "/srv/isos" || b.GuestTools != "/srv/virtio.exe" || b.Packer != "" {
		t.Fatalf("image_build = %+v", a.ImageBuild)
	}
	if a, err := ParseFleetAgent([]byte(head), none); err != nil || a.ImageBuild != nil {
		t.Fatalf("without it: %+v, %v", a, err)
	}
	bad := head + "image_build:\n  templates_dir: rel\n  uri_base: ftp://x\n  guest_tools: /g\n  cache_dir: c\n"
	_, err = ParseFleetAgent([]byte(bad), none)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"templates_dir", "worker", "out_dir", "iso_dir", "uri_base", "guest_tools", "cache_dir"} {
		if !strings.Contains(err.Error(), "image_build."+want) {
			t.Errorf("%v lacks %s", err, want)
		}
	}
	other := strings.Replace(ok, "kind: proxmox", "kind: microvm", 1)
	if _, err := ParseFleetAgent([]byte(other), none); err == nil || !strings.Contains(err.Error(), "hyperv or proxmox") {
		t.Errorf("microvm: %v", err)
	}
}

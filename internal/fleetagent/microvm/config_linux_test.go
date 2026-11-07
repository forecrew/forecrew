package microvm

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "missing.yaml"))
	if err != nil || c.StateDir != "/var/lib/forecrew-microvm" || c.DNS != "1.1.1.1" {
		t.Errorf("defaults = %+v, %v", c, err)
	}
	path := filepath.Join(dir, "microvm.yaml")
	_ = os.WriteFile(path, []byte("state_dir: /srv/vm\nsubnet: 10.200.0.0/20\ngpus: [\"0000:01:00.0\"]\n"), 0o600)
	if c, err = LoadConfig(path); err != nil || c.StateDir != "/srv/vm" || c.Subnet != "10.200.0.0/20" || len(c.GPUs) != 1 || c.UIDBase != 900000 {
		t.Errorf("config = %+v, %v", c, err)
	}
	_ = os.WriteFile(path, []byte("state_dir: rel\nsubnet: 10.0.0.0/30\ndns: x\nuid_base: 5\nimage_cache_days: 0\negress: all\n"), 0o600)
	_, err = LoadConfig(path)
	for _, want := range []string{"state_dir", "subnet", "dns", "uid_base", "image_cache_days", "egress"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
	_ = os.WriteFile(path, []byte("unknown: 1\n"), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("a directory was read")
	}
	if _, err := New(Config{StateDir: "x"}, nil); err == nil {
		t.Error("New took a bad config")
	}
}

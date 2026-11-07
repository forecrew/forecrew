package proxmox

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxmox.yaml")
	write(t, path, `
api_url: https://pve1.example.com:8006
nodes: [pve1, pve2]
bridge: vmbr3
vlan: 20
dns: [192.168.1.1]
max_machines: 3
poll_interval: 500ms
ca_file: /etc/pve/pve-root-ca.pem
tls_server_name: pve1.example.com
`, 0o600)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bridge != "vmbr3" || c.VLAN != 20 || c.MaxMachines != 3 || c.PollInterval != 500*time.Millisecond || len(c.Nodes) != 2 || c.TLSServerName != "pve1.example.com" {
		t.Errorf("config = %+v", c)
	}
	if c.TokenFile != "/etc/forecrew-fleet-agent/proxmox-token" || c.TrustedKeysFile != "/etc/forecrew-fleet-agent/image-keys.pub" {
		t.Errorf("defaults lost: %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"empty file":      "",
		"no nodes":        "bridge: vmbr0\n",
		"bad node":        "nodes: ['a/b']\n",
		"unknown key":     "nodes: [pve]\nstorage: x\n",
		"relative token":  "nodes: [pve]\ntoken_file: token\n",
		"relative keys":   "nodes: [pve]\ntrusted_keys_file: keys\n",
		"relative ca":     "nodes: [pve]\nca_file: ca.pem\n",
		"bad bridge":      "nodes: [pve]\nbridge: 'a b'\n",
		"bad vlan":        "nodes: [pve]\nvlan: 5000\n",
		"ipv6 dns":        "nodes: [pve]\ndns: ['::1']\n",
		"no machines":     "nodes: [pve]\nmax_machines: 0\n",
		"zero poll":       "nodes: [pve]\npoll_interval: 0s\n",
		"not yaml":        "nodes: [\n",
		"wrong type":      "nodes: pve\n",
		"first dash node": "nodes: ['-x']\n",
	}
	for name, body := range tests {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		write(t, path, body, 0o600)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadConfig(dir); err == nil { // a directory
		t.Error("a directory was read")
	}
}

func TestParseShape(t *testing.T) {
	cfg := Config{Nodes: []string{"pve"}, Bridge: "vmbr0", VLAN: 5}
	s, err := parseShape(map[string]any{"template": 9000}, cfg) // one node: no need to name it
	if err != nil || s.Node != "pve" || s.Disk != "scsi0" || s.Bridge != "vmbr0" || *s.VLAN != 5 {
		t.Fatalf("%+v %v", s, err)
	}
	cfg.Nodes = []string{"a", "b"}
	if _, err := parseShape(map[string]any{"template": 9000}, cfg); err == nil || !strings.Contains(err.Error(), "not one of the nodes") {
		t.Errorf("which node? %v", err)
	}
	if _, err := parseShape(map[string]any{"template": "x"}, cfg); err == nil {
		t.Error("a template that is text")
	}
	for _, id := range []int{0, 99, -1} {
		if _, err := parseShape(map[string]any{"template": id, "node": "a"}, cfg); err == nil {
			t.Errorf("template %d accepted", id)
		}
	}
}

func TestDiskBytes(t *testing.T) {
	tests := map[string]struct {
		n  int64
		ok bool
	}{
		"local-lvm:vm-1-disk-0,size=32G":          {32 << 30, true},
		"local:1/disk.qcow2,discard=on,size=512M": {512 << 20, true},
		"zfs:vm-2-disk-0,size=2T,ssd=1":           {2 << 40, true},
		"x:y,size=100K":                           {100 << 10, true},
		"x:y,size=1000":                           {1000, true},
		"x:y":                                     {0, false},
		"x:y,size=":                               {0, false},
		"x:y,size=abcG":                           {0, false},
		"x:y,size=-5G":                            {0, false},
		"local-lvm:vm-1-disk-0,size=32G,cache=writeback": {32 << 30, true},
	}
	for line, want := range tests {
		n, ok := diskBytes(line)
		if n != want.n || ok != want.ok {
			t.Errorf("%q = %d, %v; want %d, %v", line, n, ok, want.n, want.ok)
		}
	}
}

func TestPVETags(t *testing.T) {
	got := pveTags(map[string]string{"forecrew:machine": "AbC-123", "forecrew:org": "o r;g", "forecrew:job": "J-7/x", "other": "ignored"})
	if got != "forecrew-job-j-7x;forecrew-machine-abc-123;forecrew-org-org" {
		t.Errorf("tags = %q", got)
	}
	if pveTags(nil) != "" {
		t.Error("tags of nothing")
	}
}

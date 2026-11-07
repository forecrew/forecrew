package hyperv

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hyperv.yaml")
	writeFile(t, path, `
switch: Lab
state_dir: D:\Forecrew
vlan: 30
generation: 2
dns: [192.168.1.1]
max_machines: 4
script_timeout: 2m
powershell: C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.VMDir != `D:\Forecrew\vms` || c.ImageDir != `D:\Forecrew\images` || c.VLAN != 30 || c.Generation != 2 || c.MaxMachines != 4 || c.ScriptTimeout != 2*time.Minute {
		t.Errorf("config = %+v", c)
	}
	if c.TrustedKeysFile != `C:\ProgramData\forecrew-fleet-agent\image-keys.pub` {
		t.Errorf("default lost: %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"empty file":      "",
		"no switch":       "vlan: 3\n",
		"bad switch":      "switch: 'a;b'\n",
		"unknown key":     "switch: Lab\ninsecure: true\n",
		"unc share":       "switch: Lab\nstate_dir: '\\\\host\\share'\n",
		"relative state":  "switch: Lab\nstate_dir: forecrew\n",
		"shell in vm_dir": "switch: Lab\nvm_dir: 'D:\\vms;calc'\n",
		"quote in images": "switch: Lab\nimage_dir: 'D:\\im\"ages'\n",
		"bad keys path":   "switch: Lab\ntrusted_keys_file: keys\n",
		"bad vlan":        "switch: Lab\nvlan: 5000\n",
		"generation 3":    "switch: Lab\ngeneration: 3\n",
		"ipv6 dns":        "switch: Lab\ndns: ['::1']\n",
		"no machines":     "switch: Lab\nmax_machines: 0\n",
		"no powershell":   "switch: Lab\npowershell: ''\n",
		"no cache days":   "switch: Lab\nimage_cache_days: 0\n",
		"short timeout":   "switch: Lab\nscript_timeout: 1ms\n",
		"not yaml":        "switch: [\n",
		"wrong type":      "switch: [a]\n",
	}
	for name, body := range tests {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		writeFile(t, path, body)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Error("a directory was read")
	}
}

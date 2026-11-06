package fleetagent

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/testkit"
)

func fakeHost(goos string) host {
	return host{
		goos: goos, goarch: "arm64", cpus: 8, name: func() (string, error) { return "nas", nil },
		memoryMB: func() (int, error) { return 16384, nil }, diskFreeGB: func(string) (int, error) { return 412, nil },
		exists: func(string) bool { return false }, readFile: func(string) (string, error) { return "", errors.New("none") },
		lookPath: func(string) bool { return false }, getenv: func(string) string { return "" },
	}
}

func check(c domain.ProviderCapabilities, name string) domain.ProviderCheck {
	i := slices.IndexFunc(c.Checks, func(k domain.ProviderCheck) bool { return k.Name == name })
	if i < 0 {
		return domain.ProviderCheck{}
	}
	return c.Checks[i]
}

func TestDetectHost(t *testing.T) {
	h := fakeHost("linux")
	h.exists = func(p string) bool { return p == "/dev/kvm" }
	c := detect(t.Context(), h, nil, "v1")
	if c.Host != "nas" || c.AgentVersion != "v1" || c.MaxCPU != 8 || c.MaxMemoryMB != 16384 || c.MaxDiskGB != 412 || c.OS[0] != domain.OSLinux || c.Arch[0] != domain.ArchARM64 {
		t.Fatalf("c = %+v", c)
	}
	if check(c, "kvm").Status != "ok" || check(c, "memory").Detail != "16384 MB" || check(c, "disk").Detail != "412 GB free" {
		t.Fatalf("checks = %+v", c.Checks)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := detect(t.Context(), fakeHost("linux"), nil, "v1"); check(got, "kvm").Status != "warn" {
		t.Fatalf("no kvm: %+v", got.Checks)
	}
	if got := detect(t.Context(), fakeHost("darwin"), nil, "v1"); check(got, "kvm").Name != "" || got.OS[0] != domain.OSMacOS {
		t.Fatalf("darwin: %+v", got)
	}
	unknown := fakeHost("plan9")
	unknown.goarch = "mips"
	if got := detect(t.Context(), unknown, nil, "v1"); len(got.OS) != 0 || len(got.Arch) != 0 {
		t.Fatalf("plan9: %+v", got)
	}
}

func TestDetectUnreadable(t *testing.T) {
	h := fakeHost("windows")
	h.memoryMB = func() (int, error) { return 0, errors.New("no") }
	h.diskFreeGB = func(string) (int, error) { return 0, errors.New("no") }
	c := detect(t.Context(), h, nil, "v1")
	if check(c, "memory").Status != "warn" || check(c, "disk").Status != "warn" || c.MaxMemoryMB != 0 {
		t.Fatalf("c = %+v", c)
	}
}

func TestDetectProviderSaysFirst(t *testing.T) {
	p := testkit.NewFakeProvider()
	p.Caps = domain.ProviderCapabilities{Host: "pve-1", MaxCPU: 2, MaxMemoryMB: 1024, MaxDiskGB: 10, OS: []domain.MachineOS{domain.OSWindows},
		Arch: []domain.MachineArch{domain.ArchAMD64}, Checks: []domain.ProviderCheck{{Name: "storage", Status: "ok", Detail: "line one\nline two " + strings.Repeat("x", 400)}}}
	c := detect(t.Context(), fakeHost("linux"), p, "v1")
	if c.Host != "pve-1" || c.MaxCPU != 2 || c.MaxMemoryMB != 1024 || c.MaxDiskGB != 10 || c.OS[0] != domain.OSWindows {
		t.Fatalf("c = %+v", c)
	}
	if d := check(c, "storage").Detail; strings.Contains(d, "\n") || len(d) > 200 {
		t.Fatalf("detail %q", d)
	}
}

func TestDetectProviderFailureIsACheckWithoutSecrets(t *testing.T) {
	p := testkit.NewFakeProvider()
	secret := "fcp_" + strings.Repeat("a", 43)
	p.ListErr = errors.New("login with " + secret + " failed")
	c := detect(t.Context(), fakeHost("linux"), p, "v1")
	k := check(c, "provider")
	if k.Status != "fail" || strings.Contains(k.Detail, secret) {
		t.Fatalf("check = %+v", k)
	}
}

func TestThisHost(t *testing.T) {
	c := Detect(t.Context(), nil, "v1")
	if c.MaxCPU == 0 || c.Host == "" || len(c.Checks) < 2 {
		t.Fatalf("c = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if k := DetectKind(); k != "" && !slices.Contains(domain.ProviderKinds(), k) {
		t.Fatalf("kind %q", k)
	}
}

func TestParseMeminfo(t *testing.T) {
	if mb, err := parseMeminfo("MemFree: 1 kB\nMemTotal:       16384000 kB\n"); err != nil || mb != 16000 {
		t.Fatalf("%d, %v", mb, err)
	}
	for _, bad := range []string{"", "MemTotal:\n", "MemTotal: lots kB\n"} {
		if _, err := parseMeminfo(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestDetectKind(t *testing.T) {
	tests := []struct {
		name string
		h    func() host
		want domain.ProviderKind
	}{
		{"windows", func() host { return fakeHost("windows") }, domain.ProviderHyperV},
		{"macos", func() host { return fakeHost("darwin") }, domain.ProviderTart},
		{"other", func() host { return fakeHost("plan9") }, ""},
		{"linux without kvm", func() host { return fakeHost("linux") }, ""},
		{"kvm", func() host {
			h := fakeHost("linux")
			h.exists = func(string) bool { return true }
			return h
		}, domain.ProviderMicroVM},
		{"proxmox", func() host {
			h := fakeHost("linux")
			h.exists = func(string) bool { return true }
			h.lookPath = func(n string) bool { return n == "pveversion" }
			return h
		}, domain.ProviderProxmox},
		{"ec2 by vendor", func() host {
			h := fakeHost("linux")
			h.readFile = func(p string) (string, error) {
				if strings.HasSuffix(p, "sys_vendor") {
					return "Amazon EC2\n", nil
				}
				return "", errors.New("none")
			}
			return h
		}, domain.ProviderAWS},
		{"ec2 by asset tag", func() host {
			h := fakeHost("linux")
			h.readFile = func(p string) (string, error) {
				if strings.HasSuffix(p, "board_asset_tag") {
					return "i-0123\n", nil
				}
				return "other", nil
			}
			return h
		}, domain.ProviderAWS},
	}
	for _, tt := range tests {
		if got := detectKind(tt.h()); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDoctor(t *testing.T) {
	Register(domain.ProviderFake, func() (fleet.Provider, error) { return testkit.NewFakeProvider(), nil })
	all := fakeHost("linux")
	all.exists = func(string) bool { return true }
	all.lookPath = func(string) bool { return true }
	mac := all
	mac.goos = "darwin"
	win := all
	win.goos = "windows"
	ec2 := fakeHost("linux")
	ec2.readFile = func(string) (string, error) { return "Amazon EC2", nil }
	profile := fakeHost("linux")
	profile.getenv = func(n string) string {
		if n == "AWS_PROFILE" {
			return "agent"
		}
		return ""
	}
	tests := []struct {
		kind domain.ProviderKind
		h    host
		fail []string
		ok   []string
	}{
		{domain.ProviderMicroVM, fakeHost("linux"), []string{"kvm", "vmm", "provider"}, nil},
		{domain.ProviderMicroVM, all, []string{"provider"}, []string{"kvm", "vmm"}},
		{domain.ProviderProxmox, fakeHost("windows"), []string{"os"}, nil},
		{domain.ProviderHyperV, fakeHost("linux"), []string{"os", "powershell"}, nil},
		{domain.ProviderHyperV, win, nil, []string{"os", "powershell"}},
		{domain.ProviderTart, mac, nil, []string{"os", "arch", "tart"}},
		{domain.ProviderAWS, fakeHost("linux"), []string{"credentials"}, nil},
		{domain.ProviderAWS, ec2, nil, []string{"credentials"}},
		{domain.ProviderAWS, profile, nil, []string{"credentials"}},
		{"vmware", all, []string{"kind"}, nil},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		doctor(context.Background(), &out, tt.h, tt.kind, "v1")
		for _, name := range tt.fail {
			if !strings.Contains(out.String(), "fail "+name+":") {
				t.Errorf("%s: no failure of %s in:\n%s", tt.kind, name, out.String())
			}
		}
		for _, name := range tt.ok {
			if !strings.Contains(out.String(), "ok   "+name+":") {
				t.Errorf("%s: no ok of %s in:\n%s", tt.kind, name, out.String())
			}
		}
	}
	var out bytes.Buffer
	if doctor(context.Background(), &out, all, domain.ProviderMicroVM, "v1") {
		t.Error("a kind with no provider passed")
	}
	out.Reset()
	Doctor(context.Background(), &out, domain.ProviderFake, "v1")
	if strings.Contains(out.String(), "this build") {
		t.Errorf("a registered provider counts as missing:\n%s", out.String())
	}
}

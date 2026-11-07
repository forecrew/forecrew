package fleetagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// Check statuses (domain.ProviderCheck).
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// host is what the agent can find out about the machine it runs on. Tests
// replace it.
type host struct {
	goos, goarch string
	cpus         int
	name         func() (string, error)
	memoryMB     func() (int, error)
	diskFreeGB   func(path string) (int, error)
	exists       func(path string) bool
	readFile     func(path string) (string, error)
	lookPath     func(name string) bool
	getenv       func(name string) string
}

func thisHost() host {
	return host{
		goos: runtime.GOOS, goarch: runtime.GOARCH, cpus: runtime.NumCPU(), name: os.Hostname, memoryMB: MemoryMB, diskFreeGB: DiskFreeGB,
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		readFile: func(path string) (string, error) {
			b, err := os.ReadFile(path) // #nosec G304 -- fixed paths of this package
			return string(b), err
		},
		lookPath: func(name string) bool {
			_, err := exec.LookPath(name)
			return err == nil
		},
		getenv: os.Getenv,
	}
}

// Detect reports what this place can do: what the provider says, with the
// facts of the host where it says nothing, and the checks of both. The provider
// may be nil, when only the host is known. It holds facts, never secrets.
func Detect(ctx context.Context, p fleet.Provider, version string) domain.ProviderCapabilities {
	return detect(ctx, thisHost(), p, version)
}

func detect(ctx context.Context, h host, p fleet.Provider, version string) domain.ProviderCapabilities {
	var c domain.ProviderCapabilities
	var own []domain.ProviderCheck
	if p != nil {
		var err error
		if c, err = p.Capabilities(ctx); err != nil {
			own = []domain.ProviderCheck{{Name: "provider", Status: statusFail, Detail: oneLine(err.Error())}}
		}
	}
	c.AgentVersion = version
	if c.Host == "" {
		c.Host, _ = h.name() // a host with no name is still a host
	}
	if len(c.OS) == 0 {
		if os, ok := machineOS(h.goos); ok {
			c.OS = []domain.MachineOS{os}
		}
	}
	if len(c.Arch) == 0 {
		if arch, ok := machineArch(h.goarch); ok {
			c.Arch = []domain.MachineArch{arch}
		}
	}
	if c.MaxCPU == 0 {
		c.MaxCPU = h.cpus
	}
	checks := append(c.Checks, own...)
	memory, err := h.memoryMB()
	if err != nil {
		checks = append(checks, domain.ProviderCheck{Name: "memory", Status: statusWarn, Detail: "cannot read it"})
	} else {
		c.MaxMemoryMB = cmpOr(c.MaxMemoryMB, memory)
		checks = append(checks, domain.ProviderCheck{Name: "memory", Status: statusOK, Detail: fmt.Sprintf("%d MB", memory)})
	}
	disk, err := h.diskFreeGB(os.TempDir())
	if err != nil {
		checks = append(checks, domain.ProviderCheck{Name: "disk", Status: statusWarn, Detail: "cannot read it"})
	} else {
		c.MaxDiskGB = cmpOr(c.MaxDiskGB, disk)
		checks = append(checks, domain.ProviderCheck{Name: "disk", Status: statusOK, Detail: fmt.Sprintf("%d GB free", disk)})
	}
	if h.goos == "linux" {
		checks = append(checks, kvmCheck(h))
	}
	for i := range checks {
		checks[i].Detail = oneLine(checks[i].Detail) // forecrewd takes short lines only
	}
	c.Checks = checks
	return c
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func kvmCheck(h host) domain.ProviderCheck {
	if h.exists("/dev/kvm") {
		return domain.ProviderCheck{Name: "kvm", Status: statusOK, Detail: "/dev/kvm is there"}
	}
	return domain.ProviderCheck{Name: "kvm", Status: statusWarn, Detail: "no /dev/kvm"}
}

// oneLine makes an error fit a check's detail: no secrets, one short line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(fleet.CleanError(s)), " ")
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}

func machineOS(goos string) (domain.MachineOS, bool) {
	switch goos {
	case "linux":
		return domain.OSLinux, true
	case "windows":
		return domain.OSWindows, true
	case "darwin":
		return domain.OSMacOS, true
	}
	return "", false
}

func machineArch(goarch string) (domain.MachineArch, bool) {
	switch goarch {
	case "amd64":
		return domain.ArchAMD64, true
	case "arm64":
		return domain.ArchARM64, true
	}
	return "", false
}

// parseMeminfo reads MemTotal of /proc/meminfo, in MB.
func parseMeminfo(s string) (int, error) {
	for line := range strings.SplitSeq(s, "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				break
			}
			kb, err := strconv.Atoi(f[0])
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal")
}

// DetectKind says which kind fits this machine, or empty when none does: what
// the installer offers.
func DetectKind() domain.ProviderKind { return detectKind(thisHost()) }

func detectKind(h host) domain.ProviderKind {
	switch h.goos {
	case "windows":
		return domain.ProviderHyperV
	case "darwin":
		return domain.ProviderTart
	case "linux":
	default:
		return ""
	}
	if h.lookPath("pveversion") {
		return domain.ProviderProxmox
	}
	if onEC2(h) {
		return domain.ProviderAWS
	}
	if h.exists("/dev/kvm") {
		return domain.ProviderMicroVM
	}
	return ""
}

// onEC2 reads the vendor the firmware states; it needs no network.
func onEC2(h host) bool {
	vendor, err := h.readFile("/sys/class/dmi/id/sys_vendor")
	if err == nil && strings.Contains(vendor, "Amazon EC2") {
		return true
	}
	asset, err := h.readFile("/sys/class/dmi/id/board_asset_tag")
	return err == nil && strings.HasPrefix(strings.TrimSpace(asset), "i-")
}

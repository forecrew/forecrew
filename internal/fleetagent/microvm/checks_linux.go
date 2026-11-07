package microvm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
)

// Check statuses (domain.ProviderCheck).
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

func check(name string, err error, bad, ok string) domain.ProviderCheck {
	if err != nil {
		return domain.ProviderCheck{Name: name, Status: bad, Detail: err.Error()}
	}
	return domain.ProviderCheck{Name: name, Status: statusOK, Detail: ok}
}

func found(names ...string) error {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("needs %s on PATH", n)
		}
	}
	return nil
}

// checks are what doctor shows and capabilities report.
func (p *Provider) checks() []domain.ProviderCheck {
	out := []domain.ProviderCheck{
		check("kvm access", openKVM(), statusFail, "/dev/kvm opens"),
		check("root", isRoot(), statusFail, "runs as root, as the jailer and the network need"),
		check("firecracker", found(p.cfg.Firecracker, p.cfg.Jailer), statusFail, "firecracker and jailer found"),
		check("network tools", found("ip", "nft"), statusFail, "ip and nft found"),
		check("ip forwarding", ipForward(), statusFail, "net.ipv4.ip_forward is on"),
		check("image keys", p.keysError(), statusFail, "trusted image keys found"),
		check("reflinks", p.reflinks(), statusWarn, "disks are cloned instantly"),
	}
	if len(p.cfg.GPUs) > 0 {
		out = append(out, check("cloud-hypervisor", found(p.cfg.CloudHypervisor), statusFail, "found, for GPUs"))
	}
	return out
}

func openKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func isRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root: the jailer and the network need it")
	}
	return nil
}

func ipForward() error {
	raw, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != "1" {
		return fmt.Errorf("set net.ipv4.ip_forward=1: machines reach the internet through this host")
	}
	return nil
}

func (p *Provider) keysError() error {
	_, err := p.trustedKeys()
	return err
}

// reflinks tries a clone in the state directory.
func (p *Provider) reflinks() error {
	if err := os.MkdirAll(p.cfg.StateDir, 0o711); err != nil { // #nosec G301 -- as prepareDirs
		return err
	}
	dir, err := os.MkdirTemp(p.cfg.StateDir, ".reflink-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		return err
	}
	if !reflink(src, filepath.Join(dir, "b")) {
		return fmt.Errorf("%s has no reflinks: each machine's disk is a full copy; use btrfs or XFS for fast starts", p.cfg.StateDir)
	}
	return nil
}

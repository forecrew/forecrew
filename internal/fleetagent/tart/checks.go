package tart

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleetagent"
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

// checks are what doctor shows and capabilities report.
func (p *Provider) checks(ctx context.Context) []domain.ProviderCheck {
	out := []domain.ProviderCheck{
		check("apple silicon", p.appleSilicon(), statusFail, "an Apple silicon Mac"),
		check("tart", p.tartVersion(ctx), statusFail, "tart "+p.cfg.MinTartVersion+" or newer"),
		check("disk", p.diskRoom(), statusFail, fmt.Sprintf("at least %d GB free", p.cfg.MinFreeGB)),
		check("image keys", p.keysError(), statusFail, "trusted image keys found"),
		check("sleep", p.sleepOff(ctx), statusWarn, "the Mac does not sleep"),
		check("registry credentials", p.credentials(), statusFail, "found, or none needed"),
	}
	if p.cfg.Softnet {
		out = append(out, check("softnet", p.softnet(), statusFail, "softnet found: machines stay off the LAN and this host"))
	} else if p.cfg.AllowNoSoftnet {
		out = append(out, domain.ProviderCheck{Name: "softnet", Status: statusWarn, Detail: "softnet is off, as allow_no_softnet says: machines reach the LAN and this host"})
	} else {
		out = append(out, domain.ProviderCheck{Name: "softnet", Status: statusFail, Detail: "softnet is off: machines would reach the LAN and this host; turn it on, or set allow_no_softnet on this Mac"})
	}
	return out
}

func (p *Provider) appleSilicon() error {
	if p.goos != "darwin" || p.goarch != "arm64" {
		return fmt.Errorf("runs on an Apple silicon Mac, not %s/%s", p.goos, p.goarch)
	}
	return nil
}

// tartVersion checks that tart runs and is new enough.
func (p *Provider) tartVersion(ctx context.Context) error {
	got, err := p.cli.version(ctx)
	if err != nil {
		return fmt.Errorf("needs %s: %v", p.cfg.Tart, oneLine(err.Error()))
	}
	if compareVersions(got, p.cfg.MinTartVersion) < 0 {
		return fmt.Errorf("tart %s is older than %s: upgrade it", got, p.cfg.MinTartVersion)
	}
	return nil
}

// compareVersions compares dotted numbers.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func (p *Provider) diskRoom() error {
	gb, err := p.freeGB(p.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("cannot read the free disk: %v", err)
	}
	if gb < p.cfg.MinFreeGB {
		return fmt.Errorf("%d GB free, and images and clones need %d: free some room", gb, p.cfg.MinFreeGB)
	}
	return nil
}

func (p *Provider) keysError() error {
	_, err := fleetagent.LoadTrustedKeys(p.cfg.TrustedKeysFile)
	return err
}

// sleepOff reads pmset: a Mac that sleeps stops running jobs.
func (p *Provider) sleepOff(ctx context.Context) error {
	out, err := p.pmset(ctx)
	if err != nil {
		return fmt.Errorf("cannot read pmset: %v", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "sleep" {
			if f[1] == "0" {
				return nil
			}
			return fmt.Errorf("the Mac sleeps after %s minutes: run sudo pmset -a sleep 0", f[1])
		}
	}
	return fmt.Errorf("pmset does not say when the Mac sleeps")
}

func pmset(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g").Output()
	return string(out), err
}

func (p *Provider) softnet() error {
	if !p.lookPath("softnet") {
		return fmt.Errorf("needs softnet on PATH, with the setuid bit or passwordless sudo, or set softnet: false")
	}
	return nil
}

// credentials checks that what the references name is in the agent's
// environment, without reading it out.
func (p *Provider) credentials() error {
	for _, ref := range []string{p.cfg.RegistryUsername, p.cfg.RegistryPassword} {
		if ref != "" && p.getenv(refName(ref)) == "" {
			return fmt.Errorf("%s is not set in the agent's environment", refName(ref))
		}
	}
	return nil
}

// vmProcessName is the process Virtualization.framework runs for each VM.
const vmProcessName = "com.apple.Virtualization.VirtualMachine"

// parseVMProcesses counts the lines of `ps -axo comm=` that are VM processes.
func parseVMProcesses(ps string) int {
	n := 0
	for line := range strings.SplitSeq(ps, "\n") {
		line = strings.TrimSpace(line)
		if line[strings.LastIndexByte(line, '/')+1:] == vmProcessName {
			n++
		}
	}
	return n
}

func countVMProcesses(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "comm=").Output()
	if err != nil {
		return 0, err
	}
	return parseVMProcesses(string(out)), nil
}

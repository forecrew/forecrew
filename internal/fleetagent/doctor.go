package fleetagent

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/forecrew/forecrew/internal/domain"
)

// Doctor prints what a kind needs on this machine and whether it is there, and
// reports whether nothing is missing.
func Doctor(ctx context.Context, w io.Writer, kind domain.ProviderKind, version string) bool {
	return doctor(ctx, w, thisHost(), kind, version)
}

func doctor(ctx context.Context, w io.Writer, h host, kind domain.ProviderKind, version string) bool {
	checks := requirements(h, kind)
	p, err := NewProvider(kind)
	if err != nil {
		checks = append(checks, domain.ProviderCheck{Name: "provider", Status: statusFail, Detail: oneLine(err.Error())})
	}
	checks = append(checks, detect(ctx, h, p, version).Checks...)
	for _, c := range checks {
		fmt.Fprintf(w, "%-4s %s: %s\n", c.Status, c.Name, c.Detail)
	}
	return !slices.ContainsFunc(checks, func(c domain.ProviderCheck) bool { return c.Status == statusFail })
}

// requirements are what a kind cannot work without, checked from outside the
// provider, so they can be read before a provider is set up.
func requirements(h host, kind domain.ProviderKind) []domain.ProviderCheck {
	switch kind {
	case domain.ProviderMicroVM:
		return []domain.ProviderCheck{
			need(h.goos == "linux", "os", "runs on Linux"),
			need(h.exists("/dev/kvm"), "kvm", "needs /dev/kvm, usable by the agent's user"),
			need((h.lookPath("firecracker") && h.lookPath("jailer")) || h.lookPath("cloud-hypervisor"), "vmm", "needs firecracker and jailer, or cloud-hypervisor, on PATH"),
		}
	case domain.ProviderProxmox:
		return []domain.ProviderCheck{need(h.goos == "linux", "os", "runs on Linux")}
	case domain.ProviderHyperV:
		return []domain.ProviderCheck{
			need(h.goos == "windows", "os", "runs on Windows"),
			need(h.lookPath("powershell.exe"), "powershell", "needs powershell.exe, with the Hyper-V module"),
		}
	case domain.ProviderTart:
		return []domain.ProviderCheck{
			need(h.goos == "darwin", "os", "runs on macOS"),
			need(h.goarch == "arm64", "arch", "needs Apple silicon"),
			need(h.lookPath("tart"), "tart", "needs tart on PATH"),
		}
	case domain.ProviderAWS:
		return []domain.ProviderCheck{awsCredentials(h)}
	}
	return []domain.ProviderCheck{{Name: "kind", Status: statusFail, Detail: fmt.Sprintf("%q is not a kind the fleet agent serves", kind)}}
}

func need(ok bool, name, what string) domain.ProviderCheck {
	if ok {
		return domain.ProviderCheck{Name: name, Status: statusOK, Detail: what}
	}
	return domain.ProviderCheck{Name: name, Status: statusFail, Detail: what}
}

// awsCredentials looks for the machine's own means, without using them.
func awsCredentials(h host) domain.ProviderCheck {
	if onEC2(h) {
		return domain.ProviderCheck{Name: "credentials", Status: statusOK, Detail: "on EC2: the instance role is used"}
	}
	if h.getenv("AWS_PROFILE") != "" || h.getenv("AWS_ACCESS_KEY_ID") != "" {
		return domain.ProviderCheck{Name: "credentials", Status: statusOK, Detail: "found in the environment"}
	}
	return domain.ProviderCheck{Name: "credentials", Status: statusFail, Detail: "needs an instance role, or AWS_PROFILE for the agent's user"}
}

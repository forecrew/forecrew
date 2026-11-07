package proxmox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/forecrew/forecrew/internal/domain"
)

// Check statuses (domain.ProviderCheck).
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

func ok(name, detail string) domain.ProviderCheck {
	return domain.ProviderCheck{Name: name, Status: statusOK, Detail: detail}
}

func bad(name, status string, err error) domain.ProviderCheck {
	return domain.ProviderCheck{Name: name, Status: status, Detail: err.Error()}
}

// check asks the cluster what it can give, fills the capabilities with the most
// one machine can have on a node of the configuration, and returns what doctor
// shows and capabilities report.
func (p *Provider) check(ctx context.Context, c *domain.ProviderCapabilities) []domain.ProviderCheck {
	var out []domain.ProviderCheck
	var version struct {
		Version string `json:"version"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/version", nil, &version); err != nil {
		// Nothing else can be learned without the API.
		return append(out, bad("proxmox api", statusFail, err))
	}
	out = append(out, ok("proxmox api", "Proxmox VE "+p.api.clean(version.Version)))
	if _, err := p.keys(); err != nil {
		out = append(out, bad("image keys", statusFail, err))
	} else {
		out = append(out, ok("image keys", "trusted image keys found"))
	}
	for _, node := range p.cfg.Nodes {
		out = append(out, p.checkNode(ctx, node, c))
	}
	var fw struct {
		Enable int `json:"enable"`
	}
	switch err := p.api.do(ctx, http.MethodGet, "/cluster/firewall/options", nil, &fw); {
	case err != nil:
		out = append(out, bad("datacenter firewall", statusWarn, fmt.Errorf("cannot read whether it is on (the token needs Sys.Audit on /): %w", err)))
	case fw.Enable != 1:
		out = append(out, domain.ProviderCheck{Name: "datacenter firewall", Status: statusFail, Detail: "it is off, so the rules that keep machines off your network do nothing: turn it on (Datacenter, Firewall, Options)"})
	default:
		out = append(out, ok("datacenter firewall", "on: machines' network rules apply"))
	}
	return out
}

func (p *Provider) checkNode(ctx context.Context, node string, c *domain.ProviderCapabilities) domain.ProviderCheck {
	name := "node " + node
	var st struct {
		CPUInfo struct {
			CPUs int `json:"cpus"`
		} `json:"cpuinfo"`
		Memory struct {
			Total int64 `json:"total"`
		} `json:"memory"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/nodes/"+node+"/status", nil, &st); err != nil {
		return bad(name, statusFail, fmt.Errorf("cannot read it (the token needs Sys.Audit on it): %w", err))
	}
	c.MaxCPU = max(c.MaxCPU, st.CPUInfo.CPUs)
	c.MaxMemoryMB = max(c.MaxMemoryMB, int(st.Memory.Total>>20))
	var stores []struct {
		Avail int64 `json:"avail"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/nodes/"+node+"/storage", url.Values{"content": {"images"}, "enabled": {"1"}}, &stores); err != nil {
		return bad(name, statusWarn, fmt.Errorf("reachable, but its storage cannot be read: %w", err))
	}
	var most int64
	for _, s := range stores {
		most = max(most, s.Avail)
	}
	c.MaxDiskGB = max(c.MaxDiskGB, int(most>>30))
	return ok(name, fmt.Sprintf("%d CPUs, %d MB, %d GB free in its largest storage", st.CPUInfo.CPUs, st.Memory.Total>>20, most>>30))
}

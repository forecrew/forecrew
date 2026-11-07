package fleet

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// HostedEgress is what a hosted machine reaches besides forecrewd and its
// resolver (fleet.md, Network): model APIs, GitHub and package registries.
// An organization's model connections to custom endpoints and its
// fleet_policy.egress add to it. A location of an instance's own keeps its
// machines to the list (docs/fleet.md, egress), and never opens the host, its
// networks, other machines or metadata addresses, whatever the list says.
var HostedEgress = []string{
	// Model APIs.
	"api.anthropic.com", "api.openai.com", "generativelanguage.googleapis.com", "openrouter.ai",
	// GitHub.
	"github.com", "api.github.com", "codeload.github.com", "uploads.github.com", "objects.githubusercontent.com",
	"raw.githubusercontent.com", "ghcr.io", "pkg-containers.githubusercontent.com",
	// Package registries.
	"registry.npmjs.org", "registry.yarnpkg.com", "proxy.golang.org", "sum.golang.org", "pypi.org", "files.pythonhosted.org",
	"index.crates.io", "static.crates.io", "crates.io", "rubygems.org", "index.rubygems.org", "repo.maven.apache.org",
	"repo1.maven.org", "plugins.gradle.org", "services.gradle.org", "api.nuget.org", "registry-1.docker.io", "auth.docker.io",
	"production.cloudflare.docker.com", "deb.debian.org", "security.debian.org", "archive.ubuntu.com", "security.ubuntu.com",
	"dl-cdn.alpinelinux.org",
}

// hostedEgress returns what a hosted machine of an organization may reach:
// HostedEgress, the hosts of its model connections to custom endpoints and its
// policy's egress, each once.
func (c *Controller) hostedEgress(ctx context.Context, account uuid.UUID) ([]string, error) {
	out := slices.Clone(HostedEgress)
	if account == uuid.Nil() {
		return out, nil // a warm machine: nobody's work runs on it yet
	}
	conns, err := c.cfg.Store.ModelConnections().List(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("model connections: %w", err)
	}
	for _, mc := range conns {
		if u, err := url.Parse(mc.BaseURL); err == nil && mc.Enabled && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	policy, err := c.cfg.Store.FleetPolicies().Get(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	out = append(out, policy.Egress...)
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(h string) bool {
		dup := seen[h]
		seen[h] = true
		return dup
	}), nil
}

// isHosted reports a machine of an instance-wide location.
func isHosted(p domain.CapacityProvider) bool { return p.Ownership() == domain.OwnershipHosted }

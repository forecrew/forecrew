package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestMachinesCommand(t *testing.T) {
	e := newRemoteEnv(t)
	var got mcp.ListMachinesInput
	e.svc.ListMachinesFunc = func(_ context.Context, _ domain.Actor, in mcp.ListMachinesInput) ([]mcp.MachineInfo, error) {
		got = in
		return []mcp.MachineInfo{{State: domain.MachineBusy, Name: "small-8f2", Shape: "small", Provider: "home", JobRef: "JOB-7"}}, nil
	}
	out, errOut, code := e.run(t, "machines", "--state", "busy")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if got.State != "busy" {
		t.Fatalf("input = %+v", got)
	}
	for _, want := range []string{"small-8f2", "busy", "home", "JOB-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q not in %q", want, out)
		}
	}
	if _, _, code := e.run(t, "machines"); code != 0 {
		t.Fatalf("no flag: %d", code)
	}
}

func TestShapesAndProvidersCommands(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ListShapesFunc = func(context.Context, domain.Actor, any) ([]mcp.ShapeInfo, error) {
		return []mcp.ShapeInfo{{Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, Pool: "build", PricePerMinute: 0.04, Max: 3, Provider: "home"}}, nil
	}
	e.svc.ListProvidersFunc = func(context.Context, domain.Actor, any) ([]mcp.ProviderInfo, error) {
		return []mcp.ProviderInfo{{Name: "home", Kind: domain.ProviderProxmox, Mode: domain.ProviderModeAgent, Status: domain.ProviderActive, Online: true, Ownership: domain.OwnershipOwn}}, nil
	}
	out, _, code := e.run(t, "shapes")
	if code != 0 || !strings.Contains(out, "linux/amd64") || !strings.Contains(out, "2 CPU, 4096 MB") || !strings.Contains(out, "0.04") {
		t.Fatalf("code %d: %q", code, out)
	}
	out, _, code = e.run(t, "providers")
	if code != 0 || !strings.Contains(out, "proxmox") || !strings.Contains(out, "own") {
		t.Fatalf("code %d: %q", code, out)
	}
}

func TestFleetCommand(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.FleetOverviewFunc = func(context.Context, domain.Actor, any) (fleet.Overview, error) {
		return fleet.Overview{
			Headline: "1 job waiting: Only Mac Mini can run this, and it is offline.", Routing: domain.RoutingOwnOnly,
			Spend:     &fleet.Spend{Amount: 41, Currency: "CHF"},
			Locations: []fleet.Location{{Name: "Mac Mini", Runs: "macOS", Status: "offline", InUse: 0, Capacity: 2, Cost: "Free"}, {Name: "Cloud", Status: "online", Cost: "~CHF 0.04/min"}},
			Queue:     []fleet.WaitingJob{{Ref: "JOB-214", Role: "developer", Sentence: "Only Mac Mini can run this, and it is offline"}},
			Running:   []fleet.RunningJob{{Ref: "JOB-212", Machine: "small-8f2", Location: "Home", Minutes: 4}},
		}, nil
	}
	out, _, code := e.run(t, "fleet")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"1 job waiting", "CHF 41.00", "0 of 2", "JOB-214", "small-8f2", "own only"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q not in %q", want, out)
		}
	}
}

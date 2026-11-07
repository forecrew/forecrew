package cli

import (
	"context"
	"strings"
	"testing"
	"uuid"

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
	e.svc.ListProvidersFunc = func(context.Context, domain.Actor, mcp.ListProvidersInput) ([]mcp.ProviderInfo, error) {
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

func TestProviderCommands(t *testing.T) {
	e := newRemoteEnv(t)
	joinID := uuid.New()
	e.svc.CreateProviderFunc = func(_ context.Context, _ domain.Actor, in mcp.CreateProviderInput) (mcp.CreateProviderOutput, error) {
		return mcp.CreateProviderOutput{Provider: mcp.ProviderInfo{CapacityProvider: domain.CapacityProvider{Name: in.Name}}, Token: "fcp_new"}, nil
	}
	out, errOut, code := e.run(t, "provider", "add", "home", "--kind", "microvm", "--org", "acme")
	if code != 0 || strings.TrimSpace(out) != "fcp_new" || !strings.Contains(errOut, "not stored") {
		t.Fatalf("add: code %d, %q, %q", code, out, errOut)
	}
	if in := e.svc.Calls()[0].Input.(mcp.CreateProviderInput); in.Name != "home" || in.Kind != domain.ProviderMicroVM || in.Org != "acme" {
		t.Errorf("add input = %+v", in)
	}
	if _, _, code := e.run(t, "provider", "add", "home"); code == 0 {
		t.Error("add without --kind passed")
	}

	e.svc.RevokeProviderFunc = func(_ context.Context, _ domain.Actor, in mcp.ProviderRefInput) (mcp.ProviderInfo, error) {
		return mcp.ProviderInfo{Name: in.Provider}, nil
	}
	if out, _, code := e.run(t, "provider", "revoke", "home"); code != 0 || !strings.Contains(out, "home revoked") {
		t.Errorf("revoke: code %d, %q", code, out)
	}

	e.svc.CreateJoinCodeFunc = func(_ context.Context, _ domain.Actor, in mcp.CreateJoinCodeInput) (mcp.CreateJoinCodeOutput, error) {
		if !in.Cloud {
			t.Errorf("join input = %+v", in)
		}
		return mcp.CreateJoinCodeOutput{Code: "K7PQ-2M4X"}, nil
	}
	if out, errOut, code := e.run(t, "provider", "join", "--cloud"); code != 0 || strings.TrimSpace(out) != "K7PQ-2M4X" || !strings.Contains(errOut, "provider joins") {
		t.Errorf("join: code %d, %q, %q", code, out, errOut)
	}

	e.svc.ListJoinCodesFunc = func(context.Context, domain.Actor, mcp.ListJoinCodesInput) ([]mcp.JoinCodeInfo, error) {
		c := domain.ProviderJoinCode{ID: joinID, Kind: domain.ProviderMicroVM, Facts: domain.ProviderCapabilities{Host: "nas\x1b[2J", MaxCPU: 16}}
		return []mcp.JoinCodeInfo{{ProviderJoinCode: c, Status: mcp.JoinClaimed}}, nil
	}
	out, _, code = e.run(t, "provider", "joins")
	if code != 0 || !strings.Contains(out, joinID.String()) || !strings.Contains(out, "claimed") || !strings.Contains(out, "nas") || strings.Contains(out, "\x1b") {
		t.Errorf("joins: code %d, %q", code, out)
	}

	e.svc.ConfirmJoinFunc = func(_ context.Context, _ domain.Actor, in mcp.ConfirmJoinInput) (mcp.ProviderInfo, error) {
		if in.Join != joinID.String() {
			t.Errorf("confirm input = %+v", in)
		}
		return mcp.ProviderInfo{Name: in.Name}, nil
	}
	if out, _, code := e.run(t, "provider", "confirm", joinID.String(), "--name", "home"); code != 0 || !strings.Contains(out, "provider home created") {
		t.Errorf("confirm: code %d, %q", code, out)
	}
}

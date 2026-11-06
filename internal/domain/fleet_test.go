package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

var (
	orgA = NewID()
	orgB = NewID()
)

func validShape() Shape {
	return Shape{ProviderID: NewID(), Name: "linux-small", OS: OSLinux, Arch: ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 20,
		Image: "forecrew-linux", Enabled: true}.WithDefaults()
}

func TestShapeWithDefaults(t *testing.T) {
	s := Shape{}.WithDefaults()
	if s.Pool != PoolBuild || s.BootTimeoutSeconds != DefaultBootTimeoutSeconds || s.Max != 1 || s.MaxJobs != 1 {
		t.Fatalf("defaults = %+v", s)
	}
	s = Shape{Pool: "gpu", BootTimeoutSeconds: 9, Max: 3, MaxJobs: 2}.WithDefaults()
	if s.Pool != "gpu" || s.BootTimeoutSeconds != 9 || s.Max != 3 || s.MaxJobs != 2 {
		t.Fatalf("kept = %+v", s)
	}
}

func TestShapeValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Shape)
		want   string // empty: valid
	}{
		{"minimal", func(*Shape) {}, ""},
		{"pinned image, budget and config", func(s *Shape) {
			s.Image = "forecrew-linux@2026.11.1"
			s.MonthlyBudget = new(200.0)
			s.ProviderConfig = map[string]any{"instance_type": "c7g.large", "subnet": "subnet-1", "spot": true, "zones": []any{"a", "b"},
				"license_key": "secret:aws-license", "auth": map[string]any{"api_token": "env:PROXMOX_TOKEN"}}
		}, ""},
		{"bad name", func(s *Shape) { s.Name = "Linux Small" }, "name"},
		{"no provider", func(s *Shape) { s.ProviderID = uuid.Nil() }, "provider_id"},
		{"bad os", func(s *Shape) { s.OS = "freebsd" }, "os must be one of"},
		{"bad arch", func(s *Shape) { s.Arch = "riscv64" }, "arch must be one of"},
		{"no cpu", func(s *Shape) { s.CPU = 0 }, "cpu, memory_mb and disk_gb"},
		{"negative gpu", func(s *Shape) { s.GPU = -1 }, "gpu"},
		{"pool actions", func(s *Shape) { s.Pool = PoolActions }, "pool"},
		{"bad image", func(s *Shape) { s.Image = "a@b@c" }, "image"},
		{"empty image", func(s *Shape) { s.Image = "" }, "image"},
		{"negative price", func(s *Shape) { s.PricePerMinute = -0.1 }, "price_per_minute"},
		{"negative budget", func(s *Shape) { s.MonthlyBudget = new(-1.0) }, "monthly_budget"},
		{"warm above max", func(s *Shape) { s.WarmMin, s.Max = 3, 2 }, "warm_min"},
		{"no boot timeout", func(s *Shape) { s.BootTimeoutSeconds = 0 }, "boot_timeout_seconds"},
		{"negative idle", func(s *Shape) { s.IdleSeconds = -1 }, "idle_seconds"},
		{"secret key", func(s *Shape) { s.ProviderConfig = map[string]any{"api_token": "3f1c9a7e"} }, "provider_config.api_token"},
		{"secret key, not a string", func(s *Shape) { s.ProviderConfig = map[string]any{"password": 1234} }, "provider_config.password"},
		{"nested secret key", func(s *Shape) {
			s.ProviderConfig = map[string]any{"auth": map[string]any{"clientSecret": "abc"}}
		}, "provider_config.auth.clientSecret"},
		{"secret in a list", func(s *Shape) {
			s.ProviderConfig = map[string]any{"args": []any{"ok", "ghp_abcdefghijklmnopqrstuvwxyz"}}
		}, "provider_config.args[1]"},
		{"AWS key as a value", func(s *Shape) { s.ProviderConfig = map[string]any{"id": "AKIAIOSFODNN7EXAMPLE"} }, "provider_config.id"},
		{"password in a URL", func(s *Shape) { s.ProviderConfig = map[string]any{"api": "https://root:hunter2@pve:8006"} }, "provider_config.api"},
		{"long note", func(s *Shape) { s.Note = strings.Repeat("x", maxShapeNote+1) }, "note"},
		{"secret in the note", func(s *Shape) { s.Note = "token fcw_abcdefghijklmnopqrstuvwxyz" }, "note"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validShape()
			tt.change(&s)
			err := s.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// A refused secret is never repeated in the error, which may reach a log.
func TestShapeValidateHidesTheSecret(t *testing.T) {
	s := validShape()
	s.ProviderConfig = map[string]any{"api_token": "topsecretvalue", "x": "sk-abcdefghijklmnopqrstuvwxyz"}
	err := s.Validate()
	if err == nil || strings.Contains(err.Error(), "topsecretvalue") || strings.Contains(err.Error(), "sk-abc") {
		t.Fatalf("err = %v", err)
	}
}

func homeServer() CapacityProvider {
	return CapacityProvider{ID: NewID(), AccountID: orgA, Name: "home-server", Kind: ProviderMicroVM, Mode: ProviderModeAgent, Status: ProviderActive,
		TokenHash: make([]byte, 32), Limits: ProviderLimits{MaxMachines: 4},
		Capabilities: ProviderCapabilities{OS: []MachineOS{OSLinux}, Arch: []MachineArch{ArchAMD64}, MaxCPU: 16, MaxMemoryMB: 65536, MaxDiskGB: 400,
			ImageFormats: []ImageFormat{ImageRootfs}, Checks: []ProviderCheck{{Name: "kvm", Status: "ok"}}}}
}

func TestShapeCheckProvider(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Shape, *CapacityProvider)
		want   string
	}{
		{"fits", func(*Shape, *CapacityProvider) {}, ""},
		{"other provider", func(s *Shape, _ *CapacityProvider) { s.ProviderID = NewID() }, "provider_id"},
		{"other organization", func(s *Shape, _ *CapacityProvider) { s.AccountID = orgB }, "same organization"},
		{"instance-wide on an organization's provider", func(s *Shape, _ *CapacityProvider) { s.AccountID = uuid.Nil() }, "same organization"},
		{"os", func(s *Shape, _ *CapacityProvider) { s.OS = OSWindows }, "os windows"},
		{"arch", func(s *Shape, _ *CapacityProvider) { s.Arch = ArchARM64 }, "arch arm64"},
		{"cpu", func(s *Shape, _ *CapacityProvider) { s.CPU = 32 }, "exceed"},
		{"memory", func(s *Shape, _ *CapacityProvider) { s.MemoryMB = 1 << 20 }, "exceed"},
		{"disk", func(s *Shape, _ *CapacityProvider) { s.DiskGB = 500 }, "exceed"},
		{"gpu", func(s *Shape, _ *CapacityProvider) { s.GPU = 1 }, "exceed"},
		{"warm above the provider's machines", func(s *Shape, _ *CapacityProvider) { s.WarmMin, s.Max = 5, 5 }, "warm_min"},
		{"no capabilities yet", func(_ *Shape, p *CapacityProvider) { p.Capabilities = ProviderCapabilities{} }, "does not run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := homeServer()
			s := validShape()
			s.ProviderID, s.AccountID = p.ID, p.AccountID
			tt.change(&s, &p)
			err := s.CheckProvider(p)
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (!errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func validImage() Image {
	return Image{Name: "forecrew-linux", Version: "2026.11.1", OS: OSLinux, Arch: ArchAMD64, Format: ImageRootfs,
		URI: "https://images.example.com/a.ext4", Digest: "sha256:" + strings.Repeat("a", 64),
		Signature: "ed25519:" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
		Contents: ImageContents{Labels: []string{"docker"}, Packages: []ImagePackage{{Name: "git", Version: "2.51"}},
			Kernel: &ImageKernel{URI: "https://images.example.com/vmlinux", Digest: "sha256:" + strings.Repeat("b", 64)}}}
}

func TestShapeCheckImage(t *testing.T) {
	p := homeServer()
	s := validShape()
	if err := s.CheckImage(p, validImage()); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Shape, *Image)
		want   string
	}{
		{"other name", func(_ *Shape, i *Image) { i.Name = "other" }, "is not"},
		{"other pinned version", func(s *Shape, _ *Image) { s.Image = "forecrew-linux@1" }, "is not"},
		{"other os", func(_ *Shape, i *Image) { i.OS = OSWindows }, "is for"},
		{"other arch", func(_ *Shape, i *Image) { i.Arch = ArchARM64 }, "is for"},
		{"format", func(_ *Shape, i *Image) { i.Format = ImageQCOW2 }, "does not start"},
	}
	for _, tt := range tests {
		s, i := validShape(), validImage()
		tt.change(&s, &i)
		if err := s.CheckImage(p, i); !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestProviderValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*CapacityProvider)
		want   string
	}{
		{"agent", func(*CapacityProvider) {}, ""},
		{"direct", func(p *CapacityProvider) { p.Mode, p.TokenHash = ProviderModeDirect, nil }, ""},
		{"pending has no token yet", func(p *CapacityProvider) { p.Status, p.TokenHash = ProviderPending, nil }, ""},
		{"budget, network and tags", func(p *CapacityProvider) {
			p.Limits.MonthlyBudget = new(4100.0)
			p.NetworkAllow = []string{"10.0.0.5", "192.168.1.0/24", "registry.example.com"}
			p.Tags = map[string]string{"owner": "platform"}
		}, ""},
		{"bad name", func(p *CapacityProvider) { p.Name = "" }, "name"},
		{"bad kind", func(p *CapacityProvider) { p.Kind = "vsphere" }, "kind"},
		{"bad mode", func(p *CapacityProvider) { p.Mode = "" }, "mode"},
		{"bad status", func(p *CapacityProvider) { p.Status = "online" }, "status"},
		{"short hash", func(p *CapacityProvider) { p.TokenHash = []byte{1} }, "SHA-256"},
		{"direct with a token", func(p *CapacityProvider) { p.Mode = ProviderModeDirect }, "no token"},
		{"active agent without a token", func(p *CapacityProvider) { p.TokenHash = nil }, "needs a token"},
		{"negative max", func(p *CapacityProvider) { p.Limits.MaxMachines = -1 }, "max_machines"},
		{"negative budget", func(p *CapacityProvider) { p.Limits.MonthlyBudget = new(-1.0) }, "monthly_budget"},
		{"bad capability os", func(p *CapacityProvider) { p.Capabilities.OS = []MachineOS{"bsd"} }, "capabilities.os"},
		{"bad capability arch", func(p *CapacityProvider) { p.Capabilities.Arch = []MachineArch{"mips"} }, "capabilities.arch"},
		{"bad capability format", func(p *CapacityProvider) { p.Capabilities.ImageFormats = []ImageFormat{"iso"} }, "capabilities.image_formats"},
		{"negative capability", func(p *CapacityProvider) { p.Capabilities.MaxCPU = -1 }, "negative"},
		{"bad check", func(p *CapacityProvider) { p.Capabilities.Checks = []ProviderCheck{{Name: "kvm", Status: "maybe"}} }, "checks"},
		{"bad host", func(p *CapacityProvider) { p.NetworkAllow = []string{"user@host"} }, "network_allow"},
		{"empty host", func(p *CapacityProvider) { p.NetworkAllow = []string{""} }, "network_allow"},
		{"empty tag", func(p *CapacityProvider) { p.Tags = map[string]string{"": "x"} }, "tags"},
		{"secret in a tag", func(p *CapacityProvider) { p.Tags = map[string]string{"x": "ghp_abcdefghijklmnopqrstuvwxyz"} }, "tags.x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := homeServer()
			tt.change(&p)
			err := p.Validate()
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (!errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if err := (ProviderCapabilities{MaxGPU: -1}).Validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("capabilities: %v", err)
	}
}

func TestProviderAvailability(t *testing.T) {
	now := time.Now()
	p := homeServer()
	if p.Online(now) || p.CanStart(now) {
		t.Fatal("a provider never seen is online")
	}
	p.LastSeenAt = now.Add(-OnlineWindow / 2)
	if !p.Online(now) || !p.CanStart(now) {
		t.Fatal("a provider just seen is offline")
	}
	p.Status = ProviderPaused
	if !p.Online(now) || p.CanStart(now) {
		t.Fatal("a paused provider is online and starts nothing")
	}
	p.Status = ProviderRevoked
	if p.Online(now) {
		t.Fatal("a revoked provider is online")
	}
	p.Status, p.LastSeenAt = ProviderActive, now.Add(-2*OnlineWindow)
	if p.Online(now) {
		t.Fatal("a provider long gone is online")
	}
}

func TestProviderMaxMachines(t *testing.T) {
	tests := []struct{ limit, reported, want int }{{0, 0, 0}, {3, 0, 3}, {0, 2, 2}, {3, 2, 2}, {1, 2, 1}}
	for _, tt := range tests {
		p := CapacityProvider{Limits: ProviderLimits{MaxMachines: tt.limit}, Capabilities: ProviderCapabilities{MaxMachines: tt.reported}}
		if got := p.MaxMachines(); got != tt.want {
			t.Errorf("limit %d, reported %d: %d, want %d", tt.limit, tt.reported, got, tt.want)
		}
	}
}

func TestOwnership(t *testing.T) {
	if (Shape{AccountID: orgA}).Ownership() != OwnershipOwn || (Shape{}).Ownership() != OwnershipHosted {
		t.Fatal("shape ownership")
	}
	if (CapacityProvider{AccountID: orgA}).Ownership() != OwnershipOwn || (CapacityProvider{}).Ownership() != OwnershipHosted {
		t.Fatal("provider ownership")
	}
}

func TestOSLabels(t *testing.T) {
	for os, want := range map[string][]string{"linux": {"linux"}, "windows": {"windows"}, "darwin": {"darwin", "macos"}, "macos": {"darwin", "macos"}} {
		if got := OSLabels(os); !slices.Equal(got, want) {
			t.Errorf("%s: %v", os, got)
		}
	}
	s := Shape{OS: OSMacOS, Arch: ArchARM64, Labels: []string{"Xcode:16"}}
	if got := s.OfferedLabels(); !slices.Equal(got, []string{"arm64", "darwin", "macos", "xcode:16"}) {
		t.Fatalf("offered = %v", got)
	}
}

func TestResourcesValidate(t *testing.T) {
	if err := (Resources{MinCPU: 2, GPU: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Resources{{MinCPU: -1}, {MinMemoryMB: -1}, {MinDiskGB: -1}, {GPU: -1}} {
		if err := r.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

// fleet is the shapes of the worked examples in docs/contracts/fleet.md.
func fleet() []Shape {
	shape := func(account uuid.UUID, name string, os MachineOS, cpu, memGB int, price float64, labels ...string) Shape {
		return Shape{AccountID: account, Name: name, OS: os, Arch: ArchAMD64, CPU: cpu, MemoryMB: memGB * 1024, DiskGB: 40, Pool: PoolBuild,
			PricePerMinute: price, Labels: append(labels, "runtime:claude-code"), Enabled: true}
	}
	gpu := shape(orgA, "aws-gpu", OSLinux, 8, 32, 0.21, "docker")
	gpu.GPU = 1
	off := shape(orgA, "home-off", OSLinux, 1, 1, 0)
	off.Enabled = false
	control := shape(orgA, "home-control", OSLinux, 1, 2, 0)
	control.Pool = PoolControl
	return []Shape{
		shape(orgA, "aws-large", OSLinux, 8, 16, 0.04, "docker"),
		gpu,
		shape(orgA, "home-medium", OSLinux, 4, 8, 0, "docker"),
		shape(orgA, "home-small", OSLinux, 2, 4, 0, "docker"),
		shape(orgA, "home-windows", OSWindows, 4, 8, 0),
		off,
		control,
		shape(uuid.Nil(), "cloud-small", OSLinux, 2, 4, 0.01, "docker"),
		shape(uuid.Nil(), "cloud-xl", OSLinux, 16, 64, 0.08, "docker"),
		shape(orgB, "other-free", OSLinux, 64, 256, 0, "docker"),
	}
}

// TestSelectShapes is the rule of docs/contracts/fleet.md, Picking a shape,
// with its worked examples.
func TestSelectShapes(t *testing.T) {
	own := func(r FleetRouting, allowed ...string) FleetPolicy {
		return FleetPolicy{AccountID: orgA, Routing: r, AllowedShapes: allowed}
	}
	build := func(res Resources, labels ...string) ShapeDemand {
		return ShapeDemand{Pool: PoolBuild, Labels: append(labels, "runtime:claude-code"), Resources: res}
	}
	tests := []struct {
		name   string
		policy FleetPolicy
		demand ShapeDemand
		want   []string
	}{
		{"1 cheapest, then smallest", own(RoutingOwnOnly), build(Resources{}, "docker"),
			[]string{"home-small", "home-medium", "aws-large", "aws-gpu"}},
		{"2 resources rule the small one out", own(RoutingOwnOnly), build(Resources{MinCPU: 4, MinMemoryMB: 8192}),
			[]string{"home-medium", "home-windows", "aws-large", "aws-gpu"}},
		{"3 nothing is big enough", own(RoutingOwnOnly), build(Resources{MinMemoryMB: 65536}), []string{}},
		{"4 own_first falls back to hosted", own(RoutingOwnFirst), build(Resources{MinMemoryMB: 65536}), []string{"cloud-xl"}},
		{"5 own_first keeps a paid own shape before a cheaper hosted one", own(RoutingOwnFirst), build(Resources{}, "docker"),
			[]string{"home-small", "home-medium", "aws-large", "aws-gpu", "cloud-small", "cloud-xl"}},
		{"6 hosted uses only instance-wide shapes", own(RoutingHosted), build(Resources{}, "docker"), []string{"cloud-small", "cloud-xl"}},
		{"7 gpu", own(RoutingOwnOnly), build(Resources{GPU: 1}), []string{"aws-gpu"}},
		{"8 os label", own(RoutingOwnOnly), build(Resources{}, "windows"), []string{"home-windows"}},
		{"9 the pool decides", own(RoutingOwnOnly), ShapeDemand{Pool: PoolControl, Labels: []string{"runtime:claude-code"}}, []string{"home-control"}},
		{"10 allowed shapes", own(RoutingOwnFirst, "home-medium", "cloud-small"), build(Resources{}, "docker"), []string{"home-medium", "cloud-small"}},
		{"another organization's shapes are never offered", FleetPolicy{AccountID: orgB, Routing: RoutingOwnOnly}, build(Resources{}, "docker"), []string{"other-free"}},
		{"unknown routing allows nothing", own("nope"), build(Resources{}), []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := []string{}
			for _, s := range SelectShapes(fleet(), tt.policy, tt.demand) {
				got = append(got, s.Name)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompareShapesTies(t *testing.T) {
	base := Shape{AccountID: orgA, OS: OSLinux, Arch: ArchAMD64, CPU: 2, MemoryMB: 4, DiskGB: 10, Enabled: true}
	a, b, c, d := base, base, base, base
	a.Name, b.Name, c.Name, d.Name = "z", "b", "a", "y"
	a.DiskGB = 5
	c.GPU = 1
	got := SelectShapes([]Shape{c, d, b, a}, FleetPolicy{AccountID: orgA, Routing: RoutingOwnOnly}, ShapeDemand{})
	names := []string{}
	for _, s := range got {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"z", "b", "y", "a"}) {
		t.Fatalf("order = %v", names)
	}
}

func TestJobShapeDemand(t *testing.T) {
	j := Job{Runtime: "codex", Needs: []string{"docker"}, Resources: Resources{MinCPU: 2}}
	d := j.ShapeDemand()
	if d.Pool != PoolBuild || !slices.Equal(d.Labels, []string{"docker", "runtime:codex"}) || d.Resources.MinCPU != 2 {
		t.Fatalf("demand = %+v", d)
	}
}

func TestFleetPolicyValidate(t *testing.T) {
	valid := func() FleetPolicy {
		p := DefaultFleetPolicy(orgA)
		p.AllowedShapes = []string{"linux-small"}
		p.Quotas = FleetQuotas{ConcurrentMachines: 5, MonthlyMinutes: 100, MonthlyBudget: new(200.0)}
		p.Egress = []string{"registry.example.com"}
		return p
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	if p := DefaultFleetPolicy(orgA); p.Routing != RoutingOwnOnly || p.Plan != PlanFree || p.Validate() != nil {
		t.Fatalf("default = %+v", p)
	}
	tests := []struct {
		name   string
		change func(*FleetPolicy)
		want   string
	}{
		{"no organization", func(p *FleetPolicy) { p.AccountID = uuid.Nil() }, "account_id"},
		{"routing", func(p *FleetPolicy) { p.Routing = "anywhere" }, "routing"},
		{"plan", func(p *FleetPolicy) { p.Plan = "gold" }, "plan"},
		{"shape name", func(p *FleetPolicy) { p.AllowedShapes = []string{"Not A Name"} }, "allowed_shapes"},
		{"negative concurrent", func(p *FleetPolicy) { p.Quotas.ConcurrentMachines = -1 }, "quotas"},
		{"negative minutes", func(p *FleetPolicy) { p.Quotas.MonthlyMinutes = -1 }, "quotas"},
		{"negative budget", func(p *FleetPolicy) { p.Quotas.MonthlyBudget = new(-1.0) }, "quotas.monthly_budget"},
		{"bad egress", func(p *FleetPolicy) { p.Egress = []string{"a b"} }, "egress"},
	}
	for _, tt := range tests {
		p := valid()
		tt.change(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestMachineStateMachine(t *testing.T) {
	allowed := map[[2]MachineState]bool{}
	for _, m := range [][2]MachineState{
		{MachineRequested, MachineBooting}, {MachineRequested, MachineDestroyed}, {MachineRequested, MachineFailed},
		{MachineBooting, MachineReady}, {MachineBooting, MachineDraining}, {MachineBooting, MachineFailed},
		{MachineReady, MachineBusy}, {MachineReady, MachineDraining}, {MachineReady, MachineFailed},
		{MachineBusy, MachineDraining}, {MachineBusy, MachineFailed},
		{MachineDraining, MachineDestroyed}, {MachineDraining, MachineFailed},
		{MachineFailed, MachineDestroyed},
	} {
		allowed[m] = true
	}
	for _, from := range MachineStates() {
		for _, to := range MachineStates() {
			want := allowed[[2]MachineState{from, to}]
			if from.CanTransition(to) != want {
				t.Errorf("%s -> %s = %v", from, to, !want)
			}
			got, err := from.Transition(to)
			if want && (err != nil || got != to) {
				t.Errorf("%s -> %s: %v, %v", from, to, got, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: %v", from, to, err)
			}
		}
	}
}

func TestMachineStateRules(t *testing.T) {
	if MachineBusy.CanTransition(MachineReady) {
		t.Error("a machine goes back to the warm pool after a job")
	}
	for _, s := range MachineStates() {
		if want := s != MachineDestroyed && s != MachineFailed; s.Counts() != want {
			t.Errorf("%s counts = %v", s, !want)
		}
	}
}

func TestMachineValidate(t *testing.T) {
	valid := func() Machine {
		return Machine{AccountID: orgA, ProviderID: NewID(), ShapeID: NewID(), State: MachineRequested}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	warm := valid()
	warm.AccountID = uuid.Nil()
	if err := warm.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Machine)
	}{
		{"state", func(m *Machine) { m.State = "gone" }},
		{"no shape", func(m *Machine) { m.ShapeID = uuid.Nil() }},
		{"unassigned with a worker", func(m *Machine) { m.AccountID, m.WorkerID = uuid.Nil(), NewID() }},
		{"unassigned with a job", func(m *Machine) { m.AccountID, m.JobID = uuid.Nil(), NewID() }},
		{"negative jobs", func(m *Machine) { m.JobsTaken = -1 }},
		{"destroyed without a time", func(m *Machine) { m.State = MachineDestroyed }},
		{"a time without destroyed", func(m *Machine) { m.DestroyedAt = time.Now() }},
	}
	for _, tt := range tests {
		m := valid()
		tt.change(&m)
		if err := m.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestMachineCanTakeJob(t *testing.T) {
	s := Shape{MaxJobs: 2}
	for taken, want := range map[int]bool{0: true, 1: true, 2: false, 3: false} {
		if got := (Machine{JobsTaken: taken}).CanTakeJob(s); got != want {
			t.Errorf("%d taken: %v", taken, got)
		}
	}
}

func TestMachineWarm(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		m    Machine
		want bool
	}{
		{"up with no organization", Machine{State: MachineBooting, ReadyAt: now}, true},
		{"still booting", Machine{State: MachineBooting}, false},
		{"assigned", Machine{State: MachineBooting, ReadyAt: now, AccountID: orgA}, false},
		{"ready", Machine{State: MachineReady, ReadyAt: now, AccountID: orgA}, false},
	}
	for _, tt := range tests {
		if got := tt.m.Warm(); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}

// The boot timeout counts from booting, so a machine waiting for an offline
// provider does not fail for it.
func TestMachineBootExpired(t *testing.T) {
	now := time.Now()
	s := Shape{BootTimeoutSeconds: 60}
	early, late := now.Add(-time.Second), now.Add(-time.Minute)
	tests := []struct {
		name string
		m    Machine
		want bool
	}{
		{"just booting", Machine{State: MachineBooting, BootingAt: early}, false},
		{"out of time", Machine{State: MachineBooting, BootingAt: late}, true},
		{"requested for hours", Machine{State: MachineRequested, CreatedAt: now.Add(-5 * time.Hour)}, false},
		{"booting with no time", Machine{State: MachineBooting}, false},
		{"warm", Machine{State: MachineBooting, BootingAt: late, ReadyAt: early}, false},
		{"ready", Machine{State: MachineReady, BootingAt: late, ReadyAt: early}, false},
	}
	for _, tt := range tests {
		if got := tt.m.BootExpired(s, now); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}

func TestBootstrapToken(t *testing.T) {
	now := time.Now()
	hash := make([]byte, 32)
	bound := BootstrapToken{MachineID: NewID(), AccountID: orgA, TokenHash: hash, ExpiresAt: now.Add(time.Minute)}
	warm := BootstrapToken{MachineID: NewID(), TokenHash: hash}
	if err := bound.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := warm.Validate(); err != nil {
		t.Fatal(err)
	}
	if !bound.Usable(now) || warm.Usable(now) || warm.Assigned() {
		t.Fatal("usable")
	}
	used := bound
	used.UsedAt = now
	if used.Usable(now) {
		t.Fatal("a used token is usable")
	}
	if bound.Usable(now.Add(time.Hour)) {
		t.Fatal("an expired token is usable")
	}
	for name, tok := range map[string]BootstrapToken{
		"no machine":            {TokenHash: hash},
		"no hash":               {MachineID: NewID()},
		"organization, no time": {MachineID: NewID(), TokenHash: hash, AccountID: orgA},
		"time, no organization": {MachineID: NewID(), TokenHash: hash, ExpiresAt: now},
	} {
		if err := tok.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestJoinCodeUsable(t *testing.T) {
	now := time.Now()
	c := ProviderJoinCode{ExpiresAt: now.Add(JoinCodeTTL)}
	if !c.Usable(now) || c.Usable(now.Add(JoinCodeCloudTTL)) {
		t.Fatal("expiry")
	}
	c.UsedAt = now
	if c.Usable(now) {
		t.Fatal("a used code is usable")
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		ref, name, version string
		ok                 bool
	}{
		{"linux", "linux", "", true},
		{"linux@2026.11.1", "linux", "2026.11.1", true},
		{"linux@", "linux", "", false},
		{"@1", "", "1", false},
		{"Linux", "Linux", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		name, version, ok := ParseImageRef(tt.ref)
		if name != tt.name || version != tt.version || ok != tt.ok {
			t.Errorf("%q: %q %q %v", tt.ref, name, version, ok)
		}
	}
}

func TestImageValidate(t *testing.T) {
	if err := validImage().Validate(); err != nil {
		t.Fatal(err)
	}
	ami := validImage()
	ami.Format, ami.URI, ami.Contents.Kernel = ImageAMI, "ami:eu-central-1/ami-0123", nil
	if err := ami.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Image)
		want   string
	}{
		{"name", func(i *Image) { i.Name = "" }, "name"},
		{"version", func(i *Image) { i.Version = "a b" }, "version"},
		{"os", func(i *Image) { i.OS = "" }, "os"},
		{"arch", func(i *Image) { i.Arch = "" }, "arch"},
		{"format", func(i *Image) { i.Format = "iso" }, "format"},
		{"relative uri", func(i *Image) { i.URI = "a/b" }, "uri"},
		{"unknown scheme", func(i *Image) { i.URI = "ftp://example.com/a" }, "scheme"},
		{"file uri", func(i *Image) { i.URI = "file:///var/lib/images/a.ext4" }, "scheme"},
		{"https without a host", func(i *Image) { i.URI = "https:///a.ext4" }, "needs a host"},
		{"presigned url", func(i *Image) { i.URI = "https://b.s3.amazonaws.com/a.ext4?X-Amz-Signature=abc&X-Amz-Credential=x" }, "credential"},
		{"token in the query", func(i *Image) { i.Contents.Kernel.URI = "https://example.com/vmlinux?access_token=abc" }, "kernel.uri"},
		{"signature in the query", func(i *Image) { i.URI = "https://example.com/a?signature=abc" }, "credential"},
		{"credentials in the uri", func(i *Image) { i.URI = "https://u:p@example.com/a" }, "uri"},
		{"digest", func(i *Image) { i.Digest = "md5:abc" }, "digest"},
		{"unsigned", func(i *Image) { i.Signature = "" }, "signature"},
		{"short signature", func(i *Image) { i.Signature = "ed25519:AAAA" }, "signature"},
		{"bad base64", func(i *Image) { i.Signature = "ed25519:***" }, "signature"},
		{"rootfs without a kernel", func(i *Image) { i.Contents.Kernel = nil }, "kernel"},
		{"kernel uri", func(i *Image) { i.Contents.Kernel.URI = "x" }, "kernel.uri"},
		{"kernel digest", func(i *Image) { i.Contents.Kernel.Digest = "x" }, "kernel.digest"},
		{"package", func(i *Image) { i.Contents.Packages = []ImagePackage{{Name: "git"}} }, "packages"},
	}
	for _, tt := range tests {
		i := validImage()
		k := *i.Contents.Kernel
		i.Contents.Kernel = &k
		tt.change(&i)
		if err := i.Validate(); !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

// The payload holds what makes an image that image, so a signature does
// not fit another one.
func TestImageSigningPayload(t *testing.T) {
	i := validImage()
	i.BuiltAt = time.Date(2026, 11, 1, 10, 0, 0, 999, time.UTC)
	want := "forecrew-linux\n2026.11.1\nlinux\namd64\nrootfs\n" + i.Digest + "\n" + i.Contents.Kernel.Digest + "\n2026-11-01T10:00:00Z\n"
	if got := string(i.SigningPayload()); got != want {
		t.Fatalf("payload = %q", got)
	}
	for name, change := range map[string]func(*Image){
		"version": func(x *Image) { x.Version = "2" },
		"kernel": func(x *Image) {
			x.Contents.Kernel = &ImageKernel{URI: "https://k", Digest: "sha256:" + strings.Repeat("9", 64)}
		},
		"no kernel":  func(x *Image) { x.Contents.Kernel = nil },
		"build date": func(x *Image) { x.BuiltAt = x.BuiltAt.Add(time.Hour) },
	} {
		other := i
		change(&other)
		if string(other.SigningPayload()) == want {
			t.Errorf("%s is not signed", name)
		}
	}
}

func TestUsageRecordValidate(t *testing.T) {
	now := time.Now()
	valid := func() UsageRecord {
		return UsageRecord{AccountID: orgA, MachineID: NewID(), ShapeID: NewID(), Ownership: OwnershipHosted, Minutes: 3.5, Cost: 0.14, StartedAt: now, EndedAt: now.Add(time.Minute)}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*UsageRecord)
	}{
		{"no organization", func(u *UsageRecord) { u.AccountID = uuid.Nil() }},
		{"ownership", func(u *UsageRecord) { u.Ownership = "rented" }},
		{"minutes", func(u *UsageRecord) { u.Minutes = -1 }},
		{"cost", func(u *UsageRecord) { u.Cost = -1 }},
		{"times", func(u *UsageRecord) { u.EndedAt = now.Add(-time.Hour) }},
	}
	for _, tt := range tests {
		u := valid()
		tt.change(&u)
		if err := u.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

// Hashes never leave through JSON.
func TestFleetJSONHidesHashes(t *testing.T) {
	for _, v := range []any{homeServer(), BootstrapToken{TokenHash: []byte("hash")}, ProviderJoinCode{CodeHash: []byte("hash"), ClaimHash: []byte("hash")}} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "hash") {
			t.Errorf("%s", data)
		}
	}
}

func TestSecretPatterns(t *testing.T) {
	for _, s := range []string{"fcp_abcdefghijklmnopqrstuvwxyz", "fcb_abcdefghijklmnopqrstuvwxyz", "ASIAIOSFODNN7EXAMPLE", "postgres://u:p@db/x"} {
		if !SecretPatterns.MatchString(s) {
			t.Errorf("%s does not match", s)
		}
	}
	for _, s := range []string{"subnet-0abc", "https://example.com/a", "c7g.large"} {
		if SecretPatterns.MatchString(s) {
			t.Errorf("%s matches", s)
		}
	}
}

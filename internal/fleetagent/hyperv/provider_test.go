package hyperv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/vmcommon"
	"github.com/forecrew/forecrew/internal/images"
)

func TestCreate(t *testing.T) {
	r := newRig(t)
	req := r.request()
	inst := r.create(req)
	name := vmName(req.MachineID)
	vm := r.ps.VM(name)
	if vm == nil || vm.State != "Running" || inst.ExternalID != vm.ID || inst.MachineID != req.MachineID || !inst.Running {
		t.Fatalf("instance %+v, vm %+v", inst, vm)
	}
	if vm.CPU != 4 || vm.MemoryMB != 8192 || vm.Switch != "Lab" || vm.VLAN != 0 || !vm.Differencing || vm.Dir != `C:\FC\vms\`+name {
		t.Errorf("vm = %+v", vm)
	}
	if vm.Base != r.base {
		t.Errorf("base = %s", vm.Base)
	}
	// The token reaches the guest only through the config disk.
	if want := "FORECREW_SERVER_URL=https://192.0.2.7\r\nFORECREW_BOOTSTRAP_TOKEN=" + req.Bootstrap.Token + "\r\n"; vm.ConfigDisk != want {
		t.Errorf("config disk = %q", vm.ConfigDisk)
	}
	if strings.Contains(vm.Notes, req.Bootstrap.Token) {
		t.Error("the token is in the VM's notes")
	}
	meta, ok := vmcommon.ParseMeta(vm.Notes)
	if !ok || meta.ID() != req.MachineID || meta.Port != 443 || meta.Server[0] != "192.0.2.7" || meta.Tags["site"] != "lab" {
		t.Errorf("meta = %+v %v", meta, ok)
	}
	if len(vm.Rules) == 0 || vm.Rules[0].Dest != "192.0.2.7/32" || !vm.Rules[0].Allow || vm.Rules[0].Port != 443 || vm.Rules[len(vm.Rules)-1].Allow {
		t.Errorf("rules = %+v", vm.Rules)
	}
	if *r.fetch != 1 {
		t.Errorf("fetched %d times", *r.fetch)
	}
	r.create(r.request())
	if *r.fetch != 1 {
		t.Error("the image was fetched again")
	}
}

func TestCreateWithLocalSettings(t *testing.T) {
	r := newRig(t)
	r.cfg.VLAN, r.cfg.DNS, r.cfg.Generation = 40, []string{"192.168.1.1"}, 2
	r.rebuild()
	r.ps.Host.Switches = []string{"Lab", "Build"}
	req := r.request()
	req.NetworkAllow = []string{"10.9.0.0/16"}
	req.Shape.ProviderConfig = map[string]any{"switch": "Build"}
	vm := r.ps.VM(vmName(req.MachineID))
	_ = vm
	r.create(req)
	vm = r.ps.VM(vmName(req.MachineID))
	if vm.Switch != "Build" || vm.VLAN != 40 {
		t.Errorf("vm = %+v", vm)
	}
	if !strings.Contains(r.ps.LastInput("create"), `"generation":2`) {
		t.Errorf("input = %s", r.ps.LastInput("create"))
	}
	var dns, allow bool
	for _, rule := range vm.Rules {
		dns = dns || rule.Dest == "192.168.1.1/32" && rule.Port == 53
		allow = allow || rule.Dest == "10.9.0.0/16" && rule.Allow
	}
	if !dns || !allow {
		t.Errorf("rules = %+v", vm.Rules)
	}
	req2 := r.request()
	zero := 0
	_ = zero
	req2.Shape.ProviderConfig = map[string]any{"vlan": 0}
	r.create(req2)
	if r.ps.VM(vmName(req2.MachineID)).VLAN != 0 {
		t.Error("the shape's VLAN 0 was not used")
	}
}

func TestCreateAgain(t *testing.T) {
	r := newRig(t)
	req := r.request()
	first := r.create(req)
	req.Bootstrap = &fleet.Bootstrap{ServerURL: "https://192.0.2.7", Token: "fcb_second-token"}
	second := r.create(req)
	if r.ps.Count() != 1 || r.ps.DirCount() != 1 {
		t.Fatalf("%d VMs, %d directories", r.ps.Count(), r.ps.DirCount())
	}
	if second.ExternalID == first.ExternalID {
		t.Error("the old VM was kept")
	}
	if !strings.Contains(r.ps.VM(vmName(req.MachineID)).ConfigDisk, "fcb_second-token") {
		t.Error("the new token was not delivered")
	}
}

func TestCreateRefuses(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *rig, req *fleet.CreateRequest)
		want string
	}{
		{"no bootstrap", func(_ *rig, req *fleet.CreateRequest) { req.Bootstrap = nil }, "no bootstrap"},
		{"linux guest", func(_ *rig, req *fleet.CreateRequest) { req.Shape.OS = domain.OSLinux }, "Windows guests"},
		{"no size", func(_ *rig, req *fleet.CreateRequest) { req.Shape.MemoryMB = 0 }, "needs cpu"},
		{"token the guest refuses", func(_ *rig, req *fleet.CreateRequest) { req.Bootstrap.Token = "a b" }, "characters"},
		{"unknown key", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["bridge"] = "x" }, "unknown field"},
		{"bad switch", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["switch"] = "a;b" }, "not a switch name"},
		{"bad vlan", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["vlan"] = 9999 }, "vlan"},
		{"qcow2 image", func(r *rig, req *fleet.CreateRequest) {
			req.Image.Format = domain.ImageQCOW2
			req.Image.Signature = images.Sign(r.key, req.Image)
		}, "vhdx"},
		{"unsigned image", func(_ *rig, req *fleet.CreateRequest) { req.Image.Signature = "" }, "signature"},
		{"loopback server", func(_ *rig, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://127.0.0.1:8080" }, "loopback"},
		{"missing switch", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["switch"] = "Elsewhere" }, "no virtual switch"},
		{"disk smaller than the image", func(_ *rig, req *fleet.CreateRequest) { req.Shape.DiskGB = 40 }, "more than the shape"},
		{"tags too long", func(_ *rig, req *fleet.CreateRequest) {
			for i := range 20 {
				req.Tags[strings.Repeat("k", i+1)] = strings.Repeat("x", 250)
			}
		}, "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			req := r.request()
			tt.do(r, &req)
			_, err := r.p.Create(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if r.ps.Count() != 0 || r.ps.DirCount() != 0 {
				t.Errorf("left %d VMs, %d directories", r.ps.Count(), r.ps.DirCount())
			}
			if req.Bootstrap != nil && strings.Contains(err.Error(), req.Bootstrap.Token) {
				t.Errorf("the token is in the error: %v", err)
			}
		})
	}
}

func TestCreateCapacity(t *testing.T) {
	r := newRig(t)
	r.cfg.MaxMachines = 1
	r.rebuild()
	r.create(r.request())
	if _, err := r.p.Create(context.Background(), r.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("at the limit: %v", err)
	}
	r2 := newRig(t)
	r2.ps.Host.FreeMemoryMB = 4000
	if _, err := r2.p.Create(context.Background(), r2.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("without memory: %v", err)
	}
	r3 := newRig(t)
	r3.ps.Host.FreeDiskGB = 2
	if _, err := r3.p.Create(context.Background(), r3.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("without disk: %v", err)
	}
}

func TestCreateCleansUpWhatFails(t *testing.T) {
	r := newRig(t)
	req := r.request()
	r.ps.SetFailOn(failScript("create", "Add-VMHardDiskDrive failed with "+req.Bootstrap.Token))
	_, err := r.p.Create(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "Add-VMHardDiskDrive") || strings.Contains(err.Error(), req.Bootstrap.Token) {
		t.Fatalf("err = %v", err)
	}
	if r.ps.Ran("destroy") != 1 {
		t.Errorf("destroy ran %d times", r.ps.Ran("destroy"))
	}
	// The cleaning is of the directory this machine would have had, and of no VM
	// by name: the VM, if the script left one, is found by its notes.
	if in := r.ps.LastInput("destroy"); !strings.Contains(in, vmName(req.MachineID)) || !strings.Contains(in, `"id":""`) {
		t.Errorf("destroy input = %s", in)
	}
	// Even when the caller has gone.
	r2 := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r2.ps.SetFailOn(func(script string) string {
		if script == "create" {
			cancel() // the caller goes while the machine is being made
			return "boom"
		}
		return ""
	})
	if _, err := r2.p.Create(ctx, r2.request()); err == nil {
		t.Error("no error")
	}
	if r2.ps.Ran("destroy") != 1 {
		t.Error("a cancelled caller stopped the cleaning")
	}
}

func TestCreateFailuresBeforeAnythingIsMade(t *testing.T) {
	r := newRig(t)
	r.ps.SetFailOn(failScript("list", "access denied"))
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("list: %v", err)
	}
	r.ps.SetFailOn(failScript("host", "no module"))
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "no module") {
		t.Errorf("host: %v", err)
	}
	if r.ps.Ran("create") != 0 {
		t.Error("create ran")
	}
}

func TestDestroy(t *testing.T) {
	r := newRig(t)
	req := r.request()
	inst := r.create(req)
	r.ps.AddVM("my-database", "production")
	r.ps.AddVM("forecrew-lookalike", "no notes of ours")
	other := vmcommon.NewMeta(uuid.New()) // notes of ours on a name that is not the machine's
	liar := r.ps.AddVM("forecrew-aaaaaaaaaaaa", other.Encode())
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Fatal(err)
	}
	if r.ps.VM(vmName(req.MachineID)) != nil || r.ps.DirCount() != 0 {
		t.Error("it is still there")
	}
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Errorf("again: %v", err)
	}
	if err := r.p.Destroy(context.Background(), uuid.New().String()); err != nil {
		t.Errorf("unknown: %v", err)
	}
	// A VM that does not carry Forecrew's name is not even looked at.
	if err := r.p.Destroy(context.Background(), r.ps.VM("my-database").ID); err != nil {
		t.Errorf("unlisted: %v", err)
	}
	// One that does, but is not what its notes say, is refused.
	for _, id := range []string{r.ps.VM("forecrew-lookalike").ID, liar.ID} {
		if err := r.p.Destroy(context.Background(), id); err == nil || !strings.Contains(err.Error(), "leaving it alone") {
			t.Errorf("destroy %s: %v", id, err)
		}
	}
	if r.ps.Count() != 3 {
		t.Errorf("a VM that is not ours was destroyed: %d left", r.ps.Count())
	}
	r.ps.SetFailOn(failScript("list", "denied"))
	if err := r.p.Destroy(context.Background(), "x"); err == nil {
		t.Error("list failure swallowed")
	}
	r.ps.SetFailOn(nil)
	inst = r.create(r.request())
	r.ps.SetFailOn(failScript("destroy", "the disk is in use"))
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("destroy failure: %v", err)
	}
	r.ps.SetFailOn(nil)
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil { // a failure leaves it to try again
		t.Errorf("retry: %v", err)
	}
}

func TestList(t *testing.T) {
	r := newRig(t)
	a, b := r.create(r.request()), r.create(r.request())
	r.ps.AddVM("my-database", "production")
	r.ps.AddVM("forecrew-lookalike", "not ours")
	got, err := r.p.List(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("listed %+v, %v", got, err)
	}
	for _, i := range got {
		if !i.Running || i.Tags[fleet.TagMachine] != i.MachineID.String() || (i.ExternalID != a.ExternalID && i.ExternalID != b.ExternalID) {
			t.Errorf("instance %+v", i)
		}
	}
	r.ps.SetFailOn(failScript("list", "cluster is down"))
	if _, err := r.p.List(context.Background()); err == nil {
		t.Error("no error")
	}
}

func TestListShowsAStoppedMachine(t *testing.T) {
	r := newRig(t)
	req := r.request()
	r.create(req)
	_ = r.p.ps.Run(context.Background(), "stop", map[string]any{"id": r.ps.VM(vmName(req.MachineID)).ID}, nil)
	got, _ := r.p.List(context.Background())
	if len(got) != 1 || got[0].Running {
		t.Errorf("got %+v", got)
	}
}

func TestUpdate(t *testing.T) {
	r := newRig(t)
	req := r.request()
	inst := r.create(req)
	org := uuid.New().String()
	up := fleet.UpdateRequest{Tags: map[string]string{fleet.TagOrg: org}, NetworkAllow: []string{"10.20.0.0/16"}, Deadline: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	for range 2 {
		if err := r.p.Update(context.Background(), inst.ExternalID, up); err != nil {
			t.Fatal(err)
		}
	}
	vm := r.ps.VM(vmName(req.MachineID))
	meta, _ := vmcommon.ParseMeta(vm.Notes)
	if meta.Tags[fleet.TagOrg] != org || !meta.Deadline.Equal(up.Deadline) || meta.Allow[0] != "10.20.0.0/16" || meta.Server[0] != "192.0.2.7" {
		t.Errorf("meta = %+v", meta)
	}
	var opened bool
	for _, rule := range vm.Rules {
		opened = opened || rule.Dest == "10.20.0.0/16" && rule.Allow
	}
	if !opened {
		t.Errorf("rules = %+v", vm.Rules)
	}
	// A change of tags alone leaves the rules.
	up.Tags["more"] = "x"
	if err := r.p.Update(context.Background(), inst.ExternalID, up); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.ps.LastInput("update"), `"apply_rules":false`) {
		t.Errorf("input = %s", r.ps.LastInput("update"))
	}
}

func TestUpdateFailures(t *testing.T) {
	r := newRig(t)
	inst := r.create(r.request())
	if err := r.p.Update(context.Background(), uuid.New().String(), fleet.UpdateRequest{}); err == nil || !strings.Contains(err.Error(), "no machine") {
		t.Errorf("unknown: %v", err)
	}
	r.ps.AddVM("my-database", "production")
	if err := r.p.Update(context.Background(), r.ps.VM("my-database").ID, fleet.UpdateRequest{}); err == nil {
		t.Error("changed a VM that is not ours")
	}
	if r.ps.VM("my-database").Notes != "production" {
		t.Error("its notes were changed")
	}
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{NetworkAllow: []string{"no-such-host.invalid"}}); err == nil {
		t.Error("an allowlist that does not resolve was accepted")
	}
	huge := map[string]string{}
	for i := range 20 {
		huge[strings.Repeat("k", i+1)] = strings.Repeat("x", 250)
	}
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{Tags: huge}); err == nil {
		t.Error("tags too long for the notes accepted")
	}
	r.ps.SetFailOn(failScript("update", "denied"))
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{}); err == nil {
		t.Error("script failure swallowed")
	}
	r.ps.SetFailOn(failScript("list", "denied"))
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{}); err == nil {
		t.Error("list failure swallowed")
	}
}

func TestDeadlinePowersTheMachineOff(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(time.Second)
	r.create(req)
	waitFor(t, func() bool { return r.ps.VM(vmName(req.MachineID)).State == "Off" })

	// A deadline that Update clears never fires.
	req2 := r.request()
	req2.Deadline = time.Now().Add(2 * time.Second)
	inst := r.create(req2)
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if r.ps.VM(vmName(req2.MachineID)).State != "Running" {
		t.Error("a machine whose deadline was cleared was stopped")
	}
}

func TestListArmsDeadlinesAfterARestart(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(time.Second)
	r.create(req)
	r.rebuild() // the agent restarted
	for range 2 {
		if _, err := r.p.List(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return r.ps.VM(vmName(req.MachineID)).State == "Off" })
}

func TestDestroyDisarms(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(time.Hour)
	inst := r.create(req)
	if r.p.deadlines.Armed() != 1 {
		t.Fatal("not armed")
	}
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil || r.p.deadlines.Armed() != 0 {
		t.Errorf("err %v, armed %d", err, r.p.deadlines.Armed())
	}
}

func TestStopFailureIsLogged(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(200 * time.Millisecond)
	r.ps.SetFailOn(failScript("stop", "denied"))
	r.create(req)
	waitFor(t, func() bool { return r.ps.Ran("stop") == 1 })
}

func TestCapabilities(t *testing.T) {
	r := newRig(t)
	c, err := r.p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxCPU != 16 || c.MaxMemoryMB != 65536-memoryReserveMB || c.MaxDiskGB != 500 || c.MaxMachines != 3 || c.OS[0] != domain.OSWindows || c.ImageFormats[0] != domain.ImageVHDX || c.Arch[0] != domain.ArchAMD64 {
		t.Errorf("capabilities = %+v", c)
	}
	if len(c.Checks) != 3 {
		t.Errorf("checks = %+v", c.Checks)
	}
	for _, ch := range c.Checks {
		if ch.Status != "ok" {
			t.Errorf("check %+v", ch)
		}
	}
}

func TestCapabilityChecks(t *testing.T) {
	tests := []struct {
		name   string
		do     func(r *rig)
		check  string
		status string
	}{
		{"no hyper-v", func(r *rig) { r.ps.Host.NoModule = true }, "hyper-v", "fail"},
		{"no switch", func(r *rig) { r.ps.Host.Switches = nil }, "switch", "fail"},
		{"no keys", func(r *rig) { _ = os.Remove(r.keys) }, "image keys", "fail"},
		{"little disk", func(r *rig) { r.ps.Host.FreeDiskGB = 3 }, "disk", "warn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.do(r)
			c, err := r.p.Capabilities(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, ch := range c.Checks {
				if ch.Name == tt.check {
					if ch.Status != tt.status {
						t.Errorf("%+v, want %s", ch, tt.status)
					}
					return
				}
			}
			t.Errorf("no check %q in %+v", tt.check, c.Checks)
		})
	}
}

func TestPruneCacheOnList(t *testing.T) {
	r := newRig(t)
	r.create(r.request())
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(r.base, old, old)
	if _, err := r.p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.base); err == nil {
		t.Error("an old image stayed")
	}
	// Once an hour at most.
	r.rebuild()
	r.p.lastGC = time.Now()
	r.p.cache.Dir = filepath.Join(t.TempDir(), "missing")
	if _, err := r.p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPruneFailureIsLogged(t *testing.T) {
	r := newRig(t)
	file := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(file, nil, 0o600)
	r.p.cache.Dir = file // not a directory: reading it fails
	if _, err := r.p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderIsRegistered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hyperv.yaml")
	if err := os.WriteFile(path, []byte("switch: Lab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfig, path)
	p, err := newRegistered()
	if err != nil || p == nil {
		t.Fatalf("%v %v", p, err)
	}
	t.Setenv(envConfig, filepath.Join(t.TempDir(), "none.yaml")) // defaults need a switch
	if _, err := newRegistered(); err == nil || !strings.Contains(err.Error(), "switch") {
		t.Errorf("err = %v", err)
	}
}

func TestDoctorShowsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hyperv.yaml")
	if err := os.WriteFile(path, []byte("switch: Lab\nstate_dir: "+filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfig, path)
	var out strings.Builder
	// This machine is no Windows host with Hyper-V: doctor says what it lacks,
	// and fails.
	if fleetagent.Doctor(context.Background(), &out, domain.ProviderHyperV, "test") {
		t.Errorf("doctor passed on a machine without Hyper-V:\n%s", out.String())
	}
	for _, want := range []string{"fail os: runs on Windows", "fail powershell", "fail hyper-v"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	// Nothing was written before the directories could be closed.
	if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
		t.Error("doctor made files on a host where it could not close them")
	}
}

// Names are not unique in Hyper-V: an operator's VM with the name of a machine is
// never destroyed, stopped or changed in its place.
func TestAnOperatorsVMWithTheSameNameIsLeftAlone(t *testing.T) {
	r := newRig(t)
	req := r.request()
	theirs := r.ps.AddVM(vmName(req.MachineID), "an operator's own VM")
	req.Deadline = time.Now().Add(300 * time.Millisecond)
	inst := r.create(req)
	if inst.ExternalID == theirs.ID || r.ps.Count() != 2 {
		t.Fatalf("instance %+v, %d VMs", inst, r.ps.Count())
	}
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{Deadline: req.Deadline, Tags: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.ps.VMByID(inst.ExternalID).State == "Off" })
	if got := r.ps.VMByID(theirs.ID); got.State != "Running" || got.Notes != "an operator's own VM" {
		t.Errorf("their VM was changed: %+v", got)
	}
	// Creating again replaces ours only; destroying removes ours only.
	again := r.create(req)
	if again.ExternalID == inst.ExternalID || r.ps.VMByID(inst.ExternalID) != nil || r.ps.VMByID(theirs.ID) == nil {
		t.Errorf("after a second create: %+v", again)
	}
	if err := r.p.Destroy(context.Background(), again.ExternalID); err != nil {
		t.Fatal(err)
	}
	if r.ps.Count() != 1 || r.ps.VMByID(theirs.ID) == nil {
		t.Errorf("%d VMs left, theirs there: %v", r.ps.Count(), r.ps.VMByID(theirs.ID) != nil)
	}
	// And a create that fails does not remove it either.
	r.ps.SetFailOn(failScript("create", "boom"))
	if _, err := r.p.Create(context.Background(), req); err == nil || r.ps.VMByID(theirs.ID) == nil {
		t.Errorf("err %v, theirs there: %v", err, r.ps.VMByID(theirs.ID) != nil)
	}
}

// The directories are closed first, once, and nothing is done while they are open.
func TestNothingRunsBeforeTheDirectoriesAreClosed(t *testing.T) {
	r := newRig(t)
	r.ps.SetFailOn(failScript("secure", "C:\\FC is open to BUILTIN\\Users"))
	req := r.request()
	if _, err := r.p.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "open to BUILTIN") {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.p.List(context.Background()); err == nil || !strings.Contains(err.Error(), "not closed") {
		t.Errorf("list: %v", err)
	}
	if err := r.p.Destroy(context.Background(), "x"); err == nil {
		t.Error("destroy ran")
	}
	if c, _ := r.p.Capabilities(context.Background()); len(c.Checks) != 1 || c.Checks[0].Status != "fail" {
		t.Errorf("checks = %+v", c.Checks)
	}
	if r.ps.Ran("create")+r.ps.Ran("list")+r.ps.Ran("host")+r.ps.Ran("destroy") != 0 || *r.fetch != 0 || r.ps.Count() != 0 {
		t.Errorf("something ran: %v, %d fetches", r.ps.Calls, *r.fetch)
	}
	// Closed now: it goes on, and asks only once more.
	r.ps.SetFailOn(nil)
	r.create(req)
	r.create(r.request())
	if _, err := r.p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.ps.Ran("secure") != 5 || r.ps.SecureCalls != 1 { // four refusals, one success, and no more
		t.Errorf("secure was asked %d times and succeeded %d", r.ps.Ran("secure"), r.ps.SecureCalls)
	}
	want := []string{r.cfg.StateDir, `C:\FC\vms`, `C:\FC\images`, `C:\FC\scripts`}
	if !slices.Equal(r.ps.Secured, want) {
		t.Errorf("closed %v, want %v", r.ps.Secured, want)
	}
}

// network_allow of everything opens the internet and not the host, its adapter on
// the switch or the network that holds the other machines.
func TestAllowEverythingNeverOpensTheHost(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.NetworkAllow = []string{"0.0.0.0/0"}
	r.create(req)
	rules := r.ps.VM(vmName(req.MachineID)).Rules
	at := func(dest string, allow bool) int {
		for i, rule := range rules {
			if rule.Dest == dest && rule.Allow == allow && rule.Port == 0 {
				return i
			}
		}
		return -1
	}
	open := at("0.0.0.0/0", true)
	for _, never := range []string{"192.168.1.10/32", "192.168.1.0/24", "203.0.113.9/32"} {
		if i := at(never, false); i < 0 || i > open {
			t.Errorf("%s: closed at %d, the allowlist at %d", never, i, open)
		}
	}
	if i := at("203.0.113.0/28", false); i < open {
		t.Errorf("a network the host is on is closed at %d, before the allowlist at %d: network_allow may open it", i, open)
	}
	// An update keeps them.
	inst, _ := r.p.List(context.Background())
	if err := r.p.Update(context.Background(), inst[0].ExternalID, fleet.UpdateRequest{Tags: req.Tags, NetworkAllow: []string{"0.0.0.0/0", "10.0.0.0/8"}}); err != nil {
		t.Fatal(err)
	}
	rules = r.ps.VM(vmName(req.MachineID)).Rules
	if i, j := at("192.168.1.10/32", false), at("0.0.0.0/0", true); i < 0 || i > j {
		t.Errorf("after an update: closed at %d, the allowlist at %d", i, j)
	}
}

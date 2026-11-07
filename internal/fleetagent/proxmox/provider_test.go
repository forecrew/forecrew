package proxmox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/vmcommon"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/testkit"
)

func vmidOf(t *testing.T, inst fleet.Instance) int {
	t.Helper()
	_, rest, _ := strings.Cut(inst.ExternalID, ":")
	n, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("external ID %q", inst.ExternalID)
	}
	return n
}

func TestCreateLinux(t *testing.T) {
	r := newRig(t)
	req := r.request()
	inst := r.create(req)
	vm := r.pve.VM(vmidOf(t, inst))
	if inst.MachineID != req.MachineID || !inst.Running || inst.Tags[fleet.TagMachine] != req.MachineID.String() || inst.Tags["site"] != "lab" {
		t.Fatalf("instance = %+v", inst)
	}
	if vm == nil || vm.Status != "running" || !strings.HasPrefix(vm.Name, "forecrew-") {
		t.Fatalf("vm = %+v", vm)
	}
	meta, ok := vmcommon.ParseMeta(vm.Config["description"])
	if !ok || meta.ID() != req.MachineID || meta.Port != 443 || meta.Server[0] != "192.0.2.7" {
		t.Fatalf("meta = %+v %v", meta, ok)
	}
	checkConfig(t, vm, req)
	// A linked clone, not a copy.
	if f := r.pve.Forms("/clone"); len(f) != 1 || f[0]["full"] != "0" || f[0]["storage"] != "" {
		t.Errorf("clone form = %v", f)
	}
	// The token went into the guest and nowhere else.
	if got := vm.Files["/run/forecrew/bootstrap.env"]; got != "FORECREW_SERVER_URL=https://192.0.2.7\nFORECREW_BOOTSTRAP_TOKEN="+req.Bootstrap.Token+"\n" {
		t.Errorf("guest file = %q", got)
	}
	for _, f := range r.pve.Forms("") {
		for k, v := range f {
			if k != "content" && strings.Contains(v, req.Bootstrap.Token) {
				t.Errorf("the token is in %s", k)
			}
		}
	}
	checkFirewall(t, vm)
}

func checkConfig(t *testing.T, vm *testkit.FakeVM, req fleet.CreateRequest) {
	t.Helper()
	for k, want := range map[string]string{"cores": "2", "memory": "4096", "agent": "1", "net0": "virtio,bridge=vmbr0,firewall=1", "balloon": "0", "onboot": "0"} {
		if vm.Config[k] != want {
			t.Errorf("config %s = %q, want %q", k, vm.Config[k], want)
		}
	}
	if !strings.Contains(vm.Config["tags"], "forecrew-machine-"+req.MachineID.String()) || !strings.Contains(vm.Config["tags"], "forecrew-org-") {
		t.Errorf("tags = %q", vm.Config["tags"])
	}
	if !strings.HasSuffix(vm.Config["scsi0"], "size=40G") {
		t.Errorf("disk = %q", vm.Config["scsi0"])
	}
}

func checkFirewall(t *testing.T, vm *testkit.FakeVM) {
	t.Helper()
	if vm.FirewallOptions["enable"] != "1" || vm.FirewallOptions["policy_in"] != "DROP" || vm.FirewallOptions["dhcp"] != "1" {
		t.Errorf("firewall options = %v", vm.FirewallOptions)
	}
	if len(vm.Rules) == 0 || vm.Rules[0].Dest != "192.0.2.7/32" || vm.Rules[0].Action != "ACCEPT" || vm.Rules[0].DPort != "443" || vm.Rules[0].Type != "out" {
		t.Errorf("rules = %+v", vm.Rules)
	}
	// The internet is an IPv4 rule after the private ranges, and the output
	// policy drops what no rule names, which is all of IPv6.
	n := len(vm.Rules)
	if last := vm.Rules[n-1]; last.Action != "ACCEPT" || last.Dest != "0.0.0.0/0" || vm.Rules[n-2].Action != "DROP" {
		t.Errorf("the last rules = %+v", vm.Rules[n-2:])
	}
	if _, has := vm.FirewallOptions["ipfilter"]; vm.FirewallOptions["policy_out"] != "DROP" || has {
		t.Errorf("firewall options = %v", vm.FirewallOptions)
	}
}

func TestCreateKeepsRuleOrderWhateverTheClusterDoes(t *testing.T) {
	for _, top := range []bool{true, false} {
		r := newRig(t)
		r.pve.FirewallInsertsAtTop = top
		inst := r.create(r.request())
		rules := r.pve.VM(vmidOf(t, inst)).Rules
		if rules[0].Dest != "192.0.2.7/32" || rules[len(rules)-1].Dest != "0.0.0.0/0" {
			t.Errorf("insert at top %v: rules = %+v", top, rules)
		}
	}
}

func TestCreateWindowsWithLocalSettings(t *testing.T) {
	r := newRig(t)
	r.cfg.DNS, r.cfg.VLAN = []string{"192.168.1.1"}, 30
	r.rebuild()
	img := r.img
	img.OS, img.Name = domain.OSWindows, "forecrew-windows"
	img.Signature = images.Sign(r.key, img)
	r.pve.AddTemplate(9001, "pve", imageTag(img), map[string]string{"net0": "e1000=AA:BB:CC:00:00:02,bridge=vmbr1", "sata0": "local-lvm:base-9001-disk-0,size=60G"})
	req := r.request()
	req.Image = img
	req.Shape.OS, req.Shape.DiskGB = domain.OSWindows, 80
	r.pve.Nodes["pve"].Network = append(r.pve.Nodes["pve"].Network, testkit.FakeIface{Iface: "vmbr5", Type: "bridge", CIDR: "10.50.0.1/24"})
	req.Shape.ProviderConfig = map[string]any{"template": 9001, "disk": "sata0", "storage": "local-lvm", "pool": "forecrew", "vlan": 0, "bridge": "vmbr5"}
	req.NetworkAllow = []string{"10.9.0.0/16"}
	inst := r.create(req)
	vm := r.pve.VM(vmidOf(t, inst))
	if vm.Files[`C:\ProgramData\Forecrew\run\bootstrap.env`] == "" {
		t.Errorf("files = %v", vm.Files)
	}
	if vm.Config["net0"] != "e1000,bridge=vmbr5,firewall=1" { // the model of the template, the shape's bridge, and no VLAN
		t.Errorf("net0 = %q", vm.Config["net0"])
	}
	if !strings.HasSuffix(vm.Config["sata0"], "size=80G") {
		t.Errorf("disk = %q", vm.Config["sata0"])
	}
	if f := r.pve.Forms("/clone"); f[len(f)-1]["full"] != "1" || f[len(f)-1]["storage"] != "local-lvm" || f[len(f)-1]["pool"] != "forecrew" {
		t.Errorf("clone form = %v", f)
	}
	var dns, allow bool
	for _, rule := range vm.Rules {
		dns = dns || rule.Dest == "192.168.1.1/32" && rule.DPort == "53"
		allow = allow || rule.Dest == "10.9.0.0/16" && rule.Action == "ACCEPT"
	}
	if !dns || !allow {
		t.Errorf("rules = %+v", vm.Rules)
	}
	// The location's VLAN applies where the shape names none.
	req2 := r.request()
	req2.Image, req2.Shape.OS = img, domain.OSWindows
	req2.Shape.DiskGB = 60
	req2.Shape.ProviderConfig = map[string]any{"template": 9001, "disk": "sata0"}
	vm2 := r.pve.VM(vmidOf(t, r.create(req2)))
	if vm2.Config["net0"] != "e1000,bridge=vmbr0,firewall=1,tag=30" {
		t.Errorf("net0 = %q", vm2.Config["net0"])
	}
}

func TestCreateAgain(t *testing.T) {
	r := newRig(t)
	req := r.request()
	first := r.create(req)
	req.Bootstrap = &fleet.Bootstrap{ServerURL: "https://192.0.2.7", Token: "fcb_second-token"}
	second := r.create(req)
	if r.pve.VM(vmidOf(t, first)) != nil && vmidOf(t, first) != vmidOf(t, second) {
		t.Error("the first VM is still there")
	}
	if r.pve.Count() != 1 {
		t.Fatalf("%d VMs", r.pve.Count())
	}
	if !strings.Contains(r.pve.VM(vmidOf(t, second)).Files["/run/forecrew/bootstrap.env"], "fcb_second-token") {
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
		{"macos", func(_ *rig, req *fleet.CreateRequest) { req.Shape.OS = domain.OSMacOS }, "Linux and Windows"},
		{"rootfs image", func(r *rig, req *fleet.CreateRequest) {
			req.Image.Format = domain.ImageRootfs
			req.Image.Signature = images.Sign(r.key, req.Image)
		}, "qcow2"},
		{"image of another OS", func(r *rig, req *fleet.CreateRequest) {
			req.Image.OS = domain.OSWindows
			req.Image.Signature = images.Sign(r.key, req.Image)
		}, "qcow2"},
		{"unsigned image", func(_ *rig, req *fleet.CreateRequest) { req.Image.Signature = "" }, "signature"},
		{"unknown key", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["strage"] = "x" }, "unknown field"},
		{"no template", func(_ *rig, req *fleet.CreateRequest) { delete(req.Shape.ProviderConfig, "template") }, "template"},
		{"other node", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["node"] = "elsewhere" }, "not one of the nodes"},
		{"bad storage", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["storage"] = "a/b" }, "not a name"},
		{"bad disk", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["disk"] = "../x" }, "disk key"},
		{"bad bridge", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["bridge"] = "a b" }, "bridge"},
		{"bad vlan", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["vlan"] = 5000 }, "vlan"},
		{"no size", func(_ *rig, req *fleet.CreateRequest) { req.Shape.CPU = 0 }, "needs cpu"},
		{"loopback server", func(_ *rig, req *fleet.CreateRequest) { req.Bootstrap.ServerURL = "http://127.0.0.1:8080" }, "loopback"},
		{"template missing", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["template"] = 9100 }, "does not exist"},
		{"disk bigger than the shape", func(_ *rig, req *fleet.CreateRequest) { req.Shape.DiskGB = 10 }, "more than the shape"},
		{"no such disk", func(_ *rig, req *fleet.CreateRequest) { req.Shape.ProviderConfig["disk"] = "scsi5" }, "no disk scsi5"},
		{"guest cannot read the token", func(_ *rig, req *fleet.CreateRequest) { req.Bootstrap.Token = "fcb_a b" }, "characters"},
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
			if r.pve.Count() != 0 {
				t.Errorf("a VM was left: %d", r.pve.Count())
			}
		})
	}
}

func TestCreateRefusesATemplateThatIsNotTheImage(t *testing.T) {
	r := newRig(t)
	r.pve.AddTemplate(9002, "pve", "forecrew-image-0000", nil)
	r.pve.AddTemplate(9003, "pve", "", nil)
	r.pve.AddVM(9004, "pve", "my-database", "production")
	req := r.request()
	req.Shape.ProviderConfig["template"] = 9002
	if _, err := r.p.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "was not made from image") || !strings.Contains(err.Error(), imageTag(r.img)) {
		t.Errorf("wrong tag: %v", err)
	}
	req.Shape.ProviderConfig["template"] = 9003
	if _, err := r.p.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "was not made from image") {
		t.Errorf("no tag: %v", err)
	}
	// An ordinary VM is no template, whatever the shape says.
	req.Shape.ProviderConfig["template"] = 9004
	if _, err := r.p.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not a template") {
		t.Errorf("plain VM: %v", err)
	}
	if got := r.pve.CallsTo("/clone"); len(got) != 0 {
		t.Errorf("cloned anyway: %v", got)
	}
}

func TestCreateCapacity(t *testing.T) {
	r := newRig(t)
	r.cfg.MaxMachines = 2
	r.rebuild()
	r.create(r.request())
	r.create(r.request())
	if _, err := r.p.Create(context.Background(), r.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("at the limit: %v", err)
	}
	r2 := newRig(t)
	r2.pve.Nodes["pve"].MemFree = 2 << 30
	if _, err := r2.p.Create(context.Background(), r2.request()); !errors.Is(err, fleet.ErrCapacity) {
		t.Fatalf("without memory: %v", err)
	}
}

func TestCreateCleansUpWhatFails(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *rig)
		want string
	}{
		{"resize", func(r *rig) {
			r.pve.FailOn = func(c string) (int, string) {
				if strings.HasSuffix(c, "/resize") {
					return 500, "resize failed"
				}
				return 0, ""
			}
		}, "resize failed"},
		{"config", func(r *rig) {
			r.pve.FailOn = failOn("PUT", "/config", "permission check failed (VM.Config.Network)")
		}, "permission check"},
		{"firewall", func(r *rig) {
			r.pve.FailOn = failOn("POST", "/firewall/rules", "firewall is broken")
		}, "network rules"},
		{"start", func(r *rig) { r.pve.FailTask["/status/start"] = "start failed: not enough memory" }, "not enough memory"},
		{"agent never answers", func(r *rig) { r.pve.AgentPings = 1 << 30 }, "guest agent"},
		{"file write", func(r *rig) {
			r.pve.FailOn = failOn("POST", "/agent/file-write", "guest agent error with fcb_AAAAbbbb1234-secret_token inside")
		}, "deliver the bootstrap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			req := r.request()
			req.Shape.BootTimeoutSeconds = 1
			tt.do(r)
			_, err := r.p.Create(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), req.Bootstrap.Token) {
				t.Errorf("the token is in the error: %v", err)
			}
			r.pve.FailOn = nil
			if r.pve.Count() != 0 {
				t.Errorf("%d VMs left behind", r.pve.Count())
			}
		})
	}
}

func failOn(method, suffix, msg string) func(string) (int, string) {
	return func(c string) (int, string) {
		if strings.HasPrefix(c, method+" ") && strings.HasSuffix(c, suffix) {
			return 500, msg
		}
		return 0, ""
	}
}

func TestCreateFailedCloneLeavesNothing(t *testing.T) {
	r := newRig(t)
	r.pve.FailTask["/clone"] = "clone failed: no space left"
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v", err)
	}
	if r.pve.Count() != 0 {
		t.Errorf("%d VMs", r.pve.Count())
	}
}

func TestCreateTakesAnotherVMIDWhenOneIsTaken(t *testing.T) {
	r := newRig(t)
	calls := 0
	r.pve.FailOn = func(c string) (int, string) {
		if strings.HasSuffix(c, "/clone") {
			if calls++; calls <= 2 {
				return 500, "Parameter verification failed. newid: VM 1 already exists on node 'pve'"
			}
		}
		return 0, ""
	}
	r.create(r.request())
	if calls != 3 {
		t.Errorf("clone calls = %d", calls)
	}
	// Always taken: it gives up.
	calls = -100
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "free VMID") {
		t.Errorf("err = %v", err)
	}
}

func TestCreateWaitsForTheGuestAgent(t *testing.T) {
	r := newRig(t)
	r.pve.AgentPings = 3
	inst := r.create(r.request())
	if len(r.pve.CallsTo("agent/ping")) != 4 {
		t.Errorf("pings = %v", r.pve.CallsTo("agent/ping"))
	}
	if r.pve.VM(vmidOf(t, inst)).Files["/run/forecrew/bootstrap.env"] == "" {
		t.Error("nothing delivered")
	}
}

func TestCreateStopsWhenTheCallerDoes(t *testing.T) {
	r := newRig(t)
	r.pve.AgentPings = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	req := r.request()
	req.Shape.BootTimeoutSeconds = 60
	if _, err := r.p.Create(ctx, req); err == nil {
		t.Fatal("no error")
	}
	if r.pve.Count() != 0 {
		t.Errorf("%d VMs left after cancelling", r.pve.Count())
	}
}

func TestDestroy(t *testing.T) {
	r := newRig(t)
	inst := r.create(r.request())
	r.pve.AddVM(500, "pve", "my-database", "production")
	r.pve.AddVM(501, "pve", "forecrew-lookalike", "no notes of ours")
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Fatal(err)
	}
	if r.pve.VM(vmidOf(t, inst)) != nil {
		t.Error("still there")
	}
	// Again, and what was never there.
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Errorf("again: %v", err)
	}
	if err := r.p.Destroy(context.Background(), "pve:7777"); err != nil {
		t.Errorf("unknown: %v", err)
	}
	// Somebody else's VMs are not ours to destroy, even by their IDs.
	for _, id := range []string{"pve:500", "pve:501", "pve:" + strconv.Itoa(tplID)} {
		if err := r.p.Destroy(context.Background(), id); err == nil || !strings.Contains(err.Error(), "leaving it alone") {
			t.Errorf("destroy %s: %v", id, err)
		}
	}
	if r.pve.VM(500) == nil || r.pve.VM(501) == nil || r.pve.VM(tplID) == nil {
		t.Error("a VM that is not ours was destroyed")
	}
	if err := r.p.Destroy(context.Background(), "nonsense"); err == nil {
		t.Error("nonsense accepted")
	}
	if err := r.p.Destroy(context.Background(), "pve:abc"); err == nil {
		t.Error("non-number accepted")
	}
}

func TestDestroyFailures(t *testing.T) {
	r := newRig(t)
	inst := r.create(r.request())
	r.pve.FailOn = failOn("POST", "/status/stop", "cannot stop")
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err == nil || !strings.Contains(err.Error(), "cannot stop") {
		t.Errorf("stop: %v", err)
	}
	r.pve.FailOn = failOn("DELETE", "", "storage is locked")
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err == nil || !strings.Contains(err.Error(), "storage is locked") {
		t.Errorf("delete: %v", err)
	}
	r.pve.FailOn = nil
	if r.pve.VM(vmidOf(t, inst)) == nil {
		t.Fatal("it went")
	}
	// A failure leaves it to try again.
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Errorf("retry: %v", err)
	}
	// A VM that goes while it is being destroyed.
	inst = r.create(r.request())
	r.pve.FailOn = func(c string) (int, string) {
		if strings.HasPrefix(c, "DELETE") {
			return 500, "Configuration file 'nodes/pve/qemu-server/1.conf' does not exist"
		}
		return 0, ""
	}
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Errorf("gone meanwhile: %v", err)
	}
}

func TestDestroyFindsAMovedVM(t *testing.T) {
	r := newRig(t)
	r.cfg.Nodes = []string{"pve", "pve2"}
	r.pve.Nodes["pve2"] = r.pve.Nodes["pve"]
	r.rebuild()
	inst := r.create(r.request())
	r.pve.VMs[vmidOf(t, inst)].Node = "pve2" // migrated
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil || r.pve.Count() != 0 {
		t.Fatalf("err %v, %d VMs", err, r.pve.Count())
	}
}

func TestList(t *testing.T) {
	r := newRig(t)
	a, b := r.create(r.request()), r.create(r.request())
	r.pve.AddVM(500, "pve", "my-database", "production")
	r.pve.AddVM(501, "pve", "forecrew-lookalike", "not ours")
	r.pve.AddVM(502, "elsewhere", "forecrew-x", vmcommon.NewMeta(uuid.New()).Encode()) // not a node of the configuration
	got, err := r.p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %+v", got)
	}
	for _, i := range got {
		if !i.Running || i.MachineID == uuid.Nil() || i.Tags[fleet.TagMachine] != i.MachineID.String() || (i.ExternalID != a.ExternalID && i.ExternalID != b.ExternalID) {
			t.Errorf("instance %+v", i)
		}
	}
	r.pve.FailOn = failOn("GET", "/cluster/resources", "cluster is down")
	if _, err := r.p.List(context.Background()); err == nil {
		t.Error("no error")
	}
}

func TestListSkipsWhatGoesMeanwhile(t *testing.T) {
	r := newRig(t)
	r.create(r.request())
	r.pve.FailOn = func(c string) (int, string) {
		if strings.HasSuffix(c, "/config") {
			return 500, "Configuration file 'nodes/pve/qemu-server/1.conf' does not exist"
		}
		return 0, ""
	}
	if got, err := r.p.List(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	r.pve.FailOn = failOn("GET", "/config", "boom")
	if _, err := r.p.List(context.Background()); err == nil {
		t.Error("a failing config read was swallowed")
	}
}

func TestUpdate(t *testing.T) {
	r := newRig(t)
	inst := r.create(r.request())
	vmid := vmidOf(t, inst)
	org := uuid.New().String()
	up := fleet.UpdateRequest{Tags: map[string]string{fleet.TagOrg: org, fleet.TagJob: "J-7"}, NetworkAllow: []string{"10.20.0.0/16"}, Deadline: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	for range 2 { // applying it again succeeds
		if err := r.p.Update(context.Background(), inst.ExternalID, up); err != nil {
			t.Fatal(err)
		}
	}
	vm := r.pve.VM(vmid)
	meta, _ := vmcommon.ParseMeta(vm.Config["description"])
	if meta.Tags[fleet.TagOrg] != org || meta.Tags[fleet.TagMachine] == "" || !meta.Deadline.Equal(up.Deadline) || meta.Allow[0] != "10.20.0.0/16" || meta.Server[0] != "192.0.2.7" {
		t.Errorf("meta = %+v", meta)
	}
	if !strings.Contains(vm.Config["tags"], "forecrew-job-j-7") || !strings.Contains(vm.Config["tags"], "forecrew-org-"+org) {
		t.Errorf("tags = %q", vm.Config["tags"])
	}
	var opened bool
	for _, rule := range vm.Rules {
		opened = opened || rule.Dest == "10.20.0.0/16" && rule.Action == "ACCEPT"
	}
	if !opened || vm.Rules[0].Dest != "192.0.2.7/32" {
		t.Errorf("rules = %+v", vm.Rules)
	}
	// Rules are not rewritten when the allowlist did not change.
	before := len(r.pve.CallsTo("firewall/rules"))
	up.Tags["more"] = "x"
	if err := r.p.Update(context.Background(), inst.ExternalID, up); err != nil {
		t.Fatal(err)
	}
	if len(r.pve.CallsTo("firewall/rules")) != before {
		t.Error("the rules were rewritten for a change of tags")
	}
	got, _ := r.p.List(context.Background())
	if got[0].Tags["more"] != "x" {
		t.Errorf("tags = %v", got[0].Tags)
	}
}

func TestUpdateFailures(t *testing.T) {
	r := newRig(t)
	inst := r.create(r.request())
	if err := r.p.Update(context.Background(), "pve:7777", fleet.UpdateRequest{}); err == nil || !strings.Contains(err.Error(), "no machine") {
		t.Errorf("unknown: %v", err)
	}
	r.pve.AddVM(500, "pve", "my-database", "production")
	if err := r.p.Update(context.Background(), "pve:500", fleet.UpdateRequest{}); err == nil {
		t.Error("changed a VM that is not ours")
	}
	if got := r.pve.VM(500).Config["description"]; got != "production" {
		t.Errorf("description = %q", got)
	}
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{NetworkAllow: []string{"no-such-host.invalid"}}); err == nil {
		t.Error("an allowlist that does not resolve was accepted")
	}
	r.pve.FailOn = failOn("PUT", "/config", "denied")
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{}); err == nil {
		t.Error("config failure swallowed")
	}
	r.pve.FailOn = failOn("POST", "/firewall/rules", "denied")
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{NetworkAllow: []string{"10.1.0.0/16"}}); err == nil || !strings.Contains(err.Error(), "network rules") {
		t.Errorf("firewall failure: %v", err)
	}
}

func TestDeadlinePowersTheMachineOff(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(150 * time.Millisecond)
	inst := r.create(req)
	vmid := vmidOf(t, inst)
	if r.pve.VM(vmid).Status != "running" {
		t.Fatal("not running")
	}
	waitFor(t, func() bool { return r.pve.VM(vmid).Status == "stopped" })
	// Removed by Update, nothing happens.
	req2 := r.request()
	req2.Deadline = time.Now().Add(2 * time.Second)
	inst2 := r.create(req2)
	if err := r.p.Update(context.Background(), inst2.ExternalID, fleet.UpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if r.pve.VM(vmidOf(t, inst2)).Status != "running" {
		t.Error("a machine whose deadline was cleared was stopped")
	}
}

func TestListArmsDeadlinesAfterARestart(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(200 * time.Millisecond)
	inst := r.create(req)
	r.rebuild() // the agent restarted
	if _, err := r.p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.List(context.Background()); err != nil { // listing again keeps the same kill
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.pve.VM(vmidOf(t, inst)).Status == "stopped" })
}

func TestDestroyDisarms(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.Deadline = time.Now().Add(100 * time.Millisecond)
	inst := r.create(req)
	if err := r.p.Destroy(context.Background(), inst.ExternalID); err != nil {
		t.Fatal(err)
	}
	if r.p.deadlines.Armed() != 0 {
		t.Error("the deadline is still armed")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting")
}

func TestCapabilities(t *testing.T) {
	r := newRig(t)
	c, err := r.p.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxCPU != 16 || c.MaxMemoryMB != 64<<10 || c.MaxDiskGB != 500 || c.MaxMachines != 4 {
		t.Errorf("capabilities = %+v", c)
	}
	if len(c.OS) != 2 || c.ImageFormats[0] != domain.ImageQCOW2 {
		t.Errorf("os/format = %v %v", c.OS, c.ImageFormats)
	}
	for _, ch := range c.Checks {
		if ch.Status != "ok" {
			t.Errorf("check %+v", ch)
		}
	}
	if len(c.Checks) != 4 {
		t.Errorf("checks = %+v", c.Checks)
	}
}

func TestCapabilityChecks(t *testing.T) {
	tests := []struct {
		name   string
		do     func(r *rig)
		check  string
		status string
	}{
		{"datacenter firewall off", func(r *rig) { r.pve.DatacenterFirewall = false }, "datacenter firewall", "fail"},
		{"firewall unreadable", func(r *rig) { r.pve.FailOn = failOn("GET", "/cluster/firewall/options", "denied") }, "datacenter firewall", "warn"},
		{"node unreadable", func(r *rig) { r.pve.FailOn = failOn("GET", "/nodes/pve/status", "denied") }, "node pve", "fail"},
		{"storage unreadable", func(r *rig) { r.pve.FailOn = failOn("GET", "/nodes/pve/storage", "denied") }, "node pve", "warn"},
		{"api down", func(r *rig) { r.pve.FailOn = failOn("GET", "/version", "down") }, "proxmox api", "fail"},
		{"no keys", func(r *rig) { _ = os.Remove(r.keys) }, "image keys", "fail"},
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

func TestCapabilitiesWithAWrongToken(t *testing.T) {
	r := newRig(t)
	r.pve.Token = "forecrew@pve!other=99999999-2222-3333-4444-555555555555"
	c, _ := r.p.Capabilities(context.Background())
	if len(c.Checks) != 1 || c.Checks[0].Status != "fail" || !strings.Contains(c.Checks[0].Detail, "401") {
		t.Fatalf("checks = %+v", c.Checks)
	}
	if strings.Contains(c.Checks[0].Detail, "99999999") || strings.Contains(c.Checks[0].Detail, "11111111") {
		t.Errorf("a token in the check: %s", c.Checks[0].Detail)
	}
}

func TestProviderIsRegistered(t *testing.T) {
	r := newRig(t)
	cfgPath := filepath.Join(r.dir, "proxmox.yaml")
	write(t, cfgPath, "api_url: "+r.pve.URL()+"\ntoken_file: "+filepath.Join(r.dir, "token")+"\nca_file: "+filepath.Join(r.dir, "ca.pem")+"\ntrusted_keys_file: "+r.keys+"\nnodes: [pve]\n", 0o600)
	t.Setenv(envConfig, cfgPath)
	p, err := newRegistered()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := p.Capabilities(context.Background()); err != nil || c.MaxMachines != 8 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv(envConfig, filepath.Join(r.dir, "none.yaml")) // defaults need nodes
	if _, err := newRegistered(); err == nil || !strings.Contains(err.Error(), "nodes") {
		t.Errorf("err = %v", err)
	}
}

func TestDoctorShowsWhatIsMissing(t *testing.T) {
	r := newRig(t)
	cfgPath := filepath.Join(r.dir, "proxmox.yaml")
	write(t, cfgPath, "api_url: "+r.pve.URL()+"\ntoken_file: "+filepath.Join(r.dir, "token")+"\nca_file: "+filepath.Join(r.dir, "ca.pem")+"\ntrusted_keys_file: "+r.keys+"\nnodes: [pve]\n", 0o600)
	t.Setenv(envConfig, cfgPath)
	var out strings.Builder
	if !fleetagent.Doctor(context.Background(), &out, domain.ProviderProxmox, "test") {
		t.Fatalf("doctor failed:\n%s", out.String())
	}
	for _, want := range []string{"ok   proxmox api: Proxmox VE", "ok   datacenter firewall", "ok   node pve"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	r.pve.DatacenterFirewall = false
	out.Reset()
	if fleetagent.Doctor(context.Background(), &out, domain.ProviderProxmox, "test") || !strings.Contains(out.String(), "fail datacenter firewall") {
		t.Errorf("doctor with the firewall off:\n%s", out.String())
	}
	t.Setenv(envConfig, filepath.Join(r.dir, "none.yaml"))
	out.Reset()
	if fleetagent.Doctor(context.Background(), &out, domain.ProviderProxmox, "test") || !strings.Contains(out.String(), "fail provider") {
		t.Errorf("doctor with no configuration:\n%s", out.String())
	}
}

// network_allow of everything opens the internet and still not the node, its
// management port and SSH, or the network that holds the other machines.
func TestAllowEverythingNeverOpensTheNode(t *testing.T) {
	r := newRig(t)
	req := r.request()
	req.NetworkAllow = []string{"0.0.0.0/0"}
	rules := r.pve.VM(vmidOf(t, r.create(req))).Rules
	first := func(dest, action string) int {
		for i, rule := range rules {
			if rule.Dest == dest && rule.Action == action && rule.DPort == "" {
				return i
			}
		}
		return -1
	}
	open := first("0.0.0.0/0", "ACCEPT")
	for _, never := range []string{"192.168.1.10/32", "192.168.1.0/24", "203.0.113.9/32", "127.0.0.0/8"} {
		if i := first(never, "DROP"); i < 0 || (never != "127.0.0.0/8" && i > open) {
			t.Errorf("%s: dropped at %d, the allowlist at %d", never, i, open)
		}
	}
	// forecrewd, on the node itself, is reached on its port and nothing else.
	req2 := r.request()
	req2.Bootstrap.ServerURL = "https://192.168.1.10:8443"
	rules2 := r.pve.VM(vmidOf(t, r.create(req2))).Rules
	if rules2[0].Dest != "192.168.1.10/32" || rules2[0].DPort != "8443" || rules2[0].Action != "ACCEPT" {
		t.Errorf("rules = %+v", rules2[:3])
	}
}

func TestCreateRacesForTheLastPlace(t *testing.T) {
	r := newRig(t)
	r.cfg.MaxMachines = 1
	r.rebuild()
	var wg sync.WaitGroup
	var made, full atomic.Int32
	for range 4 {
		wg.Go(func() {
			_, err := r.p.Create(context.Background(), r.request())
			switch {
			case err == nil:
				made.Add(1)
			case errors.Is(err, fleet.ErrCapacity):
				full.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if made.Load() != 1 || full.Load() != 3 || r.pve.Count() != 1 {
		t.Fatalf("made %d, refused %d, %d VMs", made.Load(), full.Load(), r.pve.Count())
	}
}

func TestCreateNeedsTheDatacenterFirewall(t *testing.T) {
	r := newRig(t)
	r.pve.DatacenterFirewall = false
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "datacenter firewall is off") {
		t.Errorf("firewall off: %v", err)
	}
	r.pve.DatacenterFirewall = true
	r.pve.FailOn = failOn("GET", "/cluster/firewall/options", "denied")
	if _, err := r.p.Create(context.Background(), r.request()); err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("firewall unreadable: %v", err)
	}
	r.pve.FailOn = nil
	if got := r.pve.CallsTo("/clone"); len(got) != 0 || r.pve.Count() != 0 {
		t.Errorf("cloned anyway: %v", got)
	}
}

func TestCreateRefusesATemplateWithMoreNetworkDevices(t *testing.T) {
	r := newRig(t)
	r.pve.AddTemplate(9010, "pve", imageTag(r.img), map[string]string{"net1": "virtio=AA:BB:CC:00:00:09,bridge=vmbr9"})
	req := r.request()
	req.Shape.ProviderConfig["template"] = 9010
	if _, err := r.p.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), "net1") {
		t.Fatalf("err = %v", err)
	}
	if len(r.pve.CallsTo("/clone")) != 0 || r.pve.Count() != 0 {
		t.Error("cloned a template with a second network device")
	}
	// A clone that has one anyway (a template changed meanwhile) is removed.
	r2 := newRig(t)
	r2.pve.FailOn = func(c string) (int, string) {
		if strings.HasSuffix(c, "/clone") {
			r2.pve.VMs[9000].Config["net1"] = "virtio=AA:BB:CC:00:00:09,bridge=vmbr9"
		}
		return 0, ""
	}
	if _, err := r2.p.Create(context.Background(), r2.request()); err == nil || !strings.Contains(err.Error(), "net1") {
		t.Fatalf("clone with net1: %v", err)
	}
}

func TestDestroyLeavesAVMOfAnotherNameAlone(t *testing.T) {
	r := newRig(t)
	r.pve.AddVM(600, "pve", "my-vm", vmcommon.NewMeta(uuid.New()).Encode())
	if err := r.p.Destroy(context.Background(), "pve:600"); err == nil || !strings.Contains(err.Error(), "leaving it alone") {
		t.Errorf("err = %v", err)
	}
	if r.pve.VM(600) == nil {
		t.Error("it was destroyed")
	}
}

// What is never opened comes from the cluster's API, and a machine is not made or
// changed when it cannot be read: with no local interfaces and no DNS, the node,
// the other nodes, the API's host and the machines' network are still closed
// first, even under network_allow of the whole LAN.
func TestNeverOpenedComesFromTheAPI(t *testing.T) {
	r := newRig(t)
	r.pve.Nodes["pve2"] = &testkit.FakeNode{CPUs: 4, MemTotal: 8 << 30, MemFree: 6 << 30, IP: "192.168.1.11", Network: r.pve.Nodes["pve"].Network}
	req := r.request()
	req.NetworkAllow = []string{"192.168.0.0/16"}
	rules := r.pve.VM(vmidOf(t, r.create(req))).Rules
	index := func(dest, action string) int {
		for i, rule := range rules {
			if rule.Dest == dest && rule.Action == action && rule.DPort == "" {
				return i
			}
		}
		return -1
	}
	open := index("192.168.0.0/16", "ACCEPT")
	for _, never := range []string{"192.168.1.10/32", "192.168.1.11/32", "127.0.0.1/32", "192.168.1.0/24"} {
		if i := index(never, "DROP"); i < 0 || i > open {
			t.Errorf("%s: dropped at %d, the allowlist at %d", never, i, open)
		}
	}
	if i, j := index("203.0.113.0/28", "DROP"), open; i < j {
		t.Errorf("another network of the node is closed at %d, before the allowlist at %d", i, j)
	}
}

func TestNothingIsMadeWhenWhatToCloseCannotBeRead(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *rig)
		want string
	}{
		{"nodes unreadable", func(r *rig) { r.pve.FailOn = failOn("GET", "/cluster/status", "denied") }, "the cluster's nodes"},
		{"a node without an address", func(r *rig) { r.pve.Nodes["pve"].IP = "" }, "no IPv4 address"},
		{"network unreadable", func(r *rig) { r.pve.FailOn = failOn("GET", "/nodes/pve/network", "denied") }, "the network of node pve"},
		{"bridge unknown", func(r *rig) { r.pve.Nodes["pve"].Network = nil }, "has no address"},
		{"bridge without an address", func(r *rig) { r.pve.Nodes["pve"].Network = []testkit.FakeIface{{Iface: "vmbr0", Type: "bridge"}} }, "has no address"},
		{"the API's host does not resolve", func(r *rig) { r.p.cfg.APIURL = "https://pve.invalid:8006" }, "the API's host pve.invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.do(r)
			_, err := r.p.Create(context.Background(), r.request())
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "none is made") {
				t.Fatalf("err = %v", err)
			}
			r.pve.FailOn = nil
			if r.pve.Count() != 0 {
				t.Errorf("%d VMs made", r.pve.Count())
			}
		})
	}
	// An update that changes the allowlist is refused the same way, and leaves the rules.
	r := newRig(t)
	inst := r.create(r.request())
	before := r.pve.VM(vmidOf(t, inst)).Rules
	r.pve.FailOn = failOn("GET", "/nodes/pve/network", "denied")
	if err := r.p.Update(context.Background(), inst.ExternalID, fleet.UpdateRequest{NetworkAllow: []string{"10.0.0.0/8"}}); err == nil || !strings.Contains(err.Error(), "none is made") {
		t.Errorf("update: %v", err)
	}
	if got := r.pve.VM(vmidOf(t, inst)).Rules; len(got) != len(before) {
		t.Errorf("rules changed from %d to %d", len(before), len(got))
	}
}

// With no resolver listed, port 53 is open to any address, after what is never
// opened.
func TestPort53OpensAfterWhatIsNeverOpened(t *testing.T) {
	r := newRig(t)
	rules := r.pve.VM(vmidOf(t, r.create(r.request()))).Rules
	var drop, dns = -1, -1
	for i, rule := range rules {
		if rule.Dest == "192.168.1.10/32" && rule.Action == "DROP" && drop < 0 {
			drop = i
		}
		if rule.Dest == "0.0.0.0/0" && rule.DPort == "53" && dns < 0 {
			dns = i
		}
	}
	if drop < 0 || dns < drop {
		t.Errorf("the node is dropped at %d, port 53 opened at %d", drop, dns)
	}
}

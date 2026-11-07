package testkit

import (
	"context"
	"strings"
	"testing"
)

func TestFakePowerShell(t *testing.T) {
	f := NewFakePowerShell()
	ctx := context.Background()
	f.Bases[`C:\img\base.vhdx`] = 60
	create := map[string]any{"name": "forecrew-aaaa", "dir": `C:\vms\forecrew-aaaa`, "vm_dir": `C:\vms`, "base": `C:\img\base.vhdx`, "cpu": 2,
		"memory_mb": 4096, "disk_gb": 80, "switch": "Lab", "vlan": 5, "notes": "n", "env": "TOKEN", "rules": []map[string]any{{"allow": true, "dest": "1.2.3.4/32", "proto": "tcp", "port": 443}}}
	var made struct {
		ID string `json:"id"`
	}
	if err := f.Run(ctx, "create", create, &made); err != nil || made.ID == "" {
		t.Fatalf("create: %v", err)
	}
	vm := f.VM("forecrew-aaaa")
	if vm.ConfigDisk != "TOKEN" || vm.VLAN != 5 || len(vm.Rules) != 1 || !vm.Rules[0].Allow || f.DirCount() != 1 || f.Count() != 1 {
		t.Errorf("vm = %+v", vm)
	}
	f.AddVM("other", "x")
	f.SetNotes("forecrew-aaaa", "changed")
	var list []map[string]string
	if err := f.Run(ctx, "list", struct{}{}, &list); err != nil || len(list) != 1 || list[0]["notes"] != "changed" {
		t.Errorf("list %v %v", list, err)
	}
	if err := f.Run(ctx, "update", map[string]any{"id": made.ID, "notes": "again", "apply_rules": true, "rules": []map[string]any{}}, nil); err != nil || len(f.VM("forecrew-aaaa").Rules) != 0 {
		t.Errorf("update: %v", err)
	}
	if err := f.Run(ctx, "stop", map[string]any{"id": made.ID}, nil); err != nil || f.VM("forecrew-aaaa").State != "Off" {
		t.Errorf("stop: %v", err)
	}
	var host map[string]any
	if err := f.Run(ctx, "host", map[string]any{"switch": "Lab"}, &host); err != nil || host["switch_found"] != true {
		t.Errorf("host: %v %v", host, err)
	}
	if f.Ran("create") != 1 || !strings.Contains(f.LastInput("create"), "forecrew-aaaa") || f.LastInput("nothing") != "" {
		t.Error("calls not recorded")
	}
	if err := f.Run(ctx, "destroy", map[string]any{"id": made.ID, "dir": `C:\vms\forecrew-aaaa`, "vm_dir": `C:\vms`}, nil); err != nil || f.DirCount() != 0 {
		t.Errorf("destroy: %v", err)
	}
}

func TestFakePowerShellRefusesAsTheScriptsDo(t *testing.T) {
	f := NewFakePowerShell()
	ctx := context.Background()
	f.Bases["base"] = 60
	ok := func() map[string]any {
		return map[string]any{"name": "n", "dir": `C:\vms\n`, "vm_dir": `C:\vms`, "base": "base", "disk_gb": 80, "switch": "Lab"}
	}
	tests := []struct {
		script string
		in     func() map[string]any
		want   string
	}{
		{"create", func() map[string]any { m := ok(); m["dir"] = `C:\elsewhere\n`; return m }, "not inside"},
		{"create", func() map[string]any { m := ok(); m["base"] = "none"; return m }, "does not exist"},
		{"create", func() map[string]any { m := ok(); m["disk_gb"] = 40; return m }, "more than the shape"},
		{"create", func() map[string]any { m := ok(); m["switch"] = "Other"; return m }, "switch Other"},
		{"update", func() map[string]any { return map[string]any{"id": "gone"} }, "unable to find"},
		{"destroy", func() map[string]any { return map[string]any{"name": "n", "dir": `C:\x\n`, "vm_dir": `C:\vms`} }, "not inside"},
		{"nonsense", func() map[string]any { return map[string]any{} }, "no script"},
	}
	for _, tt := range tests {
		if err := f.Run(ctx, tt.script, tt.in(), nil); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.script, err, tt.want)
		}
	}
	if err := f.Run(ctx, "create", ok(), nil); err != nil {
		t.Fatal(err)
	}
	// Names are not unique in Hyper-V: a second VM of the name is made, and a
	// script finds the one it means by its ID.
	if err := f.Run(ctx, "create", ok(), nil); err != nil || f.Count() != 2 {
		t.Errorf("a second create: %v, %d VMs", err, f.Count())
	}
	f.Host.NoModule = true
	if err := f.Run(ctx, "host", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "module") {
		t.Errorf("no module: %v", err)
	}
	f.FailOn = func(string) string { return "denied" }
	if err := f.Run(ctx, "list", struct{}{}, nil); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("injected: %v", err)
	}
	f.FailOn = nil
	if err := f.Run(ctx, "list", func() {}, nil); err == nil {
		t.Error("parameters that are not JSON")
	}
}

func TestFakePowerShellFindsByID(t *testing.T) {
	f := NewFakePowerShell()
	ctx := context.Background()
	f.Bases["base"] = 60
	operator := f.AddVM("forecrew-aaaa", "an operator's VM")
	var made struct {
		ID string `json:"id"`
	}
	in := map[string]any{"name": "forecrew-aaaa", "dir": `C:\vms\a`, "vm_dir": `C:\vms`, "base": "base", "disk_gb": 80, "switch": "Lab"}
	if err := f.Run(ctx, "create", in, &made); err != nil {
		t.Fatal(err)
	}
	if f.VM("forecrew-aaaa").ID != made.ID || f.VMByID(operator.ID) == nil || f.VMByID("none") != nil {
		t.Error("the VM of a name is not the one the provider made")
	}
	if err := f.Run(ctx, "destroy", map[string]any{"id": made.ID, "dir": `C:\vms\a`, "vm_dir": `C:\vms`}, nil); err != nil {
		t.Fatal(err)
	}
	if f.VMByID(operator.ID) == nil || f.Count() != 1 {
		t.Error("the operator's VM went with ours")
	}
	if err := f.Run(ctx, "secure", map[string]any{"dirs": []string{`C:\a`, `C:\b`}}, nil); err != nil || f.SecureCalls != 1 || len(f.Secured) != 2 {
		t.Errorf("secure: %v %v", err, f.Secured)
	}
}

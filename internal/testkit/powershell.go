package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"uuid"
)

// FakePowerShell is a Windows host with Hyper-V for tests, behind the scripts of
// the fleet agent's Hyper-V provider (internal/fleetagent/hyperv/scripts): it
// answers each script by its name, from the JSON the provider sends, as the
// script's contract says (what it prints, and when it fails), and keeps VMs,
// disks and network rules in memory. It is a fake, not Hyper-V: what the real
// cmdlets do is listed in docs/runbooks/add-provider.md as not run live.
type FakePowerShell struct {
	mu sync.Mutex
	// Host is what the "host" script reports.
	Host FakeHyperVHost
	// VMs by ID (names are not unique in Hyper-V, and an operator's VM may share
	// one with a machine of Forecrew's), and Dirs, the directories that exist (a
	// destroyed machine leaves none).
	VMs  map[string]*FakeHyperVVM
	Dirs map[string]bool
	// Bases are the base images that exist, by path, with their virtual size
	// in GB.
	Bases map[string]int
	// FailOn makes a script fail: it gets the script's name and returns the
	// error text on standard error, or "" to let it run.
	FailOn func(script string) string
	// Secured is the directories the "secure" script was asked to close, last
	// time; SecureCalls how often it ran.
	Secured     []string
	SecureCalls int
	// Calls is every script that ran, in order.
	Calls []string
	// Inputs is the JSON each call got, in order, as the script reads it.
	Inputs []string
}

// FakeHyperVHost is what the host reports.
type FakeHyperVHost struct {
	CPUs, MemoryMB, FreeMemoryMB, FreeDiskGB int
	Switches                                 []string
	// NoModule fails the "host" script as a host without Hyper-V does.
	NoModule bool
}

// FakeHyperVVM is a VM.
type FakeHyperVVM struct {
	ID, Name, State, Notes, Dir, Base, Switch string
	CPU, MemoryMB, VLAN                       int
	// ConfigDisk is what the machine's config disk holds: the bootstrap
	// settings, until the guest wipes it.
	ConfigDisk string
	// Rules are the network rules the script set, in the order they were given.
	Rules []FakeHyperVRule
	// Differencing says the OS disk is a differencing disk of Base.
	Differencing bool
	// Made says the "create" script made it, not an operator.
	Made bool
}

// FakeHyperVRule is a network rule as the provider sends it.
type FakeHyperVRule struct {
	Allow bool
	Dest  string
	Proto string
	Port  int
}

// NewFakePowerShell returns a host with room, a switch named "Lab" and no VMs.
func NewFakePowerShell() *FakePowerShell {
	return &FakePowerShell{
		Host:  FakeHyperVHost{CPUs: 16, MemoryMB: 65536, FreeMemoryMB: 40000, FreeDiskGB: 500, Switches: []string{"Lab"}},
		VMs:   map[string]*FakeHyperVVM{},
		Dirs:  map[string]bool{},
		Bases: map[string]int{},
	}
}

// byName finds a VM by name, preferring one the provider made.
func (f *FakePowerShell) byName(name string) *FakeHyperVVM {
	var found *FakeHyperVVM
	for _, v := range f.VMs {
		if v.Name == name && (found == nil || (v.Made && !found.Made)) {
			found = v
		}
	}
	return found
}

// VM returns a copy of the VM of a name, or nil; of two with the same name, the
// one the provider made.
func (f *FakePowerShell) VM(name string) *FakeHyperVVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.byName(name)
	if v == nil {
		return nil
	}
	c := *v
	c.Rules = slices.Clone(v.Rules)
	return &c
}

// VMByID returns a copy of the VM with an ID, or nil.
func (f *FakePowerShell) VMByID(id string) *FakeHyperVVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.VMs[id]
	if v == nil {
		return nil
	}
	c := *v
	c.Rules = slices.Clone(v.Rules)
	return &c
}

// SetNotes changes a VM's notes, as a person at the host could.
func (f *FakePowerShell) SetNotes(name, notes string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byName(name).Notes = notes
}

// AddVM adds a VM the provider did not make.
func (f *FakePowerShell) AddVM(name, notes string) *FakeHyperVVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &FakeHyperVVM{ID: uuid.New().String(), Name: name, State: "Running", Notes: notes}
	f.VMs[v.ID] = v
	return v
}

// Count says how many VMs there are.
func (f *FakePowerShell) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.VMs)
}

// DirCount says how many machine directories exist.
func (f *FakePowerShell) DirCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Dirs)
}

// Ran returns how many times the script ran.
func (f *FakePowerShell) Ran(script string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if c == script {
			n++
		}
	}
	return n
}

// LastInput returns the JSON of the last call of a script.
func (f *FakePowerShell) LastInput(script string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range slices.Backward(f.Calls) {
		if v == script {
			return f.Inputs[i]
		}
	}
	return ""
}

// SetFailOn sets FailOn under the fake's lock, so a test may change it while
// the provider runs scripts from its own goroutines.
func (f *FakePowerShell) SetFailOn(fn func(script string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailOn = fn
}

// Run implements the hyperv provider's Runner.
func (f *FakePowerShell) Run(_ context.Context, script string, in, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	f.Calls, f.Inputs = append(f.Calls, script), append(f.Inputs, string(raw))
	if f.FailOn != nil {
		if msg := f.FailOn(script); msg != "" {
			return fmt.Errorf("hyper-v %s: %s", script, msg)
		}
	}
	res, err := f.run(script, raw)
	if err != nil {
		return fmt.Errorf("hyper-v %s: %w", script, err)
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

type psRule struct {
	Allow bool   `json:"allow"`
	Dest  string `json:"dest"`
	Proto string `json:"proto"`
	Port  int    `json:"port"`
}

func ruleOf(r []psRule) []FakeHyperVRule {
	out := make([]FakeHyperVRule, 0, len(r))
	for _, x := range r {
		out = append(out, FakeHyperVRule(x))
	}
	return out
}

// psIn is the union of what the scripts are given.
type psIn struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Dirs       []string `json:"dirs"`
	Dir        string   `json:"dir"`
	VMDir      string   `json:"vm_dir"`
	Base       string   `json:"base"`
	CPU        int      `json:"cpu"`
	MemoryMB   int      `json:"memory_mb"`
	DiskGB     int      `json:"disk_gb"`
	Switch     string   `json:"switch"`
	VLAN       int      `json:"vlan"`
	Generation int      `json:"generation"`
	Notes      string   `json:"notes"`
	Env        string   `json:"env"`
	Rules      []psRule `json:"rules"`
	ApplyRules bool     `json:"apply_rules"`
}

// under checks, as the scripts do, that a directory is inside the VM directory.
func (p psIn) under(dir string) error {
	if !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(strings.TrimRight(p.VMDir, `\`))+`\`) {
		return fmt.Errorf("%s is not inside %s", dir, p.VMDir)
	}
	return nil
}

func (f *FakePowerShell) run(script string, raw []byte) (any, error) {
	var p psIn
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch script {
	case "host":
		return f.host(p)
	case "list":
		return f.list(), nil
	case "create":
		return f.create(p)
	case "secure":
		f.Secured, f.SecureCalls = p.Dirs, f.SecureCalls+1
		return map[string]bool{"ok": true}, nil
	case "update":
		v := f.VMs[p.ID]
		if v == nil {
			return nil, fmt.Errorf("Hyper-V was unable to find a virtual machine with id %q", p.ID)
		}
		v.Notes = p.Notes
		if p.ApplyRules {
			v.Rules = ruleOf(p.Rules)
		}
		return map[string]bool{"ok": true}, nil
	case "stop":
		if v := f.VMs[p.ID]; v != nil {
			v.State = "Off"
		}
		return map[string]bool{"ok": true}, nil
	case "destroy":
		if err := p.under(p.Dir); err != nil {
			return nil, err
		}
		delete(f.VMs, p.ID)
		delete(f.Dirs, p.Dir)
		return map[string]bool{"ok": true}, nil
	}
	return nil, fmt.Errorf("the fake knows no script %q", script)
}

func (f *FakePowerShell) host(p psIn) (any, error) {
	if f.Host.NoModule {
		return nil, errors.New("the Hyper-V PowerShell module is not installed")
	}
	return map[string]any{"cpus": f.Host.CPUs, "memory_mb": f.Host.MemoryMB, "free_memory_mb": f.Host.FreeMemoryMB,
		"free_disk_gb": f.Host.FreeDiskGB, "switch_found": slices.Contains(f.Host.Switches, p.Switch), "version": "10.0.20348"}, nil
}

func (f *FakePowerShell) list() []map[string]string {
	out := []map[string]string{}
	for _, v := range f.VMs {
		if strings.HasPrefix(v.Name, "forecrew-") {
			out = append(out, map[string]string{"id": v.ID, "name": v.Name, "state": v.State, "notes": v.Notes})
		}
	}
	slices.SortFunc(out, func(a, b map[string]string) int { return strings.Compare(a["name"], b["name"]) })
	return out
}

func (f *FakePowerShell) create(p psIn) (any, error) {
	if err := p.under(p.Dir); err != nil {
		return nil, err
	}
	size, ok := f.Bases[p.Base]
	if !ok {
		return nil, fmt.Errorf("the base image %s does not exist", p.Base)
	}
	if size > p.DiskGB {
		return nil, fmt.Errorf("the image's disk is %d GB, more than the shape's %d GB", size, p.DiskGB)
	}
	if !slices.Contains(f.Host.Switches, p.Switch) {
		return nil, fmt.Errorf("switch %s does not exist", p.Switch)
	}
	f.Dirs[p.Dir] = true
	v := &FakeHyperVVM{ID: uuid.New().String(), Name: p.Name, State: "Running", Notes: p.Notes, Dir: p.Dir, Base: p.Base, Switch: p.Switch,
		CPU: p.CPU, MemoryMB: p.MemoryMB, VLAN: p.VLAN, ConfigDisk: p.Env, Rules: ruleOf(p.Rules), Differencing: true, Made: true}
	f.VMs[v.ID] = v
	return map[string]string{"id": v.ID}, nil
}

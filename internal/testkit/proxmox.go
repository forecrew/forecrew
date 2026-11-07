package testkit

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// FakeProxmox is a Proxmox VE API for tests, built from the documented API
// (https://pve.proxmox.com/pve-docs/api-viewer): the endpoints a fleet agent's
// Proxmox provider calls, with the answers and errors they give, over TLS with
// API token authentication. It keeps VMs, their firewall and what is written
// into them through the guest agent in memory. It is a fake, not Proxmox: what
// it does not model is listed in docs/runbooks/add-provider.md.
type FakeProxmox struct {
	Server *httptest.Server
	// Token is the API token the fake accepts (user@realm!name=secret).
	Token string

	mu sync.Mutex
	// Nodes and what they have.
	Nodes map[string]*FakeNode
	VMs   map[int]*FakeVM
	// DatacenterFirewall is the datacenter's firewall switch.
	DatacenterFirewall bool
	// FirewallInsertsAtTop makes a new firewall rule go first, as the real
	// API does when it is given no position; false appends it.
	FirewallInsertsAtTop bool
	// FailOn makes a call fail: it gets "METHOD /path" (without /api2/json) and
	// returns a status and message, or 0 to let the call through.
	FailOn func(call string) (status int, message string)
	// FailTask makes the task of a call end with this exit status; a call
	// whose "METHOD /path" ends with the key has it.
	FailTask map[string]string
	// AgentPings is how many pings fail before the guest agent answers, for
	// a VM started after it is set.
	AgentPings int
	// Calls is every call, in order, "METHOD /path".
	Calls []string
	// Bodies is the form of every call that had one, in order.
	Bodies []map[string]string

	nextID int
	tasks  map[string]string // upid -> exit status
}

// FakeNode is a node of the fake cluster.
type FakeNode struct {
	CPUs     int
	MemTotal int64 // bytes
	MemFree  int64
	// Storage by name: free bytes, listed when it holds images.
	Storage map[string]int64
	// IP is the node's address in /cluster/status, and Network its interfaces in
	// /nodes/{node}/network.
	IP      string
	Network []FakeIface
}

// FakeIface is a network interface of a node.
type FakeIface struct {
	Iface, Type, CIDR string
}

// FakeVM is a VM or template.
type FakeVM struct {
	Node     string
	Name     string
	Status   string // running, stopped
	Template bool
	Config   map[string]string
	Rules    []FakeRule
	// FirewallOptions is what PUT firewall/options set.
	FirewallOptions map[string]string
	// Files is what the guest agent wrote, by path.
	Files map[string]string
	// pings left before the agent answers.
	pings int
}

// FakeRule is a firewall rule.
type FakeRule struct {
	Type, Action, Dest, Proto, DPort, Comment string
}

// NewFakeProxmox starts a fake with one node, "pve", and no VMs. Close it with
// the test.
func NewFakeProxmox(t interface {
	Helper()
	Cleanup(func())
}) *FakeProxmox {
	t.Helper()
	f := &FakeProxmox{ // #nosec G101 -- a made-up token for a fake
		Token: "forecrew@pve!agent=11111111-2222-3333-4444-555555555555", DatacenterFirewall: true,
		Nodes: map[string]*FakeNode{"pve": {CPUs: 16, MemTotal: 64 << 30, MemFree: 48 << 30, Storage: map[string]int64{"local-lvm": 500 << 30},
			IP: "192.168.1.10", Network: []FakeIface{{"vmbr0", "bridge", "192.168.1.10/24"}, {"eno1", "eth", "203.0.113.9/28"}, {"vmbr1", "bridge", ""}}}},
		VMs: map[int]*FakeVM{}, tasks: map[string]string{}, nextID: 100, FailTask: map[string]string{},
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the API's base URL.
func (f *FakeProxmox) URL() string { return f.Server.URL }

// CAPEM is the certificate the server presents, to put in a ca_file.
func (f *FakeProxmox) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Server.Certificate().Raw})
}

// AddTemplate adds a template with a disk and a network device, tagged as the
// provider requires for an image.
func (f *FakeProxmox) AddTemplate(vmid int, node, tags string, config map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := map[string]string{"template": "1", "tags": tags, "scsi0": "local-lvm:base-100-disk-0,size=20G", "net0": "virtio=AA:BB:CC:00:00:01,bridge=vmbr0", "cores": "2", "memory": "2048"}
	maps.Copy(c, config)
	f.VMs[vmid] = &FakeVM{Node: node, Name: "tpl-" + strconv.Itoa(vmid), Status: "stopped", Template: true, Config: c}
}

// AddVM adds a plain VM, for an operator's own machines that must be left
// alone.
func (f *FakeProxmox) AddVM(vmid int, node, name, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.VMs[vmid] = &FakeVM{Node: node, Name: name, Status: "running", Config: map[string]string{"description": description}}
}

// VM returns a copy of a VM, or nil.
func (f *FakeProxmox) VM(vmid int) *FakeVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.VMs[vmid]
	if v == nil {
		return nil
	}
	c := *v
	c.Rules = slices.Clone(v.Rules)
	return &c
}

// Count says how many VMs there are that are not templates.
func (f *FakeProxmox) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.VMs {
		if !v.Template {
			n++
		}
	}
	return n
}

// CallsTo returns the calls whose text contains s.
func (f *FakeProxmox) CallsTo(s string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.Contains(c, s) {
			out = append(out, c)
		}
	}
	return out
}

// Forms returns the forms sent to the calls whose text contains s.
func (f *FakeProxmox) Forms(s string) []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for i, c := range f.Calls {
		if strings.Contains(c, s) && f.Bodies[i] != nil {
			out = append(out, f.Bodies[i])
		}
	}
	return out
}

func (f *FakeProxmox) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "PVEAPIToken="+f.Token {
		reply(w, http.StatusUnauthorized, "no ticket", nil)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api2/json")
	if !ok {
		reply(w, http.StatusNotFound, "no such path", nil)
		return
	}
	_ = r.ParseForm()
	form := map[string]string{}
	for k, v := range r.Form {
		form[k] = v[0]
	}
	call := r.Method + " " + path
	f.Calls = append(f.Calls, call)
	if r.Method == http.MethodGet || len(form) == 0 {
		f.Bodies = append(f.Bodies, nil)
	} else {
		f.Bodies = append(f.Bodies, form)
	}
	if f.FailOn != nil {
		if status, msg := f.FailOn(call); status != 0 {
			reply(w, status, msg, nil)
			return
		}
	}
	data, status, msg := f.route(r.Method, strings.Split(strings.Trim(path, "/"), "/"), form, call)
	reply(w, status, msg, data)
}

func reply(w http.ResponseWriter, status int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		// The real API puts the message in the status line.
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": msg})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *FakeProxmox) upid(node, call string) string {
	f.nextID++
	id := fmt.Sprintf("UPID:%s:0000%04X:00000000:00000000:%s:%d:forecrew@pve!agent:", node, f.nextID, "task", f.nextID)
	f.tasks[id] = "OK"
	for suffix, exit := range f.FailTask {
		if strings.HasSuffix(call, suffix) {
			f.tasks[id] = exit
		}
	}
	return id
}

// route answers a call: its data, or a status and message for an error.
func (f *FakeProxmox) route(method string, p []string, form map[string]string, call string) (any, int, string) {
	switch {
	case method == "GET" && len(p) == 1 && p[0] == "version":
		return map[string]any{"version": "9.0.3", "release": "9.0", "repoid": "fake"}, 0, ""
	case method == "GET" && len(p) == 2 && p[0] == "cluster" && p[1] == "nextid":
		for {
			f.nextID++
			if f.VMs[f.nextID] == nil {
				return strconv.Itoa(f.nextID), 0, ""
			}
		}
	case method == "GET" && len(p) == 3 && p[0] == "cluster" && p[1] == "firewall" && p[2] == "options":
		return map[string]any{"enable": map[bool]int{true: 1, false: 0}[f.DatacenterFirewall]}, 0, ""
	case method == "GET" && len(p) == 2 && p[0] == "cluster" && p[1] == "status":
		out := []map[string]any{{"type": "cluster", "name": "fake", "nodes": len(f.Nodes)}}
		for _, name := range slices.Sorted(maps.Keys(f.Nodes)) {
			out = append(out, map[string]any{"type": "node", "name": name, "ip": f.Nodes[name].IP, "online": 1})
		}
		return out, 0, ""
	case method == "GET" && len(p) == 2 && p[0] == "cluster" && p[1] == "resources":
		return f.resources(), 0, ""
	case len(p) >= 3 && p[0] == "nodes":
		return f.node(method, p[1], p[2:], form, call)
	}
	return nil, http.StatusNotImplemented, "the fake does not know " + call
}

func (f *FakeProxmox) resources() []map[string]any {
	out := []map[string]any{}
	for id, v := range f.VMs {
		out = append(out, map[string]any{"id": "qemu/" + strconv.Itoa(id), "type": "qemu", "vmid": id, "node": v.Node, "name": v.Name,
			"status": v.Status, "template": map[bool]int{true: 1, false: 0}[v.Template], "tags": strings.ReplaceAll(v.Config["tags"], ",", ";")})
	}
	slices.SortFunc(out, func(a, b map[string]any) int { return a["vmid"].(int) - b["vmid"].(int) })
	return out
}

func (f *FakeProxmox) node(method, node string, p []string, form map[string]string, call string) (any, int, string) {
	n := f.Nodes[node]
	if n == nil {
		return nil, http.StatusInternalServerError, "hostname lookup '" + node + "' failed - failed to get address info"
	}
	switch {
	case method == "GET" && len(p) == 1 && p[0] == "status":
		return map[string]any{"cpuinfo": map[string]any{"cpus": n.CPUs}, "memory": map[string]any{"total": n.MemTotal, "free": n.MemFree, "used": n.MemTotal - n.MemFree}}, 0, ""
	case method == "GET" && len(p) == 1 && p[0] == "network":
		out := []map[string]any{}
		for _, i := range n.Network {
			e := map[string]any{"iface": i.Iface, "type": i.Type, "active": 1}
			if i.CIDR != "" {
				e["cidr"] = i.CIDR
			}
			out = append(out, e)
		}
		return out, 0, ""
	case method == "GET" && len(p) == 1 && p[0] == "storage":
		out := []map[string]any{}
		for name, avail := range n.Storage {
			out = append(out, map[string]any{"storage": name, "avail": avail, "total": avail * 2, "active": 1, "enabled": 1, "content": "images,rootdir"})
		}
		slices.SortFunc(out, func(a, b map[string]any) int { return strings.Compare(a["storage"].(string), b["storage"].(string)) })
		return out, 0, ""
	case method == "GET" && len(p) == 3 && p[0] == "tasks" && p[2] == "status":
		exit, ok := f.tasks[p[1]]
		if !ok {
			return nil, http.StatusInternalServerError, "no such task"
		}
		return map[string]any{"status": "stopped", "exitstatus": exit, "upid": p[1]}, 0, ""
	case len(p) >= 2 && p[0] == "qemu":
		return f.qemu(method, node, p[1:], form, call)
	}
	return nil, http.StatusNotImplemented, "the fake does not know " + call
}

func (f *FakeProxmox) vm(node, id string) (*FakeVM, int, string) {
	vmid, err := strconv.Atoi(id)
	v := f.VMs[vmid]
	if err != nil || v == nil || v.Node != node {
		return nil, http.StatusInternalServerError, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%s.conf' does not exist", node, id)
	}
	return v, 0, ""
}

func (f *FakeProxmox) qemu(method, node string, p []string, form map[string]string, call string) (any, int, string) {
	vmid, _ := strconv.Atoi(p[0])
	v, status, msg := f.vm(node, p[0])
	switch {
	case method == "DELETE" && len(p) == 1:
		if v == nil {
			return nil, status, msg
		}
		if v.Template || v.Status == "running" {
			return nil, http.StatusInternalServerError, "VM is running - destroy failed"
		}
		delete(f.VMs, vmid)
		return f.upid(node, call), 0, ""
	case method == "POST" && len(p) == 2 && p[1] == "clone":
		if v == nil {
			return nil, status, msg
		}
		return f.clone(node, v, form, call)
	}
	if v == nil {
		return nil, status, msg
	}
	rest := strings.Join(p[1:], "/")
	switch {
	case strings.HasPrefix(rest, "firewall/"):
		return f.firewall(method, v, p[2:], form)
	case strings.HasPrefix(rest, "agent/"):
		return f.agent(method, v, p[0], strings.TrimPrefix(rest, "agent/"), form)
	}
	return f.vmCall(method, node, v, rest, form, call)
}

// clone makes a VM from a template or a VM, with a new MAC address on its
// network device, as the API does.
func (f *FakeProxmox) clone(node string, v *FakeVM, form map[string]string, call string) (any, int, string) {
	newid, err := strconv.Atoi(form["newid"])
	if err != nil {
		return nil, http.StatusBadRequest, "Parameter verification failed. newid: value is not an integer"
	}
	if f.VMs[newid] != nil {
		return nil, http.StatusInternalServerError, fmt.Sprintf("Parameter verification failed. newid: VM %d already exists on node '%s'", newid, f.VMs[newid].Node)
	}
	cfg := map[string]string{}
	for k, val := range v.Config {
		switch {
		case k == "template" || k == "tags":
		case strings.HasPrefix(k, "net"):
			// Every network device is cloned, each with a new MAC address.
			model, rest, _ := strings.Cut(val, ",")
			model, _, _ = strings.Cut(model, "=")
			cfg[k] = model + "=AA:BB:CC:00:99:" + fmt.Sprintf("%02X", newid%256) + "," + rest
		default:
			cfg[k] = val
		}
	}
	if d, ok := form["description"]; ok {
		cfg["description"] = d
	}
	cfg["name"] = form["name"]
	f.VMs[newid] = &FakeVM{Node: node, Name: form["name"], Status: "stopped", Config: cfg}
	return f.upid(node, call), 0, ""
}

// vmCall answers the configuration, resize and power calls of a VM.
func (f *FakeProxmox) vmCall(method, node string, v *FakeVM, rest string, form map[string]string, call string) (any, int, string) {
	switch {
	case rest == "config" || rest == "resize":
		return f.configCall(method, v, rest, form)
	case method == "GET" && rest == "status/current":
		return map[string]any{"status": v.Status, "qmpstatus": v.Status}, 0, ""
	case method == "POST" && rest == "status/start":
		if v.Template {
			return nil, http.StatusInternalServerError, "you can't start a vm if it's a template"
		}
		v.Status, v.pings = "running", f.AgentPings
		return f.upid(node, call), 0, ""
	case method == "POST" && rest == "status/stop":
		v.Status = "stopped"
		return f.upid(node, call), 0, ""
	}
	return nil, http.StatusNotImplemented, "the fake does not know " + call
}

// agent answers the guest agent calls: it answers once the VM runs and the pings
// the test asked to fail are over.
func (f *FakeProxmox) agent(method string, v *FakeVM, id, call string, form map[string]string) (any, int, string) {
	switch {
	case method == "POST" && call == "ping":
		if v.Status != "running" || v.pings > 0 {
			if v.pings > 0 {
				v.pings--
			}
			return nil, http.StatusInternalServerError, "QEMU guest agent is not running"
		}
		return map[string]any{}, 0, ""
	case method == "POST" && call == "file-write":
		if v.Status != "running" {
			return nil, http.StatusInternalServerError, "VM " + id + " is not running"
		}
		if len(form["content"]) > 61440 {
			return nil, http.StatusBadRequest, "Parameter verification failed. content: value may only be 61440 characters long"
		}
		if v.Files == nil {
			v.Files = map[string]string{}
		}
		v.Files[form["file"]] = form["content"] // the real API base64-encodes it for the guest agent
		return nil, 0, ""
	}
	return nil, http.StatusNotImplemented, "the fake does not know agent/" + call
}

// firewall answers the rules and options of a VM's firewall; p is what follows
// "firewall/".
func (f *FakeProxmox) firewall(method string, v *FakeVM, p []string, form map[string]string) (any, int, string) {
	switch {
	case method == "GET" && len(p) == 1 && p[0] == "rules":
		out := []map[string]any{}
		for i, r := range v.Rules {
			out = append(out, map[string]any{"pos": i, "type": r.Type, "action": r.Action, "dest": r.Dest, "proto": r.Proto, "dport": r.DPort, "comment": r.Comment, "enable": 1})
		}
		return out, 0, ""
	case method == "POST" && len(p) == 1 && p[0] == "rules":
		r := FakeRule{Type: form["type"], Action: form["action"], Dest: form["dest"], Proto: form["proto"], DPort: form["dport"], Comment: form["comment"]}
		if r.Type == "" || r.Action == "" {
			return nil, http.StatusBadRequest, "Parameter verification failed. type: property is missing and it is not optional"
		}
		if f.FirewallInsertsAtTop {
			v.Rules = append([]FakeRule{r}, v.Rules...)
		} else {
			v.Rules = append(v.Rules, r)
		}
		return nil, 0, ""
	case method == "DELETE" && len(p) == 2 && p[0] == "rules":
		pos, err := strconv.Atoi(p[1])
		if err != nil || pos < 0 || pos >= len(v.Rules) {
			return nil, http.StatusInternalServerError, "no rule at position " + p[1]
		}
		v.Rules = slices.Delete(v.Rules, pos, pos+1)
		return nil, 0, ""
	case method == "PUT" && len(p) == 1 && p[0] == "options":
		if v.FirewallOptions == nil {
			v.FirewallOptions = map[string]string{}
		}
		maps.Copy(v.FirewallOptions, form)
		return nil, 0, ""
	}
	return nil, http.StatusNotImplemented, "the fake does not know firewall/" + strings.Join(p, "/")
}

// configCall answers reading and changing a VM's configuration, and resizing a
// disk.
func (f *FakeProxmox) configCall(method string, v *FakeVM, rest string, form map[string]string) (any, int, string) {
	switch {
	case method == "GET" && rest == "config":
		out := map[string]any{}
		for k, val := range v.Config {
			out[k] = val
		}
		if v.Template {
			out["template"] = 1
		}
		return out, 0, ""
	case method == "PUT" && rest == "config":
		if v.Template {
			return nil, http.StatusInternalServerError, "VM is a template - config change not allowed"
		}
		maps.Copy(v.Config, form)
		return nil, 0, ""
	case method == "PUT" && rest == "resize":
		cur, ok := v.Config[form["disk"]]
		if !ok {
			return nil, http.StatusInternalServerError, "disk '" + form["disk"] + "' does not exist"
		}
		if i := strings.Index(cur, "size="); i >= 0 {
			cur = cur[:i]
		}
		v.Config[form["disk"]] = strings.TrimSuffix(cur, ",") + ",size=" + form["size"]
		return nil, 0, ""
	}
	return nil, http.StatusNotImplemented, method + " " + rest + " is not known to the fake"
}

package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/imagecache"
	"github.com/forecrew/forecrew/internal/fleetagent/vmcommon"
)

const (
	// namePrefix starts the name of every VM Forecrew makes; the rest is the
	// first 12 hex digits of the machine's ID. A script only ever acts on a
	// name and a directory made from it.
	namePrefix = "forecrew-"
	// memoryReserveMB is left to the host when machines are placed.
	memoryReserveMB = 2048
	// minFreeDiskGB is the room the VM directory must have to start another.
	minFreeDiskGB = 10
	// maxNotes keeps the notes inside what Hyper-V stores.
	maxNotes = 4000
)

var guestValueRE = regexp.MustCompile(`^[A-Za-z0-9._/:@+,-]*$`)

func init() { fleetagent.Register(domain.ProviderHyperV, newRegistered) }

// newRegistered is the provider the fleet agent makes, from hyperv.yaml.
func newRegistered() (fleet.Provider, error) {
	path := os.Getenv(envConfig)
	if path == "" {
		path = defaultConfig
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(cfg, nil, nil)
}

// Provider makes machines as VMs of this host. Everything it keeps is in the VMs'
// own notes and directories, so a restart loses nothing.
type Provider struct {
	cfg      Config
	log      *slog.Logger
	ps       Runner
	cache    *imagecache.Cache
	resolver *net.Resolver

	// ifaces lists the host's addresses; tests replace it.
	ifaces func() []vmcommon.IfaceAddr
	// secureMu guards secured: whether the directories were found closed.
	secureMu  sync.Mutex
	secured   bool
	deadlines vmcommon.Deadlines
	mu        sync.Mutex // guards armed and lastGC
	startMu   sync.Mutex // makes Create's check of room and the start one step
	armed     map[string]time.Time
	lastGC    time.Time
}

// New returns the provider of cfg. ps and log may be nil: the first runs
// PowerShell, the second logs to the default logger.
func New(cfg Config, ps Runner, log *slog.Logger) (*Provider, error) {
	cfg.fill()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if ps == nil {
		ps = &PowerShell{Exe: cfg.PowerShell, Dir: strings.TrimRight(cfg.StateDir, `\`) + `\scripts`, Timeout: cfg.ScriptTimeout}
	}
	return &Provider{
		cfg: cfg, log: log, ps: ps, resolver: net.DefaultResolver, armed: map[string]time.Time{}, ifaces: vmcommon.InterfaceAddrs,
		cache: imagecache.New(cfg.ImageDir, cfg.TrustedKeysFile, ".vhdx", domain.ImageVHDX, log),
	}, nil
}

type ruleIn struct {
	Allow bool   `json:"allow"`
	Dest  string `json:"dest"`
	Proto string `json:"proto,omitempty"`
	Port  int    `json:"port,omitempty"`
}

func ruleList(rules []vmcommon.Rule) []ruleIn {
	out := make([]ruleIn, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleIn{Allow: r.Allow, Dest: r.Dest.String(), Proto: r.Proto, Port: r.Port})
	}
	return out
}

type vmOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Notes string `json:"notes"`
}

// managed is a VM of Forecrew's.
type managed struct {
	id    string
	name  string
	state string
	meta  vmcommon.Meta
}

// vmName is the name of the VM of a machine.
func vmName(id uuid.UUID) string {
	return namePrefix + strings.ReplaceAll(id.String(), "-", "")[:12]
}

func (p *Provider) dir(name string) string { return strings.TrimRight(p.cfg.VMDir, `\`) + `\` + name }

// run runs a script, after making sure that the directories it works in are closed
// to other users of this host.
func (p *Provider) run(ctx context.Context, script string, in, out any) error {
	if err := p.ready(ctx); err != nil {
		return err
	}
	return p.ps.Run(ctx, script, in, out)
}

// scriptsDir is where the scripts are written.
func (p *Provider) scriptsDir() string { return strings.TrimRight(p.cfg.StateDir, `\`) + `\scripts` }

// ready checks, before anything else is done on this host, that the state,
// machine, image and script directories are closed to everyone but SYSTEM,
// Administrators and the agent's own account (setting that where it can), and
// refuses to go on if they are open. A machine's config disk holds a token and
// lives under them. It is asked again until it has succeeded once.
func (p *Provider) ready(ctx context.Context) error {
	p.secureMu.Lock()
	defer p.secureMu.Unlock()
	if p.secured {
		return nil
	}
	dirs := []string{p.cfg.StateDir, p.cfg.VMDir, p.cfg.ImageDir, p.scriptsDir()}
	if err := p.ps.Run(ctx, "secure", map[string]any{"dirs": dirs}, nil); err != nil {
		return fmt.Errorf("the agent's folders are not closed to other users: %w", err)
	}
	p.secured = true
	return nil
}

func (p *Provider) host(ctx context.Context, switchName string) (hostOut, error) {
	var h hostOut
	err := p.run(ctx, "host", map[string]any{"vm_dir": p.cfg.VMDir, "switch": switchName}, &h)
	return h, err
}

type hostOut struct {
	CPUs         int    `json:"cpus"`
	MemoryMB     int    `json:"memory_mb"`
	FreeMemoryMB int    `json:"free_memory_mb"`
	FreeDiskGB   int    `json:"free_disk_gb"`
	SwitchFound  bool   `json:"switch_found"`
	Version      string `json:"version"`
}

// forecrewVMs lists the VMs that are Forecrew's: named as ours are, with notes
// only this provider writes, for the machine whose ID the name carries.
func (p *Provider) forecrewVMs(ctx context.Context) ([]managed, error) {
	var all []vmOut
	if err := p.run(ctx, "list", struct{}{}, &all); err != nil {
		return nil, err
	}
	var out []managed
	for _, v := range all {
		meta, ok := vmcommon.ParseMeta(v.Notes)
		if ok && v.Name == vmName(meta.ID()) && v.ID != "" {
			out = append(out, managed{id: v.ID, name: v.Name, state: v.State, meta: meta})
		}
	}
	return out, nil
}

// find returns the VM with an external ID, nil when there is none, and an
// error when it is not Forecrew's.
func (p *Provider) find(ctx context.Context, external string) (*managed, error) {
	var all []vmOut
	if err := p.run(ctx, "list", struct{}{}, &all); err != nil {
		return nil, err
	}
	for _, v := range all {
		if !strings.EqualFold(v.ID, external) {
			continue
		}
		meta, ok := vmcommon.ParseMeta(v.Notes)
		if !ok || v.Name != vmName(meta.ID()) {
			return nil, fmt.Errorf("VM %s is not one of Forecrew's machines: leaving it alone", external)
		}
		return &managed{id: v.ID, name: v.Name, state: v.State, meta: meta}, nil
	}
	return nil, nil
}

// Capabilities implements fleet.Provider. A host that cannot be asked gives a
// failing check, not an error: forecrewd should still see why.
func (p *Provider) Capabilities(ctx context.Context) (domain.ProviderCapabilities, error) {
	c := domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSWindows}, Arch: []domain.MachineArch{domain.ArchAMD64},
		ImageFormats: []domain.ImageFormat{domain.ImageVHDX}, MaxMachines: p.cfg.MaxMachines,
	}
	c.Checks = p.check(ctx, &c)
	return c, nil
}

func (p *Provider) check(ctx context.Context, c *domain.ProviderCapabilities) []domain.ProviderCheck {
	var out []domain.ProviderCheck
	h, err := p.host(ctx, p.cfg.Switch)
	if err != nil {
		return append(out, domain.ProviderCheck{Name: "hyper-v", Status: statusFail, Detail: err.Error()})
	}
	out = append(out, domain.ProviderCheck{Name: "hyper-v", Status: statusOK, Detail: "the module and the management service are there, on Windows " + clean(h.Version)})
	if h.SwitchFound {
		out = append(out, domain.ProviderCheck{Name: "switch", Status: statusOK, Detail: "virtual switch " + p.cfg.Switch + " exists"})
	} else {
		out = append(out, domain.ProviderCheck{Name: "switch", Status: statusFail, Detail: "there is no virtual switch named " + p.cfg.Switch + ": set switch in hyperv.yaml"})
	}
	if _, err := fleetagent.LoadTrustedKeys(p.cfg.TrustedKeysFile); err != nil {
		out = append(out, domain.ProviderCheck{Name: "image keys", Status: statusFail, Detail: err.Error()})
	} else {
		out = append(out, domain.ProviderCheck{Name: "image keys", Status: statusOK, Detail: "trusted image keys found"})
	}
	if h.FreeDiskGB < minFreeDiskGB {
		out = append(out, domain.ProviderCheck{Name: "disk", Status: statusWarn, Detail: fmt.Sprintf("only %d GB are free where the machines go", h.FreeDiskGB)})
	}
	c.MaxCPU, c.MaxMemoryMB, c.MaxDiskGB = h.CPUs, max(h.MemoryMB-memoryReserveMB, 0), h.FreeDiskGB
	return out
}

// Check statuses (domain.ProviderCheck).
const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

// Create implements fleet.Provider.
func (p *Provider) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	if req.Bootstrap == nil {
		return fleet.Instance{}, errors.New("no bootstrap settings")
	}
	if req.Shape.OS != domain.OSWindows {
		return fleet.Instance{}, fmt.Errorf("a Hyper-V location runs Windows guests, not %s", req.Shape.OS)
	}
	if req.Shape.CPU < 1 || req.Shape.MemoryMB < 1 || req.Shape.DiskGB < 1 {
		return fleet.Instance{}, errors.New("the shape needs cpu, memory_mb and disk_gb")
	}
	if !guestValueRE.MatchString(req.Bootstrap.ServerURL) || !guestValueRE.MatchString(req.Bootstrap.Token) {
		return fleet.Instance{}, errors.New("the server URL or the token has characters the guest does not accept")
	}
	sc, err := p.shape(req.Shape.ProviderConfig)
	if err != nil {
		return fleet.Instance{}, err
	}
	// Nothing is fetched or made before the directories are known to be closed.
	if err := p.ready(ctx); err != nil {
		return fleet.Instance{}, err
	}
	// A retry carries a new token: what exists goes first.
	if err := p.destroyMachine(ctx, req.MachineID); err != nil {
		return fleet.Instance{}, err
	}
	base, err := p.cache.Ensure(ctx, req.Image)
	if err != nil {
		return fleet.Instance{}, err
	}
	meta, rules, err := p.plan(ctx, req, sc.Switch)
	if err != nil {
		return fleet.Instance{}, err
	}
	notes := meta.Encode()
	if len(notes) > maxNotes {
		return fleet.Instance{}, errors.New("the machine's tags are too long for the notes of a VM")
	}
	name := vmName(req.MachineID)
	in := map[string]any{
		"name": name, "dir": p.dir(name), "vm_dir": p.cfg.VMDir, "base": base, "cpu": req.Shape.CPU, "memory_mb": req.Shape.MemoryMB,
		"disk_gb": req.Shape.DiskGB, "switch": sc.Switch, "vlan": *sc.VLAN, "generation": p.cfg.Generation, "notes": notes,
		"rules": ruleList(rules), "env": "FORECREW_SERVER_URL=" + req.Bootstrap.ServerURL + "\r\nFORECREW_BOOTSTRAP_TOKEN=" + req.Bootstrap.Token + "\r\n",
	}
	// Room is checked and the machine started under one lock, so two Creates
	// cannot both take the last place.
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if err := p.room(ctx, sc.Switch, req.Shape); err != nil {
		return fleet.Instance{}, err
	}
	var made struct {
		ID string `json:"id"`
	}
	if err := p.run(ctx, "create", in, &made); err != nil {
		// A caller that stopped must not stop the cleaning. A VM the script made
		// and could not remove is found by its notes and removed by its ID; the
		// machine's directory goes either way. Nothing is removed by name: an
		// operator's VM may have the same one.
		clean := context.WithoutCancel(ctx)
		if derr := errors.Join(p.destroyMachine(clean, req.MachineID), p.destroyVM(clean, managed{name: name, meta: meta})); derr != nil {
			p.log.Warn("clean up a failed machine", "machine_id", req.MachineID, "error", derr)
		}
		return fleet.Instance{}, errors.New(strings.ReplaceAll(err.Error(), req.Bootstrap.Token, "[redacted]"))
	}
	m := managed{id: made.ID, name: name, state: "Running", meta: meta}
	p.mu.Lock()
	p.armLocked(m)
	p.mu.Unlock()
	return fleet.Instance{ExternalID: made.ID, MachineID: req.MachineID, Running: true, Tags: meta.Tags, CreatedAt: time.Now().UTC()}, nil
}

// shape reads a shape's provider_config strictly and fills in the location's
// switch and VLAN.
func (p *Provider) shape(raw map[string]any) (shapeConfig, error) {
	var s shapeConfig
	b, err := json.Marshal(raw)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("provider_config: %w", err)
	}
	if s.Switch == "" {
		s.Switch = p.cfg.Switch
	}
	if !switchName.MatchString(s.Switch) {
		return s, fmt.Errorf("provider_config.switch %q is not a switch name", s.Switch)
	}
	vlan := p.cfg.VLAN
	if s.VLAN != nil {
		vlan = *s.VLAN
	}
	if vlan < 0 || vlan > 4094 {
		return s, fmt.Errorf("provider_config.vlan must be 0 to 4094, got %d", vlan)
	}
	s.VLAN = &vlan
	return s, nil
}

// plan makes the VM's notes and network rules: where forecrewd is, as this host
// resolves it, and what the location allows.
func (p *Provider) plan(ctx context.Context, req fleet.CreateRequest, switchName string) (vmcommon.Meta, []vmcommon.Rule, error) {
	server, port, err := vmcommon.ResolveServer(ctx, p.resolver, req.Bootstrap.ServerURL)
	if err != nil {
		return vmcommon.Meta{}, nil, err
	}
	meta := vmcommon.NewMeta(req.MachineID)
	meta.Tags = withMachine(req.Tags, req.MachineID)
	meta.Allow, meta.Port, meta.Deadline, meta.Bridge = req.NetworkAllow, port, req.Deadline, adapter(switchName)
	for _, s := range server {
		meta.Server = append(meta.Server, s.String())
	}
	rules, err := p.rules(ctx, meta)
	return meta, rules, err
}

func withMachine(tags map[string]string, id uuid.UUID) map[string]string {
	out := make(map[string]string, len(tags)+1)
	maps.Copy(out, tags)
	out[fleet.TagMachine] = id.String()
	return out
}

func (p *Provider) rules(ctx context.Context, meta vmcommon.Meta) ([]vmcommon.Rule, error) {
	allow, err := fleetagent.ResolveAllow(ctx, p.resolver, meta.Allow)
	if err != nil {
		return nil, err
	}
	// Never the host's own addresses, nor the network of the switch, which holds
	// the other machines, whatever network_allow says; the other networks the
	// host is on are blocked like the private ones.
	never, local := vmcommon.HostNets(p.ifaces(), meta.Bridge)
	in := vmcommon.RuleInput{Port: meta.Port, Allow: allow, Never: never, Local: local}
	for _, s := range meta.Server {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("the machine's notes name a server address %q that is not one", s)
		}
		in.Server = append(in.Server, a)
	}
	for _, d := range p.cfg.DNS {
		in.DNS = append(in.DNS, netip.MustParseAddr(d)) // validated by the configuration
	}
	return vmcommon.Rules(in), nil
}

// adapter is the name of the host's network adapter on a switch.
func adapter(switchName string) string { return "vEthernet (" + switchName + ")" }

// room answers ErrCapacity when the host runs as many machines as allowed, or has
// no memory or disk left for one more. Its switch must exist.
func (p *Provider) room(ctx context.Context, switchName string, shape domain.Shape) error {
	all, err := p.forecrewVMs(ctx)
	if err != nil {
		return err
	}
	if len(all) >= p.cfg.MaxMachines {
		return fleet.ErrCapacity
	}
	h, err := p.host(ctx, switchName)
	if err != nil {
		return err
	}
	if !h.SwitchFound {
		return fmt.Errorf("there is no virtual switch named %s", switchName)
	}
	if h.FreeMemoryMB < shape.MemoryMB+memoryReserveMB || h.FreeDiskGB < minFreeDiskGB {
		return fleet.ErrCapacity
	}
	return nil
}

// Update implements fleet.Provider.
func (p *Provider) Update(ctx context.Context, externalID string, req fleet.UpdateRequest) error {
	m, err := p.find(ctx, externalID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("no machine %q", externalID)
	}
	old := m.meta
	m.meta.Tags, m.meta.Allow, m.meta.Deadline = withMachine(req.Tags, old.ID()), req.NetworkAllow, req.Deadline
	in := map[string]any{"id": m.id, "notes": m.meta.Encode(), "apply_rules": false}
	if len(m.meta.Encode()) > maxNotes {
		return errors.New("the machine's tags are too long for the notes of a VM")
	}
	if !slices.Equal(old.Allow, req.NetworkAllow) {
		rules, err := p.rules(ctx, m.meta)
		if err != nil {
			return err
		}
		in["apply_rules"], in["rules"] = true, ruleList(rules)
	}
	if err := p.run(ctx, "update", in, nil); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armLocked(*m)
	return nil
}

// Destroy implements fleet.Provider. Destroying what is gone succeeds; a VM that
// is not Forecrew's is never touched.
func (p *Provider) Destroy(ctx context.Context, externalID string) error {
	m, err := p.find(ctx, externalID)
	if err != nil || m == nil {
		return err
	}
	return p.destroyVM(ctx, *m)
}

func (p *Provider) destroyMachine(ctx context.Context, id uuid.UUID) error {
	all, err := p.forecrewVMs(ctx)
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.meta.ID() == id {
			if err := p.destroyVM(ctx, m); err != nil {
				return err
			}
		}
	}
	return nil
}

// destroyVM removes the VM, its disks and its directory, deleting where
// another provider would reset: a differencing disk makes a new machine as cheap
// as a restored checkpoint, and nothing of the last job survives.
func (p *Provider) destroyVM(ctx context.Context, m managed) error {
	p.deadlines.Stop(m.meta.MachineID)
	p.mu.Lock()
	delete(p.armed, m.meta.MachineID)
	p.mu.Unlock()
	return p.run(ctx, "destroy", map[string]any{"id": m.id, "dir": p.dir(m.name), "vm_dir": p.cfg.VMDir}, nil)
}

// armLocked sets the wall-time kill of m, which powers the VM off whatever
// forecrewd says or can say. The caller holds mu.
func (p *Provider) armLocked(m managed) {
	id := m.meta.MachineID
	if at, ok := p.armed[id]; ok && at.Equal(m.meta.Deadline) {
		return
	}
	if m.meta.Deadline.IsZero() {
		delete(p.armed, id)
	} else {
		p.armed[id] = m.meta.Deadline
	}
	p.deadlines.Set(id, m.meta.Deadline, func() {
		p.log.Info("machine past its deadline: powering it off", "machine_id", id)
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ScriptTimeout)
		defer cancel()
		if err := p.run(ctx, "stop", map[string]any{"id": m.id}, nil); err != nil {
			p.log.Warn("power off machine", "machine_id", id, "error", err)
		}
	})
}

// List implements fleet.Provider. It also arms the deadlines of the machines it
// finds, which is how a restart of the agent gets them back, and cleans the image
// cache now and then.
func (p *Provider) List(ctx context.Context) ([]fleet.Instance, error) {
	p.pruneCache()
	all, err := p.forecrewVMs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]fleet.Instance, 0, len(all))
	p.mu.Lock()
	for _, m := range all {
		p.armLocked(m)
		out = append(out, fleet.Instance{ExternalID: m.id, MachineID: m.meta.ID(), Running: m.state == "Running", Tags: m.meta.Tags})
	}
	p.mu.Unlock()
	return out, nil
}

// pruneCache removes base images unused for a while, at most once an hour. A
// base in use by a differencing disk is open, so it stays.
func (p *Provider) pruneCache() {
	p.mu.Lock()
	due := time.Since(p.lastGC) >= time.Hour
	if due {
		p.lastGC = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	removed, err := p.cache.Prune(time.Duration(p.cfg.ImageCacheDays) * 24 * time.Hour)
	if err != nil {
		p.log.Warn("clean the image cache", "error", err)
	}
	if len(removed) > 0 {
		p.log.Info("cleaned the image cache", "files", len(removed))
	}
}

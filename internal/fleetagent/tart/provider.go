package tart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
)

const (
	// hostReserveMB is memory left to macOS and Tart.
	hostReserveMB = 4096
	// stopWait is how long a stopping VM gets before Tart kills it.
	stopWait = 15
	// deadlineStopWait is the same at a deadline, which does not wait on a guest.
	deadlineStopWait = 3
	pollEvery        = 2 * time.Second
)

func init() { fleetagent.Register(domain.ProviderTart, newRegistered) }

// newRegistered is the provider the fleet agent makes, from tart.yaml.
func newRegistered() (fleet.Provider, error) {
	path := os.Getenv(envConfig)
	if path == "" {
		path = defaultConfig
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(cfg, nil)
}

// Provider runs machines on this Mac. A restart loses nothing: what it holds is
// in its records and in Tart, and the VMs keep running.
type Provider struct {
	cfg      Config
	log      *slog.Logger
	cli      cli
	resolver *net.Resolver
	poll     time.Duration
	// The host, as the checks see it; tests replace them.
	goos, goarch string
	memoryMB     func() (int, error)
	freeGB       func(path string) (int, error)
	lookPath     func(name string) bool
	getenv       func(string) string
	pmset        func(ctx context.Context) (string, error)
	// vmProcesses counts the virtual machines running on the whole Mac, whoever
	// started them.
	vmProcesses func(ctx context.Context) (int, error)
	// hostNets are the host's own addresses, with their networks.
	hostNets func() []netip.Prefix

	// mu guards the records of machines being changed and the timers.
	mu       sync.Mutex
	timers   map[string]*time.Timer
	lastGC   time.Time
	fetching sync.Mutex // one pull or prune at a time; also guards verified
	// verified holds the fingerprint of each pulled image that was checked.
	verified map[string]string
}

// New returns the provider of cfg. log may be nil.
func New(cfg Config, log *slog.Logger) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Provider{
		cfg: cfg, log: log, resolver: net.DefaultResolver, poll: pollEvery, goos: runtime.GOOS, goarch: runtime.GOARCH,
		memoryMB: fleetagent.MemoryMB, freeGB: fleetagent.DiskFreeGB, getenv: os.Getenv, pmset: pmset, vmProcesses: countVMProcesses, hostNets: localNets,
		lookPath: func(name string) bool { _, err := exec.LookPath(name); return err == nil },
		timers:   map[string]*time.Timer{}, verified: map[string]string{},
	}
	p.cli = cli{bin: cfg.Tart, home: cfg.StateDir, getenv: p.getenv, user: cfg.RegistryUsername, pass: cfg.RegistryPassword}
	if err := p.armTimers(); err != nil {
		return nil, err
	}
	return p, nil
}

// armTimers sets the deadlines of the machines a restart found.
func (p *Provider) armTimers() error {
	all, err := p.records()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range all {
		p.arm(r)
	}
	return nil
}

// arm sets r's hard wall-time stop. It ends the VM whatever forecrewd says or
// can say; forecrewd then destroys the rest. The caller holds mu.
func (p *Provider) arm(r *record) {
	if t := p.timers[r.ID]; t != nil {
		t.Stop()
		delete(p.timers, r.ID)
	}
	if r.Deadline.IsZero() {
		return
	}
	id := r.ID
	p.timers[id] = time.AfterFunc(time.Until(r.Deadline), func() {
		p.log.Info("machine past its deadline: stopping it", "machine_id", id)
		if err := p.cli.stop(context.Background(), vmName(id), deadlineStopWait); err != nil {
			p.log.Warn("stop machine", "machine_id", id, "error", err)
		}
	})
}

// Capabilities implements fleet.Provider.
func (p *Provider) Capabilities(ctx context.Context) (domain.ProviderCapabilities, error) {
	c := domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSMacOS}, Arch: []domain.MachineArch{domain.ArchARM64},
		ImageFormats: []domain.ImageFormat{domain.ImageTart},
		MaxMachines:  p.cfg.limit(), MaxCPU: runtime.NumCPU(), Checks: p.checks(ctx),
	}
	// Apple's two are the Mac's, so a VM of the operator's own takes one.
	if foreign, err := p.foreignVMs(ctx); err == nil && foreign > 0 {
		c.MaxMachines = min(c.MaxMachines, max(maxVMs-foreign, 1)) // 0 would mean no limit
		c.Checks = append(c.Checks, domain.ProviderCheck{Name: "other VMs", Status: statusWarn,
			Detail: fmt.Sprintf("%d VMs not of Forecrew's run on this Mac, and Apple allows %d", foreign, maxVMs)})
	}
	if memory, err := p.memoryMB(); err == nil {
		c.MaxMemoryMB = max(memory-hostReserveMB, 0)
	}
	if err := os.MkdirAll(p.cfg.StateDir, 0o700); err == nil {
		if gb, err := p.freeGB(p.cfg.StateDir); err == nil {
			c.MaxDiskGB = gb
		}
	}
	return c, nil
}

// providerConfig is a shape's provider_config.
type providerConfig struct {
	// BaseVM is a VM already on this Mac, to clone instead of the image.
	BaseVM string
}

func parseProviderConfig(m map[string]any) (providerConfig, error) {
	var c providerConfig
	for k, v := range m {
		s, ok := v.(string)
		if k != "base_vm" || !ok {
			return c, fmt.Errorf("provider_config %q is not a setting of a tart location (base_vm is the only one, a string)", k)
		}
		c.BaseVM = s
	}
	return c, nil
}

// Create implements fleet.Provider.
func (p *Provider) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	id := req.MachineID.String()
	if req.Bootstrap == nil {
		return fleet.Instance{}, errors.New("no bootstrap settings")
	}
	if req.Shape.OS != domain.OSMacOS || req.Shape.Arch != domain.ArchARM64 {
		return fleet.Instance{}, fmt.Errorf("a Tart location runs macOS arm64 guests, not %s %s", req.Shape.OS, req.Shape.Arch)
	}
	cfg, err := parseProviderConfig(req.Shape.ProviderConfig)
	if err != nil {
		return fleet.Instance{}, err
	}
	settings, err := bootstrapEnv(req.Bootstrap)
	if err != nil {
		return fleet.Instance{}, err
	}
	// A retry carries a new token: what exists goes first.
	if err := p.Destroy(ctx, id); err != nil {
		return fleet.Instance{}, err
	}
	allow, err := p.allowList(ctx, req)
	if err != nil {
		return fleet.Instance{}, err
	}
	r, err := p.reserve(ctx, id, req)
	if err != nil {
		return fleet.Instance{}, err
	}
	if err := p.start(ctx, r, req, cfg, allow, settings); err != nil {
		// A caller that stopped must not stop the cleaning.
		if derr := p.Destroy(context.WithoutCancel(ctx), id); derr != nil {
			p.log.Warn("clean up a failed machine", "machine_id", id, "error", derr)
		}
		return fleet.Instance{}, err
	}
	return p.instance(r, true), nil
}

func (p *Provider) instance(r *record, running bool) fleet.Instance {
	inst := fleet.Instance{ExternalID: r.ID, Running: running, Tags: r.Tags, CreatedAt: r.CreatedAt}
	if id, err := uuid.Parse(r.ID); err == nil {
		inst.MachineID = id
	}
	return inst
}

// allowList is what Softnet lets a machine reach beyond the internet: the
// location's network_allow, and forecrewd's address, which a machine of a home
// network cannot do without. The host's own loopback cannot be reached.
func (p *Provider) allowList(ctx context.Context, req fleet.CreateRequest) ([]string, error) {
	u, err := url.Parse(req.Bootstrap.ServerURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("bad server URL")
	}
	found, err := fleetagent.Lookup4(ctx, p.resolver, u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolve the server: %w", err)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("the server %s has no IPv4 address, and machines have IPv4 only", u.Hostname())
	}
	nets, err := fleetagent.ResolveAllow(ctx, p.resolver, req.NetworkAllow)
	if err != nil {
		return nil, err
	}
	for _, a := range found {
		if a.IsLoopback() {
			return nil, fmt.Errorf("the server's address %s is this host's loopback, which a machine cannot reach: set public_base_url to an address machines can reach", a)
		}
		nets = append(nets, netip.PrefixFrom(a, 32))
	}
	if p.cfg.Softnet {
		gw := netip.MustParseAddr(p.cfg.SoftnetGateway)
		if slices.Contains(found, gw) {
			return nil, fmt.Errorf("the server's address %s is the Mac's own address on the machines' network, which is blocked: set public_base_url to an address machines can reach", gw)
		}
		// A resolver on the LAN is not a public address, and Softnet refuses it
		// unless it is allowed.
		for _, d := range p.cfg.DNS {
			if a := netip.MustParseAddr(d); !a.IsGlobalUnicast() || a.IsPrivate() {
				nets = append(nets, netip.PrefixFrom(a, 32))
			}
		}
	}
	var out []string
	for _, n := range nets {
		if s := n.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// foreignVMs counts the VMs running on this Mac that are not Forecrew's: the
// operator's own, in another Tart home or another tool. Tart's list shows only
// the agent's own home, so the count is of the processes Virtualization.framework
// runs, one per VM, on the whole Mac.
func (p *Provider) foreignVMs(ctx context.Context) (int, error) {
	vms, err := p.cli.list(ctx)
	if err != nil {
		return 0, err
	}
	return p.foreign(ctx, vms)
}

func (p *Provider) foreign(ctx context.Context, vms []vmInfo) (int, error) {
	system, err := p.vmProcesses(ctx)
	if err != nil {
		return 0, fmt.Errorf("count the VMs of this Mac: %w", err)
	}
	own := 0
	for _, v := range vms {
		if v.Running {
			own++
		}
	}
	return max(system-own, 0), nil
}

// reserve takes a place and writes the record, or answers ErrCapacity. Apple's
// limit is on the Mac, so it counts every VM running on it, Forecrew's or not, and
// the machines of the records that Tart does not run yet.
func (p *Provider) reserve(ctx context.Context, id string, req fleet.CreateRequest) (*record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	all, err := p.records()
	if err != nil {
		return nil, err
	}
	vms, err := p.cli.list(ctx)
	if err != nil {
		return nil, err
	}
	foreign, err := p.foreign(ctx, vms)
	if err != nil {
		return nil, err
	}
	own := len(all) // a record is a machine, running or being made
	for _, v := range vms {
		if mid, ok := machineID(v.Name); v.Running && !(ok && slices.ContainsFunc(all, func(r *record) bool { return r.ID == mid })) {
			own++ // an orphan not destroyed yet still holds a place
		}
	}
	if own >= p.cfg.limit() || own+foreign >= maxVMs {
		return nil, fleet.ErrCapacity
	}
	memory, err := p.memoryMB()
	if err != nil {
		return nil, err
	}
	used := req.Shape.MemoryMB
	for _, r := range all {
		used += r.MemoryMB
	}
	if used > memory-hostReserveMB {
		return nil, fleet.ErrCapacity
	}
	if gb, err := p.freeGB(p.cfg.StateDir); err != nil || gb < req.Shape.DiskGB {
		return nil, fleet.ErrCapacity
	}
	r := &record{
		ID: id, MemoryMB: req.Shape.MemoryMB, DiskGB: req.Shape.DiskGB, Tags: req.Tags, Allow: req.NetworkAllow,
		Deadline: req.Deadline, CreatedAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(filepath.Dir(p.recordPath(id)), 0o700); err != nil {
		return nil, err
	}
	if err := p.save(r); err != nil {
		return nil, err
	}
	p.arm(r)
	return r, nil
}

// start makes the machine: a clone of its source, sized, run, given its
// settings.
func (p *Provider) start(ctx context.Context, r *record, req fleet.CreateRequest, cfg providerConfig, allow []string, settings string) error {
	name := vmName(r.ID)
	src, err := p.source(ctx, cfg, req.Image)
	if err != nil {
		return err
	}
	if err := p.cli.clone(ctx, src, name); err != nil {
		return err
	}
	set := []string{"--cpu", strconv.Itoa(req.Shape.CPU), "--memory", strconv.Itoa(req.Shape.MemoryMB)}
	// A disk only grows, and only the shape's size is the quota. A size Tart does
	// not report is unknown, and the disk is left alone rather than risk shrinking it.
	if info, err := p.cli.info(ctx, name); err != nil {
		return err
	} else if info.Disk != nil && req.Shape.DiskGB > *info.Disk {
		set = append(set, "--disk-size", strconv.Itoa(req.Shape.DiskGB))
	}
	if err := p.cli.set(ctx, name, set...); err != nil {
		return err
	}
	exited, err := p.cli.start(name, p.logPath(r.ID), p.runFlags(allow))
	if err != nil {
		return err
	}
	if err := p.waitGuest(ctx, r.ID, exited); err != nil {
		return err
	}
	if p.cfg.Softnet {
		if err := p.checkGateway(ctx, name); err != nil {
			return err
		}
	}
	return p.deliver(ctx, name, settings)
}

// runFlags run a machine headless, with nothing of the host shared: no
// directory, no clipboard, no audio.
func (p *Provider) runFlags(allow []string) []string {
	flags := []string{"--no-graphics", "--no-audio", "--no-clipboard"}
	if p.cfg.Softnet {
		// Softnet lets a machine reach the Mac on the VM network's gateway unless it
		// is blocked, and what is blocked beats what is allowed.
		flags = append(flags, "--net-softnet", "--net-softnet-block="+p.cfg.SoftnetGateway+"/32")
		if len(allow) > 0 {
			flags = append(flags, "--net-softnet-allow="+hostList(allow))
		}
	}
	return flags
}

// Update implements fleet.Provider. A running VM's network cannot change, so a
// different network_allow is refused, not ignored.
func (p *Provider) Update(_ context.Context, externalID string, req fleet.UpdateRequest) error {
	r, err := p.load(externalID)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("no machine %q", externalID)
	}
	if !slices.Equal(r.Allow, req.NetworkAllow) && (len(r.Allow) > 0 || len(req.NetworkAllow) > 0) {
		return errors.New("the network of a running Tart machine cannot change: its network_allow was fixed when it was made")
	}
	r.Tags, r.Deadline = maps.Clone(req.Tags), req.Deadline
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.save(r); err != nil {
		return err
	}
	p.arm(r)
	return nil
}

// Destroy implements fleet.Provider. Each step is safe to repeat, and the record
// goes last, so a failure leaves something to try again. It finds a VM by its
// name, with or without a record.
func (p *Provider) Destroy(ctx context.Context, externalID string) error {
	if !validID.MatchString(externalID) {
		return nil // nothing of ours has that name
	}
	p.mu.Lock()
	if t := p.timers[externalID]; t != nil {
		t.Stop()
		delete(p.timers, externalID)
	}
	p.mu.Unlock()
	name := vmName(externalID)
	vms, err := p.cli.list(ctx)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(vms, func(v vmInfo) bool { return v.Name == name }); i >= 0 {
		if vms[i].Running {
			// A VM that ended meanwhile is what was wanted.
			if err := p.cli.stop(ctx, name, stopWait); err != nil && p.runningNow(ctx, name) {
				return err
			}
		}
		if err := p.cli.delete(ctx, name); err != nil {
			return err
		}
	}
	_ = os.Remove(p.logPath(externalID))
	if err := os.Remove(p.recordPath(externalID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove record: %w", err)
	}
	return nil
}

// runningNow asks Tart whether name still runs.
func (p *Provider) runningNow(ctx context.Context, name string) bool {
	vms, err := p.cli.list(ctx)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(vms, func(v vmInfo) bool { return v.Name == name && v.Running })
}

// List implements fleet.Provider: the machines of the records, and the VMs of
// Forecrew's name that have none, which forecrewd then finds to be orphans.
func (p *Provider) List(ctx context.Context) ([]fleet.Instance, error) {
	p.pruneCache(ctx)
	all, err := p.records()
	if err != nil {
		return nil, err
	}
	vms, err := p.cli.list(ctx)
	if err != nil {
		return nil, err
	}
	up := map[string]bool{}
	for _, v := range vms {
		if id, ok := machineID(v.Name); ok {
			up[id] = v.Running
		}
	}
	out := make([]fleet.Instance, 0, len(all))
	for _, r := range all {
		out = append(out, p.instance(r, up[r.ID]))
		delete(up, r.ID)
	}
	for _, id := range slices.Sorted(maps.Keys(up)) {
		out = append(out, p.instance(&record{ID: id}, up[id]))
	}
	return out, nil
}

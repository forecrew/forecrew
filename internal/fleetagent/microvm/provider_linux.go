package microvm

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/guestinit"
	"github.com/forecrew/forecrew/internal/images"
)

type ed25519Key = ed25519.PublicKey

const (
	// hostReserveMB is memory left to the host and the VMMs.
	hostReserveMB = 1024
	// minMachineMB sizes the default number of machines.
	minMachineMB = 512
	// maxSlots is the most machines one host numbers.
	maxSlots = 1024
	// killWait bounds how long a killed VMM takes to go.
	killWait = 10 * time.Second
	// pruneEvery spaces the cleaning of the image cache.
	pruneEvery = time.Hour
	// fetchTimeout bounds fetching one file of an image.
	fetchTimeout = 30 * time.Minute
	// earlyExit is how long Create watches a VMM for dying at once.
	earlyExit = 200 * time.Millisecond
)

// caFiles are where hosts keep their CA bundle.
var caFiles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem", "/etc/ssl/cert.pem"}

func init() { fleetagent.Register(domain.ProviderMicroVM, newRegistered) }

// newRegistered is the provider the fleet agent makes, from microvm.yaml.
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

// Provider runs machines on this host. A restart loses nothing: what it holds is
// in its records, and the machines keep running.
type Provider struct {
	cfg      Config
	log      *slog.Logger
	subnet   netip.Prefix
	dns      netip.Addr
	vmms     map[string]vmm
	http     *http.Client
	resolver *net.Resolver
	// run runs a host command with stdin; tests replace it.
	run func(ctx context.Context, stdin, name string, args ...string) error
	// chown gives a file to a machine's user; tests replace it.
	chown func(path string, uid, gid int) error
	// memoryMB is the host's memory, availableMB what it can still give.
	memoryMB, availableMB func() (int, error)
	// localNets are the networks the host is on; hasDevice says whether a
	// network device exists.
	localNets func(subnet netip.Prefix) []netip.Prefix
	// freeBytes is the room left on a file system.
	freeBytes func(path string) (int64, error)
	hasDevice func(name string) bool
	// mknod makes node the block device device is; hasMapping says whether a
	// dm-crypt mapping is open.
	mknod      func(device, node string) error
	hasMapping func(name string) bool
	// output runs a host command and returns what it wrote; cpuTime reads a
	// process's CPU time; now is the time. Tests replace them.
	output  func(ctx context.Context, name string, args ...string) ([]byte, error)
	cpuTime func(pid int) (time.Duration, error)
	now     func() time.Time

	// mu guards slots and the records of machines being changed.
	mu       sync.Mutex
	timers   map[string]*time.Timer
	lastGC   time.Time
	fetching sync.Mutex // also guards verified
	caching  sync.Mutex // guards the organizations' caches
	watchMu  sync.Mutex // guards watch
	watch    map[string]*cpuWatch
	// verified holds, by image signature, the files as they were when their
	// digests were last checked.
	verified map[string][2]stamp
}

// New returns the provider of cfg. log may be nil.
func New(cfg Config, log *slog.Logger) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	subnet, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cfg.StateDir, "jail")
	p := &Provider{
		cfg: cfg, log: log, subnet: subnet, dns: netip.MustParseAddr(cfg.DNS),
		vmms: map[string]vmm{
			vmmFirecracker:     firecracker{jailer: lookPath(cfg.Jailer), bin: lookPath(cfg.Firecracker), base: base},
			vmmCloudHypervisor: cloudHypervisor{bin: lookPath(cfg.CloudHypervisor), base: base},
		},
		http: &http.Client{Timeout: fetchTimeout}, resolver: net.DefaultResolver,
		run: runCommand, chown: os.Chown, memoryMB: fleetagent.MemoryMB, availableMB: fleetagent.AvailableMemoryMB, localNets: localNets, freeBytes: freeBytes,
		hasDevice: func(name string) bool { _, err := net.InterfaceByName(name); return err == nil },
		mknod:     mknodOf,
		output:    outputOf, cpuTime: cpuTime, now: time.Now, watch: map[string]*cpuWatch{},
		hasMapping: func(name string) bool { _, err := os.Stat(filepath.Join("/dev/mapper", name)); return err == nil },
		timers:     map[string]*time.Timer{}, verified: map[string][2]stamp{},
	}
	if err := p.armTimers(); err != nil {
		return nil, err
	}
	return p, nil
}

// lookPath makes a command absolute, as the jailer wants it; a missing one stays
// as it is, for the checks to report.
func lookPath(name string) string {
	if abs, err := exec.LookPath(name); err == nil {
		if a, err := filepath.Abs(abs); err == nil {
			return a
		}
	}
	return name
}

// mknodOf makes node a block device node of what device is.
func mknodOf(device, node string) error {
	var st syscall.Stat_t
	if err := syscall.Stat(device, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return fmt.Errorf("%s is not a block device", device)
	}
	return syscall.Mknod(node, syscall.S_IFBLK|0o600, int(st.Rdev)) // #nosec G115 -- a device number
}

func runCommand(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed commands, with values of this package
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
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

// arm sets r's hard wall-time kill. It ends the VMM whatever forecrewd says or
// can say; the rest of the machine goes when it is destroyed. The caller holds
// mu.
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
		p.log.Info("machine past its deadline: killing it", "machine_id", id)
		now, err := p.load(id) // its PID may be newer than this timer
		if err == nil && now != nil {
			err = kill(now)
		}
		if err != nil {
			p.log.Warn("kill machine", "machine_id", id, "error", err)
		}
	})
}

// kill ends r's VMM, and everything in its session, and waits until it is
// gone. The VMM leads its own session (spawn), so its group is the jail.
func kill(r *record) error {
	if !running(r) {
		return nil
	}
	if err := syscall.Kill(-r.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill: %w", err)
	}
	if err := syscall.Kill(r.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill: %w", err)
	}
	for end := time.Now().Add(killWait); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if !running(r) {
			return nil
		}
	}
	return fmt.Errorf("machine %s: its VMM did not go", r.ID)
}

// maxMachines is how many machines this host runs at once.
func (p *Provider) maxMachines() int {
	if p.cfg.MaxMachines > 0 {
		return min(p.cfg.MaxMachines, maxSlots, p.slots())
	}
	memory, err := p.memoryMB()
	if err != nil {
		return 1
	}
	return min(max((memory-hostReserveMB)/minMachineMB, 1), maxSlots, p.slots())
}

// slots is how many /30 networks fit the subnet.
func (p *Provider) slots() int {
	bits := 32 - p.subnet.Bits() - 2
	if bits >= 20 {
		return math.MaxInt32
	}
	return 1 << bits
}

// Capabilities implements fleet.Provider.
func (p *Provider) Capabilities(context.Context) (domain.ProviderCapabilities, error) {
	c := domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSLinux}, ImageFormats: []domain.ImageFormat{domain.ImageRootfs},
		MaxMachines: p.maxMachines(), MaxCPU: runtime.NumCPU(), Checks: p.checks(),
	}
	if memory, err := p.memoryMB(); err == nil {
		c.MaxMemoryMB = max(memory-hostReserveMB, 0)
	}
	if _, err := exec.LookPath(p.cfg.CloudHypervisor); err == nil {
		c.MaxGPU = len(p.cfg.GPUs)
	}
	if err := os.MkdirAll(p.cfg.StateDir, 0o711); err == nil { // #nosec G301 -- each machine's own directory is closed
		if gb, err := fleetagent.DiskFreeGB(p.cfg.StateDir); err == nil {
			c.MaxDiskGB = gb
		}
	}
	return c, nil
}

// Create implements fleet.Provider.
func (p *Provider) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	id := req.MachineID.String()
	if req.Bootstrap == nil {
		return fleet.Instance{}, errors.New("no bootstrap settings")
	}
	if req.Shape.OS != domain.OSLinux {
		return fleet.Instance{}, fmt.Errorf("a microVM location runs Linux guests, not %s", req.Shape.OS)
	}
	name, err := pickVMM(req.Shape)
	if err != nil {
		return fleet.Instance{}, err
	}
	// A hosted machine runs a paying organization's code on the instance's
	// own network: only an allowlist keeps it to what that organization may
	// reach.
	if req.Shape.Ownership() == domain.OwnershipHosted {
		if err := p.hostedEgress(); err != nil {
			return fleet.Instance{}, fmt.Errorf("refusing a hosted machine: %w", err)
		}
	}
	if err := p.prepareDirs(); err != nil {
		return fleet.Instance{}, err
	}
	// A retry carries a new token: what exists goes first.
	if err := p.Destroy(ctx, id); err != nil {
		return fleet.Instance{}, err
	}
	rootImage, kernel, err := p.ensureImage(ctx, req.Image)
	if err != nil {
		return fleet.Instance{}, err
	}
	settings, port, err := p.settings(ctx, req.Bootstrap)
	if err != nil {
		return fleet.Instance{}, err
	}
	r, err := p.reserve(id, name, req, settings.ServerAddrs, port)
	if err != nil {
		return fleet.Instance{}, err
	}
	if err := p.start(ctx, r, req.Shape, rootImage, kernel, settings); err != nil {
		// A caller that stopped must not stop the cleaning.
		if derr := p.Destroy(context.WithoutCancel(ctx), id); derr != nil {
			p.log.Warn("clean up a failed machine", "machine_id", id, "error", derr)
		}
		return fleet.Instance{}, err
	}
	return r.instance(), nil
}

func (r *record) instance() fleet.Instance {
	inst := fleet.Instance{ExternalID: r.ID, Running: running(r), Tags: r.Tags, CreatedAt: r.CreatedAt}
	if id, err := parseID(r.ID); err == nil {
		inst.MachineID = id
	}
	return inst
}

func (p *Provider) prepareDirs() error {
	for _, d := range []string{p.cfg.StateDir, filepath.Join(p.cfg.StateDir, "machines"), p.imageDir(), filepath.Join(p.cfg.StateDir, "jail")} {
		if err := os.MkdirAll(d, 0o711); err != nil { // #nosec G301 -- a machine's user walks through to its own closed directory
			return err
		}
	}
	return os.MkdirAll(p.cachesDir(), 0o700)
}

// settings makes what goes on the config disk: the host resolves the server, and
// says what its certificate chains to, because the image may not be trusted on
// either.
func (p *Provider) settings(ctx context.Context, b *fleet.Bootstrap) (guestinit.Settings, int, error) {
	u, err := url.Parse(b.ServerURL)
	if err != nil {
		return guestinit.Settings{}, 0, errors.New("bad server URL")
	}
	found, err := fleetagent.Lookup4(ctx, p.resolver, u.Hostname())
	if err != nil {
		return guestinit.Settings{}, 0, fmt.Errorf("resolve the server: %w", err)
	}
	s := guestinit.Settings{ServerURL: b.ServerURL, Token: b.Token, Time: time.Now().UTC()}
	for _, a := range found {
		if a.IsLoopback() {
			return guestinit.Settings{}, 0, fmt.Errorf("the server's address %s is this host's loopback, which a machine cannot reach: set public_base_url to an address machines can reach", a)
		}
		s.ServerAddrs = append(s.ServerAddrs, a.String())
	}
	if len(s.ServerAddrs) == 0 {
		return guestinit.Settings{}, 0, fmt.Errorf("the server %s has no IPv4 address, and machines have IPv4 only", u.Hostname())
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
		if s.CAPEM, err = p.caBundle(); err != nil {
			return guestinit.Settings{}, 0, err
		}
	}
	if u.Port() != "" {
		if port, err = strconv.Atoi(u.Port()); err != nil {
			return guestinit.Settings{}, 0, errors.New("bad server URL")
		}
	}
	return s, port, nil
}

func (p *Provider) caBundle() (string, error) {
	files := caFiles
	if p.cfg.CAFile != "" {
		files = []string{p.cfg.CAFile}
	}
	for _, f := range files {
		if raw, err := os.ReadFile(f); err == nil { // #nosec G304 -- the operator's file, or a fixed one
			return string(raw), nil
		}
	}
	return "", fmt.Errorf("no CA bundle: looked at %s; set ca_file", strings.Join(files, ", "))
}

// reserve takes a slot and writes the record, or answers ErrCapacity.
func (p *Provider) reserve(id, vmmName string, req fleet.CreateRequest, server []string, port int) (*record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	all, err := p.records()
	if err != nil {
		return nil, err
	}
	if len(all) >= p.maxMachines() {
		return nil, fleet.ErrCapacity
	}
	memory, err := p.memoryMB()
	if err != nil {
		return nil, err
	}
	used := req.Shape.MemoryMB
	var gpus []string
	for _, r := range all {
		used += r.MemoryMB
		gpus = append(gpus, r.GPUs...)
	}
	if used > memory-hostReserveMB {
		return nil, fleet.ErrCapacity
	}
	// What the records promise is not all: the host itself says what it can
	// still give, whatever else took memory since.
	if available, err := p.availableMB(); err != nil || req.Shape.MemoryMB > available-hostReserveMB {
		return nil, fleet.ErrCapacity
	}
	var free []string
	for _, g := range p.cfg.GPUs {
		if !slices.Contains(gpus, g) {
			free = append(free, g)
		}
	}
	if req.Shape.GPU > len(free) {
		return nil, fleet.ErrCapacity
	}
	need := req.Shape.DiskGB
	if p.orgCacheOf(req) != "" {
		need += p.cfg.OrgCacheGB
	}
	if !p.diskRoom(all, need) {
		return nil, fleet.ErrCapacity
	}
	slot := 0
	for slices.ContainsFunc(all, func(r *record) bool { return r.Slot == slot }) {
		slot++
	}
	r := &record{
		ID: id, VMM: vmmName, Slot: slot, CPU: req.Shape.CPU, MemoryMB: req.Shape.MemoryMB, DiskGB: req.Shape.DiskGB, GPUs: free[:req.Shape.GPU], OrgCache: p.orgCacheOf(req),
		Tags: req.Tags, Allow: req.NetworkAllow, Server: server, Port: port, Deadline: req.Deadline, CreatedAt: time.Now().UTC(),
	}
	if err := p.save(r); err != nil {
		return nil, err
	}
	p.arm(r)
	return r, nil
}

// diskRoom reports that the state directory holds a new disk of diskGB as well
// as what the machines it has may still write: their disks are sparse, and a
// full host file system would fail them all at once.
func (p *Provider) diskRoom(all []*record, diskGB int) bool {
	free, err := p.freeBytes(p.cfg.StateDir)
	if err != nil {
		return false
	}
	need := int64(diskGB) << 30
	for _, r := range all {
		var st syscall.Stat_t
		allocated := int64(0)
		if syscall.Stat(filepath.Join(p.vmms[r.VMM].jailDir(r.ID), fileRoot), &st) == nil {
			allocated = st.Blocks * 512
		}
		need += max(int64(r.DiskGB)<<30-allocated, 0)
		if r.OrgCache != "" {
			need += int64(p.cfg.OrgCacheGB) << 30 // at worst a full copy
		}
	}
	return need <= free
}

func freeBytes(path string) (int64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * s.Bsize, nil // #nosec G115 -- a size that fits
}

// start makes everything r names and starts its VMM.
func (p *Provider) start(ctx context.Context, r *record, shape domain.Shape, rootImage, kernel string, s guestinit.Settings) error {
	v := p.vmms[r.VMM]
	dir := v.jailDir(r.ID)
	uid := p.cfg.UIDBase + r.Slot
	if err := os.MkdirAll(filepath.Dir(dir), 0o711); err != nil { // #nosec G301 -- the user walks through to its own directory
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := p.chown(dir, uid, uid); err != nil {
		return err
	}
	// A copy, not a link: linking would change the cached file's change
	// time, which is how a changed cache file is found (ensureImage).
	if err := cloneReadOnly(kernel, filepath.Join(dir, fileKernel)); err != nil {
		return fmt.Errorf("kernel: %w", err)
	}
	if err := p.rootDisk(rootImage, dir, shape.DiskGB, uid); err != nil {
		return fmt.Errorf("root disk: %w", err)
	}
	if err := p.configDisk(dir, s, uid); err != nil {
		return fmt.Errorf("config disk: %w", err)
	}
	cache := filepath.Join(p.cfg.StateDir, fileCache)
	_, statErr := os.Stat(cache)
	hasCache := statErr == nil
	if hasCache {
		// Shared by every machine through a hard link: nobody may write it.
		if err := os.Chmod(cache, 0o444); err != nil { // #nosec G302 -- read-only for all
			return fmt.Errorf("cache disk: %w", err)
		}
		if err := link(cache, filepath.Join(dir, fileCache)); err != nil {
			return fmt.Errorf("cache disk: %w", err)
		}
	}
	if r.OrgCache != "" {
		if err := p.openCache(ctx, r, dir, uid); err != nil {
			return fmt.Errorf("organization cache: %w", err)
		}
	}
	n := networkOf(p.subnet, r.Slot)
	if err := p.addNetwork(ctx, r, n); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if err := p.claimGPUs(r, uid); err != nil {
		return fmt.Errorf("gpu: %w", err)
	}
	cmd, err := v.command(spec{
		ID: r.ID, Dir: dir, CPU: shape.CPU, MemoryMB: shape.MemoryMB, Tap: n.Tap, MAC: n.MAC,
		BootArgs: bootArgs(v, n, p.cfg.DNS, hasCache, r.OrgCache != ""), Cache: hasCache, OrgCache: r.OrgCache != "", GPUs: r.GPUs, UID: uid, GID: uid,
	})
	if err != nil {
		return err
	}
	return p.spawn(cmd, r)
}

// rootDisk gives the machine its copy of the image, as big as the shape says:
// the size of the file is the disk quota, and the guest grows its file system
// to it.
func (p *Provider) rootDisk(image, dir string, diskGB, uid int) error {
	path := filepath.Join(dir, fileRoot)
	if err := clone(image, path); err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := int64(diskGB) << 30
	if st.Size() > size {
		return fmt.Errorf("the image takes %d MB, more than the shape's %d GB", st.Size()>>20, diskGB)
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	return p.chown(path, uid, uid)
}

// configDisk writes the machine's settings. It is the guest's own disk, closed
// to everyone but its user; the guest wipes it once read, and it goes with the
// machine (fleet.md: never on the host's disk outside the guest's own image).
func (p *Provider) configDisk(dir string, s guestinit.Settings, uid int) error {
	raw, err := guestinit.Encode(s)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fileConfig)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return p.chown(path, uid, uid)
}

// spawn starts the VMM in its own session, so that it outlives the agent, with
// nothing connected to it: a guest must not fill the host's disk through its
// console.
func (p *Provider) spawn(cmd *exec.Cmd, r *record) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the VMM: %w", err)
	}
	r.PID = cmd.Process.Pid
	if err := p.save(r); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // reaps it, as the parent must
	select {
	case err := <-done:
		return fmt.Errorf("the VMM ended at once: %v", err) //nolint:errorlint // a status, for a person
	case <-time.After(earlyExit):
		return nil
	}
}

// Update implements fleet.Provider.
func (p *Provider) Update(ctx context.Context, externalID string, req fleet.UpdateRequest) error {
	r, err := p.load(externalID)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("no machine %q", externalID)
	}
	changed := !slices.Equal(r.Allow, req.NetworkAllow)
	r.Tags, r.Allow, r.Deadline = req.Tags, req.NetworkAllow, req.Deadline
	if changed {
		if err := p.applyRules(ctx, r, networkOf(p.subnet, r.Slot)); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.save(r); err != nil {
		return err
	}
	p.arm(r)
	return nil
}

// Destroy implements fleet.Provider. Each step is safe to repeat, and the record
// goes last, so a failure leaves something to try again.
func (p *Provider) Destroy(ctx context.Context, externalID string) error {
	r, err := p.load(externalID)
	if err != nil || r == nil {
		return err
	}
	p.mu.Lock()
	if t := p.timers[r.ID]; t != nil {
		t.Stop()
		delete(p.timers, r.ID)
	}
	p.mu.Unlock()
	if err := kill(r); err != nil {
		return err
	}
	if err := p.removeNetwork(ctx, networkOf(p.subnet, r.Slot)); err != nil {
		return fmt.Errorf("remove network: %w", err)
	}
	if err := p.releaseGPUs(r); err != nil {
		return fmt.Errorf("release gpu: %w", err)
	}
	if r.OrgCache != "" {
		if err := p.closeCache(ctx, r, p.vmms[r.VMM].jailDir(r.ID)); err != nil {
			return fmt.Errorf("organization cache: %w", err)
		}
	}
	// The jail's parent is the machine's too.
	if err := os.RemoveAll(filepath.Dir(p.vmms[r.VMM].jailDir(r.ID))); err != nil {
		return fmt.Errorf("remove disks: %w", err)
	}
	return os.Remove(p.recordPath(r.ID))
}

// List implements fleet.Provider.
func (p *Provider) List(context.Context) ([]fleet.Instance, error) {
	p.pruneCache()
	all, err := p.records()
	if err != nil {
		return nil, err
	}
	out := make([]fleet.Instance, 0, len(all))
	for _, r := range all {
		out = append(out, r.instance())
	}
	return out, nil
}

// pruneCache removes images unused for a while, at most once in pruneEvery. A
// running machine has its own disk, so nothing depends on the cache.
func (p *Provider) pruneCache() {
	p.mu.Lock()
	due := time.Since(p.lastGC) >= pruneEvery
	if due {
		p.lastGC = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	if !p.fetching.TryLock() {
		p.mu.Lock()
		p.lastGC = time.Time{} // a slow fetch must not hold up a reconcile: next time
		p.mu.Unlock()
		return
	}
	defer p.fetching.Unlock()
	p.pruneCaches(time.Duration(p.cfg.ImageCacheDays) * 24 * time.Hour)
	removed, err := images.PruneCache(p.imageDir(), time.Duration(p.cfg.ImageCacheDays)*24*time.Hour, false)
	if err != nil {
		p.log.Warn("clean the image cache", "error", err)
	}
	if len(removed) > 0 {
		// What is gone is checked again if it comes back.
		clear(p.verified)
		p.log.Info("cleaned the image cache", "files", len(removed))
	}
}

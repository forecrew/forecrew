package proxmox

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/fleetagent/vmcommon"
	"github.com/forecrew/forecrew/internal/images"
)

const (
	// namePrefix starts the name of every VM Forecrew makes. It is a first
	// filter only: what makes a VM Forecrew's is its notes (vmcommon.Meta).
	namePrefix = "forecrew-"
	// imageTagPrefix starts the tag a template must carry: the first 32 hex
	// digits of the digest of the image it was made from.
	imageTagPrefix = "forecrew-image-"
	// memoryReserveMB is left free on a node when machines are placed.
	memoryReserveMB = 1024
	// callTimeout bounds one call that is not a task.
	callTimeout = 30 * time.Second
	// taskTimeout bounds a task such as a full clone.
	taskTimeout = 30 * time.Minute
	// Where the bootstrap settings go in the guest (docs/images.md).
	linuxEnvFile   = "/run/forecrew/bootstrap.env"
	windowsEnvFile = `C:\ProgramData\Forecrew\run\bootstrap.env`
	// lookupTimeout bounds the lookup of a node's name.
	lookupTimeout = 3 * time.Second
	// maxAttempts is how often Create takes another VMID when one is taken.
	maxAttempts = 5
)

var (
	diskKeyRE = regexp.MustCompile(`^(scsi|sata|ide|virtio)[0-9]{1,2}$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// guestValueRE is what forecrew-start accepts in a value.
	guestValueRE = regexp.MustCompile(`^[A-Za-z0-9._/:@+,-]*$`)
)

func validName(s string) bool { return nameRE.MatchString(s) }

func init() { fleetagent.Register(domain.ProviderProxmox, newRegistered) }

// newRegistered is the provider the fleet agent makes, from proxmox.yaml.
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

// Provider makes machines as VMs of a Proxmox cluster. Everything it keeps is in
// the VMs' own notes, so a restart loses nothing.
type Provider struct {
	cfg      Config
	log      *slog.Logger
	api      *Client
	resolver *net.Resolver
	// keys are read afresh, as an operator may add one.
	keys func() ([]ed25519.PublicKey, error)

	// startMu makes the check of room and the clone that takes it one step, so
	// that parallel Creates cannot all take the last place.
	startMu sync.Mutex
	// alloc serializes the choice of a VMID with the clone that takes it.
	alloc     sync.Mutex
	deadlines vmcommon.Deadlines
	mu        sync.Mutex
	armed     map[string]time.Time
}

// New returns the provider of cfg, with the API token read from its file. log
// may be nil.
func New(cfg Config, log *slog.Logger) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	token, err := readToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	api, err := NewClient(cfg.APIURL, token, cfg.CAFile, cfg.TLSServerName)
	if err != nil {
		return nil, err
	}
	p := &Provider{cfg: cfg, log: log, api: api, resolver: net.DefaultResolver, armed: map[string]time.Time{}}
	p.keys = func() ([]ed25519.PublicKey, error) { return fleetagent.LoadTrustedKeys(cfg.TrustedKeysFile) }
	return p, nil
}

// readToken reads the API token. A file others can read is refused: it is a
// secret, and the installer makes it closed.
func readToken(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("API token: %w: the installer writes it, or put the token there for the agent's user alone", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("API token: %s must be readable by its owner alone (mode 0600)", path)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's file
	if err != nil {
		return "", fmt.Errorf("API token: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// managed is a VM of Forecrew's.
type managed struct {
	vmid   int
	node   string
	status string
	meta   vmcommon.Meta
}

func (m managed) external() string { return fmt.Sprintf("%s:%d", m.node, m.vmid) }

type resource struct {
	VMID     int    `json:"vmid"`
	Node     string `json:"node"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Template int    `json:"template"`
}

func (p *Provider) resources(ctx context.Context) ([]resource, error) {
	var all []resource
	if err := p.api.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func (p *Provider) onNode(node string) bool {
	return slices.Contains(p.cfg.Nodes, node)
}

type vmConfig map[string]any

func (c vmConfig) str(key string) string {
	switch v := c[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (p *Provider) config(ctx context.Context, node string, vmid int) (vmConfig, error) {
	var c vmConfig
	err := p.api.do(ctx, http.MethodGet, vmPath(node, vmid)+"/config", nil, &c)
	return c, err
}

func vmPath(node string, vmid int) string {
	return "/nodes/" + node + "/qemu/" + strconv.Itoa(vmid)
}

// forecrewVMs lists the VMs that are Forecrew's: on the nodes of the
// configuration, named as ours are, and with notes only this provider writes.
func (p *Provider) forecrewVMs(ctx context.Context) ([]managed, error) {
	all, err := p.resources(ctx)
	if err != nil {
		return nil, err
	}
	var out []managed
	for _, r := range all {
		if r.Type != "qemu" || r.Template != 0 || !strings.HasPrefix(r.Name, namePrefix) || !p.onNode(r.Node) {
			continue
		}
		c, err := p.config(ctx, r.Node, r.VMID)
		if notFound(err) {
			continue // it went meanwhile
		}
		if err != nil {
			return nil, err
		}
		if meta, ok := vmcommon.ParseMeta(c.str("description")); ok {
			out = append(out, managed{vmid: r.VMID, node: r.Node, status: r.Status, meta: meta})
		}
	}
	return out, nil
}

// find returns the VM at an external ID (node:vmid), looked up in the cluster,
// since a VM may have moved to another node. It returns nil when there is none,
// and an error when the VM there is not Forecrew's.
func (p *Provider) find(ctx context.Context, external string) (*managed, error) {
	_, rest, ok := strings.Cut(external, ":")
	vmid, err := strconv.Atoi(rest)
	if !ok || err != nil {
		return nil, fmt.Errorf("%q is not the ID of a machine here", external)
	}
	all, err := p.resources(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.VMID != vmid || r.Type != "qemu" {
			continue
		}
		c, err := p.config(ctx, r.Node, r.VMID)
		if notFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		meta, ok := vmcommon.ParseMeta(c.str("description"))
		if !ok || r.Template != 0 || !p.onNode(r.Node) || !strings.HasPrefix(r.Name, namePrefix) {
			return nil, fmt.Errorf("VM %d is not one of Forecrew's machines: leaving it alone", vmid)
		}
		return &managed{vmid: vmid, node: r.Node, status: r.Status, meta: meta}, nil
	}
	return nil, nil
}

// Capabilities implements fleet.Provider. A cluster that cannot be asked gives a
// failing check, not an error: forecrewd should still see why.
func (p *Provider) Capabilities(ctx context.Context) (domain.ProviderCapabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	c := domain.ProviderCapabilities{
		OS: []domain.MachineOS{domain.OSLinux, domain.OSWindows}, ImageFormats: []domain.ImageFormat{domain.ImageQCOW2},
		MaxMachines: p.cfg.MaxMachines,
	}
	c.Checks = p.check(ctx, &c)
	return c, nil
}

// Create implements fleet.Provider.
func (p *Provider) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	if req.Bootstrap == nil {
		return fleet.Instance{}, errors.New("no bootstrap settings")
	}
	guestFile, err := bootstrapFile(req.Shape.OS)
	if err != nil {
		return fleet.Instance{}, err
	}
	cfg, err := p.checkRequest(req)
	if err != nil {
		return fleet.Instance{}, err
	}
	// A retry carries a new token: what exists goes first.
	if err := p.destroyMachine(ctx, req.MachineID); err != nil {
		return fleet.Instance{}, err
	}
	meta, rules, err := p.plan(ctx, req, cfg.Node, cfg.Bridge)
	if err != nil {
		return fleet.Instance{}, err
	}
	vmid, err := p.place(ctx, cfg, req, meta)
	if err != nil {
		return fleet.Instance{}, err
	}
	m := managed{vmid: vmid, node: cfg.Node, meta: meta}
	if err := p.setUp(ctx, m, cfg, req, rules, guestFile); err != nil {
		// A caller that stopped must not stop the cleaning.
		if derr := p.destroyVM(context.WithoutCancel(ctx), m); derr != nil {
			p.log.Warn("clean up a failed machine", "machine_id", req.MachineID, "error", derr)
		}
		return fleet.Instance{}, err
	}
	p.arm(m)
	return fleet.Instance{ExternalID: m.external(), MachineID: req.MachineID, Running: true, Tags: meta.Tags, CreatedAt: time.Now().UTC()}, nil
}

// place checks that the machine may be made, and makes it, as one step: room is
// counted from the VMs that exist, and the clone is one of them when the lock is
// let go.
func (p *Provider) place(ctx context.Context, cfg shapeConfig, req fleet.CreateRequest, meta vmcommon.Meta) (int, error) {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if err := p.requireFirewall(ctx); err != nil {
		return 0, err
	}
	if err := p.room(ctx, cfg.Node, req.Shape); err != nil {
		return 0, err
	}
	if err := p.checkTemplate(ctx, cfg, req.Image); err != nil {
		return 0, err
	}
	return p.clone(ctx, cfg, req, meta)
}

// requireFirewall refuses to make a machine while the datacenter firewall is
// off: the rules of a machine's own firewall do nothing then, and it would
// reach the host and the network.
func (p *Provider) requireFirewall(ctx context.Context) error {
	var fw struct {
		Enable int `json:"enable"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/cluster/firewall/options", nil, &fw); err != nil {
		return fmt.Errorf("cannot tell whether the datacenter firewall is on (the token needs Sys.Audit on /): %w", err)
	}
	if fw.Enable != 1 {
		return errors.New("the datacenter firewall is off, so a machine's network rules would do nothing: turn it on (Datacenter, Firewall, Options)")
	}
	return nil
}

// checkRequest refuses what this provider cannot start, before anything is made.
func (p *Provider) checkRequest(req fleet.CreateRequest) (shapeConfig, error) {
	cfg, err := parseShape(req.Shape.ProviderConfig, p.cfg)
	if err != nil {
		return cfg, err
	}
	img := req.Image
	if img.Format != domain.ImageQCOW2 || img.OS != req.Shape.OS {
		return cfg, fmt.Errorf("image %s@%s is %s for %s: a Proxmox location starts qcow2 images of the shape's OS (%s)", img.Name, img.Version, img.Format, img.OS, req.Shape.OS)
	}
	keys, err := p.keys()
	if err != nil {
		return cfg, err
	}
	if err := images.Verify(img, keys); err != nil {
		return cfg, err
	}
	if req.Shape.CPU < 1 || req.Shape.MemoryMB < 1 || req.Shape.DiskGB < 1 {
		return cfg, errors.New("the shape needs cpu, memory_mb and disk_gb")
	}
	return cfg, nil
}

// plan makes the VM's notes and network rules: where forecrewd is, as this host
// resolves it, and what the location allows.
func (p *Provider) plan(ctx context.Context, req fleet.CreateRequest, node, bridge string) (vmcommon.Meta, []vmcommon.Rule, error) {
	server, port, err := vmcommon.ResolveServer(ctx, p.resolver, req.Bootstrap.ServerURL)
	if err != nil {
		return vmcommon.Meta{}, nil, err
	}
	meta := vmcommon.NewMeta(req.MachineID)
	meta.Tags = withMachine(req.Tags, req.MachineID)
	meta.Allow, meta.Port, meta.Deadline, meta.Bridge = req.NetworkAllow, port, req.Deadline, bridge
	for _, s := range server {
		meta.Server = append(meta.Server, s.String())
	}
	rules, err := p.rules(ctx, node, meta)
	return meta, rules, err
}

func withMachine(tags map[string]string, id uuid.UUID) map[string]string {
	out := make(map[string]string, len(tags)+1)
	maps.Copy(out, tags)
	out[fleet.TagMachine] = id.String()
	return out
}

// rules are the outbound rules of a machine of meta on node: what the machine
// reaches, then never the cluster or the network of the other machines, whatever
// network_allow says, and last the internet. What is never opened is read from the
// cluster's API, not from the agent's own interfaces (the agent need not run on a
// node), and **a machine is not made or changed if it cannot be read**: the
// addresses of every node (so the API on 8006 and SSH), the API's own host, and
// the network of the machine's bridge, which holds the other machines. The other
// networks of the node are closed like the private ranges. The VM firewall's
// output policy is DROP, which is what closes IPv6, so the internet is an explicit
// IPv4 rule.
func (p *Provider) rules(ctx context.Context, node string, meta vmcommon.Meta) ([]vmcommon.Rule, error) {
	allow, err := fleetagent.ResolveAllow(ctx, p.resolver, meta.Allow)
	if err != nil {
		return nil, err
	}
	never, local, err := p.closed(ctx, node, meta.Bridge)
	if err != nil {
		return nil, fmt.Errorf("cannot tell what a machine must never reach, so none is made: %w", err)
	}
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
	return append(vmcommon.Rules(in), vmcommon.Rule{Allow: true, Dest: netip.MustParsePrefix("0.0.0.0/0")}), nil
}

// closed reads from the API what a machine on bridge of node must never reach
// (never) and what is closed unless network_allow opens it (local).
func (p *Provider) closed(ctx context.Context, node, bridge string) (never, local []netip.Prefix, err error) {
	if never, err = p.nodeAddrs(ctx); err != nil {
		return nil, nil, err
	}
	api, err := p.apiAddrs(ctx)
	if err != nil {
		return nil, nil, err
	}
	never = append(never, api...)
	var ifaces []struct {
		Iface string `json:"iface"`
		CIDR  string `json:"cidr"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/nodes/"+node+"/network", nil, &ifaces); err != nil {
		return nil, nil, fmt.Errorf("the network of node %s: %w", node, err)
	}
	found := false
	for _, i := range ifaces {
		pf, err := netip.ParsePrefix(i.CIDR)
		if err != nil || !pf.Addr().Is4() {
			continue // no IPv4 address
		}
		never = append(never, netip.PrefixFrom(pf.Addr(), 32))
		if i.Iface == bridge {
			never, found = append(never, pf.Masked()), true
		} else {
			local = append(local, pf.Masked())
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("bridge %s of node %s has no address in the node's network: give it one on the machines' network", bridge, node)
	}
	return never, local, nil
}

// nodeAddrs are the addresses of every node of the cluster.
func (p *Provider) nodeAddrs(ctx context.Context) ([]netip.Prefix, error) {
	var members []struct {
		Type string `json:"type"`
		IP   string `json:"ip"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/cluster/status", nil, &members); err != nil {
		return nil, fmt.Errorf("the cluster's nodes: %w", err)
	}
	var out []netip.Prefix
	for _, m := range members {
		if m.Type != "node" {
			continue
		}
		a, err := netip.ParseAddr(m.IP)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("a node of the cluster has no IPv4 address in /cluster/status (%q)", m.IP)
		}
		out = append(out, netip.PrefixFrom(a, 32))
	}
	if len(out) == 0 {
		return nil, errors.New("/cluster/status lists no node")
	}
	return out, nil
}

// apiAddrs are the addresses of the API's own host, which may be a name or an
// address of something else.
func (p *Provider) apiAddrs(ctx context.Context) ([]netip.Prefix, error) {
	u, err := url.Parse(p.cfg.APIURL)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("api_url has no host")
	}
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := fleetagent.Lookup4(lctx, p.resolver, u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("the API's host %s: %w", u.Hostname(), err)
	}
	out := make([]netip.Prefix, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, netip.PrefixFrom(a, 32))
	}
	return out, nil
}

// room answers ErrCapacity when the cluster holds as many Forecrew VMs as
// allowed or the node has no memory left for this one.
func (p *Provider) room(ctx context.Context, node string, shape domain.Shape) error {
	all, err := p.forecrewVMs(ctx)
	if err != nil {
		return err
	}
	if len(all) >= p.cfg.MaxMachines {
		return fleet.ErrCapacity
	}
	var st struct {
		Memory struct {
			Free int64 `json:"free"`
		} `json:"memory"`
	}
	if err := p.api.do(ctx, http.MethodGet, "/nodes/"+node+"/status", nil, &st); err != nil {
		return err
	}
	if st.Memory.Free>>20 < int64(shape.MemoryMB+memoryReserveMB) {
		return fleet.ErrCapacity
	}
	return nil
}

var extraNICRE = regexp.MustCompile(`^net[1-9][0-9]*$`)

// extraNICs lists the network devices of a configuration besides net0.
func extraNICs(c vmConfig) []string {
	var out []string
	for k := range c {
		if extraNICRE.MatchString(k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// imageTag is the tag a template carries when it was made from img.
func imageTag(img domain.Image) string {
	return imageTagPrefix + strings.TrimPrefix(img.Digest, "sha256:")[:32]
}

// checkTemplate makes sure the VM cloned is a template made from the signed
// image: the signature covers the image's digest, and an operator who imported
// that file tags the template with it. A shape cannot name another VM.
func (p *Provider) checkTemplate(ctx context.Context, cfg shapeConfig, img domain.Image) error {
	c, err := p.config(ctx, cfg.Node, cfg.Template)
	if notFound(err) {
		return fmt.Errorf("template %d does not exist on node %s", cfg.Template, cfg.Node)
	}
	if err != nil {
		return err
	}
	if c.str("template") != "1" {
		return fmt.Errorf("VM %d is not a template", cfg.Template)
	}
	if extra := extraNICs(c); len(extra) > 0 {
		return fmt.Errorf("template %d has network devices besides net0 (%s): a machine gets one, on the location's bridge; remove the others from the template", cfg.Template, strings.Join(extra, ", "))
	}
	for tag := range strings.FieldsFuncSeq(c.str("tags"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if tag == imageTag(img) {
			return nil
		}
	}
	return fmt.Errorf("template %d was not made from image %s@%s: tag it %s once you have imported that image's file, or point the shape at the template that was", cfg.Template, img.Name, img.Version, imageTag(img))
}

// clone makes the VM from the template, with its notes, so that the machine is
// recorded in the same call that makes it.
func (p *Provider) clone(ctx context.Context, cfg shapeConfig, req fleet.CreateRequest, meta vmcommon.Meta) (int, error) {
	name := namePrefix + strings.ReplaceAll(req.MachineID.String(), "-", "")[:12]
	for range maxAttempts {
		vmid, err := p.cloneOnce(ctx, cfg, name, meta)
		if err == nil {
			return vmid, nil
		}
		if !idTaken(err) {
			return 0, err
		}
	}
	return 0, errors.New("could not get a free VMID")
}

func (p *Provider) cloneOnce(ctx context.Context, cfg shapeConfig, name string, meta vmcommon.Meta) (int, error) {
	p.alloc.Lock()
	defer p.alloc.Unlock()
	var next string
	if err := p.api.do(ctx, http.MethodGet, "/cluster/nextid", nil, &next); err != nil {
		return 0, err
	}
	vmid, err := strconv.Atoi(next)
	if err != nil {
		return 0, fmt.Errorf("proxmox: the next VMID %q is not a number", next)
	}
	form := url.Values{"newid": {next}, "name": {name}, "description": {meta.Encode()}}
	if cfg.Storage != "" {
		form.Set("full", "1")
		form.Set("storage", cfg.Storage)
	} else {
		form.Set("full", "0")
	}
	if cfg.Pool != "" {
		form.Set("pool", cfg.Pool)
	}
	var upid string
	if err := p.api.do(ctx, http.MethodPost, vmPath(cfg.Node, cfg.Template)+"/clone", form, &upid); err != nil {
		return 0, err // nothing was made: the VMID is somebody else's if it was taken
	}
	tctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	if err := p.api.task(tctx, cfg.Node, upid, p.cfg.PollInterval); err != nil {
		// The VM may exist half made: it is ours, as the VMID was free and the
		// notes are ours.
		if derr := p.destroyVM(context.WithoutCancel(ctx), managed{vmid: vmid, node: cfg.Node, meta: meta}); derr != nil {
			p.log.Warn("clean up a failed clone", "machine_id", meta.MachineID, "error", derr)
		}
		return 0, err
	}
	return vmid, nil
}

// idTaken says that err is the API refusing a VMID that has been taken since it
// was handed out.
func idTaken(err error) bool {
	var e *APIError
	return errors.As(err, &e) && strings.Contains(e.Message, "already exists")
}

// setUp sizes the clone, closes its network, starts it and gives it its token.
func (p *Provider) setUp(ctx context.Context, m managed, cfg shapeConfig, req fleet.CreateRequest, rules []vmcommon.Rule, guestFile string) error {
	started := time.Now()
	c, err := p.config(ctx, m.node, m.vmid)
	if err != nil {
		return err
	}
	if extra := extraNICs(c); len(extra) > 0 {
		return fmt.Errorf("the clone has network devices besides net0 (%s)", strings.Join(extra, ", "))
	}
	if err := p.size(ctx, m, c, cfg, req.Shape); err != nil {
		return err
	}
	if err := p.api.do(ctx, http.MethodPut, vmPath(m.node, m.vmid)+"/config", p.settings(m, c, cfg, req.Shape), nil); err != nil {
		return err
	}
	if err := p.applyFirewall(ctx, m, rules); err != nil {
		return fmt.Errorf("network rules: %w", err)
	}
	tctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	if err := p.api.start(tctx, http.MethodPost, vmPath(m.node, m.vmid)+"/status/start", m.node, nil, p.cfg.PollInterval); err != nil {
		return err
	}
	budget := time.Duration(req.Shape.BootTimeoutSeconds)*time.Second - time.Since(started)
	if req.Shape.BootTimeoutSeconds == 0 {
		budget = domain.DefaultBootTimeoutSeconds * time.Second
	}
	return p.deliver(ctx, m, guestFile, req.Bootstrap, budget)
}

// settings are the VM's new configuration: the shape's size, the guest agent,
// the network device on the location's bridge with the firewall on, and the tags.
func (p *Provider) settings(m managed, c vmConfig, cfg shapeConfig, shape domain.Shape) url.Values {
	nic := "virtio"
	if first, _, _ := strings.Cut(c.str("net0"), ","); first != "" {
		nic, _, _ = strings.Cut(first, "=") // the model; the MAC address is the clone's own
	}
	net0 := nic + ",bridge=" + cfg.Bridge + ",firewall=1"
	if *cfg.VLAN > 0 {
		net0 += ",tag=" + strconv.Itoa(*cfg.VLAN)
	}
	return url.Values{
		"cores": {strconv.Itoa(shape.CPU)}, "sockets": {"1"}, "memory": {strconv.Itoa(shape.MemoryMB)}, "balloon": {"0"},
		"agent": {"1"}, "net0": {net0}, "onboot": {"0"}, "tags": {pveTags(m.meta.Tags)},
	}
}

// size grows the disk to the shape's size. A disk bigger than the shape is
// refused: the shape's size is its quota.
func (p *Provider) size(ctx context.Context, m managed, c vmConfig, cfg shapeConfig, shape domain.Shape) error {
	cur, ok := diskBytes(c.str(cfg.Disk))
	if !ok {
		return fmt.Errorf("the template has no disk %s with a size: set provider_config.disk to its disk", cfg.Disk)
	}
	want := int64(shape.DiskGB) << 30
	switch {
	case cur > want:
		return fmt.Errorf("the template's disk takes %d GB, more than the shape's %d GB", (cur+1<<30-1)>>30, shape.DiskGB)
	case cur == want:
		return nil
	}
	return p.api.do(ctx, http.MethodPut, vmPath(m.node, m.vmid)+"/resize", url.Values{"disk": {cfg.Disk}, "size": {strconv.Itoa(shape.DiskGB) + "G"}}, nil)
}

// diskBytes reads size= of a disk line such as local-lvm:vm-1-disk-0,size=32G.
func diskBytes(line string) (int64, bool) {
	for f := range strings.SplitSeq(line, ",") {
		v, ok := strings.CutPrefix(f, "size=")
		if !ok || v == "" {
			continue
		}
		unit := int64(1)
		switch v[len(v)-1] {
		case 'K':
			unit = 1 << 10
		case 'M':
			unit = 1 << 20
		case 'G':
			unit = 1 << 30
		case 'T':
			unit = 1 << 40
		}
		if unit > 1 {
			v = v[:len(v)-1]
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n * unit, true
	}
	return 0, false
}

// pveTags are the tags a person sees in the web UI: the machine, organization
// and job. Tags of Proxmox are lowercase words, so the full set is in the notes.
func pveTags(tags map[string]string) string {
	var out []string
	for key, prefix := range map[string]string{fleet.TagMachine: "forecrew-machine-", fleet.TagOrg: "forecrew-org-", fleet.TagJob: "forecrew-job-"} {
		if v := tagWord(tags[key]); v != "" {
			out = append(out, prefix+v)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ";")
}

func tagWord(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bootstrapFile is where the guest of an OS reads its settings.
func bootstrapFile(os domain.MachineOS) (string, error) {
	switch os {
	case domain.OSLinux:
		return linuxEnvFile, nil
	case domain.OSWindows:
		return windowsEnvFile, nil
	}
	return "", fmt.Errorf("a Proxmox location runs Linux and Windows guests, not %s", os)
}

// deliver writes the bootstrap settings into the running guest through the QEMU
// guest agent, once it answers: the token goes from here to the guest's memory
// (Linux /run) or its own disk (Windows), and never to a file of this host, the
// VM's configuration or a cloud-init drive.
func (p *Provider) deliver(ctx context.Context, m managed, guestFile string, b *fleet.Bootstrap, budget time.Duration) error {
	if !guestValueRE.MatchString(b.ServerURL) || !guestValueRE.MatchString(b.Token) {
		return errors.New("the server URL or the token has characters the guest does not accept")
	}
	content := "FORECREW_SERVER_URL=" + b.ServerURL + "\nFORECREW_BOOTSTRAP_TOKEN=" + b.Token + "\n"
	ctx, cancel := context.WithTimeout(ctx, max(budget, time.Second))
	defer cancel()
	for {
		err := p.api.do(ctx, http.MethodPost, vmPath(m.node, m.vmid)+"/agent/ping", nil, nil)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest agent of VM %d did not answer in time (is qemu-guest-agent in the image, and the agent option on?): %w", m.vmid, err)
		case <-time.After(p.cfg.PollInterval):
		}
	}
	err := p.api.do(ctx, http.MethodPost, vmPath(m.node, m.vmid)+"/agent/file-write", url.Values{"file": {guestFile}, "content": {content}}, nil)
	if err != nil {
		return fmt.Errorf("deliver the bootstrap settings: %s", strings.ReplaceAll(err.Error(), b.Token, "[redacted]"))
	}
	return nil
}

// applyFirewall turns the VM's firewall on, closes what comes in, and sets its
// outbound rules. The API puts a new rule first, so rules are added last to
// first; the order is read back, and set again the other way if this cluster
// puts them last.
func (p *Provider) applyFirewall(ctx context.Context, m managed, rules []vmcommon.Rule) error {
	base := vmPath(m.node, m.vmid) + "/firewall"
	opts := url.Values{"enable": {"1"}, "policy_in": {"DROP"}, "policy_out": {"DROP"}, "dhcp": {"1"}, "macfilter": {"1"}, "radv": {"0"}}
	if err := p.api.do(ctx, http.MethodPut, base+"/options", opts, nil); err != nil {
		return err
	}
	want := make([]ruleKey, len(rules))
	for i, r := range rules {
		want[i] = keyOf(r)
	}
	for attempt := range 2 {
		if err := p.clearRules(ctx, base); err != nil {
			return err
		}
		for i := range rules {
			r := rules[len(rules)-1-i]
			if attempt == 1 {
				r = rules[i]
			}
			if err := p.api.do(ctx, http.MethodPost, base+"/rules", ruleForm(r), nil); err != nil {
				return err
			}
		}
		have, err := p.readRules(ctx, base)
		if err != nil {
			return err
		}
		if slices.Equal(have, want) {
			return nil
		}
		if attempt == 1 || !isReverse(have, want) {
			return errors.New("the cluster did not keep the rules in the order they were given")
		}
	}
	return nil
}

// isReverse says that a is b read backwards.
func isReverse(a, b []ruleKey) bool {
	r := slices.Clone(b)
	slices.Reverse(r)
	return slices.Equal(a, r)
}

type ruleKey struct{ Action, Dest, Proto, DPort string }

func keyOf(r vmcommon.Rule) ruleKey {
	k := ruleKey{Action: "DROP", Dest: r.Dest.String(), Proto: r.Proto}
	if r.Allow {
		k.Action = "ACCEPT"
	}
	if r.Port != 0 {
		k.DPort = strconv.Itoa(r.Port)
	}
	return k
}

func ruleForm(r vmcommon.Rule) url.Values {
	k := keyOf(r)
	f := url.Values{"type": {"out"}, "action": {k.Action}, "dest": {k.Dest}, "enable": {"1"}, "comment": {"forecrew"}}
	if k.Proto != "" {
		f.Set("proto", k.Proto)
	}
	if k.DPort != "" {
		f.Set("dport", k.DPort)
	}
	return f
}

type fwRule struct {
	Pos    int    `json:"pos"`
	Type   string `json:"type"`
	Action string `json:"action"`
	Dest   string `json:"dest"`
	Proto  string `json:"proto"`
	DPort  string `json:"dport"`
}

func (p *Provider) listRules(ctx context.Context, base string) ([]fwRule, error) {
	var rs []fwRule
	err := p.api.do(ctx, http.MethodGet, base+"/rules", nil, &rs)
	return rs, err
}

func (p *Provider) readRules(ctx context.Context, base string) ([]ruleKey, error) {
	rs, err := p.listRules(ctx, base)
	if err != nil {
		return nil, err
	}
	out := make([]ruleKey, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleKey{Action: r.Action, Dest: r.Dest, Proto: r.Proto, DPort: r.DPort})
	}
	return out, nil
}

func (p *Provider) clearRules(ctx context.Context, base string) error {
	rs, err := p.listRules(ctx, base)
	if err != nil {
		return err
	}
	for range rs { // the first one each time: positions shift as rules go
		if err := p.api.do(ctx, http.MethodDelete, base+"/rules/0", nil, nil); err != nil {
			return err
		}
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
	if !slices.Equal(old.Allow, req.NetworkAllow) {
		rules, err := p.rules(ctx, m.node, m.meta)
		if err != nil {
			return err
		}
		if err := p.applyFirewall(ctx, *m, rules); err != nil {
			return fmt.Errorf("network rules: %w", err)
		}
	}
	form := url.Values{"description": {m.meta.Encode()}, "tags": {pveTags(m.meta.Tags)}}
	if err := p.api.do(ctx, http.MethodPut, vmPath(m.node, m.vmid)+"/config", form, nil); err != nil {
		return err
	}
	p.arm(*m)
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

// destroyMachine destroys every VM of a machine ID.
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

// destroyVM stops the VM and removes it with its disks, firewall and every
// mention of it. Each step is safe to repeat.
func (p *Provider) destroyVM(ctx context.Context, m managed) error {
	p.deadlines.Stop(m.meta.MachineID)
	p.mu.Lock()
	delete(p.armed, m.meta.MachineID)
	p.mu.Unlock()
	tctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	if err := p.stop(tctx, m); err != nil {
		return err
	}
	err := p.api.start(tctx, http.MethodDelete, vmPath(m.node, m.vmid), m.node, url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}, p.cfg.PollInterval)
	if notFound(err) {
		return nil
	}
	return err
}

// stop powers the VM off at once, which is what a wall-time kill is.
func (p *Provider) stop(ctx context.Context, m managed) error {
	err := p.api.start(ctx, http.MethodPost, vmPath(m.node, m.vmid)+"/status/stop", m.node, nil, p.cfg.PollInterval)
	if notFound(err) {
		return nil
	}
	return err
}

// arm sets the wall-time kill of m, which powers the VM off whatever forecrewd
// says or can say. The rest goes when forecrewd destroys it.
func (p *Provider) arm(m managed) {
	id := m.meta.MachineID
	p.mu.Lock()
	defer p.mu.Unlock()
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
		ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
		defer cancel()
		if err := p.stop(ctx, m); err != nil {
			p.log.Warn("power off machine", "machine_id", id, "error", err)
		}
	})
}

// List implements fleet.Provider. It also arms the deadlines of the machines it
// finds, which is how a restart of the agent gets them back.
func (p *Provider) List(ctx context.Context) ([]fleet.Instance, error) {
	all, err := p.forecrewVMs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]fleet.Instance, 0, len(all))
	for _, m := range all {
		p.arm(m)
		out = append(out, fleet.Instance{ExternalID: m.external(), MachineID: m.meta.ID(), Running: m.status == "running", Tags: m.meta.Tags})
	}
	return out, nil
}

package domain

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"
)

// MachineOS is the operating system of a shape or image.
type MachineOS string

// Machine operating systems.
const (
	OSLinux   MachineOS = "linux"
	OSWindows MachineOS = "windows"
	OSMacOS   MachineOS = "macos"
)

// MachineOSes returns every machine operating system.
func MachineOSes() []MachineOS { return []MachineOS{OSLinux, OSWindows, OSMacOS} }

// MachineArch is the CPU architecture of a shape or image, named like
// Go's GOARCH.
type MachineArch string

// Machine architectures.
const (
	ArchAMD64 MachineArch = "amd64"
	ArchARM64 MachineArch = "arm64"
)

// MachineArches returns every machine architecture.
func MachineArches() []MachineArch { return []MachineArch{ArchAMD64, ArchARM64} }

// OSLabels returns the labels of an operating system, a GOOS value or a
// MachineOS. macOS gets both darwin (conventions.md) and macos (the name used
// by repo profiles), so jobs written either way match.
func OSLabels(os string) []string {
	if os == "darwin" || os == string(OSMacOS) {
		return []string{"darwin", "macos"}
	}
	return []string{os}
}

// Ownership says whose machines they are (fleet.md, Ownership).
type Ownership string

// Ownerships.
const (
	OwnershipOwn    Ownership = "own"    // of an organization's own provider
	OwnershipHosted Ownership = "hosted" // of an instance-wide provider
)

// Ownerships returns every ownership.
func Ownerships() []Ownership { return []Ownership{OwnershipOwn, OwnershipHosted} }

// Resources is what a job needs of a machine (jobs.resources), set by its
// role pack or its environment. Zero fields ask for nothing.
type Resources struct {
	MinCPU      int `json:"min_cpu,omitempty" yaml:"min_cpu"`
	MinMemoryMB int `json:"min_memory_mb,omitempty" yaml:"min_memory_mb"`
	MinDiskGB   int `json:"min_disk_gb,omitempty" yaml:"min_disk_gb"`
	GPU         int `json:"gpu,omitempty" yaml:"gpu"`
}

// Validate refuses negative values, wrapping ErrInvalidSpec.
func (r Resources) Validate() error {
	var p problems
	r.check(&p)
	return p.err("resources")
}

func (r Resources) check(p *problems) {
	if r.MinCPU < 0 || r.MinMemoryMB < 0 || r.MinDiskGB < 0 || r.GPU < 0 {
		p.addf("resources must not be negative")
	}
}

// Shape is a kind of machine a capacity provider can start, shown to people
// as a machine type (fleet.md). AccountID zero is an instance-wide shape.
type Shape struct {
	ID         uuid.UUID   `json:"id"`
	AccountID  uuid.UUID   `json:"account_id,omitzero"`
	ProviderID uuid.UUID   `json:"provider_id"`
	Name       string      `json:"name"` // unique per organization, and among instance-wide shapes
	OS         MachineOS   `json:"os"`
	Arch       MachineArch `json:"arch"`
	CPU        int         `json:"cpu"`
	MemoryMB   int         `json:"memory_mb"`
	DiskGB     int         `json:"disk_gb"`
	GPU        int         `json:"gpu"`
	// Pool is the pool of the workers on its machines, and so the trust
	// zone. Empty is stored as PoolBuild.
	Pool   string   `json:"pool"`
	Labels []string `json:"labels"`
	// ProviderConfig is what the provider needs to start it (instance
	// type, template, region, subnet). Never secrets: a secret-looking key
	// holds a reference (ValidSecretRef).
	ProviderConfig map[string]any `json:"provider_config"`
	// Image is an image name, or name@version to pin one (ParseImageRef).
	Image          string  `json:"image"`
	PricePerMinute float64 `json:"price_per_minute"` // in config fleet.currency; 0 is free
	// MonthlyBudget stops new machines of this shape; nil means no limit.
	MonthlyBudget *float64 `json:"monthly_budget,omitempty"`
	WarmMin       int      `json:"warm_min"` // machines up with no job, ready or Warm
	Max           int      `json:"max"`      // machines at once
	// IdleSeconds is how long a ready machine above WarmMin waits for a
	// job before it is destroyed.
	IdleSeconds int `json:"idle_seconds"`
	// BootTimeoutSeconds is how long a machine may take from booting until
	// its worker calls, and how long a bootstrap token lasts once bound.
	BootTimeoutSeconds int `json:"boot_timeout_seconds"`
	// MaxJobs is how many jobs a machine takes in its life, usually 1.
	MaxJobs   int       `json:"max_jobs"`
	Note      string    `json:"note"` // what a person should know: slow boot, minimum allocation
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultBootTimeoutSeconds is a shape's boot timeout when none is given.
const DefaultBootTimeoutSeconds = 300

const maxShapeNote = 500

// WithDefaults fills the optional fields that have a default.
func (s Shape) WithDefaults() Shape {
	s.Pool = OrDefaultPool(s.Pool)
	if s.BootTimeoutSeconds == 0 {
		s.BootTimeoutSeconds = DefaultBootTimeoutSeconds
	}
	if s.Max == 0 {
		s.Max = 1
	}
	if s.MaxJobs == 0 {
		s.MaxJobs = 1
	}
	return s
}

// Ownership returns hosted for an instance-wide shape, otherwise own.
func (s Shape) Ownership() Ownership { return ownershipOf(s.AccountID) }

func ownershipOf(accountID uuid.UUID) Ownership {
	if accountID == uuid.Nil() {
		return OwnershipHosted
	}
	return OwnershipOwn
}

// OfferedLabels returns the labels a worker on the shape's machines has:
// Labels, the OS labels and the architecture, normalized.
func (s Shape) OfferedLabels() []string {
	out := append(slices.Clone(s.Labels), OSLabels(string(s.OS))...)
	// Machines run the worker of their image, which keeps its keys away
	// from organizations' connections.
	return NormalizeLabels(append(out, string(s.Arch), OrganizationModelLabel, ModelCheckLabel))
}

// Validate checks the shape on its own, reporting every problem at once and
// wrapping ErrInvalidRecord. WithDefaults comes first. CheckProvider and
// CheckImage check it against what it runs on.
func (s Shape) Validate() error {
	var p problems
	if !ValidName(s.Name) {
		p.addf("name %q must match %s", s.Name, nameRE)
	}
	if s.ProviderID == uuid.Nil() {
		p.addf("provider_id must be set")
	}
	checkEnum(&p, "os", s.OS, MachineOSes())
	checkEnum(&p, "arch", s.Arch, MachineArches())
	if s.CPU < 1 || s.MemoryMB < 1 || s.DiskGB < 1 {
		p.addf("cpu, memory_mb and disk_gb must be positive")
	}
	if s.GPU < 0 {
		p.addf("gpu must not be negative")
	}
	if !ValidName(s.Pool) || s.Pool == PoolActions {
		p.addf("pool %q must be a pool with workers, not %s", s.Pool, PoolActions)
	}
	if _, _, ok := ParseImageRef(s.Image); !ok {
		p.addf("image %q must be a name or name@version", s.Image)
	}
	checkAmount(&p, "price_per_minute", s.PricePerMinute)
	if s.MonthlyBudget != nil {
		checkAmount(&p, "monthly_budget", *s.MonthlyBudget)
	}
	if s.WarmMin < 0 || s.Max < 1 || s.WarmMin > s.Max {
		p.addf("warm_min %d and max %d need 0 <= warm_min <= max and max >= 1", s.WarmMin, s.Max)
	}
	if s.IdleSeconds < 0 || s.BootTimeoutSeconds < 1 || s.MaxJobs < 1 {
		p.addf("idle_seconds must not be negative; boot_timeout_seconds and max_jobs must be positive")
	}
	p.checkNoSecrets("provider_config", s.ProviderConfig)
	if len(s.Note) > maxShapeNote {
		p.addf("note must be at most %d bytes", maxShapeNote)
	}
	p.checkNoSecrets("note", s.Note)
	return p.errOf(ErrInvalidRecord, "shape "+s.Name)
}

// CheckProvider checks the shape against its provider: same organization,
// and within the capabilities the provider reported. It wraps
// ErrInvalidRecord.
func (s Shape) CheckProvider(pr CapacityProvider) error {
	var p problems
	if s.ProviderID != pr.ID {
		p.addf("provider_id is not provider %s", pr.Name)
	}
	if s.AccountID != pr.AccountID {
		p.addf("the shape and provider %s must have the same organization", pr.Name)
	}
	c := pr.Capabilities
	if !slices.Contains(c.OS, s.OS) {
		p.addf("provider %s does not run os %s", pr.Name, s.OS)
	}
	if !slices.Contains(c.Arch, s.Arch) {
		p.addf("provider %s does not run arch %s", pr.Name, s.Arch)
	}
	if s.CPU > c.MaxCPU || s.MemoryMB > c.MaxMemoryMB || s.DiskGB > c.MaxDiskGB || s.GPU > c.MaxGPU {
		p.addf("%d cpu, %d MB, %d GB disk and %d gpu exceed what provider %s can give a machine", s.CPU, s.MemoryMB, s.DiskGB, s.GPU, pr.Name)
	}
	if m := pr.MaxMachines(); m > 0 && s.WarmMin > m {
		p.addf("warm_min %d exceeds the %d machines of provider %s", s.WarmMin, m, pr.Name)
	}
	return p.errOf(ErrInvalidRecord, "shape "+s.Name)
}

// CheckImage checks that the provider can start img for the shape: the
// reference, OS, architecture and format fit. It wraps ErrInvalidRecord.
func (s Shape) CheckImage(pr CapacityProvider, img Image) error {
	var p problems
	if name, version, ok := ParseImageRef(s.Image); !ok || name != img.Name || (version != "" && version != img.Version) {
		p.addf("image %q is not %s@%s", s.Image, img.Name, img.Version)
	}
	if s.OS != img.OS || s.Arch != img.Arch {
		p.addf("image %s is for %s/%s, not %s/%s", img.Name, img.OS, img.Arch, s.OS, s.Arch)
	}
	if !slices.Contains(pr.Capabilities.ImageFormats, img.Format) {
		p.addf("provider %s does not start %s images", pr.Name, img.Format)
	}
	return p.errOf(ErrInvalidRecord, "shape "+s.Name)
}

// ShapeDemand is what a job asks of a shape.
type ShapeDemand struct {
	Pool      string
	Labels    []string
	Resources Resources
}

// ShapeDemand returns what j asks of a shape.
func (j Job) ShapeDemand() ShapeDemand {
	return ShapeDemand{Pool: OrDefaultPool(j.Pool), Labels: j.RequiredLabels(), Resources: j.Resources}
}

// Fits reports whether the shape is big enough for r.
func (s Shape) Fits(r Resources) bool {
	return s.CPU >= r.MinCPU && s.MemoryMB >= r.MinMemoryMB && s.DiskGB >= r.MinDiskGB && s.GPU >= r.GPU
}

// Serves reports whether the shape can run what d asks: enabled, of d's
// pool, with every label, and big enough.
func (s Shape) Serves(d ShapeDemand) bool {
	return s.Enabled && OrDefaultPool(s.Pool) == OrDefaultPool(d.Pool) && MatchLabels(d.Labels, s.OfferedLabels()) && s.Fits(d.Resources)
}

// SelectShapes returns the shapes policy's organization may use for d, best
// first (fleet.md, Picking a shape). The first with room gets the machine:
// whether a provider is online or a limit is reached is the caller's to
// check. Under own_first own shapes come before hosted ones; otherwise, and
// within each, the cheapest comes first, then the smallest, then by name.
func SelectShapes(shapes []Shape, policy FleetPolicy, d ShapeDemand) []Shape {
	out := make([]Shape, 0, len(shapes))
	for _, s := range shapes {
		if policy.Allows(s) && s.Serves(d) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, compareShapes)
	return out
}

func compareShapes(a, b Shape) int {
	if a.Ownership() != b.Ownership() {
		if a.Ownership() == OwnershipOwn {
			return -1
		}
		return 1
	}
	if a.PricePerMinute != b.PricePerMinute {
		if a.PricePerMinute < b.PricePerMinute {
			return -1
		}
		return 1
	}
	sizes := [][2]int{{a.GPU, b.GPU}, {a.CPU, b.CPU}, {a.MemoryMB, b.MemoryMB}, {a.DiskGB, b.DiskGB}}
	for _, v := range sizes {
		if v[0] != v[1] {
			return v[0] - v[1]
		}
	}
	return strings.Compare(a.Name, b.Name)
}

// ProviderKind is what a capacity provider runs on.
type ProviderKind string

// Capacity provider kinds.
const (
	ProviderMicroVM ProviderKind = "microvm" // Firecracker or Cloud Hypervisor on a KVM host
	ProviderProxmox ProviderKind = "proxmox"
	ProviderHyperV  ProviderKind = "hyperv"
	ProviderTart    ProviderKind = "tart"
	ProviderAWS     ProviderKind = "aws"
	ProviderFake    ProviderKind = "fake" // internal/testkit; nothing in production serves it
)

// ProviderKinds returns every capacity provider kind.
func ProviderKinds() []ProviderKind {
	return []ProviderKind{ProviderMicroVM, ProviderProxmox, ProviderHyperV, ProviderTart, ProviderAWS, ProviderFake}
}

// ProviderMode is how forecrewd reaches a capacity provider.
type ProviderMode string

// Capacity provider modes.
const (
	ProviderModeAgent  ProviderMode = "agent"  // a fleet agent dials out (fleet-agent-api.md)
	ProviderModeDirect ProviderMode = "direct" // forecrewd calls fleet.Provider itself
)

// ProviderModes returns every capacity provider mode.
func ProviderModes() []ProviderMode { return []ProviderMode{ProviderModeAgent, ProviderModeDirect} }

// ProviderStatus is the state an admin or the join flow put a provider in.
// Being online is separate: CapacityProvider.Online.
type ProviderStatus string

// Capacity provider statuses.
const (
	ProviderPending ProviderStatus = "pending" // waiting for its first token or confirmation
	ProviderActive  ProviderStatus = "active"
	ProviderPaused  ProviderStatus = "paused"  // finishes running machines, starts no new ones
	ProviderRevoked ProviderStatus = "revoked" // its token no longer works
)

// ProviderStatuses returns every capacity provider status.
func ProviderStatuses() []ProviderStatus {
	return []ProviderStatus{ProviderPending, ProviderActive, ProviderPaused, ProviderRevoked}
}

// ProviderLimits caps what a provider may use; zero values mean no limit.
type ProviderLimits struct {
	MaxMachines   int      `json:"max_machines,omitempty"`
	MonthlyBudget *float64 `json:"monthly_budget,omitempty"`
}

// ProviderCheck is one health check a provider reports, shown on the
// location page.
type ProviderCheck struct {
	Name   string `json:"name"`             // e.g. kvm, disk, software
	Status string `json:"status"`           // ok, warn or fail
	Detail string `json:"detail,omitempty"` // e.g. "412 GB free"
}

// ProviderCapabilities is what a provider reports it can do, and the facts
// a person confirms before connecting it. The limits are per machine, except
// MaxMachines; zero MaxMachines means no limit of its own.
type ProviderCapabilities struct {
	OS           []MachineOS     `json:"os,omitempty"`
	Arch         []MachineArch   `json:"arch,omitempty"`
	MaxCPU       int             `json:"max_cpu,omitempty"`
	MaxMemoryMB  int             `json:"max_memory_mb,omitempty"`
	MaxDiskGB    int             `json:"max_disk_gb,omitempty"`
	MaxGPU       int             `json:"max_gpu,omitempty"`
	MaxMachines  int             `json:"max_machines,omitempty"`
	ImageFormats []ImageFormat   `json:"image_formats,omitempty"`
	Checks       []ProviderCheck `json:"checks,omitempty"`
	Host         string          `json:"host,omitempty"`          // host name
	CloudAccount string          `json:"cloud_account,omitempty"` // e.g. the AWS account ID
	Region       string          `json:"region,omitempty"`
	AgentVersion string          `json:"agent_version,omitempty"`
}

// Validate checks the values, wrapping ErrInvalidRecord.
func (c ProviderCapabilities) Validate() error {
	var p problems
	c.check(&p)
	return p.errOf(ErrInvalidRecord, "capabilities")
}

func (c ProviderCapabilities) check(p *problems) {
	for _, v := range c.OS {
		checkEnum(p, "capabilities.os", v, MachineOSes())
	}
	for _, v := range c.Arch {
		checkEnum(p, "capabilities.arch", v, MachineArches())
	}
	for _, v := range c.ImageFormats {
		checkEnum(p, "capabilities.image_formats", v, ImageFormats())
	}
	if c.MaxCPU < 0 || c.MaxMemoryMB < 0 || c.MaxDiskGB < 0 || c.MaxGPU < 0 || c.MaxMachines < 0 {
		p.addf("capabilities must not be negative")
	}
	for _, k := range c.Checks {
		if k.Name == "" || !slices.Contains([]string{"ok", "warn", "fail"}, k.Status) {
			p.addf("capabilities.checks: %q needs a name and a status of ok, warn or fail", k.Name)
		}
	}
}

// CapacityProvider is a place machines come from, shown to people as a
// location (fleet.md). AccountID zero is an instance-wide provider.
type CapacityProvider struct {
	ID        uuid.UUID      `json:"id"`
	AccountID uuid.UUID      `json:"account_id,omitzero"`
	Name      string         `json:"name"` // unique per organization, and among instance-wide providers
	Kind      ProviderKind   `json:"kind"`
	Mode      ProviderMode   `json:"mode"`
	Status    ProviderStatus `json:"status"`
	// TokenHash is the SHA-256 of the provider token; agent mode only.
	TokenHash    []byte               `json:"-"`
	LastSeenAt   time.Time            `json:"last_seen_at,omitzero"`
	Capabilities ProviderCapabilities `json:"capabilities"`
	Limits       ProviderLimits       `json:"limits"`
	// NetworkAllow lists hosts or CIDRs machines here may reach beyond the
	// default of the internet only.
	NetworkAllow []string          `json:"network_allow"`
	Tags         map[string]string `json:"tags"` // copied to everything the provider creates
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Ownership returns hosted for an instance-wide provider, otherwise own.
func (c CapacityProvider) Ownership() Ownership { return ownershipOf(c.AccountID) }

// Online reports whether the provider was seen within OnlineWindow of now.
func (c CapacityProvider) Online(now time.Time) bool {
	return c.Status != ProviderRevoked && !c.LastSeenAt.IsZero() && now.Sub(c.LastSeenAt) <= OnlineWindow
}

// CanStart reports whether new machines may start here at now.
func (c CapacityProvider) CanStart(now time.Time) bool {
	return c.Status == ProviderActive && c.Online(now)
}

// MaxMachines returns the machines the provider may run at once, the lower
// of its limit and what it reported; zero means no limit.
func (c CapacityProvider) MaxMachines() int {
	a, b := c.Limits.MaxMachines, c.Capabilities.MaxMachines
	if a == 0 || b == 0 {
		return max(a, b)
	}
	return min(a, b)
}

// Validate checks the provider, reporting every problem at once and
// wrapping ErrInvalidRecord.
func (c CapacityProvider) Validate() error {
	var p problems
	if !ValidName(c.Name) {
		p.addf("name %q must match %s", c.Name, nameRE)
	}
	checkEnum(&p, "kind", c.Kind, ProviderKinds())
	checkEnum(&p, "mode", c.Mode, ProviderModes())
	checkEnum(&p, "status", c.Status, ProviderStatuses())
	if len(c.TokenHash) != 0 && len(c.TokenHash) != 32 {
		p.addf("token hash must be a SHA-256")
	}
	if c.Mode == ProviderModeDirect && len(c.TokenHash) != 0 {
		p.addf("a direct provider has no token")
	}
	if c.Mode == ProviderModeAgent && c.Status == ProviderActive && len(c.TokenHash) == 0 {
		p.addf("an active agent provider needs a token")
	}
	c.Capabilities.check(&p)
	if c.Limits.MaxMachines < 0 {
		p.addf("limits.max_machines must not be negative")
	}
	if c.Limits.MonthlyBudget != nil {
		checkAmount(&p, "limits.monthly_budget", *c.Limits.MonthlyBudget)
	}
	checkHosts(&p, "network_allow", c.NetworkAllow)
	for _, k := range sortedKeys(c.Tags) {
		if k == "" {
			p.addf("tags: empty name")
		}
		p.checkNoSecrets("tags."+k, c.Tags[k])
	}
	return p.errOf(ErrInvalidRecord, "provider "+c.Name)
}

// Join codes last an hour, or a week for a cloud kit another team runs.
const (
	JoinCodeTTL      = time.Hour
	JoinCodeCloudTTL = 7 * 24 * time.Hour
)

// ProviderJoinCode lets a fleet agent connect without a token being shown
// to anyone (fleet-agent-api.md, Joining). It can do nothing alone: the first
// agent claims it and reports its facts, a person confirms them, and the
// agent then gets its provider token. Only hashes are stored.
type ProviderJoinCode struct {
	ID        uuid.UUID            `json:"id"`
	AccountID uuid.UUID            `json:"account_id,omitzero"` // zero: for an instance-wide provider
	CodeHash  []byte               `json:"-"`                   // SHA-256 of the code
	ClaimHash []byte               `json:"-"`                   // SHA-256 of the claim of the agent that used it first
	Facts     ProviderCapabilities `json:"facts"`               // what the agent reported
	Kind      ProviderKind         `json:"kind,omitempty"`      // what the agent detected
	// ProviderID is set when a person confirms the plan.
	ProviderID uuid.UUID `json:"provider_id,omitzero"`
	ExpiresAt  time.Time `json:"expires_at"`
	UsedAt     time.Time `json:"used_at,omitzero"` // the token was handed over
	CreatedAt  time.Time `json:"created_at"`
}

// Usable reports whether the code still works at now.
func (c ProviderJoinCode) Usable(now time.Time) bool {
	return c.UsedAt.IsZero() && now.Before(c.ExpiresAt)
}

// MachineState is the life of a machine.
type MachineState string

// Machine states. Only forecrewd sets ready, busy and draining; a fleet
// agent reports booting, destroyed and failed (fleet-agent-api.md).
const (
	MachineRequested MachineState = "requested" // a row exists; the provider may not know it yet
	MachineBooting   MachineState = "booting"   // the provider started it; a warm machine waits here
	MachineReady     MachineState = "ready"     // it has a worker; no job yet
	MachineBusy      MachineState = "busy"      // it leased a job
	MachineDraining  MachineState = "draining"  // takes no more jobs and is being destroyed
	MachineDestroyed MachineState = "destroyed" // gone
	MachineFailed    MachineState = "failed"    // broke; it may still exist at the provider
)

// MachineStates returns every machine state.
func MachineStates() []MachineState {
	return []MachineState{MachineRequested, MachineBooting, MachineReady, MachineBusy, MachineDraining, MachineDestroyed, MachineFailed}
}

var machineTransitions = transitions[MachineState]{
	MachineRequested: {MachineBooting, MachineDestroyed, MachineFailed},
	MachineBooting:   {MachineReady, MachineDraining, MachineFailed},
	MachineReady:     {MachineBusy, MachineDraining, MachineFailed},
	MachineBusy:      {MachineDraining, MachineFailed}, // never back to ready: machines are not reused
	MachineDraining:  {MachineDestroyed, MachineFailed},
	MachineFailed:    {MachineDestroyed},
}

// CanTransition reports whether a machine may move from s to to.
func (s MachineState) CanTransition(to MachineState) bool { return machineTransitions.can(s, to) }

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s MachineState) Transition(to MachineState) (MachineState, error) {
	return machineTransitions.apply("machine", s, to)
}

// Counts reports whether a machine in s counts against limits: everything
// that may still exist and was not given up on.
func (s MachineState) Counts() bool { return s != MachineDestroyed && s != MachineFailed }

// Machine is one machine of a shape, started for jobs and thrown away.
// Its worker has Ephemeral true.
type Machine struct {
	ID uuid.UUID `json:"id"`
	// AccountID is zero only on an unassigned warm machine of an
	// instance-wide shape. Once set it never changes.
	AccountID  uuid.UUID    `json:"account_id,omitzero"`
	ProviderID uuid.UUID    `json:"provider_id"`
	ShapeID    uuid.UUID    `json:"shape_id"`
	ExternalID string       `json:"external_id,omitempty"` // the provider's ID, once started
	State      MachineState `json:"state"`
	WorkerID   uuid.UUID    `json:"worker_id,omitzero"`
	JobID      uuid.UUID    `json:"job_id,omitzero"` // the job it last leased
	// JobsTaken counts the jobs it leased in its life, against the shape's
	// MaxJobs.
	JobsTaken int    `json:"jobs_taken"`
	Error     string `json:"error,omitempty"`
	// AssignedAt is when it got its organization, where metering starts.
	AssignedAt time.Time `json:"assigned_at,omitzero"`
	// BootingAt is when it entered booting, where the boot timeout starts.
	BootingAt time.Time `json:"booting_at,omitzero"`
	// ReadyAt is when its worker first called: it reached ready, or it is
	// warm, still booting and waiting for an organization.
	ReadyAt time.Time `json:"ready_at,omitzero"`
	// DeadlineAt is when it is destroyed whatever it is doing; zero while
	// it has no job.
	DeadlineAt  time.Time `json:"deadline_at,omitzero"`
	DestroyedAt time.Time `json:"destroyed_at,omitzero"`
	// FailedAt is when it failed, once; zero if it never did, and on rows
	// that failed before it was kept.
	FailedAt  time.Time `json:"failed_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CanTakeJob reports whether the machine may lease another job of shape s.
// It is an early look: store.MachineRepo.TakeJob is what enforces it.
func (m Machine) CanTakeJob(s Shape) bool { return m.JobsTaken < s.MaxJobs }

// Warm reports whether the machine is up and waiting for an organization.
func (m Machine) Warm() bool {
	return m.State == MachineBooting && m.AccountID == uuid.Nil() && !m.ReadyAt.IsZero()
}

// BootExpired reports whether the machine used up shape s's boot timeout at
// now. The time counts from BootingAt, so a machine still requested, with its
// provider offline perhaps, does not time out, and a warm one is up.
func (m Machine) BootExpired(s Shape, now time.Time) bool {
	return m.State == MachineBooting && m.ReadyAt.IsZero() && !m.BootingAt.IsZero() &&
		!now.Before(m.BootingAt.Add(time.Duration(s.BootTimeoutSeconds)*time.Second))
}

// Validate checks the state and that a worker or job comes with an
// organization, wrapping ErrInvalidRecord.
func (m Machine) Validate() error {
	var p problems
	checkEnum(&p, "state", m.State, MachineStates())
	if m.ProviderID == uuid.Nil() || m.ShapeID == uuid.Nil() {
		p.addf("provider_id and shape_id must be set")
	}
	if m.AccountID == uuid.Nil() && (m.WorkerID != uuid.Nil() || m.JobID != uuid.Nil()) {
		p.addf("an unassigned machine has no worker or job")
	}
	if m.JobsTaken < 0 {
		p.addf("jobs_taken must not be negative")
	}
	if (m.State == MachineDestroyed) != !m.DestroyedAt.IsZero() {
		p.addf("destroyed_at is set exactly when the machine is destroyed")
	}
	return p.errOf(ErrInvalidRecord, "machine")
}

// BootstrapToken lets the worker on a machine connect once, for a worker
// token (worker-api.md, POST /worker/v1/bootstrap). It is bound to one
// machine, and to exactly its organization with an expiry, counted from
// assignment. Only its hash is stored. An unassigned token has neither: it
// can only be told to wait.
type BootstrapToken struct {
	MachineID uuid.UUID `json:"machine_id"`
	AccountID uuid.UUID `json:"account_id,omitzero"`
	TokenHash []byte    `json:"-"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	UsedAt    time.Time `json:"used_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
}

// Assigned reports whether the token has its organization.
func (t BootstrapToken) Assigned() bool { return t.AccountID != uuid.Nil() }

// Usable reports whether the token gets a worker token at now: assigned,
// not used and not expired.
func (t BootstrapToken) Usable(now time.Time) bool {
	return t.Assigned() && t.UsedAt.IsZero() && now.Before(t.ExpiresAt)
}

// Validate checks the binding, wrapping ErrInvalidRecord.
func (t BootstrapToken) Validate() error {
	var p problems
	if t.MachineID == uuid.Nil() {
		p.addf("machine_id must be set")
	}
	if len(t.TokenHash) != 32 {
		p.addf("token hash must be a SHA-256")
	}
	if t.Assigned() == t.ExpiresAt.IsZero() {
		p.addf("account_id and expires_at are set together")
	}
	return p.errOf(ErrInvalidRecord, "bootstrap token")
}

// FleetRouting says which machines an organization's jobs may use.
type FleetRouting string

// Fleet routings.
const (
	RoutingOwnOnly  FleetRouting = "own_only"  // only the organization's own shapes
	RoutingOwnFirst FleetRouting = "own_first" // own shapes; hosted ones when none has room
	RoutingHosted   FleetRouting = "hosted"    // only instance-wide shapes
)

// FleetRoutings returns every routing.
func FleetRoutings() []FleetRouting {
	return []FleetRouting{RoutingOwnOnly, RoutingOwnFirst, RoutingHosted}
}

// FleetPlan is an organization's plan as far as the fleet cares, a stub
// until billing (S.3); only instance admins set it.
type FleetPlan string

// Fleet plans.
const (
	PlanFree FleetPlan = "free"
	PlanPaid FleetPlan = "paid"
)

// FleetPlans returns every plan.
func FleetPlans() []FleetPlan { return []FleetPlan{PlanFree, PlanPaid} }

// FleetQuotas cap an organization's use of machines; zero values mean no
// limit. MonthlyBudget counts hosted machines only.
type FleetQuotas struct {
	ConcurrentMachines int      `json:"concurrent_machines,omitempty"`
	MonthlyMinutes     int      `json:"monthly_minutes,omitempty"`
	MonthlyBudget      *float64 `json:"monthly_budget,omitempty"`
}

// FleetPolicy is an organization's routing and quotas (table fleet_policy).
type FleetPolicy struct {
	AccountID uuid.UUID    `json:"account_id"`
	Routing   FleetRouting `json:"routing"`
	// AllowedShapes limits the shapes used, by name; empty allows all.
	AllowedShapes []string    `json:"allowed_shapes"`
	Quotas        FleetQuotas `json:"quotas"`
	// Egress lists hosts or CIDRs its hosted machines may reach beyond the
	// defaults (fleet.md, Network).
	Egress    []string  `json:"egress"`
	Plan      FleetPlan `json:"plan"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultFleetPolicy is the policy of an organization without a row: its
// code stays on its own machines.
func DefaultFleetPolicy(accountID uuid.UUID) FleetPolicy {
	return FleetPolicy{AccountID: accountID, Routing: RoutingOwnOnly, Plan: PlanFree}
}

// Allows reports whether the policy's organization may use s: its own or an
// instance-wide shape, as routing and AllowedShapes say.
func (f FleetPolicy) Allows(s Shape) bool {
	if s.AccountID != uuid.Nil() && s.AccountID != f.AccountID {
		return false
	}
	if len(f.AllowedShapes) > 0 && !slices.Contains(f.AllowedShapes, s.Name) {
		return false
	}
	switch f.Routing {
	case RoutingOwnOnly:
		return s.Ownership() == OwnershipOwn
	case RoutingOwnFirst:
		return true
	case RoutingHosted:
		return s.Ownership() == OwnershipHosted
	}
	return false
}

// Validate checks the policy, reporting every problem at once and wrapping
// ErrInvalidRecord.
func (f FleetPolicy) Validate() error {
	var p problems
	if f.AccountID == uuid.Nil() {
		p.addf("account_id must be set")
	}
	checkEnum(&p, "routing", f.Routing, FleetRoutings())
	checkEnum(&p, "plan", f.Plan, FleetPlans())
	for _, n := range f.AllowedShapes {
		if !ValidName(n) {
			p.addf("allowed_shapes: %q must match %s", n, nameRE)
		}
	}
	if f.Quotas.ConcurrentMachines < 0 || f.Quotas.MonthlyMinutes < 0 {
		p.addf("quotas must not be negative")
	}
	if f.Quotas.MonthlyBudget != nil {
		checkAmount(&p, "quotas.monthly_budget", *f.Quotas.MonthlyBudget)
	}
	checkHosts(&p, "egress", f.Egress)
	return p.errOf(ErrInvalidRecord, "fleet policy")
}

// ImageFormat is how an image is stored.
type ImageFormat string

// Image formats.
const (
	ImageRootfs ImageFormat = "rootfs" // ext4 root filesystem plus a kernel, for microVMs
	ImageQCOW2  ImageFormat = "qcow2"
	ImageVHDX   ImageFormat = "vhdx"
	ImageAMI    ImageFormat = "ami"
	ImageTart   ImageFormat = "tart"
)

// ImageFormats returns every image format.
func ImageFormats() []ImageFormat {
	return []ImageFormat{ImageRootfs, ImageQCOW2, ImageVHDX, ImageAMI, ImageTart}
}

// ImagePackage is one thing installed in an image.
type ImagePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ImageKernel is the kernel a rootfs image boots.
type ImageKernel struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

// ImageContents is what an image holds (images.contents).
type ImageContents struct {
	Labels   []string       `json:"labels,omitempty"` // e.g. xcode:16, runtime:codex; shapes on the image list them
	Packages []ImagePackage `json:"packages,omitempty"`
	Kernel   *ImageKernel   `json:"kernel,omitempty"` // rootfs only
}

// Image is a built, signed machine image. AccountID zero is an
// instance-wide image; an organization may register its own builds. Only a
// person registers one, never a fleet agent (fleet.md, Images).
type Image struct {
	ID        uuid.UUID   `json:"id"`
	AccountID uuid.UUID   `json:"account_id,omitzero"`
	Name      string      `json:"name"`
	Version   string      `json:"version"`
	OS        MachineOS   `json:"os"`
	Arch      MachineArch `json:"arch"`
	Format    ImageFormat `json:"format"`
	URI       string      `json:"uri"`    // where to fetch it, without credentials
	Digest    string      `json:"digest"` // sha256:<hex>
	// Signature is ed25519:<base64> over SigningPayload.
	Signature string        `json:"signature"`
	Contents  ImageContents `json:"contents"`
	BuiltAt   time.Time     `json:"built_at"`
	CreatedAt time.Time     `json:"created_at"`
}

var (
	imageVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	digestRE       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParseImageRef splits a shape's image reference, name or name@version.
func ParseImageRef(ref string) (name, version string, ok bool) {
	name, version, pinned := strings.Cut(ref, "@")
	return name, version, ValidName(name) && (!pinned || imageVersionRE.MatchString(version))
}

// SigningPayload returns the bytes Signature signs: the fields that say
// what the image is, so a signature cannot be moved to another image, the
// digest of the kernel of a rootfs image (empty for any other), so nobody can
// swap it, and the build time to the second (the database keeps less than a
// nanosecond), so a build cannot be given another date. One per line, in that
// order.
func (i Image) SigningPayload() []byte {
	var kernel string
	if i.Contents.Kernel != nil {
		kernel = i.Contents.Kernel.Digest
	}
	return fmt.Appendf(nil, "%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n", i.Name, i.Version, i.OS, i.Arch, i.Format, i.Digest, kernel,
		i.BuiltAt.UTC().Format(time.RFC3339))
}

// validSignature reports whether s is ed25519:<base64 of 64 bytes>.
func validSignature(s string) bool {
	b, ok := strings.CutPrefix(s, "ed25519:")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(b)
	return err == nil && len(raw) == 64
}

// Validate checks the image, reporting every problem at once and wrapping
// ErrInvalidRecord. Verifying the signature needs the key: the fleet agent
// does it before use.
func (i Image) Validate() error {
	var p problems
	if !ValidName(i.Name) {
		p.addf("name %q must match %s", i.Name, nameRE)
	}
	if !imageVersionRE.MatchString(i.Version) {
		p.addf("version %q must match %s", i.Version, imageVersionRE)
	}
	checkEnum(&p, "os", i.OS, MachineOSes())
	checkEnum(&p, "arch", i.Arch, MachineArches())
	checkEnum(&p, "format", i.Format, ImageFormats())
	checkURI(&p, "uri", i.URI)
	if !digestRE.MatchString(i.Digest) {
		p.addf("digest must be sha256:<64 hex digits>")
	}
	if !validSignature(i.Signature) {
		p.addf("signature must be ed25519:<base64 of 64 bytes>")
	}
	if k := i.Contents.Kernel; (k != nil) != (i.Format == ImageRootfs) {
		p.addf("contents.kernel is set exactly on rootfs images")
	} else if k != nil {
		checkURI(&p, "contents.kernel.uri", k.URI)
		if !digestRE.MatchString(k.Digest) {
			p.addf("contents.kernel.digest must be sha256:<64 hex digits>")
		}
	}
	for _, pkg := range i.Contents.Packages {
		if pkg.Name == "" || pkg.Version == "" {
			p.addf("contents.packages: %q needs a name and a version", pkg.Name)
		}
	}
	return p.errOf(ErrInvalidRecord, "image "+i.Name)
}

// UsageRecord is what one machine cost one organization, written when it is
// destroyed.
type UsageRecord struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	MachineID uuid.UUID `json:"machine_id"` // one record per machine
	ShapeID   uuid.UUID `json:"shape_id"`
	Ownership Ownership `json:"ownership"`
	Minutes   float64   `json:"minutes"`
	Cost      float64   `json:"cost"` // in config fleet.currency
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks the record, wrapping ErrInvalidRecord.
func (u UsageRecord) Validate() error {
	var p problems
	if u.AccountID == uuid.Nil() || u.MachineID == uuid.Nil() || u.ShapeID == uuid.Nil() {
		p.addf("account_id, machine_id and shape_id must be set")
	}
	checkEnum(&p, "ownership", u.Ownership, Ownerships())
	checkAmount(&p, "minutes", u.Minutes)
	checkAmount(&p, "cost", u.Cost)
	if u.EndedAt.Before(u.StartedAt) {
		p.addf("ended_at must not be before started_at")
	}
	return p.errOf(ErrInvalidRecord, "usage record")
}

// checkAmount adds a problem when v is not a finite number >= 0.
func checkAmount(p *problems, field string, v float64) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		p.addf("%s must not be negative", field)
	}
}

// checkHosts adds a problem per entry that cannot be a host or CIDR.
func checkHosts(p *problems, field string, hosts []string) {
	for _, h := range hosts {
		if h == "" || strings.ContainsAny(h, " \t\r\n@") {
			p.addf("%s: %q must be a host or a CIDR", field, h)
		}
	}
}

// checkURI adds a problem unless s is an absolute URI without credentials.
func checkURI(p *problems, field, s string) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.User != nil {
		p.addf("%s must be an absolute URI without credentials", field)
		return
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case !slices.Contains(imageURISchemes, scheme):
		p.addf("%s: scheme %q is not one of %s", field, u.Scheme, strings.Join(imageURISchemes, ", "))
	case (scheme == "http" || scheme == "https") && u.Host == "":
		p.addf("%s: an http(s) URI needs a host", field)
	}
	// A signed URL carries its credential in the query: it would sit in the
	// database, the API and the logs.
	for key := range u.Query() {
		if secretKeyRE.MatchString(key) || signedURLKeyRE.MatchString(key) {
			p.addf("%s: the query has a key that looks like a credential (%q): use a URI without one", field, key)
		}
	}
}

// signedURLKeyRE matches the query keys of pre-signed URLs (AWS, Google, and
// the plain signature parameters).
var signedURLKeyRE = regexp.MustCompile(`(?i)^(x-amz-.*|x-goog-.*|signature|sig|sas)$`)

// imageURISchemes are where an image or kernel may be fetched from: HTTP(S),
// object storage, an OCI registry (Tart) and an AMI. Not file: a path on the
// host that reads it is not a place an organization may point other hosts at.
var imageURISchemes = []string{"https", "http", "s3", "gs", "oci", "ami"}

// secretKeyRE matches config keys that would hold a secret.
var secretKeyRE = regexp.MustCompile(`(?i)secret|passw(or)?d|token|credential|private[_-]?key|api[_-]?key|access[_-]?key`)

// checkNoSecrets adds a problem per secret in v, decoded JSON: a value under
// a secret-looking key that is not a reference its host resolves, or a string
// that looks like a credential. The messages never hold the value.
func (p *problems) checkNoSecrets(field string, v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if ref, ok := x[k].(string); secretKeyRE.MatchString(k) && (!ok || !validHostSecretRef(ref)) {
				p.addf("%s.%s: looks like a secret; give env:NAME or secret:name instead", field, k)
				continue
			}
			p.checkNoSecrets(field+"."+k, x[k])
		}
	case []any:
		for i, e := range x {
			p.checkNoSecrets(fmt.Sprintf("%s[%d]", field, i), e)
		}
	case string:
		if SecretPatterns.MatchString(x) {
			p.addf("%s: looks like a secret; secrets are never allowed here", field)
		}
	}
}

// Abuse kinds: what the abuse checks of a location found on a machine.
const (
	AbuseCPU    = "cpu"    // its CPUs stayed full for longer than the location allows
	AbuseMining = "mining" // full CPUs while connecting to mining pools' ports
)

// AbuseKinds returns every kind.
func AbuseKinds() []string { return []string{AbuseCPU, AbuseMining} }

// FleetNotice is something the instance's operator must look at (table
// fleet_notices): a machine its location stopped for abuse. AccountID is the
// machine's organization, zero for an unassigned one.
type FleetNotice struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id,omitzero"`
	MachineID  uuid.UUID `json:"machine_id"`
	ProviderID uuid.UUID `json:"provider_id"`
	JobID      uuid.UUID `json:"job_id,omitzero"`
	Kind       string    `json:"kind"`
	// Detail is what the location said, written by its agent: untrusted.
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
}

// Validate checks the notice, wrapping ErrInvalidRecord.
func (n FleetNotice) Validate() error {
	var p problems
	if n.MachineID == uuid.Nil() || n.ProviderID == uuid.Nil() {
		p.addf("machine_id and provider_id must be set")
	}
	checkEnum(&p, "kind", n.Kind, AbuseKinds())
	return p.errOf(ErrInvalidRecord, "fleet notice")
}

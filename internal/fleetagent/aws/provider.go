package aws

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/images"
)

// Tags of this provider, next to fleet.TagMachine, fleet.TagOrg and fleet.TagJob.
const (
	// TagFleet marks everything the agent creates and is the only thing the
	// role's policy lets it touch.
	TagFleet = "forecrew:fleet"
	// TagDeadline holds a machine's deadline, so a restarted agent finds it.
	TagDeadline = "forecrew:deadline"
	// TagPrice holds the shape's price per minute, for the spend estimate.
	TagPrice = "forecrew:price"
	// TagImageDigest is on an AMI copied into the account: the digest the image's
	// signature covers.
	TagImageDigest = "forecrew:image-digest"
)

const (
	// sweepGrace is how long a machine past its deadline is left to end itself
	// before the agent terminates it.
	sweepGrace = 2 * time.Minute
	// terminateTimeout bounds a termination the agent starts on its own.
	terminateTimeout = time.Minute
	// maxDiskGB is the largest volume the role allows.
	maxDiskGB = 1000
	// maxTags is what EC2 allows on a resource.
	maxTags = 50
)

var (
	amiURIRE   = regexp.MustCompile(`^ami:([a-z0-9-]+)/(ami-[0-9a-f]{8,17})$`)
	instanceRE = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	tTypeRE    = regexp.MustCompile(`^t[0-9]`)
)

func init() { fleetagent.Register(domain.ProviderAWS, newRegistered) }

// newRegistered is the provider the fleet agent makes, from aws.yaml and the
// credentials the SDK finds: the instance role on EC2, or AWS_PROFILE.
func newRegistered() (fleet.Provider, error) {
	path := os.Getenv(envConfig)
	if path == "" {
		path = defaultConfig
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(context.Background(), cfg, nil)
}

// Provider starts EC2 instances in the customer's account. It keeps nothing but
// the spend ledger: what exists is found in EC2 by tag, so a restart loses nothing.
type Provider struct {
	cfg    Config
	log    *slog.Logger
	ec2    ec2API
	ident  identityAPI
	now    func() time.Time
	ledger *ledger
	// keys returns the public keys images must be signed with.
	keys func() ([]ed25519.PublicKey, error)

	// launch keeps two Creates from both seeing room for the last machine.
	launch sync.Mutex

	mu      sync.Mutex
	account string                            // the account the credentials belong to
	timers  map[string]*time.Timer            // deadlines, by instance
	known   map[string]seen                   // instances the last List or Create saw
	types   map[string]types.InstanceTypeInfo // what EC2 says about the types shapes use
}

// New returns the provider of cfg, with the SDK's default credential chain. It
// makes no call: the first one is the first request. log may be nil.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Provider, error) {
	sdk, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return newWith(cfg, log, ec2.NewFromConfig(sdk), sts.NewFromConfig(sdk), time.Now)
}

func newWith(cfg Config, log *slog.Logger, api ec2API, ident identityAPI, now func() time.Time) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Provider{cfg: cfg, log: log, ec2: api, ident: ident, now: now, timers: map[string]*time.Timer{}, known: map[string]seen{}, types: map[string]types.InstanceTypeInfo{}}
	p.keys = func() ([]ed25519.PublicKey, error) {
		raw, err := os.ReadFile(cfg.TrustedKeysFile) // #nosec G304 -- the operator's file
		if err != nil {
			return nil, fmt.Errorf("trusted image keys: %w", err)
		}
		keys, err := images.ParsePublicKeys(string(raw))
		if err != nil {
			return nil, fmt.Errorf("trusted image keys: %w", err)
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("trusted image keys: %s lists none", cfg.TrustedKeysFile)
		}
		return keys, nil
	}
	if cfg.MonthlyBudget > 0 {
		l, err := openLedger(cfg.StateDir, now)
		if err != nil {
			return nil, err
		}
		p.ledger = l
	}
	return p, nil
}

// accountID returns the account of the credentials, asked once.
func (p *Provider) accountID(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account != "" {
		return p.account, nil
	}
	out, err := p.ident.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("who am I: %w", err)
	}
	p.account = awssdk.ToString(out.Account)
	return p.account, nil
}

// plan is what a machine is started with, once the request has passed every check
// that needs no change in the account.
type plan struct {
	shape ShapeConfig
	ami   amiInfo
	data  string
	tags  []types.Tag
}

// prepare checks req and makes what RunInstances needs. Nothing it does changes
// the account, so a refusal costs nothing.
func (p *Provider) prepare(ctx context.Context, req fleet.CreateRequest) (plan, error) {
	var pl plan
	if req.Bootstrap == nil {
		return pl, errors.New("no bootstrap settings")
	}
	if req.Shape.OS != domain.OSLinux {
		return pl, fmt.Errorf("an AWS location starts Linux machines, not %s: macOS runs on a Mac host through tart (docs/runbooks/byoc-aws.md)", req.Shape.OS)
	}
	if len(req.NetworkAllow) > 0 {
		return pl, errors.New("network_allow does not apply to an AWS location: what a machine reaches is set by its security group; clear network_allow on the location")
	}
	if req.Shape.DiskGB < 1 || req.Shape.DiskGB > maxDiskGB {
		return pl, fmt.Errorf("disk_gb must be between 1 and %d", maxDiskGB)
	}
	var err error
	if pl.shape, err = p.cfg.shapeConfig(req.Shape.ProviderConfig); err != nil {
		return pl, err
	}
	if err = p.checkSize(ctx, pl.shape.InstanceType, req.Shape); err != nil {
		return pl, err
	}
	if pl.ami, err = p.checkImage(ctx, req.Image, req.Shape); err != nil {
		return pl, err
	}
	if pl.data, err = userData(req.Bootstrap, p.cfg.MaxLifetime); err != nil {
		return pl, err
	}
	pl.tags, err = p.tags(req)
	return pl, err
}

// Create implements fleet.Provider.
func (p *Provider) Create(ctx context.Context, req fleet.CreateRequest) (fleet.Instance, error) {
	pl, err := p.prepare(ctx, req)
	if err != nil {
		return fleet.Instance{}, err
	}
	id := req.MachineID.String()

	p.launch.Lock()
	defer p.launch.Unlock()
	// A retry carries a new token: what exists for this machine goes first.
	if err := p.destroyMachine(ctx, id); err != nil {
		return fleet.Instance{}, err
	}
	if err := p.room(ctx); err != nil {
		return fleet.Instance{}, err
	}
	out, err := p.ec2.RunInstances(ctx, p.runInput(req, pl.shape, pl.ami, pl.data, pl.tags))
	if err != nil {
		return fleet.Instance{}, fmt.Errorf("run instance: %w", capacity(err))
	}
	if len(out.Instances) != 1 {
		return fleet.Instance{}, fmt.Errorf("run instance: EC2 started %d instances, not 1", len(out.Instances))
	}
	inst := out.Instances[0]
	external := awssdk.ToString(inst.InstanceId)
	made := p.instance(inst)
	p.mu.Lock()
	p.known[external] = seen{machine: id, inst: made, at: p.now()}
	p.mu.Unlock()
	if p.ledger != nil {
		if err := p.ledger.start(external, req.Shape.PricePerMinute); err != nil {
			// The machine exists: end it rather than run it unmetered.
			if derr := p.terminate(context.WithoutCancel(ctx), external); derr != nil {
				p.log.Warn("terminate a machine that could not be metered", "instance", external, "error", derr)
			}
			return fleet.Instance{}, err
		}
	}
	if !req.Deadline.IsZero() {
		p.arm(external, req.Deadline)
	}
	return made, nil
}

// checkSize refuses a shape that is bigger than its instance type, or of another
// architecture: jobs would be sent to a machine that cannot run them, and paid for.
// What EC2 says about a type does not change, so it is asked once.
func (p *Provider) checkSize(ctx context.Context, instanceType string, shape domain.Shape) error {
	p.mu.Lock()
	info, ok := p.types[instanceType]
	p.mu.Unlock()
	if !ok {
		out, err := p.ec2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)}})
		if err != nil {
			return fmt.Errorf("describe instance type %s: %w", instanceType, err)
		}
		if len(out.InstanceTypes) != 1 {
			return fmt.Errorf("EC2 does not know the instance type %s in %s", instanceType, p.cfg.Region)
		}
		info = out.InstanceTypes[0]
		p.mu.Lock()
		p.types[instanceType] = info
		p.mu.Unlock()
	}
	var c domain.ProviderCapabilities
	sizeBy(&c, info)
	switch {
	case shape.CPU > c.MaxCPU || shape.MemoryMB > c.MaxMemoryMB || shape.GPU > c.MaxGPU:
		return fmt.Errorf("the shape asks for %d CPUs, %d MB and %d GPUs, and %s has %d, %d and %d: a shape must not promise more than its instance type", shape.CPU, shape.MemoryMB, shape.GPU, instanceType, c.MaxCPU, c.MaxMemoryMB, c.MaxGPU)
	case !slices.Contains(c.Arch, shape.Arch):
		return fmt.Errorf("the shape is %s and %s is %v", shape.Arch, instanceType, c.Arch)
	}
	return nil
}

// room answers ErrCapacity when the customer's own limits are reached: the
// machines at once, and the month's estimated spend. The caller holds launch.
func (p *Provider) room(ctx context.Context) error {
	live, err := p.listOurs(ctx)
	if err != nil {
		return err
	}
	if len(live)+p.pending(live, p.now()) >= p.cfg.MaxMachines {
		return fmt.Errorf("this agent's limit of %d machines is reached: %w", p.cfg.MaxMachines, fleet.ErrCapacity)
	}
	if p.ledger != nil && p.ledger.spent() >= p.cfg.MonthlyBudget {
		return fmt.Errorf("this agent's monthly budget of %g is used up: %w", p.cfg.MonthlyBudget, fleet.ErrCapacity)
	}
	return nil
}

func (p *Provider) runInput(req fleet.CreateRequest, s ShapeConfig, ami amiInfo, data string, tags []types.Tag) *ec2.RunInstancesInput {
	in := &ec2.RunInstancesInput{
		ImageId: awssdk.String(ami.id), InstanceType: types.InstanceType(s.InstanceType), MinCount: awssdk.Int32(1), MaxCount: awssdk.Int32(1),
		// A random token per call: EC2 repeats what a token already did, and a
		// retried Create must start a new machine with its new bootstrap token.
		ClientToken: awssdk.String(clientToken()),
		UserData:    awssdk.String(data),
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex: awssdk.Int32(0), SubnetId: awssdk.String(s.Subnet), Groups: []string{s.SecurityGroup},
			AssociatePublicIpAddress: awssdk.Bool(p.cfg.AssignPublicIP), DeleteOnTermination: awssdk.Bool(true),
		}},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: awssdk.String(ami.rootDevice),
			Ebs: &types.EbsBlockDevice{
				VolumeSize: awssdk.Int32(int32(req.Shape.DiskGB)), VolumeType: types.VolumeTypeGp3, // #nosec G115 -- checked in Create
				DeleteOnTermination: awssdk.Bool(true), Encrypted: awssdk.Bool(true),
			},
		}},
		// No instance profile: the machine holds no AWS credentials. IMDSv2 only,
		// one hop, so a container on the machine cannot reach it either.
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpEndpoint: types.InstanceMetadataEndpointStateEnabled, HttpTokens: types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: awssdk.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled,
		},
		// A machine that shuts itself down ends, with its disks.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		// Said outright: the role requires default tenancy, and IAM may not read a
		// key the request leaves out.
		Placement: &types.Placement{Tenancy: types.TenancyDefault},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
			{ResourceType: types.ResourceTypeNetworkInterface, Tags: tags},
		},
	}
	if tTypeRE.MatchString(s.InstanceType) {
		in.CreditSpecification = &types.CreditSpecificationRequest{CpuCredits: awssdk.String("standard")} // no surprise bill
	}
	if s.Spot {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType: types.SpotInstanceTypeOneTime, InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
			},
		}
		in.TagSpecifications = append(in.TagSpecifications, types.TagSpecification{ResourceType: types.ResourceTypeSpotInstancesRequest, Tags: tags})
	}
	return in
}

func clientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return "fc-" + hex.EncodeToString(b)
}

// tags are the tags of everything a machine owns: the provider's, then the
// request's, then ours, which nothing can override.
func (p *Provider) tags(req fleet.CreateRequest) ([]types.Tag, error) {
	m := maps.Clone(req.Tags)
	if m == nil {
		m = map[string]string{}
	}
	m[fleet.TagMachine] = req.MachineID.String()
	m[TagFleet] = p.cfg.Fleet
	m[TagPrice] = strconv.FormatFloat(req.Shape.PricePerMinute, 'f', -1, 64)
	if !req.Deadline.IsZero() {
		m[TagDeadline] = req.Deadline.UTC().Format(time.RFC3339)
	}
	if _, ok := m["Name"]; !ok {
		m["Name"] = "forecrew-" + req.MachineID.String()[:8]
	}
	return tagList(m)
}

// tagList checks tags against what EC2 accepts and sorts them.
func tagList(m map[string]string) ([]types.Tag, error) {
	if len(m) > maxTags {
		return nil, fmt.Errorf("%d tags, and EC2 allows %d on a resource", len(m), maxTags)
	}
	out := make([]types.Tag, 0, len(m))
	for k, v := range m {
		if k == "" || len(k) > 128 || len(v) > 256 || strings.HasPrefix(strings.ToLower(k), "aws:") {
			return nil, fmt.Errorf("tag %q cannot be put on an EC2 resource: keys are 1 to 128 characters and do not start with aws:, values at most 256", k)
		}
		out = append(out, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	slices.SortFunc(out, func(a, b types.Tag) int { return strings.Compare(*a.Key, *b.Key) })
	return out, nil
}

// amiInfo is what the image check found about the AMI.
type amiInfo struct {
	id, rootDevice string
}

// checkImage refuses an image the agent should not boot: unsigned or signed by
// a stranger, in another region or architecture, or an AMI the account does not
// vouch for. The signature covers the build's manifest, not the AMI's ID, so
// what binds the two is where the AMI lives: in this account, tagged with the
// digest by the kit's copy step, or in an account the operator trusts.
func (p *Provider) checkImage(ctx context.Context, img domain.Image, shape domain.Shape) (amiInfo, error) {
	name := img.Name + "@" + img.Version
	if img.Format != domain.ImageAMI {
		return amiInfo{}, fmt.Errorf("image %s: an AWS location boots an AMI, not %s", name, img.Format)
	}
	if img.OS != domain.OSLinux || img.Arch != shape.Arch {
		return amiInfo{}, fmt.Errorf("image %s is %s/%s and the shape is %s/%s", name, img.OS, img.Arch, shape.OS, shape.Arch)
	}
	keys, err := p.keys()
	if err != nil {
		return amiInfo{}, err
	}
	if err := images.Verify(img, keys); err != nil {
		return amiInfo{}, err
	}
	m := amiURIRE.FindStringSubmatch(img.URI)
	if m == nil {
		return amiInfo{}, fmt.Errorf("image %s: the URI must be ami:<region>/<ami id>", name)
	}
	if m[1] != p.cfg.Region {
		return amiInfo{}, fmt.Errorf("image %s is in %s, this agent works in %s: copy it with the kit's AMI copy", name, m[1], p.cfg.Region)
	}
	out, err := p.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{m[2]}})
	if err != nil {
		return amiInfo{}, fmt.Errorf("describe image %s: %w", m[2], err)
	}
	if len(out.Images) != 1 {
		return amiInfo{}, fmt.Errorf("image %s: this account cannot see %s", name, m[2])
	}
	ami := out.Images[0]
	if ami.State != types.ImageStateAvailable || ami.RootDeviceType != types.DeviceTypeEbs || ami.VirtualizationType != types.VirtualizationTypeHvm || ami.RootDeviceName == nil {
		return amiInfo{}, fmt.Errorf("image %s: %s is not an available EBS-backed HVM image", name, m[2])
	}
	ebs := 0
	for _, b := range ami.BlockDeviceMappings {
		if b.Ebs != nil {
			ebs++
		}
	}
	if ebs > 1 {
		return amiInfo{}, fmt.Errorf("image %s: %s has %d EBS volumes and a machine is started with the root volume only: build an image with one", name, m[2], ebs)
	}
	if want := amiArch(img.Arch); ami.Architecture != want {
		return amiInfo{}, fmt.Errorf("image %s: %s is %s, the manifest says %s", name, m[2], ami.Architecture, want)
	}
	if err := p.vouched(ctx, ami, img); err != nil {
		return amiInfo{}, fmt.Errorf("image %s: %w", name, err)
	}
	return amiInfo{id: m[2], rootDevice: *ami.RootDeviceName}, nil
}

func amiArch(a domain.MachineArch) types.ArchitectureValues {
	if a == domain.ArchARM64 {
		return types.ArchitectureValuesArm64
	}
	return types.ArchitectureValuesX8664
}

// vouched says that the AMI belongs to someone this agent trusts.
func (p *Provider) vouched(ctx context.Context, ami types.Image, img domain.Image) error {
	owner := awssdk.ToString(ami.OwnerId)
	if slices.Contains(p.cfg.TrustedAMIOwners, owner) {
		return nil
	}
	own, err := p.accountID(ctx)
	if err != nil {
		return err
	}
	if owner != own {
		return fmt.Errorf("%s belongs to account %s, which this agent does not trust: copy it into this account (the kit's AMI copy) or list the account in trusted_ami_owners", awssdk.ToString(ami.ImageId), owner)
	}
	for _, t := range ami.Tags {
		if awssdk.ToString(t.Key) == TagImageDigest && awssdk.ToString(t.Value) == img.Digest {
			return nil
		}
	}
	return fmt.Errorf("%s has no %s tag with the digest the image is signed with", awssdk.ToString(ami.ImageId), TagImageDigest)
}

// instance turns what EC2 says into what the fleet knows.
func (p *Provider) instance(i types.Instance) fleet.Instance {
	tags := map[string]string{}
	for _, t := range i.Tags {
		k := awssdk.ToString(t.Key)
		if k == "Name" || strings.HasPrefix(k, "aws:") {
			continue
		}
		tags[k] = awssdk.ToString(t.Value)
	}
	out := fleet.Instance{ExternalID: awssdk.ToString(i.InstanceId), Tags: tags, CreatedAt: awssdk.ToTime(i.LaunchTime)}
	if i.State != nil {
		out.Running = i.State.Name == types.InstanceStateNameRunning || i.State.Name == types.InstanceStateNamePending
	}
	if id, err := uuid.Parse(tags[fleet.TagMachine]); err == nil {
		out.MachineID = id
	}
	return out
}

func parsePrice(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad price %q", s)
	}
	return v, nil
}

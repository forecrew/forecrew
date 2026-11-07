package aws

import (
	"bytes"
	"crypto/ed25519"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/images"
)

const (
	testAMI   = "ami-0123456789abcdef0"
	testToken = "fcb_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
	testOrg   = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
)

// clock is real time plus an offset, so that timers armed from it fire when they
// should while a test can still move the clock forward.
type clock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.off)
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// env is a provider on a fake EC2 that enforces the kit's policy.
type env struct {
	t     *testing.T
	p     *Provider
	ec2   *fakeEC2
	sts   *fakeSTS
	clock *clock
	logs  *syncBuffer
	dir   string
	key   ed25519.PrivateKey
	img   domain.Image
	shape domain.Shape
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testConfig(dir string) Config {
	c := defaults()
	c.Region = testRegion
	c.Fleet = testFleet
	c.Subnets = []string{testSubnet, testSubnet2}
	c.SecurityGroup = testGroup
	c.InstanceTypes = []string{"c7i.*", "m7g.large"}
	c.TrustedKeysFile = filepath.Join(dir, "keys.pub")
	c.StateDir = filepath.Join(dir, "state")
	c.MaxMachines = 3
	c.MaxLifetime = 2 * time.Hour
	return c
}

func newEnv(t *testing.T, edit func(*Config)) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), clock: &clock{}, logs: &syncBuffer{}}
	pub, key, err := images.GenerateKey()
	must(t, err)
	e.key = key
	cfg := testConfig(e.dir)
	must(t, os.WriteFile(cfg.TrustedKeysFile, []byte(images.FormatPublicKey(pub)+"\n"), 0o600))
	if edit != nil {
		edit(&cfg)
	}
	e.ec2 = newFakeEC2(e.clock.now)
	policy := kitPolicy(t)
	e.ec2.policy = &policy
	e.ec2.instanceTypes = []types.InstanceTypeInfo{
		instanceType("c7i.large", 2, 4096, types.ArchitectureTypeX8664, 0),
		instanceType("c7i.xlarge", 4, 8192, types.ArchitectureTypeX8664, 0),
		instanceType("m7g.large", 2, 8192, types.ArchitectureTypeArm64, 0),
	}
	e.sts = &fakeSTS{account: testAccount}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.p, err = newWith(cfg, log, e.ec2, e.sts, e.clock.now)
	must(t, err)
	t.Cleanup(func() {
		e.p.mu.Lock()
		defer e.p.mu.Unlock()
		for _, tm := range e.p.timers {
			tm.Stop()
		}
	})

	e.img = domain.Image{
		Name: "linux", Version: "1", OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageAMI,
		URI: "ami:" + testRegion + "/" + testAMI, Digest: "sha256:" + strings.Repeat("ab", 32), BuiltAt: time.Unix(1700000000, 0).UTC(),
	}
	e.img.Signature = images.Sign(key, e.img)
	e.addAMI(testAMI, testAccount, e.img.Digest)
	e.shape = domain.Shape{
		Name: "aws-small", OS: domain.OSLinux, Arch: domain.ArchAMD64, CPU: 2, MemoryMB: 4096, DiskGB: 30, PricePerMinute: 0.01,
		ProviderConfig: map[string]any{"instance_type": "c7i.large", "subnet": testSubnet},
	}
	return e
}

func (e *env) addAMI(id, owner, digest string) {
	img := types.Image{
		ImageId: awssdk.String(id), OwnerId: awssdk.String(owner), State: types.ImageStateAvailable, RootDeviceType: types.DeviceTypeEbs,
		RootDeviceName: awssdk.String("/dev/xvda"), VirtualizationType: types.VirtualizationTypeHvm, Architecture: types.ArchitectureValuesX8664,
	}
	if digest != "" {
		img.Tags = []types.Tag{{Key: awssdk.String(TagImageDigest), Value: awssdk.String(digest)}}
	}
	e.ec2.images[id] = img
}

func (e *env) request() fleet.CreateRequest {
	id := uuid.New()
	return fleet.CreateRequest{
		MachineID: id, Shape: e.shape, Image: e.img,
		Bootstrap: &fleet.Bootstrap{ServerURL: "https://forecrew.example.com", Token: testToken},
		Tags:      map[string]string{fleet.TagMachine: id.String(), fleet.TagOrg: testOrg, "team": "platform"},
	}
}

func (e *env) create(req fleet.CreateRequest) fleet.Instance {
	e.t.Helper()
	inst, err := e.p.Create(e.t.Context(), req)
	must(e.t, err)
	return inst
}

func signed(e *env) string { return images.Sign(e.key, e.img) }

func imagesKey() (ed25519.PublicKey, ed25519.PrivateKey, error) { return images.GenerateKey() }

func signWith(k ed25519.PrivateKey, img domain.Image) string { return images.Sign(k, img) }

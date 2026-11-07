package microvm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
)

// cacheEnv is a provider with organization caches, whose dm-crypt and mkfs
// are recorded: each open says which file it opened with which key.
type cacheEnv struct {
	*testEnv
	opens  []cacheOpen
	mkfs   int
	closes int
	mapped map[string]bool
}

type cacheOpen struct {
	file, key string
	args      []string
}

func newCacheEnv(t *testing.T) *cacheEnv {
	t.Helper()
	c := &cacheEnv{testEnv: newEnv(t, func(c *Config) { c.OrgCacheGB = 1; c.MaxMachines = 4 }), mapped: map[string]bool{}}
	record := c.p.run
	c.p.run = func(ctx context.Context, stdin, name string, args ...string) error {
		switch {
		case name == "cryptsetup" && args[0] == "open":
			c.mu.Lock()
			c.opens = append(c.opens, cacheOpen{file: args[len(args)-2], key: stdin, args: args})
			c.mapped[args[len(args)-1]] = true
			c.mu.Unlock()
			return nil
		case name == "cryptsetup" && args[0] == "close":
			c.mu.Lock()
			c.closes++
			delete(c.mapped, args[1])
			c.mu.Unlock()
			return nil
		case name == "mkfs.ext4":
			c.mkfs++
			return nil
		}
		return record(ctx, stdin, name, args...)
	}
	c.p.hasMapping = func(name string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.mapped[name]
	}
	c.p.mknod = func(device, node string) error {
		if !strings.HasPrefix(device, "/dev/mapper/"+mapperPrefix) {
			t.Errorf("node of %s", device)
		}
		return os.WriteFile(node, nil, 0o600)
	}
	return c
}

func (c *cacheEnv) machine(t *testing.T, org string) (string, string) {
	t.Helper()
	req := c.request()
	if org != "" {
		req.Tags[fleet.TagOrg] = org
	}
	if _, err := c.p.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	id := req.MachineID.String()
	return id, c.p.vmms[vmmFirecracker].jailDir(id)
}

// Each organization gets a cache of its own, under its own key; a machine of
// another organization never gets a copy of it, and what one machine of an
// organization wrote is what its next machine finds.
func TestOrgCachesStayApart(t *testing.T) {
	c := newCacheEnv(t)
	orgA, orgB := uuid.New().String(), uuid.New().String()

	id, dir := c.machine(t, orgA)
	if c.mkfs != 1 || len(c.opens) != 1 {
		t.Fatalf("mkfs %d, opens %d: a new cache gets a file system", c.mkfs, len(c.opens))
	}
	first := c.opens[0]
	if len(first.key) != cacheKeySize || slices.ContainsFunc(first.args, func(a string) bool { return strings.Contains(a, first.key) }) {
		t.Fatalf("the key is not on stdin alone: %q", first.args)
	}
	if !slices.Contains(first.args, "aes-xts-plain64") || first.file != filepath.Join(dir, fileOrgCacheImage) {
		t.Fatalf("open = %+v", first)
	}
	var vm struct {
		Boot struct {
			Args string `json:"boot_args"`
		} `json:"boot-source"`
		Drives []struct {
			ID       string `json:"drive_id"`
			Path     string `json:"path_on_host"`
			ReadOnly bool   `json:"is_read_only"`
		} `json:"drives"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileVM))
	if err := json.Unmarshal(raw, &vm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vm.Boot.Args, "forecrew.orgcache=/dev/vdc") || vm.Drives[len(vm.Drives)-1].Path != fileOrgCache || vm.Drives[len(vm.Drives)-1].ReadOnly {
		t.Fatalf("vm = %s", raw)
	}
	// The guest writes; the copy goes back as the organization's cache.
	if err := os.WriteFile(filepath.Join(dir, fileOrgCacheImage), []byte("A's dependencies"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if c.closes != 1 || len(c.mapped) != 0 {
		t.Fatalf("closes %d, mapped %v", c.closes, c.mapped)
	}
	stored := c.p.orgCachePath(orgA + "/build")
	if got, _ := os.ReadFile(stored); string(got) != "A's dependencies" {
		t.Fatalf("A's cache = %q", got)
	}

	// The next machine of A finds it, under the same key, with no new file system.
	_, dirA := c.machine(t, orgA)
	if got, _ := os.ReadFile(filepath.Join(dirA, fileOrgCacheImage)); string(got) != "A's dependencies" || c.mkfs != 1 || c.opens[1].key != first.key {
		t.Fatalf("A's second machine: %q, mkfs %d", got, c.mkfs)
	}
	// A machine of B gets an empty cache under another key.
	_, dirB := c.machine(t, orgB)
	got, _ := os.ReadFile(filepath.Join(dirB, fileOrgCacheImage))
	if bytes.Contains(got, []byte("A's")) || c.mkfs != 2 || c.opens[2].key == first.key {
		t.Fatalf("B's machine sees A's cache: mkfs %d", c.mkfs)
	}
	// A warm machine, with no organization yet, gets none.
	_, dirW := c.machine(t, "")
	if _, err := os.Stat(filepath.Join(dirW, fileOrgCacheImage)); err == nil || len(c.opens) != 3 {
		t.Fatal("a warm machine got a cache")
	}
}

// A cache changed on the host since it was written back is not used: the
// next machine starts with an empty one.
func TestOrgCacheCheckedAtUse(t *testing.T) {
	c := newCacheEnv(t)
	org := uuid.New().String()
	id, dir := c.machine(t, org)
	_ = os.WriteFile(filepath.Join(dir, fileOrgCacheImage), []byte("good"), 0o600)
	if err := c.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stored := c.p.orgCachePath(org + "/build")
	if err := os.WriteFile(stored, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dir = c.machine(t, org)
	if got, _ := os.ReadFile(filepath.Join(dir, fileOrgCacheImage)); bytes.Contains(got, []byte("evil")) || c.mkfs != 2 {
		t.Fatalf("a changed cache was used: %q, mkfs %d", got, c.mkfs)
	}
	if _, err := os.Stat(stored); err == nil {
		t.Fatal("the changed cache was kept")
	}
}

// A machine whose VMM never ran, or that the abuse checks stopped, does not
// write its copy back.
func TestOrgCacheNotKept(t *testing.T) {
	c := newCacheEnv(t)
	org := uuid.New().String()
	id, dir := c.machine(t, org)
	r, _ := c.p.load(id)
	r.Abuse = "mining"
	if err := c.p.save(r); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, fileOrgCacheImage), []byte("miner"), 0o600)
	if err := c.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.p.orgCachePath(org + "/build")); err == nil {
		t.Fatal("the cache of a stopped machine was kept")
	}
	if c.p.orgCacheOf(fleet.CreateRequest{Tags: map[string]string{fleet.TagOrg: "../../etc"}}) != "" {
		t.Fatal("a tag named a file")
	}
}

func TestPruneCaches(t *testing.T) {
	c := newCacheEnv(t)
	org := uuid.New().String()
	id, _ := c.machine(t, org)
	if err := c.p.Destroy(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stored := c.p.orgCachePath(org + "/build")
	c.p.pruneCaches(1 << 40)
	if _, err := os.Stat(stored); err != nil {
		t.Fatal("a cache in use was pruned")
	}
	c.p.pruneCaches(0)
	if _, err := os.Stat(stored); err == nil {
		t.Fatal("an unused cache stayed")
	}
}

func TestCacheKey(t *testing.T) {
	c := newCacheEnv(t)
	if err := c.p.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	a, err := c.p.cacheKey("a/build")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := c.p.cacheKey("a/build")
	other, _ := c.p.cacheKey("a/control")
	if len(a) != cacheKeySize || !bytes.Equal(a, again) || bytes.Equal(a, other) {
		t.Fatal("keys are not one per cache")
	}
	if st, err := os.Stat(filepath.Join(c.p.cfg.StateDir, cacheKeyFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	_ = os.WriteFile(filepath.Join(c.p.cfg.StateDir, cacheKeyFile), []byte("short"), 0o600)
	if _, err := c.p.cacheKey("a/build"); err == nil {
		t.Fatal("a short key was used")
	}
}

func TestMknodOf(t *testing.T) {
	if err := mknodOf("/dev/null", filepath.Join(t.TempDir(), "n")); err == nil || !strings.Contains(err.Error(), "not a block device") {
		t.Errorf("a character device: %v", err)
	}
	if err := mknodOf("/nonexistent", filepath.Join(t.TempDir(), "n")); err == nil {
		t.Error("a missing device")
	}
}

func TestCacheChecks(t *testing.T) {
	c := newCacheEnv(t)
	if !slices.ContainsFunc(c.p.checks(), func(x domain.ProviderCheck) bool { return x.Name == "cache tools" }) {
		t.Error("no check of the cache tools")
	}
}

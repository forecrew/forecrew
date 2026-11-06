package testkit

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// TestImage returns a valid rootfs image for account (the zero ID is
// instance-wide), signed with a throwaway key, built at built.
func TestImage(account uuid.UUID, name, version string, built time.Time) domain.Image {
	i := domain.Image{
		AccountID: account, Name: name, Version: version, OS: domain.OSLinux, Arch: domain.ArchAMD64, Format: domain.ImageRootfs,
		URI: "https://images.example.com/" + name + "-" + version + ".ext4", Digest: "sha256:" + strings.Repeat("ab", 32),
		Contents: domain.ImageContents{
			Labels: []string{"docker"}, Packages: []domain.ImagePackage{{Name: "git", Version: "2.50"}},
			Kernel: &domain.ImageKernel{URI: "https://images.example.com/vmlinux", Digest: "sha256:" + strings.Repeat("cd", 32)},
		},
		BuiltAt: built,
	}
	_, key, _ := ed25519.GenerateKey(nil)
	i.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, i.SigningPayload()))
	return i
}

// ImageStoreSuite checks the contract of store.ImageRepo on st, the fake
// and the Postgres store alike: other is a second organization.
func ImageStoreSuite(t *testing.T, st store.Store, account, other uuid.UUID) {
	t.Helper()
	s := &imageSuite{t: t, repo: st.Images(), account: account, other: other}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.base = base
	s.inst1 = TestImage(uuid.Nil(), "linux", "1", base)
	s.inst2 = TestImage(uuid.Nil(), "linux", "2", base.Add(24*time.Hour))
	s.own2 = TestImage(account, "linux", "2", base.Add(24*time.Hour)) // same build time as inst2
	s.foreign = TestImage(other, "linux", "9", base.Add(72*time.Hour))
	s.qcow = TestImage(uuid.Nil(), "linux", "3", base.Add(48*time.Hour))
	s.qcow.Format, s.qcow.Contents.Kernel = domain.ImageQCOW2, nil
	for _, i := range []*domain.Image{&s.inst1, &s.inst2, &s.own2, &s.foreign, &s.qcow} {
		if err := s.repo.Create(t.Context(), i); err != nil || i.ID == uuid.Nil() || i.CreatedAt.IsZero() {
			t.Fatalf("Create %s@%s: %v, %+v", i.Name, i.Version, err, i)
		}
	}
	t.Run("create refuses duplicates and invalid images", s.create)
	t.Run("get", s.get)
	t.Run("list sees the organization's and the instance's images", s.list)
	t.Run("resolve", s.resolve)
	t.Run("delete", s.delete)
}

type imageSuite struct {
	t                                 *testing.T
	repo                              store.ImageRepo
	account, other                    uuid.UUID
	base                              time.Time
	inst1, inst2, own2, foreign, qcow domain.Image
}

func (s *imageSuite) create(t *testing.T) {
	ctx := t.Context()
	dup := TestImage(uuid.Nil(), "linux", "1", s.base)
	if err := s.repo.Create(ctx, &dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	// The same version is free for an organization.
	sameOrg := TestImage(s.account, "linux", "1", s.base)
	if err := s.repo.Create(ctx, &sameOrg); err != nil {
		t.Fatalf("own copy of an instance version: %v", err)
	}
	unsigned := TestImage(s.account, "linux", "5", s.base)
	unsigned.Signature = ""
	if err := s.repo.Create(ctx, &unsigned); !errors.Is(err, domain.ErrInvalidRecord) {
		t.Fatalf("unsigned: %v", err)
	}
	unbuilt := TestImage(s.account, "linux", "6", time.Time{})
	if err := s.repo.Create(ctx, &unbuilt); !errors.Is(err, domain.ErrInvalidRecord) {
		t.Fatalf("no build time: %v", err)
	}
	if err := s.repo.Delete(ctx, sameOrg.ID); err != nil {
		t.Fatal(err)
	}
}

func (s *imageSuite) get(t *testing.T) {
	ctx := t.Context()
	got, err := s.repo.Get(ctx, s.inst1.ID)
	if err != nil || got.ID != s.inst1.ID || got.Digest != s.inst1.Digest || got.Signature != s.inst1.Signature ||
		got.Contents.Kernel == nil || got.Contents.Kernel.URI != s.inst1.Contents.Kernel.URI || !got.BuiltAt.Equal(s.base) ||
		got.AccountID != uuid.Nil() || got.Contents.Packages[0].Name != "git" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.repo.Get(ctx, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func (s *imageSuite) list(t *testing.T) {
	ctx := t.Context()
	for _, tt := range []struct {
		name    string
		account uuid.UUID
		filter  store.ImageFilter
		want    string
	}{
		{"all", s.account, store.ImageFilter{}, "linux@3 linux@2 linux@2 linux@1"},
		{"instance-wide only", uuid.Nil(), store.ImageFilter{}, "linux@3 linux@2 linux@1"},
		{"by format", s.account, store.ImageFilter{Format: domain.ImageQCOW2}, "linux@3"},
		{"by name with a limit", s.account, store.ImageFilter{Name: "linux", OS: domain.OSLinux, Arch: domain.ArchAMD64, Limit: 2}, "linux@3 linux@2"},
		{"no windows images", s.account, store.ImageFilter{OS: domain.OSWindows}, ""},
	} {
		got, err := s.repo.List(ctx, tt.account, tt.filter)
		if err != nil || refs(got) != tt.want {
			t.Errorf("%s = %q, %v; want %q", tt.name, refs(got), err, tt.want)
		}
	}
}

func (s *imageSuite) resolve(t *testing.T) {
	ctx := t.Context()
	rootfs := []domain.ImageFormat{domain.ImageRootfs}
	both := []domain.ImageFormat{domain.ImageQCOW2, domain.ImageRootfs}
	for _, tt := range []struct {
		name, ref string
		account   uuid.UUID
		formats   []domain.ImageFormat
		want      uuid.UUID
	}{
		{"newest rootfs for an organization: its own wins a tie", "linux", s.account, rootfs, s.own2.ID},
		{"the other organization has its own newest", "linux", s.other, rootfs, s.foreign.ID},
		{"a pinned version", "linux@1", s.account, rootfs, s.inst1.ID},
		{"the provider's first format, then newest", "linux", s.account, both, s.qcow.ID},
		{"a format the provider does not start", "linux", s.account, []domain.ImageFormat{domain.ImageVHDX}, uuid.Nil()},
		{"unknown version", "linux@77", s.account, rootfs, uuid.Nil()},
		{"unknown name", "windows", s.account, rootfs, uuid.Nil()},
	} {
		got, err := s.repo.Resolve(ctx, tt.account, tt.ref, domain.OSLinux, domain.ArchAMD64, tt.formats)
		if tt.want == uuid.Nil() {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, %v", tt.name, got, err)
			}
			continue
		}
		if err != nil || got.ID != tt.want {
			t.Errorf("%s: %s@%s, %v", tt.name, got.Name, got.Version, err)
		}
	}
	if _, err := s.repo.Resolve(ctx, s.account, "linux", domain.OSLinux, domain.ArchARM64, rootfs); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other arch: %v", err)
	}
	if _, err := s.repo.Resolve(ctx, s.account, "Not Valid", domain.OSLinux, domain.ArchAMD64, rootfs); !errors.Is(err, domain.ErrInvalidRecord) {
		t.Errorf("bad reference: %v", err)
	}
}

func (s *imageSuite) delete(t *testing.T) {
	ctx := t.Context()
	if err := s.repo.Delete(ctx, s.inst1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.repo.Get(ctx, s.inst1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.repo.Delete(ctx, s.inst1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("twice: %v", err)
	}
}

func refs(images []domain.Image) string {
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = fmt.Sprintf("%s@%s", img.Name, img.Version)
	}
	return strings.Join(out, " ")
}

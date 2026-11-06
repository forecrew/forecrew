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
	ctx := t.Context()
	repo := st.Images()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowhere := uuid.Nil()

	inst1 := TestImage(nowhere, "linux", "1", base)
	inst2 := TestImage(nowhere, "linux", "2", base.Add(24*time.Hour))
	own2 := TestImage(account, "linux", "2", base.Add(24*time.Hour)) // same build time as inst2
	foreign := TestImage(other, "linux", "9", base.Add(72*time.Hour))
	qcow := TestImage(nowhere, "linux", "3", base.Add(48*time.Hour))
	qcow.Format, qcow.Contents.Kernel = domain.ImageQCOW2, nil
	for _, i := range []*domain.Image{&inst1, &inst2, &own2, &foreign, &qcow} {
		if err := repo.Create(ctx, i); err != nil || i.ID == uuid.Nil() || i.CreatedAt.IsZero() {
			t.Fatalf("Create %s@%s: %v, %+v", i.Name, i.Version, err, i)
		}
	}

	t.Run("create refuses duplicates and invalid images", func(t *testing.T) {
		dup := TestImage(nowhere, "linux", "1", base)
		if err := repo.Create(ctx, &dup); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		// The same version is free for an organization and for another format.
		sameOrg := TestImage(account, "linux", "1", base)
		if err := repo.Create(ctx, &sameOrg); err != nil {
			t.Fatalf("own copy of an instance version: %v", err)
		}
		unsigned := TestImage(account, "linux", "5", base)
		unsigned.Signature = ""
		if err := repo.Create(ctx, &unsigned); !errors.Is(err, domain.ErrInvalidRecord) {
			t.Fatalf("unsigned: %v", err)
		}
		unbuilt := TestImage(account, "linux", "6", time.Time{})
		if err := repo.Create(ctx, &unbuilt); !errors.Is(err, domain.ErrInvalidRecord) {
			t.Fatalf("no build time: %v", err)
		}
		if err := repo.Delete(ctx, sameOrg.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("get", func(t *testing.T) {
		got, err := repo.Get(ctx, inst1.ID)
		if err != nil || got.ID != inst1.ID || got.Digest != inst1.Digest || got.Signature != inst1.Signature ||
			got.Contents.Kernel == nil || got.Contents.Kernel.URI != inst1.Contents.Kernel.URI || !got.BuiltAt.Equal(base) ||
			got.AccountID != uuid.Nil() || got.Contents.Packages[0].Name != "git" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := repo.Get(ctx, domain.NewID()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown: %v", err)
		}
	})

	t.Run("list sees the organization's and the instance's images", func(t *testing.T) {
		all, err := repo.List(ctx, account, store.ImageFilter{})
		if err != nil || refs(all) != "linux@3 linux@2 linux@2 linux@1" {
			t.Fatalf("List = %s, %v", refs(all), err)
		}
		inst, err := repo.List(ctx, nowhere, store.ImageFilter{})
		if err != nil || refs(inst) != "linux@3 linux@2 linux@1" {
			t.Fatalf("instance-wide = %s, %v", refs(inst), err)
		}
		got, err := repo.List(ctx, account, store.ImageFilter{Format: domain.ImageQCOW2})
		if err != nil || refs(got) != "linux@3" {
			t.Fatalf("by format = %s, %v", refs(got), err)
		}
		got, err = repo.List(ctx, account, store.ImageFilter{Name: "linux", OS: domain.OSLinux, Arch: domain.ArchAMD64, Limit: 2})
		if err != nil || refs(got) != "linux@3 linux@2" {
			t.Fatalf("by name with a limit = %s, %v", refs(got), err)
		}
		if got, err = repo.List(ctx, account, store.ImageFilter{OS: domain.OSWindows}); err != nil || len(got) != 0 {
			t.Fatalf("no windows images = %v, %v", got, err)
		}
	})

	t.Run("resolve", func(t *testing.T) {
		rootfs := []domain.ImageFormat{domain.ImageRootfs}
		both := []domain.ImageFormat{domain.ImageQCOW2, domain.ImageRootfs}
		for _, tt := range []struct {
			name, ref string
			account   uuid.UUID
			formats   []domain.ImageFormat
			want      uuid.UUID
		}{
			{"newest rootfs for an organization: its own wins a tie", "linux", account, rootfs, own2.ID},
			{"an organization never sees another's", "linux", account, []domain.ImageFormat{domain.ImageRootfs}, own2.ID},
			{"the other organization has its own newest", "linux", other, rootfs, foreign.ID},
			{"a pinned version", "linux@1", account, rootfs, inst1.ID},
			{"the provider's first format, then newest", "linux", account, both, qcow.ID},
			{"a format the provider does not start", "linux", account, []domain.ImageFormat{domain.ImageVHDX}, uuid.Nil()},
			{"unknown version", "linux@77", account, rootfs, uuid.Nil()},
			{"unknown name", "windows", account, rootfs, uuid.Nil()},
		} {
			got, err := repo.Resolve(ctx, tt.account, tt.ref, domain.OSLinux, domain.ArchAMD64, tt.formats)
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
		if _, err := repo.Resolve(ctx, account, "linux", domain.OSLinux, domain.ArchARM64, rootfs); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("other arch: %v", err)
		}
		if _, err := repo.Resolve(ctx, account, "Not Valid", domain.OSLinux, domain.ArchAMD64, rootfs); !errors.Is(err, domain.ErrInvalidRecord) {
			t.Errorf("bad reference: %v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := repo.Delete(ctx, inst1.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Get(ctx, inst1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
		if err := repo.Delete(ctx, inst1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("twice: %v", err)
		}
	})
}

func refs(images []domain.Image) string {
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = fmt.Sprintf("%s@%s", img.Name, img.Version)
	}
	return strings.Join(out, " ")
}

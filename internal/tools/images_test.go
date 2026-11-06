package tools_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/images"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

// signedManifest returns a signed manifest of a qcow2 image.
func signedManifest(t *testing.T, name, version string) images.Manifest {
	t.Helper()
	img := testkit.TestImage(uuid.Nil(), name, version, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	img.Format, img.Contents.Kernel = domain.ImageQCOW2, nil
	_, key, err := images.GenerateKey()
	must(t, err)
	img.Signature = images.Sign(key, img)
	return images.ManifestOf(img)
}

func TestAddAndListImages(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())

	added, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: signedManifest(t, "linux", "1")})
	if err != nil || added.Scope != "organization" || added.AccountID != f.account.ID {
		t.Fatalf("AddImage = %+v, %v", added, err)
	}
	if _, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: signedManifest(t, "linux", "1")}); !errors.Is(err, mcp.ErrConflict) {
		t.Errorf("duplicate: %v", err)
	}
	unsigned := signedManifest(t, "linux", "2")
	unsigned.Signature = ""
	if _, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: unsigned}); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("unsigned: %v", err)
	}
	unbuilt := signedManifest(t, "linux", "3")
	unbuilt.BuiltAt = time.Time{}
	if _, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: unbuilt}); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("no build time: %v", err)
	}

	// An instance-wide image is for instance admins only, even an organization owner is refused.
	owner, _ := f.member(t, orgScope, domain.RoleOwner, uuid.Nil())
	if _, err := f.svc.AddImage(ctx, owner, images.AddInput{Manifest: signedManifest(t, "shared", "1"), Instance: true}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("instance image by an owner: %v", err)
	}
	root := f.st.AddMember(f.account.ID, domain.User{Name: "root", InstanceAdmin: true}, "")
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, root.ID, orgScope, domain.RoleAdmin, uuid.Nil())
	rootActor := domain.UserActor(f.account.ID, root.ID, domain.Credential{})
	shared, err := f.svc.AddImage(ctx, rootActor, images.AddInput{Manifest: signedManifest(t, "shared", "1"), Instance: true})
	if err != nil || shared.Scope != "instance" || shared.AccountID != uuid.Nil() {
		t.Fatalf("instance image = %+v, %v", shared, err)
	}

	// Anyone with worker.read lists both; a viewer lacks the permission.
	got, err := f.svc.ListImages(ctx, admin, mcp.ListImagesInput{OS: "linux"})
	if err != nil || len(got) != 2 {
		t.Fatalf("ListImages = %v, %v", got, err)
	}
	scopes := []string{got[0].Scope, got[1].Scope}
	slices.Sort(scopes)
	if scopes[0] != "instance" || scopes[1] != "organization" {
		t.Errorf("scopes = %v", scopes)
	}
	if got, err = f.svc.ListImages(ctx, admin, mcp.ListImagesInput{Name: "shared", OS: "linux", Arch: "amd64", Format: "qcow2", Limit: 1}); err != nil || len(got) != 1 {
		t.Errorf("filtered = %v, %v", got, err)
	}
	for _, bad := range []mcp.ListImagesInput{{OS: "plan9"}, {Arch: "m68k"}, {Format: "iso"}, {Limit: -1}} {
		if _, err := f.svc.ListImages(ctx, admin, bad); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("ListImages(%+v): %v", bad, err)
		}
	}
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	if _, err := f.svc.ListImages(ctx, viewer, mcp.ListImagesInput{}); err != nil {
		t.Errorf("viewer lists: %v", err)
	}
	none, _ := f.member(t, orgScope, "", uuid.Nil())
	if _, err := f.svc.ListImages(ctx, none, mcp.ListImagesInput{}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("no role lists: %v", err)
	}
}

func TestInstanceImagesAreLinuxOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	root := f.st.AddMember(f.account.ID, domain.User{Name: "root", InstanceAdmin: true}, "")
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, root.ID, orgScope, domain.RoleAdmin, uuid.Nil())
	rootActor := domain.UserActor(f.account.ID, root.ID, domain.Credential{})
	// Windows and macOS images are never instance-wide, not even from an instance admin;
	// the organization registers its own.
	for _, osName := range []domain.MachineOS{domain.OSWindows, domain.OSMacOS} {
		m := signedManifest(t, "licensed", "1")
		m.OS, m.Version = osName, string(osName)
		if _, err := f.svc.AddImage(ctx, rootActor, images.AddInput{Manifest: m, Instance: true}); !errors.Is(err, mcp.ErrInvalidInput) {
			t.Errorf("instance %s image: %v", osName, err)
		}
		if _, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: m}); err != nil {
			t.Errorf("organization %s image: %v", osName, err)
		}
	}
}

func TestOnlyAPersonManagesImages(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// f.by acts as an ownerless MCP token; a worker and the system are no people either.
	for _, by := range []domain.Actor{
		f.by,
		{AccountID: f.account.ID, Kind: domain.ActorServiceAccount, Name: uuid.Nil().String()},
		{AccountID: f.account.ID, Kind: domain.ActorWorker, Name: "box"},
		domain.SystemActor(f.account.ID, domain.ForecrewdName),
	} {
		if _, err := f.svc.AddImage(ctx, by, images.AddInput{Manifest: signedManifest(t, "linux", "1")}); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s adds: %v", by, err)
		}
		if _, err := f.svc.DeleteImage(ctx, by, domain.NewID().String()); !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%s deletes: %v", by, err)
		}
	}
	// A user without worker.manage is refused too.
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	if _, err := f.svc.AddImage(ctx, viewer, images.AddInput{Manifest: signedManifest(t, "linux", "1")}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("viewer adds: %v", err)
	}
}

func TestDeleteImage(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin, _ := f.member(t, orgScope, domain.RoleAdmin, uuid.Nil())
	own, err := f.svc.AddImage(ctx, admin, images.AddInput{Manifest: signedManifest(t, "linux", "1")})
	must(t, err)

	shared := testkit.TestImage(uuid.Nil(), "shared", "1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	must(t, f.st.Images().Create(ctx, &shared))
	otherAccount := domain.Account{Name: "other"}
	must(t, f.st.Accounts().Create(ctx, &otherAccount))
	foreign := testkit.TestImage(otherAccount.ID, "theirs", "1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	must(t, f.st.Images().Create(ctx, &foreign))

	if _, err := f.svc.DeleteImage(ctx, admin, "not-a-uuid"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("bad ref: %v", err)
	}
	if _, err := f.svc.DeleteImage(ctx, admin, foreign.ID.String()); !errors.Is(err, mcp.ErrNotFound) {
		t.Errorf("another organization's image: %v", err)
	}
	if _, err := f.svc.DeleteImage(ctx, admin, domain.NewID().String()); !errors.Is(err, mcp.ErrNotFound) {
		t.Errorf("unknown image: %v", err)
	}
	if _, err := f.svc.DeleteImage(ctx, admin, shared.ID.String()); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("instance image by an organization admin: %v", err)
	}
	viewer, _ := f.member(t, orgScope, domain.RoleViewer, uuid.Nil())
	if _, err := f.svc.DeleteImage(ctx, viewer, own.ID.String()); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("viewer deletes: %v", err)
	}
	got, err := f.svc.DeleteImage(ctx, admin, own.ID.String())
	if err != nil || got.ID != own.ID || got.Scope != "organization" {
		t.Fatalf("DeleteImage = %+v, %v", got, err)
	}
	if _, err := f.st.Images().Get(ctx, own.ID); err == nil {
		t.Error("image still there")
	}
	root := f.st.AddMember(f.account.ID, domain.User{Name: "root", InstanceAdmin: true}, "")
	testkit.Bind(t, f.st, f.account.ID, domain.SubjectUser, root.ID, orgScope, domain.RoleAdmin, uuid.Nil())
	if got, err = f.svc.DeleteImage(ctx, domain.UserActor(f.account.ID, root.ID, domain.Credential{}), shared.ID.String()); err != nil || got.Scope != "instance" {
		t.Errorf("instance admin deletes: %+v, %v", got, err)
	}
	// Registering and removing left audit entries.
	entries, err := f.st.Audit().List(ctx, f.account.ID, store.AuditFilter{})
	must(t, err)
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["image.registered"] != 1 || actions["image.deleted"] != 2 {
		t.Errorf("audit = %v", actions)
	}
}

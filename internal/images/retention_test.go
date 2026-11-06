package images

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestRetentionExpired(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	org := domain.NewID()
	build := func(account uuid.UUID, name string, day int, f domain.ImageFormat) domain.Image {
		i := testkit.TestImage(account, name, strconv.Itoa(day), base.AddDate(0, 0, day))
		i.Format = f
		return i
	}
	var all []domain.Image
	for day := 1; day <= 5; day++ { // listed oldest first or not: the rule sorts
		all = append(all, build(org, "linux", 6-day, domain.ImageRootfs))
	}
	all = append(all, build(org, "linux", 1, domain.ImageQCOW2), build(uuid.Nil(), "linux", 1, domain.ImageRootfs), build(org, "windows", 1, domain.ImageRootfs))

	versions := func(imgs []domain.Image) []string {
		out := []string{}
		for _, i := range imgs {
			out = append(out, i.Name+"@"+i.Version)
		}
		return out
	}
	// Three builds stay by default; formats, owners and names count apart.
	def := Retention{}
	if got := versions(def.Expired(all)); !slices.Equal(got, []string{"linux@1", "linux@2"}) {
		t.Errorf("default = %v", got)
	}
	one := Retention{Keep: 1}
	if got := versions(one.Expired(all)); !slices.Equal(got, []string{"linux@1", "linux@2", "linux@3", "linux@4"}) {
		t.Errorf("keep 1 = %v", got)
	}
	five := Retention{Keep: 5}
	if got := five.Expired(all); len(got) != 0 {
		t.Errorf("keep 5 = %v", versions(got))
	}
	if got := def.Expired(nil); len(got) != 0 {
		t.Errorf("nothing = %v", got)
	}
}

func TestResolver(t *testing.T) {
	st := testkit.NewStore(nil)
	ctx := context.Background()
	acc := domain.Account{Name: "a"}
	if err := st.Accounts().Create(ctx, &acc); err != nil {
		t.Fatal(err)
	}
	old := testkit.TestImage(uuid.Nil(), "linux", "1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := testkit.TestImage(uuid.Nil(), "linux", "2", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	for _, i := range []*domain.Image{&old, &newer} {
		if err := st.Images().Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	provider := domain.CapacityProvider{Name: "home", Capabilities: domain.ProviderCapabilities{ImageFormats: []domain.ImageFormat{domain.ImageRootfs}}}
	shape := domain.Shape{Name: "small", OS: domain.OSLinux, Arch: domain.ArchAMD64, Image: "linux"}
	resolve := Resolver(st.Images())

	got, err := resolve(ctx, provider, shape, acc.ID)
	if err != nil || got.Version != "2" {
		t.Fatalf("unpinned = %s, %v", got.Version, err)
	}
	shape.Image = "linux@1"
	if got, err = resolve(ctx, provider, shape, acc.ID); err != nil || got.Version != "1" {
		t.Fatalf("pinned = %s, %v", got.Version, err)
	}
	shape.Image = "linux@9"
	if _, err = resolve(ctx, provider, shape, acc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
	shape.Image = "linux"
	provider.Capabilities.ImageFormats = []domain.ImageFormat{domain.ImageVHDX}
	if _, err = resolve(ctx, provider, shape, acc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("format the provider does not start: %v", err)
	}
}

package images

import (
	"slices"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// DefaultKeep is how many builds of an image Retention keeps.
const DefaultKeep = 3

// Retention is the rule for old images: of the builds of one image (same
// owner, name, os, arch and format) the Keep newest by build time stay, the
// rest expire. The store refuses to delete one a live machine still uses
// (store.ImageRepo.Delete), so an expired image in use is kept until the
// machine is gone.
type Retention struct {
	Keep int
}

type imageGroup struct {
	account uuid.UUID
	name    string
	os      domain.MachineOS
	arch    domain.MachineArch
	format  domain.ImageFormat
}

// Expired returns the images the rule removes, oldest first within each
// group, in the order the groups first appear. A Keep below 1 keeps
// DefaultKeep.
func (r Retention) Expired(all []domain.Image) []domain.Image {
	keep := r.Keep
	if keep < 1 {
		keep = DefaultKeep
	}
	var order []imageGroup
	groups := map[imageGroup][]domain.Image{}
	for _, i := range all {
		g := imageGroup{i.AccountID, i.Name, i.OS, i.Arch, i.Format}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], i)
	}
	var out []domain.Image
	for _, g := range order {
		builds := groups[g]
		slices.SortStableFunc(builds, func(a, b domain.Image) int { return b.BuiltAt.Compare(a.BuiltAt) }) // newest first
		if len(builds) > keep {
			expired := builds[keep:]
			slices.Reverse(expired)
			out = append(out, expired...)
		}
	}
	return out
}

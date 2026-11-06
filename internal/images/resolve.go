package images

import (
	"context"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Resolver returns the function the fleet controller calls to find the image
// of a shape (fleet.Config.Image): what the shape's reference names for the
// organization, in a format the provider starts. The organization's own
// image wins over an instance-wide one of the same build. The error says
// what is missing, so the job's wait reason can.
func Resolver(repo store.ImageRepo) func(ctx context.Context, p domain.CapacityProvider, s domain.Shape, accountID uuid.UUID) (domain.Image, error) {
	return func(ctx context.Context, p domain.CapacityProvider, s domain.Shape, accountID uuid.UUID) (domain.Image, error) {
		img, err := repo.Resolve(ctx, accountID, s.Image, s.OS, s.Arch, p.Capabilities.ImageFormats)
		if err != nil {
			return domain.Image{}, fmt.Errorf("shape %s: no image %s for %s/%s in a format %s starts (%v): %w", s.Name, s.Image, s.OS, s.Arch, p.Name, p.Capabilities.ImageFormats, err)
		}
		return img, nil
	}
}

package microvm

import (
	"fmt"
	"os"
	"path/filepath"
	"uuid"
)

func parseID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// sysPCI is where the host lists its PCI devices.
var sysPCI = "/sys/bus/pci/devices"

// vfioGroup returns the VFIO device of a PCI address.
func vfioGroup(address string) (string, error) {
	link, err := os.Readlink(filepath.Join(sysPCI, address, "iommu_group"))
	if err != nil {
		return "", fmt.Errorf("%s has no IOMMU group: %w", address, err)
	}
	return filepath.Join("/dev/vfio", filepath.Base(link)), nil
}

// claimGPUs gives r's devices to its user: Cloud Hypervisor runs as that user.
func (p *Provider) claimGPUs(r *record, uid int) error {
	return p.ownGPUs(r, uid)
}

// releaseGPUs gives them back to root.
func (p *Provider) releaseGPUs(r *record) error {
	return p.ownGPUs(r, 0)
}

func (p *Provider) ownGPUs(r *record, uid int) error {
	for _, g := range r.GPUs {
		dev, err := vfioGroup(g)
		if err != nil {
			return err
		}
		if err := p.chown(dev, uid, uid); err != nil {
			return err
		}
	}
	return nil
}

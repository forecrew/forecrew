// Package tart is the capacity provider of an Apple silicon Mac: each machine is
// a clone of a signed Tart image, run on this Mac, and deleted after its jobs.
// Apple's licence allows two macOS VMs on one Mac, so a host never offers more
// (docs/fleet.md, macOS locations). The package has no build constraint, so
// that its tests run against a fake tart on any host; only the fleet agent's
// darwin build registers it.
package tart

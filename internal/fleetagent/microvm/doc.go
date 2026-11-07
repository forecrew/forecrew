// Package microvm is the capacity provider of a Linux host with KVM: each
// machine is a fresh microVM, under Firecracker and its jailer, or under Cloud
// Hypervisor when the shape asks for a GPU. Everything a machine owns is named
// in a record written before it is made, so that a crash at any point leaves
// something Destroy can finish (docs/fleet.md, MicroVM locations).
package microvm

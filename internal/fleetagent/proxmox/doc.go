// Package proxmox is the fleet agent's provider for Proxmox VE: each machine is a
// VM cloned from a template, started, given its bootstrap token through the
// QEMU guest agent, and destroyed after its jobs. The agent holds the API
// token; forecrewd never sees it.
package proxmox

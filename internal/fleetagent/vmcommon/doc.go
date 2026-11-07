// Package vmcommon is what the providers that drive someone else's hypervisor
// (Proxmox, Hyper-V) share: what they write into a VM's own notes, the network
// rules of a machine, the resolving of forecrewd's address, and the wall-time
// kill. A hypervisor holds the machines; nothing here is kept on the agent's
// disk, so a restart finds everything again from the hypervisor.
package vmcommon

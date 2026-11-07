// Package hyperv is the fleet agent's provider for a Windows host with Hyper-V:
// each machine is a VM with a differencing disk of a base vhdx, a config disk
// that carries its bootstrap token, and a network device with rules of its own;
// it is removed after its jobs. It drives Hyper-V through PowerShell scripts
// (the scripts directory) that get their parameters as JSON on standard input.
package hyperv

//go:build !linux

package worker

// killLeftovers does nothing outside Linux: an agent's leftover processes are
// not found, see docs/worker.md.
func killLeftovers(string, bool, map[int]struct{}) int { return 0 }

//go:build !linux

package worker

// ProtectProcess does nothing outside Linux: there is no /proc to hide the
// worker's environment in, so agents may be able to read it. See
// docs/worker.md.
func ProtectProcess() error { return nil }

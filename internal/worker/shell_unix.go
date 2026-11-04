//go:build !windows

package worker

// shellIsCmd reports whether command strings run with cmd /c.
func shellIsCmd() bool { return false }

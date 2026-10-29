//go:build windows

package runtime

import "os/exec"

// setProcessGroup keeps the default cancellation (kill the process) on
// Windows, where there are no POSIX process groups.
func setProcessGroup(*exec.Cmd) {}

// killGroup does nothing on Windows.
func killGroup(*exec.Cmd) {}

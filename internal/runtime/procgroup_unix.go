//go:build !windows

package runtime

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the command in its own process group and makes
// cancellation kill the whole group, so no child outlives the run.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup kills what is left of the command's process group after the
// command exited.
func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

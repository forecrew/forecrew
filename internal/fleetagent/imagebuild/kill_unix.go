//go:build !windows

package imagebuild

import (
	"os/exec"
	"syscall"
	"time"
)

// killTreeOnCancel puts the command in a process group of its own and, when its
// context ends, kills the whole group: Packer starts QEMU or Hyper-V helpers that
// would otherwise run on for the rest of the hour.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
}

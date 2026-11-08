//go:build windows

package imagebuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// killTreeOnCancel kills the command and everything it started when its context
// ends: Packer's helpers would otherwise run on for the rest of the hour.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// By its full path: a lookup through PATH would run whatever a folder there calls taskkill.
		taskkill := filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe")
		return exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() // #nosec G204 -- a fixed program and a process ID
	}
	cmd.WaitDelay = 10 * time.Second
}

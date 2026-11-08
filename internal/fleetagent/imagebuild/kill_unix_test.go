//go:build !windows

package imagebuild

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancelKillsPackersChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- execCommand(ctx, t.TempDir(), io.Discard, os.Environ(), "sh", "-c", "sleep 300 & echo $! > "+pidFile+"; wait")
	}()
	var pid int
	for range 500 {
		if raw, err := os.ReadFile(pidFile); err == nil && len(raw) > 1 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the child never started")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled command ended without error")
	}
	for range 500 {
		if err := syscall.Kill(pid, 0); err != nil { // gone
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the child %d of the cancelled command is still running", pid)
}

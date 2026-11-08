//go:build !windows

package fleetagent_test

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

//go:build unix

package fleetagent

import "syscall"

// DiskFreeGB says how much of the file system of path is free.
func DiskFreeGB(path string) (int, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return int(s.Bavail * uint64(s.Bsize) >> 30), nil // #nosec G115 -- a block size is small and positive
}

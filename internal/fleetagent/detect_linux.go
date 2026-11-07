package fleetagent

import "os"

// MemoryMB says how much memory this machine has.
func MemoryMB() (int, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMeminfo(string(b))
}

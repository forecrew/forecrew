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

// AvailableMemoryMB says how much memory this machine can still give, as the
// kernel counts it (MemAvailable), whoever holds the rest.
func AvailableMemoryMB() (int, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return meminfoField(string(b), "MemAvailable")
}

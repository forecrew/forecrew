package fleetagent

import "os"

func memoryMB() (int, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMeminfo(string(b))
}

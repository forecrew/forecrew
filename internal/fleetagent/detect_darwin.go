package fleetagent

import (
	"os/exec"
	"strconv"
	"strings"
)

func memoryMB() (int, error) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return int(bytes >> 20), err
}

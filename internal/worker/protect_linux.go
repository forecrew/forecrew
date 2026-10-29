package worker

import "syscall"

// Linux prctl options.
const (
	prGetDumpable = 3
	prSetDumpable = 4
)

// ProtectProcess hardens the worker process on Linux: it becomes non
// dumpable, so processes of the same user (the agents) cannot read its
// /proc/<pid>/environ or memory, and a child subreaper, so processes an
// agent orphans become children of the worker, where the reaper finds them.
func ProtectProcess() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return e
	}
	return becomeSubreaper()
}

// dumpable returns the PR_GET_DUMPABLE flag of this process.
func dumpable() int {
	r, _, _ := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	return int(r)
}

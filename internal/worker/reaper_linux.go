package worker

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// prSetChildSubreaper is the prctl option that makes orphaned descendants
// children of this process.
const prSetChildSubreaper = 36

// becomeSubreaper makes processes orphaned below this one become its
// children, where the sweep finds them.
func becomeSubreaper() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return e
	}
	return nil
}

type procInfo struct {
	ppid   int
	state  byte // R, S, T, Z, ...
	marked bool
}

// scanProcs reads /proc: parent pid of every process, and whether its
// environment holds markEnv=mark.
func scanProcs(mark string) map[int]procInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := markEnv + "=" + mark
	procs := map[int]procInfo{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat") //nolint:gosec // /proc path
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parentheses.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		env, _ := os.ReadFile("/proc/" + e.Name() + "/environ") //nolint:gosec // /proc path
		procs[pid] = procInfo{ppid: ppid, state: f[0][0], marked: hasEnv(env, want)}
	}
	return procs
}

// hasEnv reports whether a NUL separated environment holds the entry.
func hasEnv(env []byte, entry string) bool {
	for e := range bytes.SplitSeq(env, []byte{0}) {
		if string(e) == entry {
			return true
		}
	}
	return false
}

// candidates returns the processes to kill: the ones marked with the job
// and, when everything is set, every child of this process, each with its
// descendants, except the processes in keep.
func candidates(procs map[int]procInfo, everything bool, keep map[int]struct{}, self int) map[int]struct{} {
	children := map[int][]int{}
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	victims := map[int]struct{}{}
	var add func(pid int)
	add = func(pid int) {
		if _, ok := keep[pid]; ok || pid == self {
			return
		}
		if _, ok := victims[pid]; ok {
			return
		}
		victims[pid] = struct{}{}
		for _, c := range children[pid] {
			add(c)
		}
	}
	for pid, p := range procs {
		if p.marked {
			add(pid)
		}
	}
	if everything {
		for _, c := range children[self] {
			add(c)
		}
	}
	return victims
}

// sweepTimeout bounds one sweep.
const sweepTimeout = 10 * time.Second

// killLeftovers kills the processes marked with mark and their descendants
// and, when everything is set, every child of this process (and its
// descendants) not in keep. It waits until they are gone and returns how many
// it killed. The caller must keep new processes from starting meanwhile.
//
// A process can start new ones faster than a scan of /proc and a kill can
// keep up (a chain of processes that each start the next and exit), so
// killing as it goes does not work. Instead it stops every candidate with
// SIGSTOP, which cannot start anything more, then chases the children of the
// stopped processes quickly, and scans /proc again, until nothing new
// appears. Only then does it kill them all.
func killLeftovers(mark string, everything bool, keep map[int]struct{}) int {
	sw := &sweep{self: os.Getpid(), keep: keep, deadline: time.Now().Add(sweepTimeout), frozen: map[int]struct{}{}}
	// The first scan reads all of /proc, which is slow. Later rounds only
	// look at the children of this process, which is quick enough to catch a
	// chain; a full scan confirms that nothing was missed.
	full := true
	for time.Now().Before(sw.deadline) {
		var cands []int
		switch {
		case full:
			for pid := range candidates(scanProcs(mark), everything, keep, sw.self) {
				cands = append(cands, pid)
			}
		case everything:
			cands = childrenOf(sw.self)
		}
		var work []int
		for _, pid := range cands {
			if sw.freeze(pid) {
				work = append(work, pid)
			}
		}
		if len(work) > 0 {
			sw.chase(work)
			full = false
			continue
		}
		if full {
			break // a full scan found nothing new
		}
		full = true
	}
	return sw.killAll()
}

// sweep is the state of one killLeftovers call.
type sweep struct {
	self     int
	keep     map[int]struct{}
	deadline time.Time
	frozen   map[int]struct{}
}

// freeze stops a process, unless it is spared or already stopped, and
// reports whether it did.
func (sw *sweep) freeze(pid int) bool {
	if _, ok := sw.frozen[pid]; ok || pid == sw.self {
		return false
	}
	if _, ok := sw.keep[pid]; ok {
		return false
	}
	sw.frozen[pid] = struct{}{}
	_ = syscall.Kill(pid, syscall.SIGSTOP)
	return true
}

// chase stops the children of the stopped processes, and theirs, quickly:
// faster than a process can start its successor.
func (sw *sweep) chase(work []int) {
	for len(work) > 0 && time.Now().Before(sw.deadline) {
		var next []int
		for _, pid := range work {
			waitStopped(pid, sw.deadline) // it may be mid-fork
			for _, c := range childrenOf(pid) {
				if sw.freeze(c) {
					next = append(next, c)
				}
			}
		}
		work = next
	}
}

// killAll kills the stopped processes and waits until they are gone.
func (sw *sweep) killAll() int {
	for pid := range sw.frozen {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for pid := range sw.frozen {
		for !gone(pid) && time.Now().Before(sw.deadline) {
			// Reap only a process the sweep killed, and not one an exec.Cmd
			// waits for: the caller blocks agent and git starts.
			if parentOf(pid) == sw.self {
				var ws syscall.WaitStatus
				_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return len(sw.frozen)
}

// statFields returns the fields of /proc/<pid>/stat after the command name:
// state, ppid, ...
func statFields(pid int) []string {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // /proc path
	if err != nil {
		return nil
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(string(stat[i+1:]))
}

func parentOf(pid int) int {
	if f := statFields(pid); len(f) > 1 {
		ppid, _ := strconv.Atoi(f[1])
		return ppid
	}
	return 0
}

// waitStopped waits until the process is stopped, dead or gone, so that
// whatever it was forking shows in its children.
func waitStopped(pid int, deadline time.Time) {
	for time.Now().Before(deadline) {
		f := statFields(pid)
		if len(f) == 0 || strings.ContainsRune("TtZX", rune(f[0][0])) {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// childrenOf returns the children of a process, from the per-thread
// children files of /proc (nil when the kernel does not provide them; the
// next full scan finds the children then).
func childrenOf(pid int) []int {
	base := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []int
	for _, t := range tasks {
		b, err := os.ReadFile(base + "/" + t.Name() + "/children") //nolint:gosec // /proc path
		if err != nil {
			continue
		}
		for f := range strings.FieldsSeq(string(b)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// gone reports whether the process no longer exists.
func gone(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err != nil
}

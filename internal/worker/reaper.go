package worker

import (
	"sync"
	"sync/atomic"
)

// markEnv is set in the environment of every agent run to the job id, so the
// processes an agent leaves behind can be recognized.
const markEnv = "FORECREW_JOB_MARK"

// reaper kills what an agent leaves running. An agent may start a process
// that outlives its run (a daemon, a setsid child). As the same user, such a
// process could read the environment of the next git process, which carries
// the repo token. So the worker sweeps after every run, before any token
// carrying git call.
//
// On Linux the sweep kills the processes marked with the job id (and their
// descendants) and, when no other agent is running, every other child of the
// worker except its own git processes (the worker is a child subreaper, so
// orphans end up as its children). Other systems have no sweep, see
// docs/worker.md.
type reaper struct {
	gitPIDs func() map[int]struct{}
	// gate is held for writing during a sweep, and for reading while an
	// agent is counted in and while git processes start (the gitClient takes
	// it too). So nothing starts while a sweep decides whom to kill, and no
	// process is killed that started after the decision.
	gate   *sync.RWMutex
	agents atomic.Int32
}

func newReaper(gitPIDs func() map[int]struct{}, gate *sync.RWMutex) *reaper {
	return &reaper{gitPIDs: gitPIDs, gate: gate}
}

// agentStarted counts an agent run in. It waits for a running sweep to end.
func (r *reaper) agentStarted() {
	r.gate.RLock()
	r.agents.Add(1)
	r.gate.RUnlock()
}

// agentDone records the end of an agent run and kills its leftovers. It
// returns the number of processes killed.
func (r *reaper) agentDone(mark string) int {
	r.gate.Lock()
	defer r.gate.Unlock()
	alone := r.agents.Add(-1) == 0
	return killLeftovers(mark, alone, r.gitPIDs())
}

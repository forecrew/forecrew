package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// leaver is an agent that starts sleep processes and returns without
// stopping them. start gets the run spec and returns the processes.
func leaver(t *testing.T, pids chan<- int, start func(spec runtime.RunSpec) []*exec.Cmd) funcAdapter {
	return funcAdapter{fn: func(_ context.Context, spec runtime.RunSpec, _ func(domain.JobEvent)) (runtime.Result, error) {
		for _, cmd := range start(spec) {
			if err := cmd.Start(); err != nil {
				t.Error(err)
				continue
			}
			pids <- cmd.Process.Pid
		}
		return runtime.Result{}, nil
	}}
}

func specEnv(spec runtime.RunSpec) []string {
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func alive(pid int) bool { return !gone(pid) }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("process %d is still running after the job", pid)
	}
}

// A process the agent leaves running could read the environment of the next
// git process, which carries the repo token.
func TestLeftoverAgentProcessesAreKilled(t *testing.T) {
	tests := []struct {
		name  string
		start func(spec runtime.RunSpec) []*exec.Cmd
	}{
		{"setsid escapee", func(spec runtime.RunSpec) []*exec.Cmd {
			c := exec.Command("sleep", "300")
			c.Env, c.SysProcAttr = specEnv(spec), &syscall.SysProcAttr{Setsid: true}
			return []*exec.Cmd{c}
		}},
		{"background child of a shell", func(spec runtime.RunSpec) []*exec.Cmd {
			c := exec.Command("sh", "-c", "sleep 300 & sleep 300")
			c.Env, c.SysProcAttr = specEnv(spec), &syscall.SysProcAttr{Setpgid: true}
			return []*exec.Cmd{c}
		}},
		{"process that scrubbed its environment", func(runtime.RunSpec) []*exec.Cmd {
			c := exec.Command("sleep", "300")
			c.Env, c.SysProcAttr = []string{}, &syscall.SysProcAttr{Setsid: true}
			return []*exec.Cmd{c}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pids := make(chan int, 4)
			e := newEnv(t, leaver(t, pids, tt.start))
			if c := e.doComplete(e.spec()); c.Status != StatusSucceeded {
				t.Fatalf("completion = %+v", c)
			}
			close(pids)
			n := 0
			for pid := range pids {
				n++
				waitGone(t, pid)
			}
			if n == 0 {
				t.Fatal("the agent started nothing")
			}
		})
	}
}

// Concurrent jobs share the user: the processes of another running agent are
// left alone when they carry another job's mark.
func TestSweepSparesOtherJobs(t *testing.T) {
	other := exec.Command("sleep", "300")
	other.Env = append(os.Environ(), markEnv+"=another-job-"+strconv.Itoa(os.Getpid()))
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	r := newReaper(func() map[int]struct{} { return nil }, &sync.RWMutex{})
	r.agentStarted() // the other job's agent is running
	r.agentStarted()
	mark := "this-job-" + strconv.Itoa(os.Getpid())
	mine := exec.Command("sleep", "300")
	mine.Env = append(os.Environ(), markEnv+"="+mark)
	if err := mine.Start(); err != nil {
		t.Fatal(err)
	}
	if n := r.agentDone(mark); n != 1 {
		t.Errorf("killed %d processes, want only the one marked for this job", n)
	}
	waitGone(t, mine.Process.Pid)
	if !alive(other.Process.Pid) {
		t.Error("the other job's process was killed")
	}
}

func TestSweepKeepsGitProcesses(t *testing.T) {
	git := exec.Command("sleep", "300") // stands for a running git of another job
	if err := git.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = git.Process.Kill(); _ = git.Wait() }()
	n := killLeftovers("none-"+strconv.Itoa(os.Getpid()), true, map[int]struct{}{git.Process.Pid: {}})
	if n != 0 || !alive(git.Process.Pid) {
		t.Errorf("killed %d processes, git process alive = %v", n, alive(git.Process.Pid))
	}
}

// running reports whether the process exists and is not a zombie.
func running(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // /proc path
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(stat, ')')
	f := strings.Fields(string(stat[i+1:]))
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}

// A chain of processes, each with a scrubbed environment in its own session,
// that starts the next one and exits (after snooping once) outlived a single
// pass sweep: the next link started between the scan and the kill.
func TestRespawningChainIsKilled(t *testing.T) {
	if err := becomeSubreaper(); err != nil {
		t.Skipf("no subreaper: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "chain.sh")
	// Each link starts its successor first, then lingers briefly.
	body := `#!/bin/sh
echo $$ >> "$1/pids"
[ -e "$1/stop" ] && exit 0
env -i PATH="$PATH" setsid sh "$0" "$1" >/dev/null 2>&1 &
sleep 0.03
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test script
		t.Fatal(err)
	}
	pids := make(chan int, 1)
	e := newEnv(t, scriptAdapter(t, runtime.Script{}))
	// Let the chain run for a while before the agent returns.
	e.cfg.Runtimes = runtime.NewRegistry(funcAdapter{fn: func(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
		res, err := leaver(t, pids, func(runtime.RunSpec) []*exec.Cmd {
			c := exec.Command("sh", script, dir)
			c.Env = []string{"PATH=" + os.Getenv("PATH")}
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			return []*exec.Cmd{c}
		}).fn(ctx, spec, emit)
		time.Sleep(200 * time.Millisecond)
		return res, err
	}})
	e.rebuild()
	if c := e.doComplete(e.spec()); c.Status != StatusSucceeded {
		t.Fatalf("completion = %+v", c)
	}
	count := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "pids")) //nolint:gosec // test path
		return len(strings.Fields(string(b)))
	}
	n := count()
	time.Sleep(300 * time.Millisecond)
	if after := count(); after != n {
		t.Errorf("the chain kept starting processes after the job: %d -> %d", n, after)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pids")) //nolint:gosec // test path
	for f := range strings.FieldsSeq(string(b)) {
		pid, _ := strconv.Atoi(f)
		if running(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("link %d of the chain is still running", pid)
		}
	}
	if n < 3 {
		t.Errorf("the chain only had %d links; the test did not exercise the sweep", n)
	}
	_ = os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600)
}

// The sweep decides whom to kill under the gate, which also blocks agents
// from starting and git processes from starting.
func TestSweepBlocksStartsWhileItRuns(t *testing.T) {
	g := newGitClient("git", nil, hermeticEnv(), "")
	inSweep, release := make(chan struct{}), make(chan struct{})
	r := newReaper(func() map[int]struct{} {
		close(inSweep)
		<-release // the sweep is deciding
		return nil
	}, &g.gate)
	r.agentStarted()
	sweepDone := make(chan struct{})
	go func() { r.agentDone("m"); close(sweepDone) }()
	<-inSweep

	started := make(chan string, 2)
	go func() { r.agentStarted(); started <- "agent" }()
	go func() { _, _ = g.version(t.Context()); started <- "git" }()
	select {
	case what := <-started:
		t.Fatalf("a %s started during the sweep", what)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	<-sweepDone
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("starts stayed blocked after the sweep")
		}
	}
}

// Job A's sweep must never kill job B's agent, whenever B starts.
func TestSweepNeverKillsAnAgentThatStartsMeanwhile(t *testing.T) {
	for i := range 60 {
		r := newReaper(func() map[int]struct{} { return nil }, &sync.RWMutex{})
		r.agentStarted() // job A
		var wg sync.WaitGroup
		var b *exec.Cmd
		wg.Go(func() { r.agentDone("job-a") })
		wg.Go(func() {
			r.agentStarted() // job B
			b = exec.Command("sleep", "30")
			b.Env = append(os.Environ(), markEnv+"=job-b")
			if err := b.Start(); err != nil {
				t.Error(err)
			}
		})
		wg.Wait()
		if b != nil && b.Process != nil {
			if !running(b.Process.Pid) {
				t.Fatalf("round %d: job B's agent was killed by job A's sweep", i)
			}
			_ = b.Process.Kill()
			_ = b.Wait()
		}
	}
}

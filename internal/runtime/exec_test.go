package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lines collects the lines a callback gets.
type lines struct {
	mu  sync.Mutex
	got []string
}

func (l *lines) add(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, string(b))
}

func (l *lines) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

func shell(script string, env ...string) Process {
	return Process{Path: "sh", Args: []string{"-c", script}, Dir: "", Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...)}
}

func TestExecStreamsBothPipes(t *testing.T) {
	var out, errs lines
	p := shell("echo one; echo oops >&2; echo two; echo $GREETING; pwd; exit 3", "GREETING=hi")
	p.Dir = t.TempDir()
	exit, err := Exec(t.Context(), p, out.add, errs.add)
	if !errors.Is(err, ErrRunFailed) || exit.Code != 3 {
		t.Fatalf("exit = %+v, err = %v", exit, err)
	}
	want := []string{"one", "two", "hi", realPath(t, p.Dir)}
	if got := out.list(); !slices.Equal(got, want) {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := errs.list(); !slices.Equal(got, []string{"oops"}) || exit.Stderr != "oops" {
		t.Errorf("stderr = %q, tail %q", got, exit.Stderr)
	}
}

func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecSuccessWithoutCallbacks(t *testing.T) {
	exit, err := Exec(t.Context(), shell("echo out; echo err >&2"), nil, nil)
	if err != nil || exit.Code != 0 || exit.Stderr != "err" {
		t.Fatalf("exit = %+v, err = %v", exit, err)
	}
}

func TestExecEnvironmentIsExactlyTheOneGiven(t *testing.T) {
	t.Setenv("FORECREW_LEAK_TEST", "secret")
	var out lines
	if _, err := Exec(t.Context(), shell(`echo "[${FORECREW_LEAK_TEST}]"`), out.add, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.list(); !slices.Equal(got, []string{"[]"}) {
		t.Errorf("stdout = %q: the worker's environment leaked", got)
	}
	// A nil Env is an empty environment, not the caller's.
	out = lines{}
	if _, err := Exec(t.Context(), Process{Path: "/bin/sh", Args: []string{"-c", `echo "[${FORECREW_LEAK_TEST}]"`}}, out.add, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.list(); !slices.Equal(got, []string{"[]"}) {
		t.Errorf("stdout = %q with a nil Env", got)
	}
}

func TestExecNotInstalled(t *testing.T) {
	exit, err := Exec(t.Context(), Process{Path: "/nonexistent/program"}, nil, nil)
	if !errors.Is(err, ErrNotInstalled) || exit.Code != -1 {
		t.Errorf("exit = %+v, err = %v", exit, err)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	// The timeout runs from the start of the run; under load the shell can be
	// slower than it. A run killed before its child existed proves nothing, so
	// it is repeated with twice the timeout until the child was seen.
	timeout := 500 * time.Millisecond
	for range 6 {
		pidfile := filepath.Join(t.TempDir(), "pid")
		p := shell("sleep 300 & echo $! > " + pidfile + "; wait")
		p.Timeout = timeout
		start := time.Now()
		exit, err := Exec(t.Context(), p, nil, nil)
		if !errors.Is(err, ErrTimeout) || exit.Code != -1 {
			t.Fatalf("exit = %+v, err = %v", exit, err)
		}
		if d := time.Since(start); d > timeout+10*time.Second {
			t.Errorf("took %s", d)
		}
		if childStarted(pidfile) {
			assertChildGone(t, pidfile)
			return
		}
		timeout *= 2
	}
	t.Fatal("the shell never started its child before the timeout")
}

func TestExecCancellationKillsProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	ctx := cancelWhenStarted(t, pidfile)
	start := time.Now()
	exit, err := Exec(ctx, shell("sleep 300 & echo $! > "+pidfile+"; wait"), nil, nil)
	if !errors.Is(err, context.Canceled) || exit.Code != -1 {
		t.Fatalf("exit = %+v, err = %v", exit, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
	assertChildGone(t, pidfile)
}

func TestExecKillsLeftoverChildAfterExit(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	exit, err := Exec(t.Context(), shell("sleep 300 & echo $! > "+pidfile), nil, nil)
	if err != nil || exit.Code != 0 {
		t.Fatalf("exit = %+v, err = %v", exit, err)
	}
	assertChildGone(t, pidfile)
}

func TestExecChattyChildDrainIsCapped(t *testing.T) {
	// A child that left the process group and never stops writing must not
	// keep the run open after the command is gone.
	pidfile := filepath.Join(t.TempDir(), "pid")
	script := `setsid sh -c 'while :; do echo noise; sleep 0.02; done' & echo $! > ` + pidfile + `; sleep 0.3`
	var out lines
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := Exec(t.Context(), shell(script), out.add, nil)
		done <- err
	}()
	defer func() { killPidfile(pidfile) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > drainCap+3*time.Second {
			t.Errorf("took %s, want about %s", d, drainCap)
		}
	case <-time.After(drainCap + 10*time.Second):
		t.Fatal("run still open: a chatty child kept the output drain going")
	}
	if len(out.list()) == 0 || !strings.Contains(out.list()[0], "noise") {
		t.Errorf("no output captured: %q", out.list())
	}
}

func killPidfile(pidfile string) {
	b, _ := os.ReadFile(pidfile)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestExecCapsLongLines(t *testing.T) {
	// A line without a newline for 20 MB, then normal lines: the long line is
	// cut at maxLine and the stream goes on.
	script := "head -c 20000000 /dev/zero | tr '\\0' x; echo; echo after; head -c 3000 /dev/zero | tr '\\0' y"
	var out lines
	if _, err := Exec(t.Context(), shell(script), out.add, nil); err != nil {
		t.Fatal(err)
	}
	got := out.list()
	if len(got) != 3 || len(got[0]) != maxLine || got[1] != "after" || len(got[2]) != 3000 {
		var sizes []int
		for _, l := range got {
			sizes = append(sizes, len(l))
		}
		t.Errorf("line sizes = %v, want [%d 5 3000]", sizes, maxLine)
	}
}

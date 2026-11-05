package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/forecrew/forecrew/internal/domain"
)

// Errors of a run.
var (
	// ErrTimeout means the run exceeded RunSpec.Timeout and was killed.
	ErrTimeout = errors.New("runtime: timeout")
	// ErrRunFailed means the CLI failed: non-zero exit or an error result.
	ErrRunFailed = errors.New("runtime: run failed")
)

const (
	waitDelay  = 3 * time.Second
	drainGrace = time.Second
	// drainCap is the most time spent reading what is left in the pipes once
	// the process group is stopped, whatever still writes to them.
	drainCap      = 5 * time.Second
	stderrTail    = 2048
	maxToolOutput = 8000
)

// Config is the shared setup of the CLI adapters.
type Config struct {
	// Binary is the CLI to run; empty means the adapter's default name,
	// looked up in PATH.
	Binary string
	// Env is the base environment of the CLI, normally os.Environ() of the
	// worker; nil inherits the current process environment. RunSpec.Env is
	// added on top.
	Env []string
	// Now returns the event time; nil means time.Now in UTC.
	Now func() time.Time
}

func (c Config) binary(def string) string {
	if c.Binary == "" {
		return def
	}
	return c.Binary
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// cliRun describes one process to run.
type cliRun struct {
	binary string
	args   []string
	dir    string
	// env is added after the base and RunSpec environment.
	env     []string
	stdin   string
	timeout time.Duration
}

// startEnv returns the environment for the process: base plus spec env.
func startEnv(base []string, extra map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := append([]string(nil), base...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// execute runs the process, calls onLine with every stdout line, and kills
// the whole process group on timeout or cancellation. It returns the stderr
// tail. The error is nil for exit 0; ErrNotInstalled, ErrTimeout, the
// context error, or ErrRunFailed otherwise.
func execute(ctx context.Context, r cliRun, onLine func([]byte)) (string, error) {
	x, err := run(ctx, r, onLine, nil)
	return x.Stderr, err
}

// Process is a command for Exec.
type Process struct {
	Path string   // program, looked up in PATH when it has no separator
	Args []string // arguments, without the program
	Dir  string
	// Env is the whole environment of the process; a nil Env is an empty
	// one, never the caller's.
	Env     []string
	Timeout time.Duration // zero means none
}

// Exit is how a process ended.
type Exit struct {
	// Code is the exit code, or -1 when the process did not exit by itself
	// (killed on timeout or cancellation, or never started).
	Code int
	// Stderr is the last part of what the process wrote to stderr.
	Stderr string
}

// Exec runs a command like the CLI adapters run theirs: in its own process
// group that is killed on timeout, cancellation and after the command ends,
// with the pipes drained for a bounded time. onStdout and onStderr, when
// set, get every non-empty line. The error is nil for exit 0; ErrNotInstalled,
// ErrTimeout, the context error, or ErrRunFailed (a non-zero exit, Exit.Code
// is then set) otherwise.
func Exec(ctx context.Context, p Process, onStdout, onStderr func([]byte)) (Exit, error) {
	env := p.Env
	if env == nil {
		env = []string{}
	}
	return run(ctx, cliRun{binary: p.Path, args: p.Args, dir: p.Dir, env: env, timeout: p.Timeout}, onStdout, onStderr)
}

func run(ctx context.Context, r cliRun, onOut, onErr func([]byte)) (Exit, error) {
	none := Exit{Code: -1}
	runCtx := ctx
	if r.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, r.binary, r.args...) //nolint:gosec // the binary is operator config
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.Stdin = strings.NewReader(r.stdin)
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)
	// Plain os.Pipe files, not StdoutPipe or a Writer: exec then does not
	// wait for the pipes to close, so a background child that keeps them open
	// cannot hold the run up after the CLI itself has exited.
	outR, outW, err := os.Pipe()
	if err != nil {
		return none, fmt.Errorf("pipe %s stdout: %w", r.binary, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return none, fmt.Errorf("pipe %s stderr: %w", r.binary, err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	closeAll(outW, errW) // the child has its own copies
	if err != nil {
		closeAll(outR, errR)
		if isNotFound(err) {
			return none, fmt.Errorf("%s: %w", r.binary, ErrNotInstalled)
		}
		return none, fmt.Errorf("start %s: %w", r.binary, err)
	}
	var stderr tailBuffer
	var draining atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		if onOut == nil {
			onOut = func([]byte) {}
		}
		readLines(&idleReader{f: outR, draining: &draining}, onOut)
	})
	wg.Go(func() {
		errIn := &idleReader{f: errR, draining: &draining}
		if onErr == nil {
			_, _ = io.Copy(&stderr, errIn)
			return
		}
		readLines(io.TeeReader(errIn, &stderr), onErr)
	})
	waitErr := cmd.Wait()
	// Leftover children (a dev server the agent started) die with the run.
	killGroup(cmd)
	// Whatever the CLI wrote is already in the pipes. Readers end at EOF; the
	// idle deadline only matters for a child that left the process group.
	draining.Store(true)
	deadline := time.Now().Add(drainGrace)
	_ = outR.SetReadDeadline(deadline)
	_ = errR.SetReadDeadline(deadline)
	// A child that left the group and keeps writing would never let the
	// readers go idle: closing the pipes ends them after drainCap.
	capTimer := time.AfterFunc(drainCap, func() { closeAll(outR, errR) })
	wg.Wait()
	capTimer.Stop()
	closeAll(outR, errR)
	exit := Exit{Code: cmd.ProcessState.ExitCode(), Stderr: stderr.String()}
	switch {
	case ctx.Err() != nil:
		exit.Code = -1
		return exit, ctx.Err()
	case runCtx.Err() != nil:
		exit.Code = -1
		return exit, fmt.Errorf("%w after %s", ErrTimeout, r.timeout)
	case waitErr != nil:
		return exit, fmt.Errorf("%w: %s", ErrRunFailed, waitErr)
	}
	return exit, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// idleReader reads a pipe. Once draining is set (the CLI exited), every read
// gets a fresh deadline, so the pipe is only abandoned after drainGrace of
// silence and a slow consumer cannot lose the end of the stream.
type idleReader struct {
	f        *os.File
	draining *atomic.Bool
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.draining.Load() {
		_ = r.f.SetReadDeadline(time.Now().Add(drainGrace))
	}
	return r.f.Read(p)
}

// maxLine is the longest line readLines passes on; the rest of a longer one
// is read and dropped, so a process that never prints a newline cannot grow
// the worker's memory.
const maxLine = 8 << 20

// readLines calls onLine with each non-empty line of rd, without the newline
// and cut to maxLine bytes.
func readLines(rd io.Reader, onLine func([]byte)) {
	br := bufio.NewReaderSize(rd, 64<<10)
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if room := maxLine - len(line); len(chunk) > room {
			chunk = chunk[:room]
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if t := bytes.TrimSpace(line); len(t) > 0 {
			onLine(t)
		}
		line = nil // onLine may keep what it got
		if err != nil {
			return
		}
	}
}

// tailBuffer keeps the last stderrTail bytes written to it.
type tailBuffer struct{ buf []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = t.buf[len(t.buf)-stderrTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	return strings.TrimSpace(strings.ToValidUTF8(string(t.buf), ""))
}

// emitter normalizes, redacts and stamps events before handing them on.
type emitter struct {
	emit func(domain.JobEvent)
	red  *Redactor
	now  func() time.Time
}

// event emits one event; data is marshaled to JSON (nil for none).
func (e *emitter) event(t domain.JobEventType, text string, data any) {
	ev := domain.JobEvent{Time: e.now(), Type: t, Text: e.red.String(text)}
	if data != nil {
		b, err := json.Marshal(data)
		if err == nil {
			s := e.red.String(string(b))
			if !json.Valid([]byte(s)) { // a redaction broke the JSON
				s, _ = marshalString(s)
			}
			ev.Data = json.RawMessage(s)
		}
	}
	e.emit(ev)
}

func marshalString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// truncate cuts s to max bytes at a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "... [truncated]"
}

// usageData is the Data of a usage event.
type usageData struct {
	domain.Usage
	CostUSD float64 `json:"cost_usd"`
}

// parser turns the stdout of one CLI into events.
type parser interface {
	// line handles one stdout line.
	line(raw []byte, e *emitter)
	// done returns the outcome once the stream ended: the final message,
	// turns, usage, cost; and a failure message when the CLI reported one.
	done() outcome
}

type outcome struct {
	text    string
	turns   int
	usage   domain.Usage
	costUSD float64
	failure string // set when the CLI reported a failed run
	// incomplete is set when the stream ended without a final result.
	incomplete bool
}

// runCLI runs one CLI invocation with a parser and builds the Result.
func runCLI(ctx context.Context, cfg Config, spec RunSpec, r cliRun, p parser, emit func(domain.JobEvent)) (Result, error) {
	red := NewRedactor(spec.Env)
	e := &emitter{emit: emit, red: red, now: cfg.now}
	r.env = append(spec.baseEnv(cfg), r.env...) // r.env: adapter additions, last wins
	r.dir = spec.WorkDir
	if spec.Container != nil {
		w := spec.Container.Wrap(Process{Path: r.binary, Args: r.args, Dir: r.dir, Env: r.env})
		r.binary, r.args, r.dir, r.env = w.Path, w.Args, w.Dir, w.Env
	}
	r.timeout = spec.Timeout
	stderr, err := execute(ctx, r, func(b []byte) { p.line(b, e) })
	out := p.done()
	res := Result{Text: red.String(out.text), Turns: out.turns, Usage: out.usage, CostUSD: out.costUSD}
	if out.usage != (domain.Usage{}) || out.costUSD != 0 {
		e.event(domain.EventUsage, "", usageData{Usage: out.usage, CostUSD: out.costUSD})
	}
	if err != nil && !errors.Is(err, ErrRunFailed) {
		return res, redactErr(red, err)
	}
	if err != nil || out.failure != "" || out.incomplete {
		msg := out.failure
		switch {
		case msg != "":
		case err != nil:
			msg = strings.TrimPrefix(err.Error(), ErrRunFailed.Error()+": ")
			if stderr != "" {
				msg += ": " + stderr
			}
		default:
			msg = "stream ended without a result"
		}
		e.event(domain.EventError, msg, nil)
		return res, fmt.Errorf("%w: %s", ErrRunFailed, red.String(msg))
	}
	if res.Output, err = ExtractOutput(res.Text, spec.OutputSchema); err != nil {
		e.event(domain.EventError, err.Error(), nil)
		return res, redactErr(red, err)
	}
	return res, nil
}

// redactErr returns err with secrets removed from its message. Typed errors
// keep their identity through Unwrap.
func redactErr(red *Redactor, err error) error {
	if ioe, ok := errors.AsType[*InvalidOutputError](err); ok {
		return &InvalidOutputError{Message: red.String(ioe.Message)}
	}
	if msg := red.String(err.Error()); msg != err.Error() {
		return &redactedError{msg: msg, err: err}
	}
	return err
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

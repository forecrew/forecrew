package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// Scripted is the adapter name of the script replayer.
const Scripted = "scripted"

// Script is the file format the scripted adapter replays (JSON): events to
// emit, file edits to make in the work dir, and the final result. It lets
// end-to-end tests run a job without a real agent.
type Script struct {
	Events []ScriptEvent `json:"events,omitempty"`
	Edits  []ScriptEdit  `json:"edits,omitempty"`
	// Commands run in the work dir after the events and before the edits;
	// they need a toolbox container (RunSpec.Container).
	Commands []ScriptCommand `json:"commands,omitempty"`
	// Text is the final assistant message. When Output is set, it is
	// appended as a fenced json block, as a real agent would end its answer.
	Text   string          `json:"text,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Turns  int             `json:"turns,omitempty"`
	Usage  domain.Usage    `json:"usage,omitzero"`
	// CostUSD is the reported cost.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Error, when set, fails the run with ErrRunFailed after the edits.
	Error string `json:"error,omitempty"`
}

// ScriptEvent is one event to emit.
type ScriptEvent struct {
	Type domain.JobEventType `json:"type"`
	Text string              `json:"text,omitempty"`
	Data json.RawMessage     `json:"data,omitempty"`
	// Delay is waited before the event (a Go duration), to test timeouts and
	// cancellation.
	Delay domain.Duration `json:"delay,omitempty"`
}

// ScriptCommand runs a program of the toolbox. Its output goes to the job
// log as log events, and a failing exit code is logged, not an error: a
// scanner exits non-zero when it finds something.
type ScriptCommand struct {
	Argv []string `json:"argv"`
}

// ScriptEdit writes (or deletes) one file, relative to the work dir.
type ScriptEdit struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

type scripted struct {
	path string
	cfg  Config
}

// NewScripted returns an adapter that replays the script file at path. The
// file is read on every Run. cfg.Now sets the event time.
func NewScripted(path string, cfg Config) Adapter { return &scripted{path: path, cfg: cfg} }

func (s *scripted) Name() string { return Scripted }

func (s *scripted) Detect(context.Context) (string, error) { return "scripted", nil }

func (s *scripted) load() (Script, error) {
	var sc Script
	b, err := os.ReadFile(s.path)
	if err != nil {
		return sc, fmt.Errorf("read script: %w", err)
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("parse script %s: %w", s.path, err)
	}
	return sc, nil
}

func (s *scripted) Run(ctx context.Context, spec RunSpec, emit func(domain.JobEvent)) (Result, error) {
	sc, err := s.load()
	if err != nil {
		return Result{}, err
	}
	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	red := NewRedactor(spec.Env)
	e := &emitter{emit: emit, red: red, now: s.cfg.now}
	if err := replay(runCtx, sc, e); err != nil {
		if ctx.Err() == nil && runCtx.Err() != nil {
			return Result{}, fmt.Errorf("%w after %s", ErrTimeout, spec.Timeout)
		}
		return Result{}, err
	}
	if err := runCommands(runCtx, spec, sc.Commands, e); err != nil {
		if ctx.Err() == nil && runCtx.Err() != nil {
			return Result{}, fmt.Errorf("%w after %s", ErrTimeout, spec.Timeout)
		}
		return Result{}, err
	}
	if err := applyEdits(spec.WorkDir, sc.Edits); err != nil {
		return Result{}, err
	}
	res := Result{Text: red.String(sc.Text), Turns: sc.Turns, Usage: sc.Usage, CostUSD: sc.CostUSD}
	if sc.Usage != (domain.Usage{}) || sc.CostUSD != 0 {
		e.event(domain.EventUsage, "", usageData{Usage: sc.Usage, CostUSD: sc.CostUSD})
	}
	if sc.Error != "" {
		e.event(domain.EventError, sc.Error, nil)
		return res, fmt.Errorf("%w: %s", ErrRunFailed, red.String(sc.Error))
	}
	text := sc.Text
	if len(sc.Output) > 0 {
		text += "\n\n```json\n" + string(sc.Output) + "\n```"
		res.Text = red.String(text)
	}
	if res.Output, err = ExtractOutput(res.Text, spec.OutputSchema); err != nil {
		e.event(domain.EventError, err.Error(), nil)
		return res, redactErr(red, err)
	}
	return res, nil
}

func replay(ctx context.Context, sc Script, e *emitter) error {
	for _, ev := range sc.Events {
		if d := time.Duration(ev.Delay); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var data any
		if len(ev.Data) > 0 {
			data = ev.Data
		}
		e.event(ev.Type, ev.Text, data)
	}
	return nil
}

var errNoContainer = errors.New("script commands run only in a toolbox container")

// runCommands runs the script's commands in spec's container, in the work
// dir, with the environment of Env only.
func runCommands(ctx context.Context, spec RunSpec, cmds []ScriptCommand, e *emitter) error {
	if len(cmds) == 0 {
		return nil
	}
	if spec.Container == nil {
		return errNoContainer
	}
	log := func(b []byte) { e.event(domain.EventLog, string(b), nil) }
	for _, c := range cmds {
		if len(c.Argv) == 0 {
			return errors.New("script command has no argv")
		}
		e.event(domain.EventLog, "$ "+strings.Join(c.Argv, " "), nil)
		p := spec.Container.Wrap(Process{Path: c.Argv[0], Args: c.Argv[1:], Dir: spec.WorkDir, Env: spec.baseEnv(Config{})})
		exit, err := Exec(ctx, p, log, log)
		if err != nil && !errors.Is(err, ErrRunFailed) {
			return fmt.Errorf("script command %s: %w", c.Argv[0], err)
		}
		e.event(domain.EventLog, fmt.Sprintf("exit code %d", exit.Code), nil)
	}
	return nil
}

func applyEdits(dir string, edits []ScriptEdit) error {
	for _, ed := range edits {
		if !filepath.IsLocal(ed.Path) {
			return fmt.Errorf("script edit %q: %w", ed.Path, errEditOutside)
		}
		p := filepath.Join(dir, ed.Path)
		if ed.Delete {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("script delete %s: %w", ed.Path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return fmt.Errorf("script edit %s: %w", ed.Path, err)
		}
		if err := os.WriteFile(p, []byte(ed.Content), 0o600); err != nil {
			return fmt.Errorf("script edit %s: %w", ed.Path, err)
		}
	}
	return nil
}

var errEditOutside = errors.New("path is outside the work dir")

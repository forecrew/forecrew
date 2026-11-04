package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
)

// ErrInvalidPlaybook means a playbook failed validation.
var ErrInvalidPlaybook = errors.New("invalid playbook")

// StepType is what a playbook step does.
type StepType string

// Step types.
const (
	StepJob     StepType = "job"     // start an agent job and wait for job.completed
	StepAction  StepType = "action"  // call a registered Go action
	StepGate    StepType = "gate"    // open a gate and wait for gate.resolved
	StepWait    StepType = "wait"    // wait for a signal
	StepForeach StepType = "foreach" // run steps once per item, in order
	StepLoop    StepType = "loop"    // repeat steps until a condition holds
)

// FailurePolicy says what a failed step does to its run.
type FailurePolicy string

// Failure policies. The default is FailRun.
const (
	FailRun  FailurePolicy = "fail"     // the run fails
	Escalate FailurePolicy = "escalate" // open a needs_you gate: approve retries, reject stops the run
)

// Playbook is a declarative workflow (packs/playbooks/<name>.yaml).
type Playbook struct {
	Name        string   `yaml:"-"` // the file name without .yaml
	Description string   `yaml:"description"`
	Inputs      []string `yaml:"inputs"` // required input names, referenced as ${inputs.<name>}
	Steps       []Step   `yaml:"steps"`
	// Finally holds action steps that run once, in order, when the run ends
	// failed or canceled (not when it succeeds). They can reference only
	// the inputs. A failing one is logged and the next still runs.
	Finally []Step `yaml:"finally"`
}

// Step is one step of a playbook. String values in With, Items and Until
// may reference earlier values with ${...}: inputs.<name>,
// steps.<id>.output[.<field>...] (job steps also have job_id and head_sha,
// gate steps gate_id),
// item and index inside a foreach, iteration inside a loop. A string that
// is exactly one reference takes the referenced value as is (list, object,
// number); otherwise references are interpolated as text. A reference to a
// step that has not run yet is empty.
//
// A gate step outputs decision and feedback. Rejected cancels the run.
// Changes requested continues inside a loop (the next round reads the
// feedback) and fails the step outside one, so on_failure applies.
type Step struct {
	ID   string         `yaml:"id"` // unique in the playbook
	Type StepType       `yaml:"type"`
	With map[string]any `yaml:"with"` // job: workflow.JobRequest keys; gate: gates.OpenRequest keys; wait: signal and its key fields; action: free-form

	Action        string          `yaml:"action"`         // action: registered name
	Items         string          `yaml:"items"`          // foreach: a reference to a list
	Steps         []Step          `yaml:"steps"`          // foreach and loop: the body
	MaxIterations int             `yaml:"max_iterations"` // loop: rounds before on_failure applies
	Until         string          `yaml:"until"`          // loop: <ref> [== | != <value>], checked after each round
	Timeout       domain.Duration `yaml:"timeout"`        // wait: fail after this long; zero waits forever
	OnFailure     FailurePolicy   `yaml:"on_failure"`     // job, action, gate, wait and loop
}

// LoadPlaybooks parses and validates every dir/*.yaml file of fsys, keyed
// by name.
func LoadPlaybooks(fsys fs.FS, dir string) (map[string]*Playbook, error) {
	names, err := fs.Glob(fsys, path.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load playbooks: %w", err)
	}
	out := make(map[string]*Playbook, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("load playbooks: %w", err)
		}
		p, err := ParsePlaybook(strings.TrimSuffix(path.Base(name), ".yaml"), data)
		if err != nil {
			return nil, err
		}
		out[p.Name] = p
	}
	return out, nil
}

// ParsePlaybook decodes and validates a playbook. Errors wrap
// ErrInvalidPlaybook and name the offending step.
func ParsePlaybook(name string, data []byte) (*Playbook, error) {
	p := &Playbook{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil {
		return nil, fmt.Errorf("playbook %q: %w: %w", name, ErrInvalidPlaybook, err)
	}
	p.Name = name
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("playbook %q: %w", name, err)
	}
	return p, nil
}

var stepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// validator checks a playbook; ids holds every step ID.
type validator struct {
	p   *Playbook
	ids map[string]bool
}

func (p *Playbook) validate() error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("no steps: %w", ErrInvalidPlaybook)
	}
	v := validator{p: p, ids: map[string]bool{}}
	if err := v.collect(p.Steps); err != nil {
		return err
	}
	if err := v.collect(p.Finally); err != nil {
		return err
	}
	if err := v.steps(p.Steps, nil); err != nil {
		return err
	}
	for _, s := range p.Finally {
		if s.Type != StepAction || s.OnFailure != "" {
			return fmt.Errorf("finally step %q: %w: must be an action without on_failure", s.ID, ErrInvalidPlaybook)
		}
	}
	// Only the inputs exist when finally runs: no step may be referenced.
	return validator{p: p, ids: map[string]bool{}}.steps(p.Finally, nil)
}

// collect checks that step IDs are well-formed and unique.
func (v validator) collect(steps []Step) error {
	for _, s := range steps {
		if !stepIDPattern.MatchString(s.ID) {
			return fmt.Errorf("step %q: id must match %s: %w", s.ID, stepIDPattern, ErrInvalidPlaybook)
		}
		if v.ids[s.ID] {
			return fmt.Errorf("step %q: duplicate id: %w", s.ID, ErrInvalidPlaybook)
		}
		v.ids[s.ID] = true
		if err := v.collect(s.Steps); err != nil {
			return err
		}
	}
	return nil
}

// steps validates steps; vars are the variables in scope (item, index,
// iteration).
func (v validator) steps(steps []Step, vars []string) error {
	for _, s := range steps {
		if err := v.step(s, vars); err != nil {
			return fmt.Errorf("step %q: %w: %w", s.ID, ErrInvalidPlaybook, err)
		}
	}
	return nil
}

func (v validator) step(s Step, vars []string) error {
	if err := s.checkFields(); err != nil {
		return err
	}
	if s.Type == StepJob {
		if err := checkPrompt(s.With["prompt"]); err != nil {
			return err
		}
	}
	if err := v.refs(s.With, vars); err != nil {
		return err
	}
	switch s.Type {
	case StepJob:
		if s.With["kind"] == string(domain.JobKindCommand) {
			return checkWith(s.With, keysOf(JobRequest{}), "command")
		}
		return checkWith(s.With, keysOf(JobRequest{}), "role")
	case StepGate:
		return checkGate(s.With)
	case StepWait:
		return checkWait(s.With)
	case StepAction:
		if s.Action == "" {
			return errors.New("action is required")
		}
	case StepForeach:
		if _, ok := singleRef(s.Items); !ok {
			return fmt.Errorf("items %q must be one ${...} reference", s.Items)
		}
		if err := v.refs(s.Items, vars); err != nil {
			return err
		}
		return v.steps(s.Steps, append(slices.Clip(vars), "item", "index"))
	case StepLoop:
		if s.MaxIterations < 1 {
			return errors.New("max_iterations must be at least 1")
		}
		c, err := parseCond(s.Until)
		if err != nil {
			return err
		}
		if err := v.ref(c.ref, append(slices.Clip(vars), "iteration")); err != nil {
			return fmt.Errorf("until: %w", err)
		}
		return v.steps(s.Steps, append(slices.Clip(vars), "iteration"))
	default:
		return fmt.Errorf("unknown type %q", s.Type)
	}
	return nil
}

// checkFields rejects fields that do not belong to the step's type.
func (s Step) checkFields() error {
	compound := s.Type == StepForeach || s.Type == StepLoop
	fields := []struct {
		name string
		set  bool
		ok   bool
	}{
		{"with", s.With != nil, !compound},
		{"action", s.Action != "", s.Type == StepAction},
		{"items", s.Items != "", s.Type == StepForeach},
		{"steps", len(s.Steps) > 0, compound},
		{"max_iterations", s.MaxIterations != 0, s.Type == StepLoop},
		{"until", s.Until != "", s.Type == StepLoop},
		{"timeout", s.Timeout != 0, s.Type == StepWait},
		{"on_failure", s.OnFailure != "", s.Type != StepForeach},
	}
	for _, f := range fields {
		if f.set && !f.ok {
			return fmt.Errorf("%s is not allowed on a %s step", f.name, s.Type)
		}
	}
	if compound && len(s.Steps) == 0 {
		return errors.New("steps are required")
	}
	if s.Timeout < 0 {
		return errors.New("timeout must not be negative")
	}
	switch s.OnFailure {
	case "", FailRun, Escalate:
		return nil
	}
	return fmt.Errorf("on_failure %q must be fail or escalate", s.OnFailure)
}

// checkWith checks that with has only known keys and every required one.
func checkWith(with map[string]any, known []string, required ...string) error {
	for k := range with {
		if !slices.Contains(known, k) {
			return fmt.Errorf("with: unknown key %q", k)
		}
	}
	for _, k := range required {
		if with[k] == nil || with[k] == "" {
			return fmt.Errorf("with: %s is required", k)
		}
	}
	return nil
}

// checkPrompt requires a job's prompt to be literal text from the playbook
// author. The prompt is rendered as trusted instructions, so it must not
// carry values that come from job outputs, signals or inputs.
func checkPrompt(prompt any) error {
	switch p := prompt.(type) {
	case nil:
		return nil
	case string:
		if strings.Contains(p, "${") {
			return errors.New("with: prompt must be literal text, not a ${...} reference")
		}
		return nil
	}
	return errors.New("with: prompt must be literal text")
}

func checkGate(with map[string]any) error {
	if err := checkWith(with, keysOf(gates.OpenRequest{}), "kind", "title", "subject_repo"); err != nil {
		return err
	}
	kind, _ := with["kind"].(string)
	if _, isRef := singleRef(kind); !isRef && !slices.Contains(domain.GateKinds(), domain.GateKind(kind)) {
		return fmt.Errorf("with: unknown gate kind %q", kind)
	}
	return nil
}

// waitKeyFields are the with keys each signal kind's key is built from.
var waitKeyFields = map[domain.SignalKind][]string{
	domain.SignalJobCompleted:    {"job_id"},
	domain.SignalGateResolved:    {"gate_id"},
	domain.SignalPRMerged:        {"repo", "number"},
	domain.SignalPRClosed:        {"repo", "number"},
	domain.SignalChecksCompleted: {"repo", "sha"},
}

func checkWait(with map[string]any) error {
	kind, _ := with["signal"].(string)
	fields, ok := waitKeyFields[domain.SignalKind(kind)]
	if !ok {
		return fmt.Errorf("with: signal %q must be one of %v", kind, domain.SignalKinds())
	}
	return checkWith(with, append([]string{"signal"}, fields...), fields...)
}

// keysOf returns the json keys of a struct's fields.
func keysOf(v any) []string {
	var keys []string
	for f := range reflect.TypeOf(v).Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" && name != "account_id" && name != "project_id" {
			keys = append(keys, name)
		}
	}
	return keys
}

// refs checks every ${...} reference in v, a with value or a string.
func (v validator) refs(val any, vars []string) error {
	switch x := val.(type) {
	case string:
		refs, err := findRefs(x)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if err := v.ref(r, vars); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range x {
			if err := v.refs(e, vars); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := v.refs(e, vars); err != nil {
				return err
			}
		}
	}
	return nil
}

// ref checks one reference against the playbook and the variables in scope.
func (v validator) ref(r string, vars []string) error {
	parts := strings.Split(r, ".")
	if slices.Contains(parts, "") {
		return fmt.Errorf("reference %q is malformed", r)
	}
	switch root := parts[0]; root {
	case "inputs":
		if len(parts) < 2 || !slices.Contains(v.p.Inputs, parts[1]) {
			return fmt.Errorf("reference %q names an undeclared input", r)
		}
	case "steps":
		if len(parts) < 3 || !v.ids[parts[1]] {
			return fmt.Errorf("reference %q names an unknown step", r)
		}
	default:
		if !slices.Contains(vars, root) {
			return fmt.Errorf("reference %q is not in scope", r)
		}
	}
	return nil
}

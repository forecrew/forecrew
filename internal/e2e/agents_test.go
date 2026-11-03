//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/runtime"
)

// call is one run of the scripted runtime.
type call struct {
	Role string
	// Task is the number of the task in the prompt, 0 when there is none.
	Task int
	// Round counts the runs of this role for this task (for a role without
	// a task: for the project), from 1.
	Round    int
	Feedback bool
	Prompt   string
	WorkDir  string
}

// agents decides what the scripted runtime does for each role. The zero
// value plans one task and approves every review.
type agents struct {
	mu        sync.Mutex
	scriptDir string // where the scripts of the calls are written
	count     map[string]int
	workers   map[string][]string // by job
	// Tasks is how many tasks the architect plans, at least 1.
	Tasks int
	// Approve reports whether the reviewer approves round of task.
	Approve func(task, round int) bool
	// Before, when set, runs before every call; its error fails the run.
	Before func(ctx context.Context, c call) error
}

var (
	taskRef = regexp.MustCompile(`Task TASK-(\d+)`)
)

// roleOf names the role from the title of the output schema it must match.
func roleOf(schema json.RawMessage) (string, error) {
	var s struct{ Title string }
	if err := json.Unmarshal(schema, &s); err != nil {
		return "", fmt.Errorf("output schema: %w", err)
	}
	roles := map[string]string{
		"Profile draft": "onboarding-analyst", "Spec": "product-owner", "Task breakdown": "architect",
		"Developer result": "developer", "Review verdict": "reviewer",
	}
	if r, ok := roles[s.Title]; ok {
		return r, nil
	}
	return "", fmt.Errorf("no role has the output schema %q", s.Title)
}

func (a *agents) next(spec runtime.RunSpec) (call, error) {
	role, err := roleOf(spec.OutputSchema)
	if err != nil {
		return call{}, err
	}
	c := call{Role: role, Prompt: spec.Prompt, WorkDir: spec.WorkDir, Feedback: strings.Contains(spec.Prompt, "Feedback to address")}
	if m := taskRef.FindStringSubmatch(spec.Prompt); m != nil {
		c.Task, _ = strconv.Atoi(m[1])
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.count == nil {
		a.count = map[string]int{}
	}
	key := role + "#" + strconv.Itoa(c.Task)
	a.count[key]++
	c.Round = a.count[key]
	return c, nil
}

// ranOn records the worker that runs the job whose checkout is dir.
func (a *agents) ranOn(dir, worker string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.workers == nil {
		a.workers = map[string][]string{}
	}
	id := filepath.Base(dir)
	a.workers[id] = append(a.workers[id], worker)
}

// workersOf returns the workers that ran the job, in order.
func (a *agents) workersOf(id uuid.UUID) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.workers[id.String()])
}

// fakeRuntime is the runtime.Adapter of the e2e workers.
type fakeRuntime struct {
	name   string
	worker string
	ag     *agents
}

func (f fakeRuntime) Name() string                           { return f.name }
func (f fakeRuntime) Detect(context.Context) (string, error) { return "e2e", nil }

func (f fakeRuntime) Run(ctx context.Context, spec runtime.RunSpec, emit func(domain.JobEvent)) (runtime.Result, error) {
	c, err := f.ag.next(spec)
	if err != nil {
		return runtime.Result{}, err
	}
	f.ag.ranOn(spec.WorkDir, f.worker)
	if f.ag.Before != nil {
		if err := f.ag.Before(ctx, c); err != nil {
			return runtime.Result{}, err
		}
	}
	out, edits, err := f.ag.act(c)
	if err != nil {
		return runtime.Result{}, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return runtime.Result{}, err
	}
	sc := runtime.Script{
		Events: []runtime.ScriptEvent{{Type: domain.EventLog, Text: fmt.Sprintf("scripted %s, round %d, key %s", c.Role, c.Round, spec.Env["ANTHROPIC_API_KEY"])}},
		Text:   "done", Output: raw, Turns: 1, CostUSD: 0.01,
	}
	for _, path := range slices.Sorted(maps.Keys(edits)) {
		sc.Edits = append(sc.Edits, runtime.ScriptEdit{Path: path, Content: edits[path]})
	}
	// The script goes through the scripted adapter, as for any e2e run: it
	// makes the edits, emits the events and extracts and validates the output
	// against the role's schema.
	path := filepath.Join(f.ag.scriptDir, fmt.Sprintf("%s-%s-%d.json", filepath.Base(spec.WorkDir), c.Role, c.Round))
	b, err := json.Marshal(sc)
	if err != nil {
		return runtime.Result{}, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return runtime.Result{}, err
	}
	return runtime.NewScripted(path, runtime.Config{}).Run(ctx, spec, emit)
}

var errUnknownRole = errors.New("unknown role")

// act returns the output of the role and the files it writes, by path.
func (a *agents) act(c call) (map[string]any, map[string]string, error) {
	assumptions := []any{map[string]any{"statement": "scripted", "confidence": "high", "impact": "low", "verify_by": "test"}}
	switch c.Role {
	case "onboarding-analyst":
		profile := map[string]any{
			"version": 1, "name": "app", "summary": "A small Go service.", "platforms": []any{"linux"},
			"components": []any{map[string]any{"path": ".", "stack": "go", "build": "go build ./...", "test": "go test ./...", "platforms": []any{"linux"}}},
		}
		raw, _ := json.Marshal(profile)
		edits := map[string]string{
			".forecrew/profile.yaml": string(raw) + "\n",
			".forecrew/README.md":    fmt.Sprintf("The profile of app, round %d.\n", c.Round),
		}
		return map[string]any{"summary": "A Go service.", "profile": profile, "assumptions": assumptions}, edits, nil
	case "product-owner":
		return map[string]any{
			"summary":             "Document how to run the tests.",
			"acceptance_criteria": []any{map[string]any{"id": "AC-1", "text": "The guide says how to run the tests."}, map[string]any{"id": "AC-2", "text": "The guide says how to read a failure."}},
			"scope":               map[string]any{"in": []any{"the guide"}, "out": []any{"CI changes"}},
			"assumptions":         assumptions, "open_questions": []any{},
		}, nil, nil
	case "architect":
		return a.plan(assumptions), nil, nil
	case "developer":
		path := fmt.Sprintf("work/task-%d.txt", c.Task)
		edits := map[string]string{path: fmt.Sprintf("task %d, round %d\n", c.Task, c.Round)}
		return map[string]any{
			"summary": fmt.Sprintf("Round %d of task %d.", c.Round, c.Task), "files": []any{map[string]any{"path": path, "change": "added"}},
			"tests_added": []any{}, "commands_run": []any{}, "assumptions": assumptions, "escalation": nil,
		}, edits, nil
	case "reviewer":
		return a.review(c, assumptions), nil, nil
	}
	return nil, nil, fmt.Errorf("%s: %w", c.Role, errUnknownRole)
}

func (a *agents) plan(assumptions []any) map[string]any {
	var tasks []any
	for i := range max(a.Tasks, 1) {
		tasks = append(tasks, map[string]any{
			"title": fmt.Sprintf("Part %d of the guide", i+1), "description": "Write it.", "criteria": []any{"AC-1", "AC-2"},
			"needs": []any{"linux"}, "size": "s", "depends_on": []any{},
		})
	}
	return map[string]any{"summary": "One part per task.", "tasks": tasks, "assumptions": assumptions, "escalation": nil}
}

func (a *agents) review(c call, assumptions []any) map[string]any {
	verdict, comments := "approve", []any{}
	if a.Approve != nil && !a.Approve(c.Task, c.Round) {
		verdict = "request_changes"
		comments = []any{map[string]any{"path": fmt.Sprintf("work/task-%d.txt", c.Task), "line": 1, "severity": "major", "body": "Say more."}}
	}
	status := map[string]string{"approve": "met", "request_changes": "not_met"}[verdict]
	return map[string]any{
		"verdict": verdict, "summary": "Reviewed.", "comments": comments, "assumptions": assumptions,
		"criteria_check": []any{
			map[string]any{"criterion": "AC-1", "status": status, "evidence": "work file"},
			map[string]any{"criterion": "AC-2", "status": status, "evidence": "work file"},
		},
	}
}

func writeFiles(dir string, files map[string]string) error {
	for path, content := range files {
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

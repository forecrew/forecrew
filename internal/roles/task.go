package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// TasksDir holds, under PacksDir, the tasks of roles: <role>.<task>.md is the
// instructions and <role>.<task>.json the output schema (it may refer to
// ../schemas/common.json).
const TasksDir = "tasks"

// Task is a job a role does besides what its role file describes: the
// knowledge curator's retro of a goal, or its periodic curation. It replaces
// the role's instructions and output schema and takes away its writes.
type Task struct {
	Role         string
	Name         string
	Instructions string
	OutputSchema json.RawMessage
}

// Key is how tasks are looked up: role.name.
func (t Task) Key() string { return t.Role + "." + t.Name }

// LoadTasks reads every packs/tasks/<role>.<task>.md in fsys with its schema,
// keyed by Task.Key. There may be none. It reports every problem at once,
// wrapping ErrInvalidRole.
func LoadTasks(fsys fs.FS) (map[string]Task, error) {
	names, err := fs.Glob(fsys, path.Join(PacksDir, TasksDir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("list task files: %w", err)
	}
	out := map[string]Task{}
	var errs []error
	for _, name := range names {
		base := strings.TrimSuffix(path.Base(name), ".md")
		role, task, ok := strings.Cut(base, ".")
		if !ok || role == "" || task == "" || strings.Contains(task, ".") {
			errs = append(errs, fmt.Errorf("%w: %s: the name must be <role>.<task>.md", ErrInvalidRole, name))
			continue
		}
		prompt, err := fs.ReadFile(fsys, name)
		if err == nil && strings.TrimSpace(string(prompt)) == "" {
			err = errors.New("empty")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s: %w", ErrInvalidRole, name, err))
			continue
		}
		schema, err := BundleSchema(fsys, strings.TrimSuffix(name, ".md")+".json")
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %s: %w", ErrInvalidRole, name, err))
			continue
		}
		t := Task{Role: role, Name: task, Instructions: strings.TrimSpace(string(prompt)), OutputSchema: schema}
		out[t.Key()] = t
	}
	return out, errors.Join(errs...)
}

// ForTask returns r doing t: read-only, with t's instructions and output
// schema. It keeps the role's tools besides the ones that write, so the
// knowledge curator can still search and propose.
func (r Role) ForTask(t Task) Role {
	out := r.WithoutWrites()
	out.AllowedTools = slices.Clone(out.AllowedTools)
	out.Instructions = t.Instructions
	out.OutputSchema = slices.Clone(t.OutputSchema)
	out.DefinitionOfDone, out.EscalateWhen = nil, nil
	return out
}

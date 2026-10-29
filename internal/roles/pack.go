package roles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/config"
)

// ErrInvalidRole means a role file is malformed or inconsistent.
var ErrInvalidRole = errors.New("invalid role")

// PacksDir is the directory, relative to the root of the files Load reads,
// that holds roles/ and schemas/. Prompt and schema paths in role files are
// relative to it.
const PacksDir = "packs"

// tools maps the tools a role file may name to the Claude Code tools and
// permission rules they allow. Codex maps these to its sandbox settings.
var tools = map[string][]string{
	"repo.read":  {"Glob", "Grep", "Read"},
	"repo.write": {"Edit", "Write"},
	"git.read":   {"Bash(git diff *)", "Bash(git log *)", "Bash(git show *)", "Bash(git status *)"},
	"git.commit": {"Bash(git add *)", "Bash(git commit *)"},
	// The test command comes from the project profile, so it cannot be
	// narrowed here. Bash can change any file, so tools do not make a role
	// read-only: the worker does, by never committing or pushing for a
	// role without write paths and by checking the changed paths after
	// every run (docs/contracts/job-spec.md, worker rules).
	"run.tests": {"Bash"},
}

// writeTools need write paths, and write paths need repo.write.
var writeTools = []string{"repo.write", "git.commit"}

var roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Definition is a role file from packs/roles, with its prompt read and its
// output schema bundled.
type Definition struct {
	Name string
	// Tier is the tier the role is written for. forecrew.yaml roles decide
	// the tier jobs run with.
	Tier         config.Tier
	Instructions string
	Tools        []string // as named in the role file
	// WritePaths empty means read-only, enforced by the worker's post-run
	// path check rather than by Tools.
	WritePaths       []string
	Produces         []string
	OutputSchema     json.RawMessage
	DefinitionOfDone []string
	EscalateWhen     []string
	MaxTurns         int
	Timeout          time.Duration
}

// AllowedTools returns the runtime tools the definition's tools allow,
// sorted and without duplicates.
func (d Definition) AllowedTools() []string {
	var out []string
	for _, t := range d.Tools {
		out = append(out, tools[t]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// file is the YAML shape of a role file.
type file struct {
	Role             string        `yaml:"role"`
	Model            config.Tier   `yaml:"model"`
	Prompt           string        `yaml:"prompt"`
	Tools            []string      `yaml:"tools"`
	WritePaths       *[]string     `yaml:"write_paths"`
	Produces         []string      `yaml:"produces"`
	OutputSchema     string        `yaml:"output_schema"`
	DefinitionOfDone []string      `yaml:"definition_of_done"`
	EscalateWhen     []string      `yaml:"escalate_when"`
	MaxTurns         int           `yaml:"max_turns"`
	Timeout          time.Duration `yaml:"timeout"`
}

// Load reads every packs/roles/*.yaml file in fsys, keyed by role name. It
// reports every problem of every file at once, wrapping ErrInvalidRole.
func Load(fsys fs.FS) (map[string]Definition, error) {
	names, err := fs.Glob(fsys, path.Join(PacksDir, "roles", "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list role files: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no role files in %s/roles", ErrInvalidRole, PacksDir)
	}
	defs := map[string]Definition{}
	var errs []error
	for _, name := range names {
		d, err := loadFile(fsys, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		defs[d.Name] = d
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return defs, nil
}

func loadFile(fsys fs.FS, name string) (Definition, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Definition{}, fmt.Errorf("%w: %w", ErrInvalidRole, err)
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return Definition{}, fmt.Errorf("%w: %s: %w", ErrInvalidRole, name, err)
	}
	var p []string
	d := f.check(name, &p)
	if f.Prompt != "" {
		prompt, err := fs.ReadFile(fsys, path.Join(PacksDir, f.Prompt))
		switch {
		case err != nil:
			p = append(p, fmt.Sprintf("prompt: %v", err))
		case strings.TrimSpace(string(prompt)) == "":
			p = append(p, fmt.Sprintf("prompt: %s is empty", f.Prompt))
		}
		d.Instructions = strings.TrimSpace(string(prompt))
	}
	if f.OutputSchema != "" {
		schema, err := BundleSchema(fsys, path.Join(PacksDir, f.OutputSchema))
		if err != nil {
			p = append(p, fmt.Sprintf("output_schema: %v", err))
		}
		d.OutputSchema = schema
	}
	if len(p) > 0 {
		return Definition{}, fmt.Errorf("%w: %s: %s", ErrInvalidRole, name, strings.Join(p, "; "))
	}
	return d, nil
}

// check validates the fields that need no other file and appends problems
// to p.
func (f file) check(name string, p *[]string) Definition {
	add := func(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }
	if stem := strings.TrimSuffix(path.Base(name), ".yaml"); f.Role != stem {
		add("role %q must match the file name %q", f.Role, stem)
	}
	if !roleName.MatchString(f.Role) {
		add("role %q must match %s", f.Role, roleName)
	}
	if f.Model != config.TierFast && f.Model != config.TierStrong && f.Model != config.TierBest {
		add("model must be fast, strong or best, got %q", f.Model)
	}
	if f.Prompt == "" {
		add("prompt must be set")
	}
	if f.OutputSchema == "" {
		add("output_schema must be set")
	}
	if len(f.Tools) == 0 {
		add("tools must not be empty")
	}
	for _, t := range f.Tools {
		if _, ok := tools[t]; !ok {
			add("unknown tool %q", t)
		}
	}
	if f.WritePaths == nil {
		add("write_paths must be set; use [] for a read-only role")
	} else {
		checkWritePaths(f.Tools, *f.WritePaths, add)
	}
	if len(f.DefinitionOfDone) == 0 {
		add("definition_of_done must not be empty")
	}
	if f.MaxTurns < 0 {
		add("max_turns must not be negative")
	}
	if f.Timeout <= 0 {
		add("timeout must be a positive duration such as 30m")
	}
	var writePaths []string
	if f.WritePaths != nil {
		writePaths = *f.WritePaths
	}
	return Definition{
		Name:             f.Role,
		Tier:             f.Model,
		Tools:            f.Tools,
		WritePaths:       writePaths,
		Produces:         f.Produces,
		DefinitionOfDone: f.DefinitionOfDone,
		EscalateWhen:     f.EscalateWhen,
		MaxTurns:         f.MaxTurns,
		Timeout:          f.Timeout,
	}
}

func checkWritePaths(roleTools, writePaths []string, add func(string, ...any)) {
	for _, w := range writePaths {
		pat := strings.TrimPrefix(w, "!")
		if pat == "" || strings.HasPrefix(pat, "/") || slices.Contains(strings.Split(pat, "/"), "..") {
			add("write_paths: %q must be a glob relative to the repo root", w)
		}
	}
	if len(writePaths) == 0 {
		for _, t := range writeTools {
			if slices.Contains(roleTools, t) {
				add("tool %q needs write_paths; the role is read-only", t)
			}
		}
		return
	}
	if !slices.Contains(roleTools, "repo.write") {
		add("write_paths are set but tools lack repo.write")
	}
	if !slices.ContainsFunc(writePaths, func(w string) bool { return !strings.HasPrefix(w, "!") }) {
		add("write_paths have no include pattern")
	}
}
